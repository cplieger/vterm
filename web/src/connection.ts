// WebSocket lifecycle with reliable input delivery across reconnects.
//
// This is the client → server half of the terminal protocol (the
// server → client half is the binary screen/scroll/modes wire format
// decoded by wire-binary.ts). It owns the socket, the reconnect
// backoff, and the resume/inputAck reliability layer.
//
// Protocol (resume / inputAck):
//   - Client maintains a resume key for the page lifetime, an `outbox` of
//     input bytes sent but not yet acknowledged, and a `bytesSent` counter.
//     Unmanaged mode: a per-tab UUID. Managed mode: `<serverSessionId>#<per-
//     client instance id>`, so every device/tab attached to one session owns
//     its OWN server-side input ledger (see resumeKey — shared ledgers acked
//     other devices' bytes to this one and corrupted its outbox).
//   - On WS open, client sends control: {type:"resume", sessionId, sentBytes}.
//   - Server replies with {type:"resumeAck", received:M}; subsequent
//     screen/scroll messages also carry inputAck = bytesReceived. Client
//     trims the outbox by acked count and retransmits the remainder.
//   - This handles the network-blip failure mode where ws.send() reports
//     success but TCP couldn't deliver before the connection broke.
//
// Outbox is bounded at MAX_OUTBOX_BYTES; once full, sendBinary refuses
// new input and surfaces the failure via onOutboxFull. This prevents
// holding-down a key during a long disconnect from growing the outbox
// without bound.

import { wsURL } from "./wsurl.js";
import { WS_PATH } from "./routes.js";
import { controlFrame } from "./wire.js";
import { decodeWireBinary } from "./wire-binary.js";
import {
  MIN_SUPPORTED_SERVER_WIRE_VERSION,
  WIRE_INCOMPATIBLE_CLOSE_CODE,
  WIRE_PROTOCOL_VERSION,
  type WireIncompatibility,
} from "./wire-compatibility.js";
import * as modes from "./modes.js";
// ONE page size for the whole feature. The transport used to declare its own
// identical literal, and nothing held the two equal: the fetch trigger anchors a
// request from the STORE's value while the transport clamps the length to its
// own, so tuning either one alone produces a shrunken page with a full-size
// anchor — the shape that leaves the rows under the reader blank while the far
// end of the gap fills.
import { PAGE_SIZE } from "./store.js";
import * as render from "./render.js";
import type { ControlMessage, ScrollMessage, ServerMessage } from "./types.js";
import { INITIAL_DELAY_MS, nextBackoffDelay } from "./reconnect.js";

/**
 * First wire revision with typed client→server framing (text = control,
 * binary = full-alphabet input). A resumeAck whose serverWireVersion is at
 * least this triggers the per-socket upgrade (design §4 phase 3). Mirrors
 * typedFramingMinVersion in the Go terminal package.
 */
const TYPED_FRAMING_MIN_VERSION = 4;

type ConnState =
  | { status: "disconnected" }
  | { status: "connecting"; sock: WebSocket; abort: AbortController }
  /**
   * `upgraded` is the v4 typed-framing latch for THIS socket (per-socket by
   * construction: the state object is replaced on every transition, so it can
   * never leak across a reconnect). false = v3 mode: controls go as
   * 0x00-sentinel binary frames and input is leading-NUL-split. true = the
   * resumeAck proved a v4 server and the text `upgrade` transition was sent:
   * controls go as text frames and input is raw full-alphabet binary.
   */
  | { status: "connected"; sock: WebSocket; abort: AbortController; upgraded: boolean }
  | { status: "reconnecting"; timer: ReturnType<typeof setTimeout>; delayMs: number }
  | { status: "incompatible" };

// The server's application close code for "the session's child process has
// exited" (terminal/terminal.go statusProcessExited). It marks a close as
// definitive — the session cannot produce output again — as opposed to a
// transient network drop that the backoff reconnect should heal. Private
// application range (4000-4999) per RFC 6455.
const PROCESS_EXITED_CLOSE_CODE = 4001;

// The server's application close code for "the manager does not know this
// session id" (terminal/terminal.go statusUnknownSession): reaped, closed
// elsewhere, or a restarted server. The server ACCEPTS the upgrade and closes
// with this code precisely so the client can read it (a pre-upgrade 404 is an
// opaque 1006 in browser JS). Like 4001 it is definitive — the session will
// never produce output — so it routes to the same no-reconnect ended path
// instead of an endless "Reconnecting…" flap.
const SESSION_UNKNOWN_CLOSE_CODE = 4004;

let connState: ConnState = { status: "disconnected" };
let reconnectDelay = INITIAL_DELAY_MS;
let lastSentCols = 0;
let lastSentRows = 0;
let wsPath: string = WS_PATH;

// --- Per-session resume state (the switching cache's connection half) ---
//
// Each server session carries its own reliable-input accounting: an outbox of
// unacked bytes, byte counters, and the last server boot-epoch it saw. Scoping
// this per session (rather than one module-global set) is what lets a tab switch
// reconnect to a different session without replaying the previous tab's unacked
// bytes onto it and without firing a false server-restart reset, because each
// session's epoch is compared only against its own bootEpoch (design section 8).
interface ResumeState {
  id: string; // server session id: the routing id (?session=); resumeKey derives the ledger key
  bytesSent: number; // total bytes ever passed to sendBinary for this session
  bytesAcked: number; // confirmed by server inputAck/resumeAck
  outbox: Uint8Array[]; // unacked chunks (sum of lengths = bytesSent - bytesAcked)
  outboxBytes: number; // running sum of outbox chunk lengths; keeps applyAck O(n) not O(n²)
  lastServerEpoch: number | null; // process-start nanos last seen for this session
  // Whether lastServerEpoch was SEEDED from persisted content rather than learned
  // from a resumeAck. A seeded epoch is a claim about content restored from
  // storage, and it is only worth anything if a server confirms or contradicts it —
  // so a resume that reports no epoch at all leaves it unverifiable, which is
  // handled as a restart. Learned epochs must not get that treatment: an ack
  // without an epoch from a server that never reports one is ordinary operation.
  epochSeeded: boolean;
  /** The session's last-announced DEC-mode state (P3: per-session mode
   *  mirror). Written on every inbound modes frame; restored synchronously
   *  into the modes singleton by setSession, so a keystroke in the switch
   *  window encodes under THIS session's modes, never the previous tab's.
   *  Power-on defaults until the session announces modes. */
  modes: modes.ModeSnapshot;
}

const sessions = new Map<string, ResumeState>();
// The session the live socket currently serves. null until the first connect or
// setSession; the unmanaged single-terminal path lazily creates a default
// session with a sessionStorage-backed id.
let activeId: string | null = null;
// managed = a consumer selected sessions explicitly via setSession, so the WS URL
// carries ?session=<id>. Unmanaged keeps the bare wsPath and a sessionStorage id,
// preserving the original single-terminal behavior and its iOS-resume semantics
// (sessionStorage survives iOS tab-suspend/BFCache, so an unmanaged reload
// resumes rather than orphaning its outbox). A tabbed shell is managed; it
// rebuilds tabs from GET /api/sessions on reload (section 17), so it needs no
// client-side id persistence.
let managed = false;

const SESSION_ID_KEY = "vterm-session-id";

// --- Per-client-instance resume key (P1: per-sender input-ack scoping) ---
//
// In managed mode the routing id (?session=) is SHARED by every device/tab
// attached to that session, but the server-side input ledger (bytesReceived,
// keyed by the resume frame's sessionId) must be PER SENDER: with a shared
// key, device B's input advances the one ledger and the server acks that
// total to device A, whose applyAck then trims bytes the server never
// received from A — silent input loss on A's next resume (the cross-device
// outbox corruption class). So the resume frame carries
// `<serverSessionId>#<clientInstanceId>` — the URL keeps the bare routing id
// (the server routes on it), while the registry keys A's and B's ledgers
// separately. The unmanaged path already has per-sender semantics (its
// sessionStorage id is per tab) and is unchanged.
//
// The instance id is crypto-random and page-lifetime (NOT persisted): a
// reload is a fresh sender whose ledger starts at zero, which matches its
// empty outbox. Lazy so a cryptoless environment throws on first CONNECT
// (as before), not at module import.
let clientInstanceId: string | null = null;

function resumeKey(st: ResumeState): string {
  if (!managed) {
    return st.id;
  }
  clientInstanceId ??= generateSessionId();
  return `${st.id}#${clientInstanceId}`;
}

function newResumeState(id: string): ResumeState {
  return {
    id,
    bytesSent: 0,
    bytesAcked: 0,
    outbox: [],
    outboxBytes: 0,
    lastServerEpoch: null,
    epochSeeded: false,
    modes: { ...modes.POWER_ON_MODES },
  };
}

function ensureState(id: string): ResumeState {
  let s = sessions.get(id);
  if (s === undefined) {
    s = newResumeState(id);
    sessions.set(id, s);
  }
  return s;
}

// activeState returns the ResumeState the live socket serves, lazily creating
// the default (unmanaged) session from a sessionStorage-backed id on first use.
function activeState(): ResumeState {
  activeId ??= loadOrCreateSessionId();
  return ensureState(activeId);
}

// --- Client-side liveness (bug 2 defense-in-depth) ---
//
// On iOS wake, visibilitychange + pageshow fire and call reconnectNow(),
// which is the primary fix. But a socket can also go silently half-open
// without any wake event (a NAT/idle timeout on a backgrounded-then-
// foregrounded tab, a flaky network that drops the path without a close
// frame). The socket then reads OPEN forever and delivers nothing. The
// server's ping loop notices the dead client, but those are WS-protocol
// pings the browser answers without surfacing to JS, so the client can't
// see them. So the client runs its own probe: after a stretch of silence
// it sends an app-level ping; the server echoes a pong. Any inbound frame
// (the pong, or normal output) proves the socket is alive and clears the
// probe. If the probe goes unanswered, the socket is stale and we
// reconnectNow() — which resumes by absolute index, so nothing is lost or
// duplicated. The probe is what distinguishes "idle but alive" from
// "dead": without it, a quiet-but-healthy terminal would reconnect-flap.
let lastActivityAt = 0; // Date.now() of the last inbound frame (any kind)
let probeSentAt = 0; // Date.now() the outstanding probe ping was sent; 0 = none
let heartbeatTimer: ReturnType<typeof setInterval> | null = null;

/** How often liveness is evaluated. */
const HEARTBEAT_INTERVAL_MS = 5_000;
/** Inbound silence that must elapse before we actively probe with a ping. */
const IDLE_BEFORE_PROBE_MS = 10_000;
/** How long an unanswered probe is tolerated before declaring the socket stale. */
const PONG_TIMEOUT_MS = 7_000;

// --- demand-paged scrollback: per-socket fetch state (docs/paged-scrollback.md) ---

/** Requests served back-to-back before the client's own bucket throttles. */
const HISTORY_BURST = 4;

/**
 * Client-side refill interval. Deliberately SLOWER than the server's floor
 * (1.5 s): independent clocks and latency jitter compress arrival spacing, so
 * identical constants would let a healthy client trip the server's silent drop.
 * The slack absorbs that by construction rather than by coincidence.
 */
const HISTORY_REFILL_MS = 2_000;

/**
 * How long to wait for a page before releasing single-flight and retrying. It
 * fires FIRST by design: the server's write context is 10 s, and in
 * coder/websocket a write-deadline expiry CLOSES the socket, so a server that
 * gave up first would turn every slow reply into a reconnect and make this
 * retry path unreachable.
 */
const HISTORY_DATA_TIMEOUT_MS = 8_000;

/** The smallest request the adaptive budget will shrink to. */
const HISTORY_MIN_PAGE = 125;

/**
 * The server's clamp on `replayMax`, mirrored here so the client sends the
 * value the server will actually honor. The equality is load-bearing: the
 * replay-jump prediction computes `committed - sentReplayMax`, so a server
 * honoring something smaller would place the real replay start above the
 * prediction and leave a genuine jump undetected (§4.5).
 */
export const MAX_REPLAY_LINES = 2000;

/**
 * Everything the fetch controller owns for ONE socket. Held in the socket's
 * closure lifetime — replaced wholesale on reconnect — because every field is
 * a statement about that socket: a surviving empty bucket would stall against a
 * fresh server bucket, a carried-over capability would page against a server
 * that never declared it, and a carried-over 125-line budget would punish a new
 * link for the old one's congestion.
 */
interface HistoryState {
  /** The server declared paging on this socket's resume ack. */
  paging: boolean;
  /** An ack has been processed; before that, content frames are suppressed. */
  acked: boolean;
  /** The in-flight request's window, or null when nothing is outstanding. */
  inFlight: { fromAbs: number; end: number } | null;
  /** Token bucket fill and its last refill instant. */
  tokens: number;
  lastRefill: number;
  /** The adaptive request budget, and the recovery ceiling above it. */
  effMax: number;
  budgetCeiling: number;
  /** Timers: the in-flight data timeout, and the coalesced pending demand. */
  dataTimer: ReturnType<typeof setTimeout> | null;
  demandTimer: ReturnType<typeof setTimeout> | null;
  /** The values this socket SENT on its resume, which its reply answers. */
  sentHaveThrough: number;
  sentReplayMax: number | null;
}

function newHistoryState(): HistoryState {
  return {
    paging: false,
    acked: false,
    inFlight: null,
    // The bucket starts FULL so a fresh socket can burst; the server's bucket
    // does the same, so the two agree on the first few requests.
    tokens: HISTORY_BURST,
    lastRefill: Date.now(),
    effMax: PAGE_SIZE,
    budgetCeiling: PAGE_SIZE,
    dataTimer: null,
    demandTimer: null,
    sentHaveThrough: -1,
    sentReplayMax: null,
  };
}

let history: HistoryState = newHistoryState();

/** Cancel both timers and release single-flight. */
function clearHistoryTimers(): void {
  if (history.dataTimer !== null) {
    clearTimeout(history.dataTimer);
    history.dataTimer = null;
  }
  if (history.demandTimer !== null) {
    clearTimeout(history.demandTimer);
    history.demandTimer = null;
  }
}

/**
 * Reset the fetch state for a new socket: capability, single-flight, pacing and
 * the adaptive budget all go together, atomically. Splitting them is how a
 * client ends up paging against a server that never declared it, or bursting
 * into a depleted server bucket.
 */
function resetHistoryForSocket(): void {
  clearHistoryTimers();
  history = newHistoryState();
}

/**
 * Spend a pacing token, returning the wait in ms when the bucket is empty (so
 * the caller can arm the coalesced pending demand for exactly that instant)
 * or 0 when a token was granted.
 */
function takeHistoryToken(): number {
  const now = Date.now();
  history.tokens = Math.min(
    HISTORY_BURST,
    history.tokens + (now - history.lastRefill) / HISTORY_REFILL_MS,
  );
  history.lastRefill = now;
  if (history.tokens < 1) {
    return Math.ceil((1 - history.tokens) * HISTORY_REFILL_MS);
  }
  history.tokens -= 1;
  return 0;
}

/**
 * Maximum bytes we keep in the outbox before refusing new input. 1
 * MiB at typical typing rates is hours of held keys; fast enough to
 * accept any normal disconnect, low enough that an offline tab can't
 * silently grow memory unbounded.
 */
export const MAX_OUTBOX_BYTES = 1 << 20;

function loadOrCreateSessionId(): string {
  // sessionStorage is per-tab and survives most iOS lifecycle events
  // (suspend/resume, BFCache restore, page reload). It does NOT survive
  // a true tab close + reopen, which is the desired semantic: a fresh
  // tab should be a fresh terminal session, not a resume of an older one.
  try {
    const existing = sessionStorage.getItem(SESSION_ID_KEY);
    if (existing) {
      return existing;
    }
    const fresh = generateSessionId();
    sessionStorage.setItem(SESSION_ID_KEY, fresh);
    return fresh;
  } catch {
    // Private mode or storage disabled — fall back to in-memory only.
    // Reload-as-new-session semantics in this fallback path are
    // unavoidable; the outbox-clear safeguard in handleResumeAck below
    // protects against duplicate retransmission when the server returns
    // bytesReceived=0 for a session it doesn't recognize.
    return generateSessionId();
  }
}

// Exported for unit testing of the RNG fallback. Not part of the
// stable client API surface; callers use loadOrCreateSessionId.
export function generateSessionId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  // Fallback when crypto.randomUUID is unavailable. randomUUID requires a
  // secure context (HTTPS/localhost); getRandomValues does not, so it
  // covers plain-HTTP origins while still being a CSPRNG. sessionId is a
  // resume token the server trusts to re-attach a client to its prior
  // session, so it must not be predictable — Math.random() (a non-crypto
  // PRNG whose state is recoverable from output) would allow guessing
  // another client's session. Emit 16 random bytes as hex (128 bits).
  if (typeof crypto !== "undefined" && typeof crypto.getRandomValues === "function") {
    const bytes = new Uint8Array(16);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  }
  // No Web Crypto at all: refuse rather than mint a guessable token.
  throw new Error("vterm: no cryptographically secure RNG available for session id");
}

export interface Callbacks {
  onMessage(msg: ServerMessage): void;
  onOpen(): void;
  onClose(): void;
  onConnecting?(): void;
  onOutboxFull?(): void;
  /** Fired instead of onClose when the server closes with a DEFINITIVE
   *  application close code — process-exited (4001: the session's child
   *  process has ended) or unknown-session (4004: the server does not know
   *  the id at all — reaped, closed elsewhere, or a restarted server). In
   *  both cases the session will never produce output again, so a transient
   *  treatment could only earn the same close code forever (the endless
   *  "Reconnecting…" flash). When this callback is wired, the module does
   *  NOT auto-reconnect that socket and does not call onClose for it. The
   *  consumer decides what "ended" looks like (banner, tab state) and may
   *  still reconnect explicitly (setSession / reconnectNow) to re-view a
   *  dead session's final screen. When absent, every close keeps the legacy
   *  transient treatment (onClose + backoff reconnect), so existing
   *  consumers are unaffected until they opt in. */
  onProcessExit?(): void;
  /** Fired when the client's queued input can no longer be trusted, which
   *  happens two ways: the server's boot-epoch in resumeAck differs from the
   *  one observed on a previous connection (the server restarted), or the
   *  server no longer holds this session's input ledger AND the outbox still
   *  held unacked bytes. A forgotten ledger whose every byte was already acked
   *  does NOT fire this — nothing was lost, and reporting it made a phone
   *  waking from sleep announce a restart that never happened. By the time this
   *  fires, the connection module has already reset bytesSent/bytesAcked/outbox
   *  so subsequent input starts from zero. UI should surface a banner so the
   *  user knows recent input may have been lost. */
  onServerRestart?(): void;
  /** Fired when a resumeAck carries an explicit server revision outside
   *  [MIN_SUPPORTED_SERVER_WIRE_VERSION, WIRE_PROTOCOL_VERSION]. A newer
   *  server warns but continues because it may retain this client's baseline.
   *  A below-floor server also fires onWireIncompatible and stops the socket.
   *  Version-silent servers never fire either callback. */
  onWireVersionMismatch?(server: number, client: number): void;
  /** Fired when the module definitively stops a socket for an incompatible
   *  declared wire revision, either from resumeAck metadata or close code
   *  4002. Automatic and wake-triggered reconnects remain blocked until an
   *  explicit disconnect clears the terminal state (normally a page reload).
   *  The callback is optional; refusal is enforced even when no UI consumes
   *  it. */
  onWireIncompatible?(details: WireIncompatibility): void;
  computeSize(): { cols: number; rows: number };
  /** Returns the highest absolute line index the client will not ask the server
   *  to re-send, or -1 to request a full retained replay. Sent as `haveThrough`
   *  on resume so the server replays only the lines missed (e.g. printed while
   *  the device slept).
   *
   *  NOT simply the highest index held. A client that holds a row PROVISIONALLY
   *  — a screen row the application drew, which the server has not committed at
   *  that absolute index — must not claim it, or the replay starts above it and
   *  the provisional copy is never corrected. Wire this to
   *  `render.getReplayBoundary`, which answers exactly that question.
   *  When absent, the client requests a full retained replay (-1). */
  getHaveThrough?(): number;
  /** Fired on resume with the server's retained-history bounds: `committed`
   *  is one past the newest retained line, `oldest` the oldest retained
   *  absolute index. The consumer forwards these to the renderer/store so it
   *  can tell a genuine history trim (the server evicted lines the client was
   *  missing) from a still-loading state. Resync guard 8.2.2. */
  onResumeBounds?(committed: number, oldest: number): void;
  /**
   * How many lines of resume replay this client wants at most — a REFINEMENT of
   * the protocol ceiling, never an opt-out: the server bounds the replay to
   * MAX_REPLAY_LINES whether or not this is wired, and the client sends that
   * ceiling when it is absent so both sides predict the same replay start.
   * Return the depth the consumer intends to keep resident.
   *
   * The value is clamped to MAX_REPLAY_LINES before it is sent, because the
   * client's replay-jump prediction is only exact while the SENT value equals the
   * honored one (docs/paged-scrollback.md §4.5).
   */
  getReplayMax?(): number | null;
  /** Fired when a correlated history page arrives, carrying the reply and how
   *  the client should read it. The consumer forwards `msg` to the store's
   *  `applyHistoryScroll` (which classifies it as browse cache), and applies
   *  `raiseFloorTo` when present — a clamped or empty reply is the server
   *  proving nothing at or below that index survives. */
  onHistoryReply?(msg: ScrollMessage, raiseFloorTo: number | null): void;
  /** Fired once per resume ack with everything the store's single ack
   *  transition needs, including the values this socket SENT (which the
   *  server's reply is a function of) and the capability the server declared.
   *  The consumer builds a closure over its store and renderer viewport and
   *  calls `store.applyResumeAck(...)`; see docs/paged-scrollback.md §4.5 for
   *  why the five steps must not be split across separate callbacks. */
  onResumeTransition?(ack: {
    epochChanged: boolean;
    committed: number | null;
    serverOldest: number | null;
    paging: boolean;
    sentHaveThrough: number;
    sentReplayMax: number | null;
  }): void;
  /** The store port's two solicited-window methods. `connection` is store-blind
   *  by design, but the events that open and close a solicited window all
   *  originate here (send, data timeout, socket close), so the store needs a
   *  bridge rather than a reach-in. */
  noteSolicited?(fromAbs: number, end: number): void;
  clearSolicited?(): void;
  /** Re-run the fetch trigger: the coalesced pending demand fired (a paced
   *  denial's refill instant, or a data timeout's retry). The controller
   *  re-evaluates its full guard set rather than replaying a stale range. */
  onHistoryRetry?(): void;
  /** The size to announce to the server BEFORE asking to resume, or null when
   *  the client cannot yet measure itself trustworthily (web fonts still
   *  loading, a viewport transition in flight).
   *
   *  Establishing the size first is what makes a resume snapshot arrive at the
   *  client's own geometry: the server reflows, and only then computes the
   *  window and the replay. Without it the client is answered at whatever size
   *  the session last held, repaints, and then its resize lands afterwards —
   *  which on a program that redraws on SIGWINCH means the redraw interleaves
   *  with the replay it just applied.
   *
   *  Returning null is the explicit "not yet" answer and is byte-identical to
   *  omitting the callback: nothing is sent, the server keeps its current size,
   *  and the consumer's own later sendResize() (typically once the viewport
   *  settles) establishes it. Announcing an untrustworthy measurement is worse
   *  than announcing none, because it costs a second resize and therefore a
   *  second redraw. */
  initialSize?(): { cols: number; rows: number } | null;
  /** Optional WebSocket endpoint path (default "/ws"). vibekit serves
   *  the shell at "/api/shell/ws"; web-terminal-kiro at "/ws". */
  wsPath?: string;
}

let cb: Callbacks | null = null;

/**
 * The members this module answers for ITSELF, from the renderer.
 *
 * Every one of them is a fact about local state, not a decision: what this client
 * already holds, how much replay it wants, where to route a resume ack or a history
 * reply. The renderer is the layer that knows, because several carry a viewport
 * reading only it can take, and both modules belong to this library. A consumer
 * that supplied them was choosing between engine accessors on the engine's behalf,
 * which is how `getHaveThrough` came to be wired to the highest index HELD rather
 * than the highest the server had confirmed: the resume then claimed rows the
 * application had merely drawn, and a frozen copy of its input box appeared in
 * scrollback on almost every reattach.
 *
 * A consumer may still override any of them, and `getReplayMax` is the one where
 * that is expected: how much history a client wants resident is its policy, not the
 * engine's fact. The rest exist as overrides only because removing them from
 * `Callbacks` would break a published surface; treat them as deprecated and prefer
 * supplying nothing.
 *
 * Read lazily, per resume, rather than captured at init: the renderer's answers are
 * functions of whichever store is currently bound, which a tab switch changes.
 */
function engineDefaults(): Partial<Callbacks> {
  return {
    getHaveThrough: render.getReplayBoundary,
    getReplayMax: render.replayMaxForResume,
    onResumeTransition: render.applyResumeTransition,
    onResumeBounds: render.noteResumeBounds,
    onHistoryReply: render.handleHistoryReply,
    noteSolicited: render.noteSolicited,
    clearSolicited: render.clearSolicited,
    onHistoryRetry: render.maybeFetchHistory,
  };
}

export function init(callbacks: Callbacks): void {
  // Consumer last: an explicit member wins, an absent one gets the engine's own.
  // The alternative, leaving them absent, is not neutral — the resume send reads
  // `cb?.getHaveThrough?.() ?? -1`, so silence asks for a full bounded replay on
  // every attach, which is the worst answer available rather than the safe one.
  cb = { ...engineDefaults(), ...callbacks };
  if (callbacks.wsPath !== undefined) {
    wsPath = callbacks.wsPath;
  }
}

/**
 * sendBinary queues data for delivery. Returns true if accepted, false
 * if the outbox is full (caller should surface a UI signal that input
 * was dropped). Always copies the input to defend against caller-side
 * buffer reuse.
 *
 * Input alphabet: any byte sequence is deliverable, including leading NULs.
 * On a v4-upgraded socket the bytes go out verbatim in one binary message;
 * on a v3-mode socket each leading 0x00 is emitted as its own solitary
 * 1-byte frame (see sendInputFrames) so nothing this module sends can be
 * misread as a control frame — byte order and count are preserved either
 * way, and servers with the parse-fallback deliver every byte to the PTY.
 */
export function sendBinary(data: Uint8Array): boolean {
  const st = activeState();
  if (st.outboxBytes + data.length > MAX_OUTBOX_BYTES) {
    cb?.onOutboxFull?.();
    return false;
  }
  // Always go through the active session's outbox. Bytes leave it only when the
  // server explicitly acks them — guarantees correct retransmission after a
  // network blip even if ws.send() reported success.
  const copy = new Uint8Array(data); // defensive copy (caller may reuse buffer)
  st.outbox.push(copy);
  st.outboxBytes += copy.length;
  st.bytesSent += copy.length;
  if (connState.status === "connected") {
    sendInputFrames(connState.sock, connState.upgraded, copy);
  }
  return true;
}

/**
 * sendInputFrames is the ONE encoder for PTY input bytes — both the live
 * sendBinary path and retransmitOutbox route through it so the two can never
 * disagree on framing (design §6). On a v4-upgraded socket the bytes go out
 * verbatim as a single binary message (full alphabet — the server never
 * inspects a sentinel after the latch). On a v3-mode socket (old server, or
 * the pre-upgrade window of any socket), each leading 0x00 byte is emitted as
 * its own solitary 1-byte message so no frame the client sends can ever be
 * misread as a control frame; servers with the parse-fallback deliver the
 * solitary NUL to the PTY, and byte accounting is unchanged either way
 * (splitting alters message count, never byte count or order).
 */
function sendInputFrames(sock: WebSocket, upgraded: boolean, chunk: Uint8Array): void {
  let rest = chunk;
  if (!upgraded) {
    while (rest.length > 0 && rest[0] === 0x00) {
      sock.send(new Uint8Array([0x00]).buffer);
      rest = rest.subarray(1);
    }
  }
  if (rest.length > 0) {
    sock.send(rest.buffer.slice(rest.byteOffset, rest.byteOffset + rest.byteLength) as ArrayBuffer);
  }
}

/**
 * textControl encodes a control message for a v4-upgraded socket: bare JSON in
 * a WebSocket TEXT frame (the transport's message type is the discriminator;
 * no sentinel byte). The binary 0x00-sentinel form (controlFrame) remains the
 * bootstrap/fallback encoding for v3-mode sockets.
 */
function textControl(msg: ControlMessage): string {
  return JSON.stringify(msg);
}

function sendControl(msg: ControlMessage): void {
  if (connState.status !== "connected") {
    return;
  }
  if (connState.upgraded) {
    connState.sock.send(textControl(msg));
    return;
  }
  connState.sock.send(controlFrame(msg));
}

/**
 * The consumer's announced bootstrap size, validated, or null when there is
 * none to announce.
 *
 * Isolated for two reasons. The validation: the server FLOORS an out-of-range
 * resize rather than dropping it (a 0 becomes the minimum size), so a malformed
 * value must be rejected here rather than silently resizing the session. And the
 * try/catch: this runs inside the socket's open handler after the connect
 * timeout has been cleared and BEFORE the resume is sent, so a consumer callback
 * that throws would otherwise abort the handler, leave the resume unsent and the
 * connection state stuck at "connecting" with no timeout left to rescue it. A
 * broken provider degrades to "announce nothing", which is the documented
 * behavior of omitting it.
 */
function readInitialSize(): { cols: number; rows: number } | null {
  let size: { cols: number; rows: number } | null;
  try {
    size = cb?.initialSize?.() ?? null;
  } catch (err) {
    console.warn("vterm: initialSize provider threw; announcing no size", err);
    return null;
  }
  if (
    size === null ||
    !Number.isInteger(size.cols) ||
    !Number.isInteger(size.rows) ||
    size.cols <= 0 ||
    size.rows <= 0
  ) {
    return null;
  }
  return { cols: size.cols, rows: size.rows };
}

export function sendResize(): void {
  if (connState.status !== "connected" || !cb) {
    return;
  }
  const { cols, rows } = cb.computeSize();
  if (cols === lastSentCols && rows === lastSentRows) {
    return;
  }
  lastSentCols = cols;
  lastSentRows = rows;
  sendControl({ type: "resize", cols, rows });
}

// resetLedger drops a session's reliable-input accounting. Called when the
// server's boot epoch changes (restart) or when the server no longer recognizes
// the session -- both invalidate the local bytesSent/bytesAcked/outbox state.
// The three entry points below differ only in whether the UI hears about it.
// resetLedger drops a session's reliable-input accounting. Called whenever the
// server no longer holds the ledger these counters were keyed to (a restart, an
// idle-GC'd resume key, a session the server does not recognize): its
// replacement counts from zero, so keeping non-zero counters would make every
// later ack read as stale in applyAck and the outbox would never trim again.
function resetLedger(st: ResumeState): void {
  st.bytesSent = 0;
  st.bytesAcked = 0;
  st.outbox.length = 0;
  st.outboxBytes = 0;
}

// resetSessionAfterRestart resets the ledger and tells the UI the server
// restarted. Reserved for a boot-epoch change, which is the one cause where
// that sentence is literally true.
function resetSessionAfterRestart(st: ResumeState): void {
  resetLedger(st);
  cb?.onServerRestart?.();
}

// resetForgottenLedger handles a ledger the server no longer holds (the
// explicit ledgerLost flag, or the old-server received=0 heuristic). The server
// forgetting a ledger is not a restart and is not by itself a loss: only the
// UNACKED remainder was ever at risk. So the UI is notified only when there was
// one, and a session whose every sent byte was already acked resets silently —
// nothing can be replayed and nothing was lost.
//
// The silent case is the common one, and reporting it was a real defect: an
// iPad whose screen slept for half an hour woke, reconnected, and announced a
// server restart that never happened. Server-side, the ledger it claimed had
// received EXACTLY as many bytes as the client claimed to have sent (10531 ==
// 10531 in the incident logs), i.e. an empty outbox and a fully-applied
// session, and the banner still fired.
function resetForgottenLedger(st: ResumeState): void {
  const lostUnacked = st.bytesSent > st.bytesAcked;
  resetLedger(st);
  if (lostUnacked) {
    cb?.onServerRestart?.();
  }
}

// applyAck drops chunks from the front of the session's outbox until the
// running total of unacked bytes matches (bytesSent - newAck). Runs in
// O(chunks_dropped) by tracking outboxBytes incrementally rather than
// re-summing on every loop iteration.
function applyAck(st: ResumeState, received: number): void {
  if (received <= st.bytesAcked) {
    return;
  }
  st.bytesAcked = Math.min(received, st.bytesSent);
  const targetUnacked = st.bytesSent - st.bytesAcked;
  while (st.outbox.length > 0 && st.outboxBytes > targetUnacked) {
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion -- length checked above
    const head = st.outbox[0]!;
    const dropFromHead = st.outboxBytes - targetUnacked;
    if (head.length <= dropFromHead) {
      st.outbox.shift();
      st.outboxBytes -= head.length;
    } else {
      st.outbox[0] = head.subarray(dropFromHead);
      st.outboxBytes -= dropFromHead;
      break;
    }
  }
}

// On reconnect, after sending the resume control message and getting resumeAck,
// replay anything still in the session's outbox over its (now open) socket. The
// server has adjusted bytesAcked already — only unacked bytes remain. Routed
// through sendInputFrames so replay framing always matches live-send framing
// (v3-mode leading-NUL split vs v4 verbatim — design §6).
function retransmitOutbox(sock: WebSocket, upgraded: boolean, st: ResumeState): void {
  for (const chunk of st.outbox) {
    sendInputFrames(sock, upgraded, chunk);
  }
}

function scheduleReconnect(): void {
  // Nothing to schedule when the state already owns a path back to a socket: a
  // backoff timer is pending ("reconnecting"), or a socket exists that will
  // drive its own retry from its close handler or its connect timeout
  // ("connecting"/"connected"). Both callers set "disconnected" before calling
  // cb.onClose(), so a consumer that reconnects from its own close hook lands
  // here at "connecting" — and scheduling over it would OVERWRITE connState,
  // hiding that socket from connect()'s double-call guard, which then never
  // aborts it: the timer's connect() adds a second live socket with the first
  // one's listeners still bound, i.e. the duplicate server connection and
  // double delivery documented inside connect().
  if (
    connState.status === "reconnecting" ||
    connState.status === "connecting" ||
    connState.status === "connected"
  ) {
    return;
  }
  const step = nextBackoffDelay(reconnectDelay);
  reconnectDelay = step.nextBaseMs;
  const timer = setTimeout(() => {
    connState = { status: "disconnected" };
    connect();
  }, step.scheduledMs);
  connState = { status: "reconnecting", timer, delayMs: step.scheduledMs };
}

function cancelScheduledReconnect(): void {
  if (connState.status === "reconnecting") {
    clearTimeout(connState.timer);
    connState = { status: "disconnected" };
  }
}

// markActivity records that the socket just delivered a frame. Any frame —
// the pong, a screen update, anything — proves the socket is alive, so it
// refreshes the liveness clock and clears any outstanding probe.
function markActivity(): void {
  lastActivityAt = Date.now();
  probeSentAt = 0;
}

function startHeartbeat(): void {
  stopHeartbeat();
  markActivity();
  heartbeatTimer = setInterval(heartbeatTick, HEARTBEAT_INTERVAL_MS);
}

function stopHeartbeat(): void {
  if (heartbeatTimer !== null) {
    clearInterval(heartbeatTimer);
    heartbeatTimer = null;
  }
  probeSentAt = 0;
}

// heartbeatTick is the one place that decides a connected socket is stale.
// It never touches scrollTop, never reconnects a healthy socket, and never
// probes a backgrounded tab (timer throttling makes hidden-tab timing
// meaningless, and the wake path handles foregrounding). Its only actions
// are: send a probe after enough silence, or reconnect after a probe goes
// unanswered.
function heartbeatTick(): void {
  if (connState.status !== "connected") {
    return;
  }
  // A hidden tab is handled by visibilitychange/pageshow on wake; probing it
  // is pointless (its timers are throttled or frozen) and could fire stale.
  if (typeof document !== "undefined" && document.visibilityState === "hidden") {
    return;
  }
  const now = Date.now();
  if (probeSentAt > 0) {
    if (now - probeSentAt >= PONG_TIMEOUT_MS) {
      // The probe drew no reply (nor any other frame) in the grace window:
      // the socket is stale. Tear it down and resume by absolute index.
      probeSentAt = 0;
      reconnectNow();
    }
    return;
  }
  if (now - lastActivityAt >= IDLE_BEFORE_PROBE_MS) {
    probeSentAt = now;
    sendControl({ type: "ping" });
  }
}

// teardown aborts and closes the live socket (if any) and stops the heartbeat
// and any scheduled reconnect, leaving the module disconnected. It never touches
// per-session resume state, so a later connect() resumes cleanly. Shared by
// reconnectNow (reconnects after) and disconnect/forgetSession (do not).
function teardown(): void {
  if (connState.status === "connecting" || connState.status === "connected") {
    // Abort BEFORE close: aborting detaches all listeners on the existing
    // sock, so frames arriving between close() and the close handshake aren't
    // processed twice (the iPad-wake duplicate-output race).
    connState.abort.abort();
    try {
      connState.sock.close();
    } catch {
      /* ignore */
    }
  }
  stopHeartbeat();
  resetHistoryForSocket();
  cb?.clearSolicited?.();
  cancelScheduledReconnect();
  connState = { status: "disconnected" };
}

export function reconnectNow(): void {
  if (connState.status === "incompatible") {
    return;
  }
  // Unconditional teardown. On iOS wake (visibilitychange + pageshow), the
  // socket frequently reads OPEN/"connected" for a while but is actually a
  // zombie — the OS froze it during sleep and frames printed meanwhile never
  // arrive. The old early-return on "connected" trusted that stale state and
  // skipped the reconnect, which is exactly why content printed during sleep
  // stayed missing until a manual refresh (bug 2). So we never trust the
  // current state on a wake: abort + close whatever socket exists and
  // reconnect. The resume protocol (by absolute index) then backfills exactly
  // the missed lines, so a reconnect over a still-healthy socket is a cheap,
  // duplicate-free no-op rather than a risk.
  teardown();
  connect();
}

/**
 * setSession switches the live socket to a different server session, keeping
 * every session's resume state intact (design section 5, the switch). The
 * current socket is torn down (its outbox and byte counters preserved for a
 * later switch back) and a fresh socket connects to `id` with its own resume
 * state, sending `?session=id`. Calling this marks the module "managed": the WS
 * URL then carries the session id. A no-op when `id` is already the active,
 * connected session.
 */
export function setSession(id: string): void {
  managed = true;
  const target = ensureState(id);
  if (id === activeId && (connState.status === "connected" || connState.status === "connecting")) {
    return; // already serving this session
  }
  activeId = id;
  // Restore the target session's DEC-mode mirror SYNCHRONOUSLY (P3): a
  // keystroke fired in the switch window — after this call returns, before
  // the new session's modes frame arrives — must encode under the target's
  // last-known modes (power-on defaults for a session never seen), never
  // under the previous tab's. The kernel already disarms every other latched
  // input class on switch (composition, sticky-Ctrl); this was the one it
  // could not reach.
  modes.applySnapshot(target.modes);
  reconnectNow();
}

/**
 * forgetSession drops a session's resume state (on tab close, design section 17).
 * If it was the active session, the live socket is torn down without
 * reconnecting; the shell then selects another tab via setSession.
 */
export function forgetSession(id: string): void {
  sessions.delete(id);
  if (id === activeId) {
    activeId = null;
    teardown();
  }
}

/**
 * Seed the server boot epoch a session's PERSISTED content belongs to, before
 * connecting.
 *
 * Restart detection is otherwise in-memory: the first resumeAck of a page load
 * records the epoch with nothing to compare against, and only a LATER mismatch
 * fires onServerRestart. That is correct for a store built during this page's
 * lifetime, and actively wrong for one hydrated from disk — a restarted server
 * begins its absolute indices again at 0, so a hydrated store would be presented
 * as live while the new session's low-index output was silently refused by the
 * store's staleness guard.
 *
 * Seeding gives the first resumeAck something to compare against, so the
 * existing restart path fires and resets the stale content exactly as it would
 * mid-session. A consumer that hydrates a store MUST call this with the
 * snapshot's `serverEpoch` (see StoreSnapshot in store.ts).
 *
 * Ignores a zero or non-finite epoch: those mean "no epoch was ever recorded",
 * which is indistinguishable from a version-silent server, and seeding a
 * sentinel would make the next real epoch look like a restart.
 *
 * Also ignores a session that already knows its epoch. "Seed" is the whole
 * contract: this exists to give the FIRST resumeAck something to compare against,
 * and overwriting a value a server already reported would make a genuine restart
 * look like agreement, or a live session look restarted. A caller that reaches
 * here twice, or after connecting, has a bug the library can simply refuse.
 */
export function adoptPersistedEpoch(sessionId: string, epoch: number): void {
  if (!Number.isFinite(epoch) || epoch === 0) {
    return;
  }
  const st = ensureState(sessionId);
  if (st.lastServerEpoch !== null) {
    return;
  }
  st.lastServerEpoch = epoch;
  // Marked as a CLAIM about restored content, not an observation. A resume that
  // reports no epoch cannot confirm it, and unverifiable restored content is
  // handled as a restart — see handleResumeAck. This is the one hole the epoch
  // comparison alone leaves: a server downgraded to reporting no epoch between the
  // save and the load never contradicts the seeded value, so the stale store would
  // otherwise be presented as live and then refuse the new session's low indices.
  st.epochSeeded = true;
}

/**
 * The current adaptive request budget: how many lines the next page request
 * should ask for. The fetch trigger lives in the renderer (the only layer that
 * maps scroll position to absolute indices) while this budget is per-SOCKET
 * state owned here, and this accessor is the only channel between them.
 *
 * The trigger must read it for BOTH the request's length and its ANCHOR. Using
 * it for the length alone is a real defect, not a nicety: an anchor computed
 * from the full page size while the length is shrunken serves a range that ends
 * far from the reader, so a slow link heals the far end of a gap and leaves the
 * rows under the viewport blank (docs/paged-scrollback.md §4.2).
 */
export function historyBudget(): number {
  return history.effMax;
}

/** Whether this socket's server declared demand-paged scrollback. */
export function historyPagingAvailable(): boolean {
  return history.paging && history.acked;
}

/** Whether a page request is currently outstanding (single-flight). */
export function historyRequestInFlight(): boolean {
  return history.inFlight !== null;
}

/**
 * Request a page of history: at most `maxLines` lines starting at `fromAbs`.
 *
 * Returns true when the request went out. It can decline for four reasons, all
 * of them normal: the server never declared paging, an ack has not arrived yet,
 * a request is already in flight (single-flight), or the pacing bucket is empty.
 * The last case ARMS the coalesced pending demand — a denied trigger is not
 * merely dropped, because on an idle session no future scroll or flush event
 * would ever re-fire it and a byte-short continuation would stall forever.
 *
 * The caller (the renderer's controller) owns the geometry; this owns the
 * transport, single-flight, pacing, the timers and the adaptive budget.
 */
export function requestHistory(fromAbs: number, maxLines: number): boolean {
  if (!history.paging || !history.acked) {
    return false;
  }
  if (history.inFlight !== null) {
    return false;
  }
  if (
    !Number.isSafeInteger(fromAbs) ||
    !Number.isSafeInteger(maxLines) ||
    fromAbs < 0 ||
    maxLines < 1 ||
    // The subtraction form: the addition form is itself the overflow it would
    // exist to reject (§4.1).
    fromAbs > Number.MAX_SAFE_INTEGER - maxLines
  ) {
    return false;
  }
  if (connState.status !== "connected" || !connState.upgraded) {
    return false;
  }
  const wait = takeHistoryToken();
  if (wait > 0) {
    armPendingDemand(wait);
    return false;
  }
  const bounded = Math.min(maxLines, history.effMax);
  const end = fromAbs + bounded;
  history.inFlight = { fromAbs, end };
  cb?.noteSolicited?.(fromAbs, end);
  // Through sendControl, which owns the ENCODING decision. Sending
  // `controlFrame` directly here was a defect with three simultaneous
  // consequences, none of them visible in this repo's tests: this function
  // refuses to run unless the socket is `upgraded`, and a post-upgrade server
  // parses the v3 0x00-sentinel only while UNLATCHED — so every request was
  // written straight to the PTY instead (terminal.go's handleBinaryFrame). No
  // page reply ever arrived (the 8 s data timeout halved the budget and retried
  // forever, so paging was inert); the control's JSON was TYPED INTO THE USER'S
  // SHELL, and fed to the session-title deriver; and its bytes advanced the
  // server's received-byte ledger while the client never counted them, so
  // applyAck's min(received, bytesSent) clamp trimmed genuinely-unacked
  // keystrokes out of the outbox.
  sendControl({ type: "history", fromAbs, maxLines: bounded });
  history.dataTimer = setTimeout(onHistoryDataTimeout, HISTORY_DATA_TIMEOUT_MS);
  return true;
}

/**
 * Arm the coalesced pending demand: ONE timer that re-runs the full trigger
 * when the bucket refills. Coalesced because a burst of denied triggers should
 * cost one retry, not one each; and re-running the FULL guard set rather than
 * replaying the denied request, because the world may have changed (the gap may
 * have healed, the session may have entered alt).
 */
function armPendingDemand(waitMs: number): void {
  if (history.demandTimer !== null) {
    return;
  }
  history.demandTimer = setTimeout(() => {
    history.demandTimer = null;
    cb?.onHistoryRetry?.();
  }, waitMs);
}

/**
 * The in-flight request did not answer in time. Release single-flight so the
 * controller can try again, HALVE the budget, and remember the size that failed
 * as the recovery ceiling.
 *
 * The ceiling is RFC 5681's `ssthresh` role — a growth target, not a cap below
 * the current value. Halving alone would oscillate (a link that carries 500 but
 * not 1000 would time out on every other request forever); dropping to the
 * floor and climbing back toward half the failed size converges on the largest
 * size the link actually carries, approaching it from below.
 */
function onHistoryDataTimeout(): void {
  const failed = history.inFlight;
  history.dataTimer = null;
  history.inFlight = null;
  cb?.clearSolicited?.();
  if (failed !== null) {
    const size = failed.end - failed.fromAbs;
    history.budgetCeiling = Math.max(HISTORY_MIN_PAGE, Math.floor(size / 2));
    history.effMax = HISTORY_MIN_PAGE;
  }
  // Retry through the same coalescing path a denied trigger uses, so the
  // controller re-evaluates its guards rather than replaying a stale range.
  armPendingDemand(0);
}

/**
 * Correlate an inbound scroll frame against the in-flight request.
 *
 * A frame is the reply iff its `firstIndex` lies inside the request window.
 * CONTAINMENT then decides the CONTROL effects separately from the content: only
 * a reply that fits entirely inside the window releases single-flight and grows
 * the budget. A correlated frame extending BEYOND the window — a timed-out
 * larger reply sharing a retry's `fromAbs` — has its in-window intersection
 * applied by the store and changes no control state, so the attempt's own reply
 * still gets to complete it (§4.3).
 *
 * Returns the floor to raise to when the reply proves history is gone
 * (`firstIndex > fromAbs` is the CLAMP signal; an empty reply means nothing in
 * the whole window survives), or null.
 */
function correlateHistoryReply(msg: ScrollMessage): {
  correlated: boolean;
  raiseFloorTo: number | null;
  contained: boolean;
} {
  const req = history.inFlight;
  if (req === null) {
    return { correlated: false, raiseFloorTo: null, contained: false };
  }
  if (msg.firstIndex < req.fromAbs || msg.firstIndex >= req.end) {
    return { correlated: false, raiseFloorTo: null, contained: false };
  }
  const count = msg.lines.length;
  let raiseFloorTo: number | null = null;
  if (count === 0) {
    // Nothing in [fromAbs, end) is retained: the whole window is condemned.
    raiseFloorTo = req.end;
  } else if (msg.firstIndex > req.fromAbs) {
    // The server served from higher up than asked: everything below the served
    // start is evicted server-side, permanently.
    raiseFloorTo = msg.firstIndex;
  }
  const contained = msg.firstIndex + count <= req.end;
  if (contained) {
    if (history.dataTimer !== null) {
      clearTimeout(history.dataTimer);
      history.dataTimer = null;
    }
    history.inFlight = null;
    // Recovery: climb toward the remembered ceiling, never past it.
    history.effMax = Math.min(history.effMax * 2, history.budgetCeiling);
  }
  return { correlated: true, raiseFloorTo, contained };
}

/**
 * The server boot epoch last observed for a session, or 0 when none is known —
 * the read half of adoptPersistedEpoch.
 *
 * A consumer that persists a session's content must record WHICH server process
 * the absolute indices in it belong to, and this is the only place that fact
 * exists: it arrives in a resumeAck and is otherwise kept privately for restart
 * detection. Pass the result to `LineStore.snapshot(serverEpoch)`.
 *
 * 0 means "never learned one", which happens before the first resumeAck and
 * against a version-silent server that sends no epoch at all. It is deliberately
 * the same value `adoptPersistedEpoch` ignores, so a snapshot taken without an
 * epoch cannot later be mistaken for one taken under a known epoch.
 */
export function serverEpochOf(sessionId: string): number {
  return sessions.get(sessionId)?.lastServerEpoch ?? 0;
}

/**
 * The session id this module's socket serves, resolving the UNMANAGED id if no
 * session has been set.
 *
 * A managed consumer (one calling setSession) already holds its own ids and does
 * not need this. The unmanaged single-terminal path does: its identity is a
 * per-tab, sessionStorage-backed id minted inside this module, so a consumer
 * that wants to key anything per-session — persisted scrollback, per-session
 * scroll memory — has no id to key it by, and any id it invented would be wrong
 * in one direction or the other (localStorage would make two tabs share one
 * terminal's state; a fresh random one would never match across a reload).
 *
 * Resolving means the same lazy load-or-mint the first send/connect performs, so
 * calling this before connecting simply moves that by a few milliseconds. The
 * semantics that make it the right persistence key are the ones sessionStorage
 * already gives: stable across a reload and an iOS tab restore, fresh in a
 * genuinely new tab.
 */
export function currentSessionId(): string {
  activeId ??= loadOrCreateSessionId();
  return activeId;
}

/**
 * disconnect tears down the live socket without reconnecting. Per-session resume
 * state is kept, so a later setSession/connect resumes cleanly. Used when the
 * shell has no active tab to show (e.g. the last tab closed).
 */
export function disconnect(): void {
  teardown();
}

export function connect(): void {
  if (connState.status === "incompatible") {
    return;
  }
  // Guard against double-call: a stray invocation while a previous
  // socket is still CONNECTING/OPEN would orphan it (its handlers
  // remain bound but the new sock assignment makes it unreachable).
  // Aborting the previous controller detaches all listeners on the
  // old sock so it can't deliver frames to the page after we've moved
  // on (the iPad-wake duplicate-output race).
  if (connState.status === "connecting" || connState.status === "connected") {
    connState.abort.abort();
    try {
      connState.sock.close();
    } catch {
      /* ignore */
    }
  }
  // Re-entry while a backoff reconnect is pending (e.g. a consumer calling
  // connect() to restore a panel during the 500ms-8s backoff window): clear the
  // scheduled timer so it cannot fire later and spawn a SECOND socket alongside
  // the one created below. The orphaned timer resets connState to disconnected
  // and calls connect() again, while the existing socket's listeners stay bound
  // (its abort never fired) -> a duplicate server connection + double delivery.
  // cancelScheduledReconnect is a no-op in any non-reconnecting state.
  cancelScheduledReconnect();

  cb?.onConnecting?.();

  // The resume state this socket serves, captured for the socket's lifetime.
  // A switch aborts this socket's listeners, so even a late frame is handled
  // against the session it was opened for, never whoever is active now.
  const st = activeState();
  let url = wsURL(location.protocol, location.host, wsPath);
  if (managed) {
    // Route the socket to this session (SessionManager's WebSocketHandler
    // dispatches on ?session=). The unmanaged single-terminal path keeps the
    // bare wsPath, matching resume purely by the resume frame's sessionId.
    url += (url.includes("?") ? "&" : "?") + "session=" + encodeURIComponent(st.id);
  }
  const sock = new WebSocket(url);
  sock.binaryType = "arraybuffer";

  // One AbortController governs the lifetime of THIS sock's listeners.
  // - Connect-timeout fallback: aborts after 10s if open never fires.
  // - Listener auto-detach: every addEventListener below uses
  //   { signal: connectAbort.signal }, so when the controller is
  //   aborted (by reconnectNow / connect / close) the listeners are
  //   removed atomically and can't fire again.
  const connectAbort = new AbortController();
  const timeoutId = setTimeout(() => {
    // Aborting detaches every listener registered with connectAbort.signal
    // (abort algorithms run BEFORE the abort event fires), INCLUDING the
    // "close" listener that normally schedules the reconnect. A connect that
    // never opens (SYN dropped by a firewall / an overloaded server) would
    // otherwise leave connState pinned at "connecting" with no auto-retry.
    // Drive the reconnect explicitly, mirroring the close handler.
    connectAbort.abort();
    if (connState.status === "connecting" && connState.sock === sock) {
      stopHeartbeat();
      connState = { status: "disconnected" };
      cb?.onClose();
      scheduleReconnect();
    }
  }, 10_000);
  connectAbort.signal.addEventListener("abort", () => {
    clearTimeout(timeoutId);
    // Force-close on abort so the OS-level socket goes away promptly,
    // not only when the browser eventually completes its close
    // handshake. Belt-and-braces with the .close() in our callers.
    try {
      sock.close();
    } catch {
      /* ignore */
    }
  });

  connState = { status: "connecting", sock, abort: connectAbort };

  sock.addEventListener(
    "open",
    () => {
      clearTimeout(timeoutId);
      // A new socket has no size on record with the server yet, so the
      // deduplication baseline resets before anything is announced below.
      lastSentCols = 0;
      lastSentRows = 0;
      // Announce the size BEFORE asking to resume, when the consumer can
      // measure trustworthily (Callbacks.initialSize). Order matters and is the
      // whole point: the server applies a resize control the moment it decodes
      // it, so a resize that precedes the resume makes the resume's screen
      // snapshot and history replay come back at THIS client's geometry, with
      // the app's SIGWINCH redraw (if any) landing after a coherent snapshot
      // instead of interleaved with the replay.
      //
      // This does not weaken the "resume is message one" invariant, which
      // exists for two reasons, neither of which a resize touches: the server
      // must see the resume's protocolVersion before any TEXT frame arrives
      // (a resize does not arm typed framing), and PTY INPUT must not precede
      // session resolution (input sent before the resume reaches the process
      // but is skipped by the server's received-byte ledger, desyncing the
      // outbox accounting). That is also why cb.onOpen() still runs after the
      // resume: a consumer may send input from it.
      const size = readInitialSize();
      if (size !== null) {
        sock.send(controlFrame({ type: "resize", cols: size.cols, rows: size.rows }));
        lastSentCols = size.cols;
        lastSentRows = size.rows;
      }
      // Bootstrap resume, on the captured socket, before consumer callbacks can
      // run: always binary-sentinel encoded, understood by every server
      // revision (the v4 negotiation bootstrap, design §4). An onOpen callback
      // that calls sendResize()/sendBinary() therefore always queues AFTER it.
      // Capture the resume's two history-relevant inputs BEFORE sending, and
      // remember them on the socket. The server's reply is a function of these
      // exact values, so the replay-jump prediction must be computed from them
      // — never from store state, which a frame arriving between send and ack
      // could have moved, masking a real jump (docs/paged-scrollback.md §4.5).
      resetHistoryForSocket();
      const sentHaveThrough = cb?.getHaveThrough?.() ?? -1;
      // ALWAYS a number. A consumer may ask for fewer lines than the protocol
      // ceiling, but it cannot opt OUT of the bound: the server clamps to the
      // same ceiling unconditionally, and a client that sent nothing while the
      // server bounded anyway would predict no replay jump where one happened —
      // leaving the stranded band classified as live tail for enforceCap to eat
      // silently. The two sides agree by construction instead of by wiring.
      const rawReplayMax = cb?.getReplayMax?.() ?? MAX_REPLAY_LINES;
      const sentReplayMax =
        Number.isSafeInteger(rawReplayMax) && rawReplayMax >= 1
          ? Math.min(rawReplayMax, MAX_REPLAY_LINES)
          : MAX_REPLAY_LINES;
      history.sentHaveThrough = sentHaveThrough;
      history.sentReplayMax = sentReplayMax;
      sock.send(
        controlFrame({
          type: "resume",
          // Managed mode: the per-sender resume key (routing id + "#" +
          // instance id), so each device/tab owns its server-side input
          // ledger. Unmanaged: the per-tab sessionStorage id as-is.
          sessionId: resumeKey(st),
          sentBytes: st.bytesSent,
          // Highest absolute line index the client holds (-1 if none). The
          // server replays everything after it, so lines printed while the
          // device slept are backfilled exactly on wake (bug 2), with no
          // duplication because applying a line by absolute index is
          // idempotent. Falls back to -1 (full retained replay) if the
          // consumer wired no getHaveThrough.
          haveThrough: sentHaveThrough,
          // The resume replay bound, already clamped to what the server will
          // honor, and always present (see above).
          replayMax: sentReplayMax,
          // Lets the server detect a client built against a different wire
          // revision (e.g. a stale cached bundle) and warn rather than
          // silently mis-decode; >= 4 also ARMS the connection for the
          // typed-framing upgrade (design §4 phase 1).
          protocolVersion: WIRE_PROTOCOL_VERSION,
        }),
      );
      // Every socket starts in v3 mode; the resumeAck's serverWireVersion
      // decides whether the typed-framing upgrade happens (design §4).
      connState = { status: "connected", sock, abort: connectAbort, upgraded: false };
      reconnectDelay = INITIAL_DELAY_MS;
      cb?.onOpen();

      // Begin client-side liveness probing for this socket. Idempotent
      // (clears any prior timer) and resets the activity clock to now.
      startHeartbeat();
    },
    { signal: connectAbort.signal },
  );

  // Queue for serializing Blob→ArrayBuffer conversion. iOS Safari can
  // deliver binary WS frames as Blob; the conversion is async via
  // .arrayBuffer() and unordered resolution would corrupt screen state.
  // We chain promises so each frame is processed in arrival order.
  let blobChain: Promise<void> = Promise.resolve();

  sock.addEventListener(
    "message",
    (ev: MessageEvent) => {
      // Any inbound frame — pong, screen update, anything — proves the
      // socket is delivering, so it refreshes the liveness clock before we
      // even decode it. A malformed frame that decodes to null still counts.
      markActivity();
      if (ev.data instanceof ArrayBuffer) {
        try {
          handleDecoded(decodeWireBinary(ev.data));
        } catch (err) {
          // Mirror the Blob branch below: a throw here (a consumer onMessage
          // callback, or the documented re-throw of a non-RangeError from
          // decodeWireBinary) is logged with engine context instead of
          // surfacing as a bare uncaught exception, so field observability is
          // the same across ArrayBuffer (non-iOS) and Blob (iOS Safari) frames.
          console.error("vterm: dropped binary frame", err);
        }
        return;
      }
      if (ev.data instanceof Blob) {
        const blob = ev.data;
        blobChain = blobChain
          .then(() => blob.arrayBuffer())
          .then((ab) => {
            // Stale-socket guard (design §4, review F2): the async
            // blob.arrayBuffer() hop can outlive this socket — teardown
            // aborts the listeners, but a conversion already queued still
            // resolves. A frame from a superseded socket must not reach
            // handleDecoded, where its resumeAck could upgrade/reset/
            // retransmit against the REPLACEMENT socket's server.
            if (
              connectAbort.signal.aborted ||
              connState.status !== "connected" ||
              connState.sock !== sock
            ) {
              return;
            }
            handleDecoded(decodeWireBinary(ab));
          })
          .catch((err: unknown) => {
            // A throw here (typically a consumer onMessage callback) must NOT
            // poison the chain: without this catch blobChain stays rejected and
            // every later Blob frame's .then is skipped, silently dropping all
            // binary frames until reconnect. iOS Safari delivers binary WS
            // frames as Blob, and markActivity() already ran on arrival, so the
            // liveness probe never fires -> the tab looks connected but renders
            // nothing. Log and continue; arrival order is preserved.
            console.error("vterm: dropped binary (blob) frame", err);
          });
        return;
      }
      // Text frames from the server are undefined in the protocol (the server
      // sends only binary frames; wire v4 made text a CLIENT->server control
      // channel). The old dormant JSON.parse branch that accepted them as
      // unvalidated ServerMessages was removed 2026-07 (judgement finding).
    },
    { signal: connectAbort.signal },
  );

  function handleDecoded(msg: ServerMessage | null): void {
    if (msg === null) {
      return;
    }
    if (msg.type === "resumeAck") {
      // An explicit below-floor revision is definitive: this decoder cannot
      // safely consume that server's frames. Stop the socket before invoking
      // consumer callbacks, latch the no-reconnect state, and require an
      // explicit disconnect/page reload before another attempt. A missing
      // tail remains the version-silent compatibility path.
      if (
        msg.serverWireVersion !== undefined &&
        msg.serverWireVersion < MIN_SUPPORTED_SERVER_WIRE_VERSION
      ) {
        const reason = `server wire protocol ${msg.serverWireVersion} is below client minimum ${MIN_SUPPORTED_SERVER_WIRE_VERSION}; upgrade the server`;
        console.warn("vterm: refusing incompatible server wire protocol", reason);
        stopHeartbeat();
        connState = { status: "incompatible" };
        try {
          sock.close(WIRE_INCOMPATIBLE_CLOSE_CODE, reason);
        } finally {
          connectAbort.abort();
        }
        cb?.onWireVersionMismatch?.(msg.serverWireVersion, WIRE_PROTOCOL_VERSION);
        cb?.onWireIncompatible?.({
          source: "server-version",
          serverVersion: msg.serverWireVersion,
          clientVersion: WIRE_PROTOCOL_VERSION,
          minimumServerVersion: MIN_SUPPORTED_SERVER_WIRE_VERSION,
          reason,
        });
        return;
      }
      // Higher revisions may retain this client's compatible baseline. Keep
      // the socket running and surface the skew as a warning only.
      if (msg.serverWireVersion !== undefined && msg.serverWireVersion > WIRE_PROTOCOL_VERSION) {
        console.warn(
          "vterm: server wire-protocol version is newer than client",
          "server",
          msg.serverWireVersion,
          "client",
          WIRE_PROTOCOL_VERSION,
          "- upgrade the client if terminal behavior is incorrect",
        );
        cb?.onWireVersionMismatch?.(msg.serverWireVersion, WIRE_PROTOCOL_VERSION);
      }
      // Typed-framing upgrade (design §4 phase 3): on proof of a v4+ server,
      // send the text transition FIRST — WebSocket ordering then guarantees
      // the server latches before any unsplit binary input that follows —
      // and only then flip the socket's mode. This must precede the
      // ledger-lost/ack/retransmit handling below so the retransmit already
      // uses the upgraded framing.
      if (
        msg.serverWireVersion !== undefined &&
        msg.serverWireVersion >= TYPED_FRAMING_MIN_VERSION &&
        connState.status === "connected" &&
        connState.sock === sock &&
        !connState.upgraded
      ) {
        sock.send(textControl({ type: "upgrade" }));
        connState.upgraded = true;
      }
      // Server-restart detection. The first resumeAck we see records
      // the epoch; subsequent ones compare to it. A mismatch means the
      // server's process has restarted, which invalidates our local
      // bytesSent/bytesAcked accounting (the new server has no record
      // of the previous boot's input). Reset state and notify the UI.
      const epoch = msg.serverEpoch;
      let epochChanged = false;
      if (epoch !== undefined && epoch !== 0) {
        if (st.lastServerEpoch !== null && st.lastServerEpoch !== epoch) {
          epochChanged = true;
          resetSessionAfterRestart(st);
        }
        st.lastServerEpoch = epoch;
        // Confirmed by a server: no longer a claim about restored content.
        st.epochSeeded = false;
      } else if (st.epochSeeded) {
        // The epoch we hold was SEEDED from persisted content and this server will
        // not say what process it is. Unverifiable restored content is handled as a
        // restart, because the alternative is presenting a previous run's output as
        // live and then refusing the new session's low absolute indices — wrong,
        // then permanently blank. Only ever reachable for a hydrated session: an
        // epoch learned from an earlier ack is an observation, and a server that
        // reports none is ordinary operation for it.
        epochChanged = true;
        resetSessionAfterRestart(st);
        st.lastServerEpoch = null;
        st.epochSeeded = false;
      }
      // Capability, read from the ack's length-gated flags tail. An absent tail
      // (a server older than it) reads the same as an unset bit: no paging,
      // nothing ever sent, and the store keeps its compatibility tail cap.
      history.paging = msg.historyPaging === true;
      history.acked = true;
      // The store's ONE ack transition, dispatched BEFORE the early returns
      // below. A ledger loss is not a capability event, and the long-absence
      // attach that loses its ledger is exactly the one carrying a replay jump
      // — appending this after those returns would skip it precisely there
      // (docs/paged-scrollback.md §4.5).
      cb?.onResumeTransition?.({
        epochChanged,
        committed: typeof msg.committed === "number" ? msg.committed : null,
        serverOldest: typeof msg.oldestIndex === "number" ? msg.oldestIndex : null,
        paging: history.paging,
        sentHaveThrough: history.sentHaveThrough,
        sentReplayMax: history.sentReplayMax,
      });
      // Resync guard 8.2.2: hand the server's retained-history bounds to the
      // consumer so it can surface a trim marker when history the client was
      // missing is gone for good. (If the ledger-lost / session-forgotten
      // paths below reset state, a fresh server's oldest=0 simply reads as
      // "no trim".)
      if (typeof msg.committed === "number" && typeof msg.oldestIndex === "number") {
        cb?.onResumeBounds?.(msg.committed, msg.oldestIndex);
      }
      // Explicit ledger-loss signal (servers with the >= 35-byte resumeAck
      // tail): the resume key missed the server's registry while we claimed
      // sent bytes — our ledger was reclaimed (idle GC / cap eviction). The
      // server cannot vouch for ANY previously sent input, so replaying the
      // outbox risks duplicate execution. Deterministic drop, plus a notify
      // only when unacked bytes were actually dropped (resetForgottenLedger),
      // covering the bytesAcked === 0 case the heuristic below cannot see
      // (acks that never reached us before the disconnect).
      if (msg.ledgerLost) {
        resetForgottenLedger(st);
        return;
      }
      // Server-doesn't-recognize-this-session safeguard (old-server
      // fallback, pre-ledgerLost tail): if the server returns received=0
      // but the client already had bytesAcked > 0, the server has forgotten
      // our session (idle GC kicked in, or sessionId persistence failed and
      // a reload created a new one). Replaying the outbox would deliver
      // every queued chunk again, causing the iOS tab-suspend
      // duplicate-resend bug. Drop the outbox, and notify only if it held
      // unacked bytes — input since the last successful ack is
      // irrecoverable but at least not duplicated. Skip this branch when
      // bytesSent = 0 (genuine first-connect; received=0 is correct).
      if (msg.received === 0 && st.bytesAcked > 0) {
        resetForgottenLedger(st);
        return;
      }
      applyAck(st, msg.received);
      retransmitOutbox(
        sock,
        connState.status === "connected" && connState.sock === sock && connState.upgraded,
        st,
      );
      return;
    }
    if (msg.type === "ackOnly") {
      // Bare ack from the flush tick's sweep: input was applied but no
      // content frame carried the new count (silent app — e.g. `read -s`).
      // Trim the outbox and stop; transport-internal, never forwarded to
      // onMessage.
      applyAck(st, msg.inputAck);
      return;
    }
    if (msg.type === "modes") {
      // Single mirror writer (P3): cache the snapshot on the session this
      // socket serves AND apply it to the active-session singleton. The two
      // targets are the same session by construction — a superseded socket's
      // listeners are aborted, so only the active session's socket delivers.
      const snap: modes.ModeSnapshot = {
        bracketedPaste: msg.bracketedPaste,
        applicationCursor: msg.applicationCursor,
        mouseSGR: msg.mouseSGR,
        focusReporting: msg.focusReporting,
        mouseMode: msg.mouseMode,
        applicationKeypad: msg.applicationKeypad,
        reverseVideo: msg.reverseVideo,
        mousePixels: msg.mousePixels,
        keyboardFlags: msg.keyboardFlags,
      };
      st.modes = snap;
      modes.applySnapshot(snap);
      if (typeof msg.inputAck === "number") {
        applyAck(st, msg.inputAck);
      }
      // Notify the UI so it can react to mode changes (e.g. clear
      // scrollback on alt-screen entry — handled by the caller).
      cb?.onMessage(msg);
      return;
    }
    if (typeof msg.inputAck === "number") {
      applyAck(st, msg.inputAck);
    }
    // PRE-ACK CONTENT SUPPRESSION (docs/paged-scrollback.md §4.5). The server
    // registers a socket and wakes its scheduler at accept, so on a busy session
    // a live screen/scroll frame is the DESIGNED first delivery — it can arrive
    // before this socket's resumeAck. Applying its rows would mutate the store
    // under a stale residency cap, and on an already-flipped store one frame is
    // enough to push the tail over and have ordinary eviction eat the very band
    // the ack transition exists to protect.
    //
    // Suppression is FIELD-AWARE, not a blanket drop, because a screen frame is
    // not only rows. Row and window mutation are held back; everything else
    // about the frame is honored:
    //   - the piggybacked inputAck was applied just above (monotone, harmless);
    //   - ED3 (`scrollbackCleared`) is a CONSUMED one-shot the resume batch
    //     hard-codes false, so dropping it would leave the client displaying
    //     history the server has already discarded — it is forwarded as a
    //     rows-less clear;
    //   - `bell` is dropped by design: it announces a screen the user has not
    //     been shown yet, and the batch is about to repaint it;
    //   - every other message class (modes, title, clipboard, pong, ackOnly)
    //     returned earlier or is unaffected.
    //
    // The suppressed rows are LOSSLESS by supersession: the batch's own window
    // frame carries the screen, and a frame's scroll lines are committed to the
    // ring before dispatch, so the replay re-delivers them.
    if (!history.acked && (msg.type === "screen" || msg.type === "scroll")) {
      if (msg.type === "screen" && msg.scrollbackCleared) {
        cb?.onMessage({ ...msg, changed: [], rows: [], bell: false });
      }
      return;
    }
    if (msg.type === "scroll") {
      // A scroll frame may be a correlated page reply rather than live output.
      // Correlation is by window membership; containment then decides whether
      // this frame COMPLETES the attempt (releasing single-flight and growing
      // the adaptive budget) or merely contributes content.
      const { correlated, raiseFloorTo, contained } = correlateHistoryReply(msg);
      if (correlated) {
        cb?.onHistoryReply?.(msg, raiseFloorTo);
        // Close the store's solicited window AFTER the page is applied, and only
        // for a reply that COMPLETED the attempt. The window is the store's
        // permission to admit lines below its stale-re-send watermark, so it has
        // to outlive the apply — but leaving it open once no request is in flight
        // means a later duplicate or malformed frame in that same range keeps
        // bypassing the guard and can resurrect an evicted row through the
        // ordinary (tail-classifying) path. An OVERSPILLING reply deliberately
        // keeps both the slot and the window: its attempt is still open.
        if (contained) {
          cb?.clearSolicited?.();
        }
        return;
      }
    }
    cb?.onMessage(msg);
  }

  sock.addEventListener(
    "close",
    (ev: CloseEvent) => {
      // Only the active sock's close should drive reconnect logic; an
      // already-superseded sock has been aborted and this listener
      // wouldn't fire (signal removes it). The check stays as a belt-
      // and-braces guard in case the abort hasn't propagated yet.
      if (connState.status !== "connecting" && connState.status !== "connected") {
        return;
      }
      if (connState.sock !== sock) {
        return;
      }
      stopHeartbeat();
      // The fetch state belonged to THIS socket: kill its timers so neither
      // fires against a dead socket, and release the solicited window so the
      // store stops admitting lines below its stale-re-send watermark.
      resetHistoryForSocket();
      cb?.clearSolicited?.();
      if (ev.code === WIRE_INCOMPATIBLE_CLOSE_CODE) {
        const reason =
          ev.reason ||
          "server rejected this client wire protocol; reload or upgrade the client/server";
        connState = { status: "incompatible" };
        cb?.onWireIncompatible?.({
          source: "server-close",
          clientVersion: WIRE_PROTOCOL_VERSION,
          minimumServerVersion: MIN_SUPPORTED_SERVER_WIRE_VERSION,
          reason,
        });
        return;
      }
      connState = { status: "disconnected" };
      // A process-exited close (4001) is definitive: the child is gone, so a
      // backoff reconnect can only replay the final screen and collect another
      // 4001 — an endless, pointless churn that reads as a flapping
      // "Reconnecting…" banner. An unknown-session close (4004) is equally
      // definitive: the server does not know the id at all (reaped, closed
      // elsewhere, restarted server), so reconnecting can only collect another
      // 4004. Route both to onProcessExit (no reconnect, no onClose) when the
      // consumer wired it; without the callback, keep the legacy transient
      // treatment so existing consumers see no change.
      if (
        (ev.code === PROCESS_EXITED_CLOSE_CODE || ev.code === SESSION_UNKNOWN_CLOSE_CODE) &&
        cb?.onProcessExit
      ) {
        cb.onProcessExit();
        return;
      }
      cb?.onClose();
      scheduleReconnect();
    },
    { signal: connectAbort.signal },
  );

  sock.addEventListener(
    "error",
    () => {
      /* no-op: prevents unhandled error */
    },
    { signal: connectAbort.signal },
  );
}
