// Client for the server's session status stream (Server-Sent Events at
// /api/sessions/events). A thin EventSource wrapper: it parses each status event
// and fans it out to a callback. The reconnect-resync policy (re-fetching the
// session list after a gap) is the consumer's job; onOpen fires on every
// (re)open so the consumer can trigger it. Pairs with the Go
// terminal.SessionManager EventsHandler.

import { nextBackoffDelay } from "./reconnect.js";

/** SessionInfo is one session's wire shape: the JSON object the session REST
 *  API (GET/POST /api/sessions) returns per session. Mirrors the Go
 *  terminal.SessionInfo — the two are kept in lockstep by hand (single 7-field
 *  type; flip to wiregen if this surface grows).
 *
 *  Three of the fields are title-shaped and they are not interchangeable:
 *  `title` is the RESOLVED display title, `pinnedTitle` is the USER's name (set
 *  and cleared only by a human action, outranking everything else), and
 *  `clientTitle` is a CLIENT-DERIVED automatic title a client asked the server to
 *  remember. */
export interface SessionInfo {
  readonly id: string;
  /** the session's current status. "working"/"idle"/"exited"/"crashed" are
   *  derived by the server; "input"/"done" are latched from a classified OSC 9
   *  notification; "failed" is OSC 9;4 progress state 2 and "warning" is state 4
   *  (iTerm2 semantics), both of which PERSIST until the program changes the
   *  progress state. "exited" and "crashed" split the session's end: "exited" is
   *  an ordinary one (status 0, or any exit the server itself caused — a closed
   *  session, the idle reaper, a server shutdown), "crashed" is a non-zero exit
   *  status or a terminating signal the program was not asked for, so a routine
   *  restart never renders as a failure. Neither is a progress state: they
   *  outrank everything, and nothing clears them. A consumer must tolerate an
   *  UNKNOWN status string from a newer server: the stream is parsed, not
   *  validated, and an unrecognised value is forwarded rather than dropped
   *  (forward compatibility across the wire floor). */
  readonly status:
    "working" | "idle" | "input" | "done" | "exited" | "crashed" | "failed" | "warning";
  readonly title: string;
  /** the raw client-derived automatic title (before the precedence baked into
   *  `title`); a consumer that treats the program's OSC window title as
   *  unreliable reads this instead of `title`. */
  readonly clientTitle?: string;
  /** the user's pinned name, set through PUT /api/sessions/{id}/pinned-title and
   *  cleared through DELETE on the same path. It outranks every automatic source
   *  in `title`; it is carried raw so a UI can tell that a pin EXISTS (to offer
   *  the automatic name again) rather than only seeing its effect. Absent or
   *  empty means the session has no user-set name. */
  readonly pinnedTitle?: string;
  readonly createdAt: string;
  /** true once the session has emitted a genuine activity signal — OSC 9;4
   *  progress (kiro-cli, Claude Code, …) or a classified OSC 9 notification.
   *  Sticky for the session's life. Consumers reveal the per-tab activity dot
   *  only when this is set; a program that emits no OSC 9 signal (a plain shell)
   *  keeps its tab dot hidden. */
  readonly reportsActivity?: boolean;
}

/** SessionStatus is one session's current status as carried on the status
 *  stream (SSE): the REST wire shape plus the stream-only removal marker. It
 *  mirrors the server's status event. */
export interface SessionStatus extends SessionInfo {
  /** true when the session is gone (closed or reaped); the consumer drops it. */
  readonly removed?: boolean;
  /** the OSC 9;4 percentage: -1 when absent or unknown (no sequence seen, or a
   *  state that carries none — clear/indeterminate), else 0-100. Paired with
   *  `status` to render a determinate bar. */
  readonly progressValue?: number;
  /** the OSC 9 notification message, delivered to EVERY subscriber whether or
   *  not the server has a status classifier installed. A notification is an
   *  EVENT, not a state: it does not latch a status, so a consumer that wants a
   *  status out of it decides that itself. */
  readonly notification?: string;
  /** the notification's sequence number (monotonic per session), so a repeated
   *  message is still recognised as a new event. */
  readonly notificationSeq?: number;
}

/** StatusStreamCallbacks are the consumer's hooks. Declared as function-typed
 *  properties (not method shorthand) so destructuring them is safe. */
export interface StatusStreamCallbacks {
  /** Called for each status event. */
  onStatus: (status: SessionStatus) => void;
  /** Called on every (re)open, including auto-reconnects, so the consumer can
   *  resync after a gap (the stream only carries future changes). */
  onOpen?: () => void;
  /** Called when the stream errors. A transient drop is auto-reconnected by
   *  EventSource; a permanent close (non-2xx / wrong content-type) is
   *  re-established by this module with capped backoff. */
  onError?: () => void;
}

/** EventSourceLike is the minimal surface used here, so the stream can be tested
 *  without a DOM EventSource (the default factory adapts a real one). */
export interface EventSourceLike {
  addEventListener: (type: string, listener: (event: MessageEvent) => void) => void;
  close: () => void;
  /** EventSource.readyState: 0 CONNECTING, 1 OPEN, 2 CLOSED. A permanent close
   *  (non-2xx response / wrong content-type / auth failure) sets CLOSED and
   *  native auto-reconnect never fires; the module re-establishes on CLOSED. */
  readonly readyState: number;
}

/** EventSourceFactory builds an EventSourceLike for a URL. */
export type EventSourceFactory = (url: string) => EventSourceLike;

export interface StatusStream {
  /** Closes the stream and stops reconnection. */
  close: () => void;
}

const defaultFactory: EventSourceFactory = (url) => {
  const es = new EventSource(url);
  return {
    addEventListener: (type, listener) => {
      es.addEventListener(type, listener as unknown as EventListener);
    },
    close: () => {
      es.close();
    },
    get readyState() {
      return es.readyState;
    },
  };
};

/** connectStatusStream opens the status stream at path and fans events out to
 *  cb. make defaults to a real EventSource; tests inject a fake. */
export function connectStatusStream(
  path: string,
  cb: StatusStreamCallbacks,
  make: EventSourceFactory = defaultFactory,
): StatusStream {
  let es: EventSourceLike;
  let closed = false;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  let backoffMs = 500;
  const { onOpen, onError } = cb;
  const open = (): void => {
    es = make(path);
    es.addEventListener("open", () => {
      backoffMs = 500; // reset backoff on a successful (re)open
      if (onOpen) {
        onOpen();
      }
    });
    es.addEventListener("message", (event) => {
      const raw: unknown = event.data;
      if (typeof raw !== "string") {
        return; // non-text frame, ignore
      }
      let status: SessionStatus;
      try {
        status = JSON.parse(raw) as SessionStatus;
      } catch {
        console.warn("vterm: dropped malformed status-stream frame");
        return; // skip a malformed frame, keep the stream
      }
      cb.onStatus(status);
    });
    es.addEventListener("error", () => {
      if (onError) {
        onError();
      }
      // Native EventSource auto-reconnect covers a transient drop but NOT a
      // permanent close (server restart -> proxy 502/non-2xx, auth 401/403,
      // wrong content-type), which sets readyState CLOSED and never retries.
      // Re-establish so per-tab status doesn't freeze until a page reload while
      // the terminal WS recovers on its own backoff. onOpen fires on the reopen,
      // so the consumer's resync still runs.
      if (es.readyState === 2 && !closed && reconnectTimer === undefined) {
        const step = nextBackoffDelay(backoffMs);
        reconnectTimer = setTimeout(() => {
          reconnectTimer = undefined;
          if (!closed) {
            open();
          }
        }, step.scheduledMs);
        backoffMs = step.nextBaseMs;
      }
    });
  };
  open();
  return {
    close: () => {
      closed = true;
      if (reconnectTimer !== undefined) {
        clearTimeout(reconnectTimer);
      }
      es.close();
    },
  };
}
