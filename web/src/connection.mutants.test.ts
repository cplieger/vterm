// @vitest-environment happy-dom
//
// The transport's RULES, stated one at a time: which timers belong to which
// socket, what a scheduled reconnect may and may not spawn, what a socket that
// never opened is allowed to do afterwards, and which frame shapes reach the
// consumer.
//
// The connection module is almost entirely about lifetime. Nearly every field it
// owns — the liveness interval, the fetch controller's two timers, the backoff
// timer, the paging capability, the per-session ledger — is a statement about
// ONE socket, and the failure mode when one of them outlives its socket is
// silent: a timer fires against a replacement and reconnects a healthy link, or
// a capability carried across a reconnect makes the client page against a server
// that never declared it. None of that shows up as an exception; it shows up as
// a flapping "Reconnecting…" banner or a blank scrollback.
//
// So the observables here are deliberately coarse and external: how many sockets
// exist, what went out on the wire, which callbacks fired and how often. Nothing
// reads the module's private state, and the fake WebSocket is driven (open,
// message, error, close) rather than the module's own methods being mocked.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  adoptPersistedEpoch,
  connect,
  currentSessionId,
  disconnect,
  forgetSession,
  generateSessionId,
  historyPagingAvailable,
  init,
  reconnectNow,
  requestHistory,
  sendBinary,
  setSession,
  type Callbacks,
} from "./connection.js";
import {
  MIN_SUPPORTED_SERVER_WIRE_VERSION,
  WIRE_INCOMPATIBLE_CLOSE_CODE,
  WIRE_PROTOCOL_VERSION,
  type WireIncompatibility,
} from "./wire-compatibility.js";
import type { ServerMessage } from "./types.js";
import type * as ConnectionModule from "./connection.js";

// --- the fake socket -------------------------------------------------------
//
// Same shape the sibling connection suites use: a constructor the module can
// `new`, a listener registry that HONORS the AbortSignal (the module's whole
// supersession story is that signal), and fire* helpers for the four events.

interface MockWS {
  url: string;
  binaryType: string;
  readyState: number;
  closed: boolean;
  closeArgs: unknown[][];
  listeners: Map<string, ((ev: unknown) => void)[]>;
  send: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  addEventListener: (
    type: string,
    handler: (ev: unknown) => void,
    opts?: { signal?: AbortSignal },
  ) => void;
  fireOpen: () => void;
  fireMessage: (data: ArrayBuffer | Blob | string) => void;
  fireClose: (code?: number, reason?: string) => void;
  fireError: () => void;
}

const sockets: MockWS[] = [];

function makeMockWebSocket(): typeof WebSocket {
  const ctor = function (url: string): MockWS {
    const sock: MockWS = {
      url,
      binaryType: "blob",
      readyState: 0, // CONNECTING
      closed: false,
      closeArgs: [],
      listeners: new Map(),
      send: vi.fn(),
      close: vi.fn(function (this: MockWS, ...args: unknown[]) {
        this.closed = true;
        this.readyState = 3;
        this.closeArgs.push(args);
      }) as unknown as ReturnType<typeof vi.fn>,
      addEventListener(
        this: MockWS,
        type: string,
        handler: (ev: unknown) => void,
        opts?: { signal?: AbortSignal },
      ) {
        if (!this.listeners.has(type)) {
          this.listeners.set(type, []);
        }
        const list = this.listeners.get(type)!;
        list.push(handler);
        if (opts?.signal) {
          opts.signal.addEventListener("abort", () => {
            const idx = list.indexOf(handler);
            if (idx >= 0) {
              list.splice(idx, 1);
            }
          });
        }
      },
      fireOpen(this: MockWS) {
        this.readyState = 1;
        for (const fn of [...(this.listeners.get("open") ?? [])]) {
          fn({});
        }
      },
      fireMessage(this: MockWS, data: ArrayBuffer | Blob | string) {
        for (const fn of [...(this.listeners.get("message") ?? [])]) {
          fn({ data });
        }
      },
      fireClose(this: MockWS, code = 1006, reason = "") {
        this.readyState = 3;
        for (const fn of [...(this.listeners.get("close") ?? [])]) {
          fn({ code, reason });
        }
      },
      fireError(this: MockWS) {
        for (const fn of [...(this.listeners.get("error") ?? [])]) {
          fn({});
        }
      },
    } as unknown as MockWS;
    Object.setPrototypeOf(sock, Mock.prototype);
    sockets.push(sock);
    return sock;
  } as unknown as typeof WebSocket;
  class Mock {}
  ctor.prototype = Mock.prototype as unknown as WebSocket;
  return ctor;
}

/** The socket the module created most recently. */
function latest(): MockWS {
  const sock = sockets[sockets.length - 1];
  if (sock === undefined) {
    throw new Error("no socket was created");
  }
  return sock;
}

// --- server frames (mirroring wire_binary.go's encoders) -------------------

/**
 * A resumeAck. `bytes` selects the tail the server carries: 9 (bare), 17
 * (+epoch), 33 (+bounds), 35 (+version and flags). The 35-byte form is what
 * arms the v4 typed-framing upgrade, which `requestHistory` requires.
 */
function resumeAckFrame(
  opts: {
    received?: number;
    serverEpoch?: number;
    committed?: number;
    oldestIndex?: number;
    version?: number;
    paging?: boolean;
    ledgerLost?: boolean;
    bytes?: 9 | 17 | 33 | 35;
  } = {},
): ArrayBuffer {
  const size = opts.bytes ?? 35;
  const buf = new ArrayBuffer(size);
  const v = new DataView(buf);
  v.setUint8(0, 2); // MSG_RESUME_ACK
  v.setBigUint64(1, BigInt(opts.received ?? 0), true);
  if (size >= 17) {
    v.setBigUint64(9, BigInt(opts.serverEpoch ?? 0), true);
  }
  if (size >= 33) {
    v.setBigUint64(17, BigInt(opts.committed ?? 0), true);
    v.setBigUint64(25, BigInt(opts.oldestIndex ?? 0), true);
  }
  if (size >= 35) {
    v.setUint8(33, opts.version ?? WIRE_PROTOCOL_VERSION);
    v.setUint8(34, (opts.ledgerLost === true ? 1 : 0) | (opts.paging === true ? 2 : 0));
  }
  return buf;
}

/** MSG_TITLE: type(1) inputAck(8) len(2) utf8. */
function titleFrame(title: string, inputAck = 0): ArrayBuffer {
  const body = new TextEncoder().encode(title);
  const buf = new ArrayBuffer(11 + body.length);
  const v = new DataView(buf);
  v.setUint8(0, 4);
  v.setBigUint64(1, BigInt(inputAck), true);
  v.setUint16(9, body.length, true);
  new Uint8Array(buf).set(body, 11);
  return buf;
}

/** One row holding a single "x" run: numRuns(2) then the run's fields. */
const ONE_ROW: number[] = [
  1,
  0, // numRuns = 1
  1,
  0, // text length = 1
  0x78, // "x"
  0xff,
  0xff,
  0xff,
  0xff, // f = -1
  0xff,
  0xff,
  0xff,
  0xff, // b = -1
  0,
  0, // a = 0
  0xff,
  0xff,
  0xff,
  0xff, // uc = -1
  0,
  0, // url length = 0
];

/** MSG_SCROLL carrying `count` rows starting at `firstIndex`. */
function scrollFrame(firstIndex: number, count: number, inputAck = 0): ArrayBuffer {
  const rows: number[] = [];
  for (let i = 0; i < count; i++) {
    rows.push(...ONE_ROW);
  }
  const buf = new ArrayBuffer(19 + rows.length);
  const v = new DataView(buf);
  v.setUint8(0, 1); // MSG_SCROLL
  v.setBigUint64(1, BigInt(inputAck), true);
  v.setBigUint64(9, BigInt(firstIndex), true);
  v.setUint16(17, count, true);
  new Uint8Array(buf).set(rows, 19);
  return buf;
}

/** MSG_SCREEN with no changed rows; `bell` and `scrollbackCleared` are flags. */
function screenFrame(
  opts: { base?: number; bell?: boolean; scrollbackCleared?: boolean; inputAck?: number } = {},
): ArrayBuffer {
  const buf = new ArrayBuffer(27);
  const v = new DataView(buf);
  v.setUint8(0, 0); // MSG_SCREEN
  v.setBigUint64(1, BigInt(opts.inputAck ?? 0), true);
  v.setBigUint64(9, BigInt(opts.base ?? 0), true);
  v.setUint16(17, 0, true); // cursorRow
  v.setUint16(19, 0, true); // cursorCol
  v.setUint16(21, 0, true); // screenHeight
  v.setUint16(23, 0, true); // numChanged
  v.setUint8(25, 0); // cursorStyle
  v.setUint8(26, (opts.bell === true ? 2 : 0) | (opts.scrollbackCleared === true ? 16 : 0));
  return buf;
}

/** MSG_MODES: type(1) inputAck(8) flags(1) mouseMode(2) kbdFlags(1). */
function modesFrame(inputAck: number): ArrayBuffer {
  const buf = new ArrayBuffer(13);
  const v = new DataView(buf);
  v.setUint8(0, 3);
  v.setBigUint64(1, BigInt(inputAck), true);
  v.setUint8(9, 0); // flags: everything off
  v.setUint16(10, 0, true); // mouseMode
  v.setUint8(12, 0); // keyboardFlags
  return buf;
}

// --- reading what the client sent -----------------------------------------

/**
 * Control messages the socket was asked to send, in order, decoded from EITHER
 * encoding: the v3 0x00-sentinel binary form (a Uint8Array) or the v4 text form
 * (a string). Raw PTY input is an ArrayBuffer and is skipped.
 */
function controlsSent(sock: MockWS): Record<string, unknown>[] {
  const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
  const out: Record<string, unknown>[] = [];
  for (const c of calls) {
    const a = c[0];
    if (typeof a === "string") {
      out.push(JSON.parse(a) as Record<string, unknown>);
      continue;
    }
    if (!(a instanceof Uint8Array) || a.length === 0 || a[0] !== 0x00) {
      continue;
    }
    out.push(JSON.parse(new TextDecoder().decode(a.subarray(1))) as Record<string, unknown>);
  }
  return out;
}

function controlsOfType(sock: MockWS, type: string): Record<string, unknown>[] {
  return controlsSent(sock).filter((m) => m["type"] === type);
}

/** PTY-input frames, as byte arrays. Input is always an ArrayBuffer. */
function inputSent(sock: MockWS): number[][] {
  const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
  const out: number[][] = [];
  for (const c of calls) {
    if (c[0] instanceof ArrayBuffer) {
      out.push([...new Uint8Array(c[0])]);
    }
  }
  return out;
}

/** Let the Blob→ArrayBuffer promise chain drain (real timers required). */
async function drainBlobChain(): Promise<void> {
  await new Promise((r) => setTimeout(r, 5));
}

// --- shared consumer -------------------------------------------------------

let onMessage: ReturnType<typeof vi.fn<(msg: ServerMessage) => void>>;
let onOpen: ReturnType<typeof vi.fn<() => void>>;
let onClose: ReturnType<typeof vi.fn<() => void>>;
let onServerRestart: ReturnType<typeof vi.fn<() => void>>;
let onResumeBounds: ReturnType<typeof vi.fn<(committed: number, oldest: number) => void>>;
let onWireIncompatible: ReturnType<typeof vi.fn<(d: WireIncompatibility) => void>>;
let onHistoryRetry: ReturnType<typeof vi.fn<() => void>>;
let onHistoryReply: ReturnType<typeof vi.fn<() => void>>;
let noteSolicited: ReturnType<typeof vi.fn<(fromAbs: number, end: number) => void>>;
let clearSolicited: ReturnType<typeof vi.fn<() => void>>;
let resumeTransitions: { epochChanged: boolean; paging: boolean; sentHaveThrough: number }[];

/**
 * A consumer wired for observation only. Every engine default this module would
 * otherwise supply from the renderer is replaced by a spy, so a test reads the
 * transport's decisions rather than the renderer's reaction to them.
 */
function baseCallbacks(): Callbacks {
  return {
    onMessage,
    onOpen,
    onClose,
    onServerRestart,
    onResumeBounds,
    onWireIncompatible,
    onHistoryRetry,
    onHistoryReply,
    noteSolicited,
    clearSolicited,
    computeSize: () => ({ cols: 80, rows: 24 }),
    getHaveThrough: () => -1,
    getReplayMax: () => 500,
    onResumeTransition: (ack) => {
      resumeTransitions.push({
        epochChanged: ack.epochChanged,
        paging: ack.paging,
        sentHaveThrough: ack.sentHaveThrough,
      });
    },
  };
}

function installConsumer(extra: Partial<Callbacks> = {}): void {
  onMessage = vi.fn<(msg: ServerMessage) => void>();
  onOpen = vi.fn<() => void>();
  onClose = vi.fn<() => void>();
  onServerRestart = vi.fn<() => void>();
  onResumeBounds = vi.fn<(committed: number, oldest: number) => void>();
  onWireIncompatible = vi.fn<(d: WireIncompatibility) => void>();
  onHistoryRetry = vi.fn<() => void>();
  onHistoryReply = vi.fn<() => void>();
  noteSolicited = vi.fn<(fromAbs: number, end: number) => void>();
  clearSolicited = vi.fn<() => void>();
  resumeTransitions = [];
  init({ ...baseCallbacks(), ...extra });
}

let seq = 0;

/** A unique session id per call: module state (the `sessions` map) is shared. */
function freshId(tag: string): string {
  seq++;
  return `mut-${tag}-${String(seq)}`;
}

/** A fresh managed session whose socket is open and acked. */
function openSession(
  tag: string,
  ack: Parameters<typeof resumeAckFrame>[0] = {},
): { id: string; sock: MockWS } {
  const id = freshId(tag);
  setSession(id);
  const sock = latest();
  sock.fireOpen();
  sock.fireMessage(resumeAckFrame(ack));
  return { id, sock };
}

// ---------------------------------------------------------------------------

describe("connection: the fetch controller's state belongs to one socket", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  /** Spend the whole burst: four grants, each completed by its own reply. */
  function spendBurst(sock: MockWS): void {
    for (let i = 1; i <= 4; i++) {
      const from = i * 1000;
      expect(requestHistory(from, 2)).toBe(true);
      sock.fireMessage(scrollFrame(from, 2));
    }
  }

  it("a request in flight when the socket goes away never fires its data timeout", () => {
    openSession("dt", { paging: true });
    expect(requestHistory(500, 10)).toBe(true);

    disconnect();
    vi.advanceTimersByTime(9_000); // past HISTORY_DATA_TIMEOUT_MS (8s)

    // The timeout's whole job is to release single-flight and retry. Fired
    // against a socket that no longer exists it asks the renderer to re-run a
    // fetch trigger with no transport under it, and it halves the budget a
    // future link will start from.
    expect(onHistoryRetry).not.toHaveBeenCalled();
  });

  it("a paced denial's pending retry never fires after the socket goes away", () => {
    const { sock } = openSession("pd", { paging: true });
    spendBurst(sock);
    // Bucket empty: the fifth request is refused and ARMS the coalesced retry
    // for the refill instant instead of being dropped.
    expect(requestHistory(9_000, 2)).toBe(false);

    disconnect();
    vi.advanceTimersByTime(9_000); // past HISTORY_REFILL_MS (2s)

    expect(onHistoryRetry).not.toHaveBeenCalled();
  });

  it("a reply that completes its request disarms the timeout it was waiting on", () => {
    const { sock } = openSession("rt", { paging: true });
    expect(requestHistory(500, 2)).toBe(true);

    sock.fireMessage(scrollFrame(500, 2)); // contained: completes the attempt
    vi.advanceTimersByTime(9_000);

    // A live timeout after a served page is a phantom failure: it would retry a
    // window the client already holds and shrink the budget on a healthy link.
    expect(onHistoryReply).toHaveBeenCalledTimes(1);
    expect(onHistoryRetry).not.toHaveBeenCalled();
  });

  it("the paging capability does not survive the socket that declared it", () => {
    openSession("cap", { paging: true });
    expect(historyPagingAvailable()).toBe(true);

    // A plain connect() supersedes the socket; the replacement has heard
    // nothing from its own server yet.
    connect();
    latest().fireOpen();

    // Carrying the capability over would page against a server that never
    // declared it — the requests are written straight to the PTY there.
    expect(historyPagingAvailable()).toBe(false);
  });

  it("a wake reconnect releases the store's solicited window", () => {
    const { sock } = openSession("sol", { paging: true });
    expect(requestHistory(500, 2)).toBe(true);
    expect(noteSolicited).toHaveBeenCalledWith(500, 502);
    clearSolicited.mockClear();

    reconnectNow();

    // The window is the store's permission to admit lines below its
    // stale-re-send watermark. Left open with no request in flight, a later
    // duplicate frame in that range keeps bypassing the guard.
    expect(clearSolicited).toHaveBeenCalled();
    expect(sock.closed).toBe(true);
  });
});

describe("connection: the reconnect schedule and what may cancel it", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(0); // jitter 0: delays are exact
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  it("disconnect cancels a scheduled reconnect instead of letting it spawn a socket", () => {
    const { sock } = openSession("cancel");
    sock.fireClose(1006); // backoff armed

    disconnect();
    vi.advanceTimersByTime(20_000); // past the whole backoff ceiling

    // A consumer with no tab to show asked for no socket. A surviving timer
    // gives it one anyway, and the server sees a connection nothing renders.
    expect(sockets).toHaveLength(1);
  });

  it("an explicit connect during the backoff window leaves no timer to spawn a second socket", () => {
    const { sock } = openSession("reentry");
    sock.fireClose(1006); // backoff armed at +500ms

    connect(); // the consumer restores a panel mid-backoff
    vi.advanceTimersByTime(5_000);

    // The orphaned timer would reset the state to disconnected and connect
    // again, while this socket's listeners stay bound: two server connections
    // for one terminal, every frame delivered twice.
    expect(sockets).toHaveLength(2);
  });

  it("a connect from the consumer's own onClose leaves no timer to spawn a third socket", () => {
    // The re-entrant shape of the test above, and the one the guard at
    // scheduleReconnect misses. The close handler sets "disconnected", calls
    // onClose, THEN schedules: a consumer that reconnects from its own close
    // hook (a "retry now" button, a status-driven reconnect) is already back at
    // "connecting" with a live socket by the time the schedule runs. Arming a
    // backoff over it also OVERWRITES connState, so connect()'s double-call
    // guard can no longer see that socket and never aborts it — the timer's
    // connect() stacks a third socket with the second one's listeners still
    // bound. That is the duplicate server connection + double delivery the
    // comment inside connect() describes, reached from the other direction.
    let reentered = 0;
    installConsumer({
      onClose: () => {
        reentered++;
        connect();
      },
    });
    const { sock } = openSession("onclose-reentry");

    sock.fireClose(1006);
    expect(reentered).toBe(1);
    expect(sockets).toHaveLength(2);
    const second = sockets[1]!;

    vi.advanceTimersByTime(5_000); // past the whole backoff ladder's first steps

    expect(sockets).toHaveLength(2);
    // One frame, one delivery: every socket still able to deliver gets the
    // server's output, so a second live socket doubles every frame the page
    // renders.
    for (const s of sockets.slice(1)) {
      s.fireOpen();
      s.fireMessage(titleFrame("live"));
    }
    expect(onMessage).toHaveBeenCalledTimes(1);
    expect(second.closed).toBe(false);
  });

  it("a connect that never opens is retried after the connect timeout", () => {
    connect();
    expect(sockets).toHaveLength(1);

    vi.advanceTimersByTime(10_000); // the connect timeout
    expect(onClose).toHaveBeenCalledTimes(1);

    vi.advanceTimersByTime(1_000); // the backoff step
    // Nothing else can rescue this state: aborting detached the close listener
    // that normally schedules the retry, so the timeout must drive it.
    expect(sockets).toHaveLength(2);
  });

  it("a socket abandoned by the connect timeout is closed and cannot deliver frames", () => {
    connect();
    const first = latest();

    vi.advanceTimersByTime(10_000);

    // The abort is what detaches the listeners AND force-closes the socket, so
    // the OS-level connection goes away rather than lingering until the browser
    // completes a handshake nobody is waiting for.
    expect(first.closed).toBe(true);
    first.fireOpen();
    first.fireMessage(titleFrame("ghost"));
    expect(onMessage).not.toHaveBeenCalled();
  });

  it("a stale connect timeout does not tear down the socket that replaced it", () => {
    // The connect timeout of a socket that CLOSED before opening is never
    // cleared: the close handler neither aborts nor clears it. It therefore
    // fires 10s later, by which time the module has moved on, and its guard is
    // the only thing standing between it and the successor's state.
    const { sock } = openSession("stale");
    sock.fireClose(1006); // -> backoff at +500ms
    vi.advanceTimersByTime(500);
    const second = latest(); // created at t=500, its own timeout at t=10_500
    expect(second).not.toBe(sock);

    second.fireClose(1006); // closes before opening -> backoff at +1000ms
    expect(onClose).toHaveBeenCalledTimes(2);
    vi.advanceTimersByTime(1_000);
    const third = latest();
    expect(third).not.toBe(second);

    // t = 10_500: `second`'s abandoned timeout fires while `third` is the live
    // socket. Acting on it would report a close nothing closed and stack a
    // second backoff on top of the one already running.
    vi.advanceTimersByTime(9_000);

    expect(onClose).toHaveBeenCalledTimes(2);
    expect(sockets).toHaveLength(3);
  });

  it("connect is refused once the server's wire revision is declared incompatible", () => {
    vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const { sock } = openSession("incompat", {
      version: MIN_SUPPORTED_SERVER_WIRE_VERSION - 1,
    });
    expect(onWireIncompatible).toHaveBeenCalledTimes(1);
    const before = sockets.length;

    connect();
    vi.advanceTimersByTime(20_000);

    // Refusal is a latch, not a one-shot: this decoder cannot consume that
    // server's frames, so retrying can only mis-decode or collect another 4002.
    expect(sockets).toHaveLength(before);
    expect(sock.closed).toBe(true);
  });

  it("a second connect while the first is still connecting orphans it", () => {
    connect();
    const first = latest();

    connect(); // e.g. a wake event arriving before the first socket opened
    expect(latest()).not.toBe(first);

    // The orphan's handlers must be gone. Left bound, it opens, sends its own
    // resume, and installs itself as the live socket over its replacement.
    first.fireOpen();
    first.fireMessage(titleFrame("ghost"));
    expect(controlsSent(first)).toEqual([]);
    expect(onMessage).not.toHaveBeenCalled();
  });

  it("a socket superseded by a wake reconnect cannot open behind the replacement", () => {
    connect();
    const first = latest();

    reconnectNow();
    expect(latest()).not.toBe(first);

    first.fireOpen();

    // Its open handler would send a resume on a socket the module has abandoned
    // and then claim "connected" for it, so the live socket's own resume ack
    // would be handled against the wrong connection state.
    expect(controlsSent(first)).toEqual([]);
  });
});

describe("connection: the liveness timer belongs to exactly one socket", () => {
  // HEARTBEAT_INTERVAL_MS 5s, IDLE_BEFORE_PROBE_MS 10s, PONG_TIMEOUT_MS 7s.
  // Every instant below is exact, and the phase of the interval is the point:
  // a second interval left running from a superseded socket evaluates the same
  // deadlines on a different phase and reaches them first.
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  it("a fresh socket's idle clock starts at its own open, not the previous socket's", () => {
    connect();
    latest().fireOpen(); // activity clock = t0
    vi.advanceTimersByTime(8_000);

    reconnectNow();
    const second = latest();
    second.fireOpen(); // activity clock must restart HERE

    vi.advanceTimersByTime(5_000); // t = 13_000: 5s of silence on this socket
    // Measuring the idle window from the previous socket's last frame probes a
    // 5-second-old connection, which costs a round trip on a metered link and
    // measures nothing.
    expect(controlsOfType(second, "ping")).toEqual([]);

    vi.advanceTimersByTime(5_000); // t = 18_000: 10s of silence
    expect(controlsOfType(second, "ping")).toEqual([{ type: "ping" }]);
  });

  it("a supersession leaves one liveness timer, so the staleness deadline is not early", () => {
    connect();
    latest().fireOpen(); // interval A: ticks at 5s, 10s, 15s, 20s…
    vi.advanceTimersByTime(2_000);

    connect(); // supersedes without stopping A's interval
    const second = latest();
    second.fireOpen(); // interval B: ticks at 7s, 12s, 17s, 22s…

    // B's own schedule: probe at t=12_000 (10s of silence since t=2_000), and
    // the first tick past the 7s grace window is t=22_000.
    vi.advanceTimersByTime(19_000); // t = 21_000
    expect(controlsOfType(second, "ping")).toEqual([{ type: "ping" }]);
    // A surviving interval reaches that deadline at t=20_000 and replaces a
    // socket whose grace window has not expired — and every replacement re-runs
    // the resume and replays the outbox.
    expect(sockets).toHaveLength(2);

    vi.advanceTimersByTime(1_000); // t = 22_000
    expect(sockets).toHaveLength(3);
  });

  it("the probe never replaces a socket while a replacement connect is in flight", () => {
    connect();
    latest().fireOpen();
    vi.advanceTimersByTime(10_000); // probe goes out on the first socket

    vi.advanceTimersByTime(2_000);
    connect(); // a replacement starts connecting; the probe is still unanswered
    const second = latest();
    expect(second.readyState).toBe(0);

    // t = 21_000: past the probe's 7s grace window (the tick at 20_000 is the
    // first one beyond it) and short of the replacement's own 10s connect
    // timeout at 22_000, so nothing else can create a socket in this window.
    vi.advanceTimersByTime(9_000);

    // The probe's verdict is about a CONNECTED socket. Applied while a connect
    // is in flight it tears down the connecting socket and starts a third,
    // which is how a single blip becomes a flap.
    expect(sockets).toHaveLength(2);
  });
});

describe("connection: session identity and the resume ledger", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  it("the unmanaged session id is minted once and re-read from sessionStorage", () => {
    // sessionStorage is per-tab and survives reload/BFCache/iOS resume, which is
    // exactly the lifetime a resume token wants. A consumer keying persisted
    // scrollback by session has nothing else to key it by, so re-minting on the
    // second read would silently orphan everything stored under the first.
    forgetSession(currentSessionId()); // "unmanaged" = no session set
    sessionStorage.clear();

    const first = currentSessionId();
    expect(sessionStorage.getItem("vterm-session-id")).toBe(first);

    forgetSession(first); // force the module to resolve it again
    expect(currentSessionId()).toBe(first);
  });

  it("forgetting a session drops its ledger, so a later switch back resumes from zero", () => {
    const id = freshId("forget");
    setSession(id);
    latest().fireOpen();
    latest().fireMessage(resumeAckFrame({ received: 0 }));
    expect(sendBinary(new Uint8Array([65, 66, 67]))).toBe(true);

    forgetSession(id);
    setSession(id); // the shell re-opens a tab with the same server id
    const revived = latest();
    revived.fireOpen();

    // Its outbox and byte counters went with the tab. Keeping them would make
    // the resume claim bytes this sender never sent on the new ledger and
    // replay three keystrokes the user typed into a closed tab.
    const resume = controlsOfType(revived, "resume");
    expect(resume).toHaveLength(1);
    expect(resume[0]!["sentBytes"]).toBe(0);
    revived.fireMessage(resumeAckFrame({ received: 0 }));
    expect(inputSent(revived)).toEqual([]);
  });

  it("forgetting a background session leaves the live socket alone", () => {
    const background = freshId("bg");
    adoptPersistedEpoch(background, 4_242); // registers the session
    const { sock } = openSession("fg");

    forgetSession(background);

    // Closing a tab is not a reason to drop the terminal the user is looking
    // at, and the shell has no way to notice: it would just stop receiving
    // frames until something else reconnected.
    expect(sock.closed).toBe(false);
    expect(sockets).toHaveLength(1);
  });

  it("a host with no Web Crypto at all fails closed instead of throwing a type error", () => {
    // The sessionId is a resume token the server trusts to re-attach a client,
    // so an unavailable CSPRNG must refuse rather than degrade. The refusal has
    // to be the module's own diagnosis: a TypeError from reading a property of
    // `undefined` tells an operator nothing about what to fix.
    vi.stubGlobal("crypto", undefined);

    expect(() => generateSessionId()).toThrow(/no cryptographically secure RNG available/);
  });
});

describe("connection: what a resume ack tells the store", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  it("an ordinary ack reports no epoch change", () => {
    openSession("epoch-first", { serverEpoch: 777 });

    // epochChanged is the store's instruction to throw away everything it holds
    // as belonging to a previous server process. Reported on a first attach it
    // discards a perfectly live screen on every connect.
    expect(resumeTransitions).toEqual([
      { epochChanged: false, paging: false, sentHaveThrough: -1 },
    ]);
  });

  it("a boot-epoch change is reported to the store's ack transition", () => {
    const id = freshId("epoch-move");
    setSession(id);
    latest().fireOpen();
    latest().fireMessage(resumeAckFrame({ serverEpoch: 777 }));

    reconnectNow();
    latest().fireOpen();
    latest().fireMessage(resumeAckFrame({ serverEpoch: 888 }));

    // Absolute line indices are only meaningful within one server process. A
    // restart the store never hears about leaves it presenting the previous
    // run's output as live and then refusing the new session's low indices.
    expect(resumeTransitions.at(-1)?.epochChanged).toBe(true);
    expect(onServerRestart).toHaveBeenCalledTimes(1);
  });

  it("a restart drops the outbox, so nothing is replayed onto the new process", () => {
    const id = freshId("restart-ledger");
    setSession(id);
    latest().fireOpen();
    latest().fireMessage(resumeAckFrame({ serverEpoch: 777 }));
    expect(sendBinary(new Uint8Array([1, 2, 3]))).toBe(true);

    reconnectNow();
    const second = latest();
    second.fireOpen();
    second.fireMessage(resumeAckFrame({ serverEpoch: 888, received: 0 }));

    // The new process has no record of the previous boot's input, so the queued
    // bytes cannot be "delivered late" — they can only be executed against a
    // shell that never saw the earlier ones.
    expect(inputSent(second)).toEqual([]);
    reconnectNow();
    const third = latest();
    third.fireOpen();
    expect(controlsOfType(third, "resume")[0]!["sentBytes"]).toBe(0);
  });

  it("a seeded epoch no server will confirm reads as a restart exactly once", () => {
    const id = freshId("seeded");
    adoptPersistedEpoch(id, 999); // a claim about content restored from disk
    setSession(id);
    latest().fireOpen();
    latest().fireMessage(resumeAckFrame({ serverEpoch: 0 })); // version-silent

    // Unverifiable restored content is handled as a restart: the alternative is
    // presenting a previous run's output as live and then permanently refusing
    // the new session's low absolute indices.
    expect(resumeTransitions.at(-1)?.epochChanged).toBe(true);
    expect(onServerRestart).toHaveBeenCalledTimes(1);

    reconnectNow();
    latest().fireOpen();
    latest().fireMessage(resumeAckFrame({ serverEpoch: 0 }));

    // The claim was settled by the first ack. Re-arming it makes a server that
    // simply never reports an epoch announce a restart on every reconnect.
    expect(onServerRestart).toHaveBeenCalledTimes(1);
  });

  it("the ack's retained-history bounds are handed to the consumer", () => {
    openSession("bounds", { committed: 500, oldestIndex: 100 });

    // Without them the client cannot tell a genuine server-side trim (lines it
    // was missing are gone for good) from a still-loading state, so it either
    // shows a false "earlier output trimmed" marker or waits forever.
    expect(onResumeBounds).toHaveBeenCalledWith(500, 100);
  });
});

describe("connection: acks that ride other frames still trim the outbox", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  /** Reconnect, answer the resume with received=0, and report what replayed. */
  function replayAfterReconnect(): number[][] {
    reconnectNow();
    const sock = latest();
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0 }));
    return inputSent(sock);
  }

  it("a modes frame's piggybacked ack trims the outbox", () => {
    const { sock } = openSession("modes-ack");
    expect(sendBinary(new Uint8Array([65, 66, 67, 68, 69]))).toBe(true);

    sock.fireMessage(modesFrame(5)); // the server applied all five bytes

    // A modes frame is the only carrier when the app changed a DEC mode and
    // printed nothing. Ignoring its ack leaves five acknowledged bytes in the
    // outbox to be executed a second time on the next reconnect.
    expect(replayAfterReconnect()).toEqual([]);
  });

  it("an ack riding an ordinary content frame trims the outbox", () => {
    const { sock } = openSession("content-ack");
    expect(sendBinary(new Uint8Array([65, 66, 67, 68, 69]))).toBe(true);

    sock.fireMessage(titleFrame("done", 5));

    expect(replayAfterReconnect()).toEqual([]);
  });

  it("a pre-ack scrollback clear is forwarded rows-less and without the bell", () => {
    const id = freshId("preack");
    setSession(id);
    const sock = latest();
    sock.fireOpen(); // open, but no resume ack yet

    sock.fireMessage(screenFrame({ bell: true, scrollbackCleared: true }));

    // ED3 is a consumed one-shot: dropping it entirely would leave the client
    // displaying history the server has already discarded. The bell is dropped
    // on purpose — it announces a screen the user has not been shown yet, and
    // the resume batch is about to repaint it.
    expect(onMessage).toHaveBeenCalledTimes(1);
    const msg = onMessage.mock.calls[0]![0];
    expect(msg).toMatchObject({
      type: "screen",
      scrollbackCleared: true,
      bell: false,
      rows: [],
      changed: [],
    });
  });
});

describe("connection: binary frame delivery and its failure modes", () => {
  // iOS Safari delivers binary WebSocket frames as Blob, so the module carries a
  // second decode path whose hop is asynchronous. Real timers here: the
  // Blob→ArrayBuffer conversion is a genuine microtask chain.
  beforeEach(() => {
    sockets.length = 0;
    vi.useRealTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
  });

  it("a Blob frame is decoded and delivered like an ArrayBuffer one", async () => {
    const { sock } = openSession("blob-ok");

    sock.fireMessage(new Blob([new Uint8Array(titleFrame("blobbed"))]));
    await drainBlobChain();

    // The whole iOS path depends on this: a client that silently drops Blob
    // frames looks connected and renders nothing, and the liveness probe never
    // fires because arrival already refreshed the activity clock.
    expect(onMessage).toHaveBeenCalledWith(expect.objectContaining({ title: "blobbed" }));
  });

  it("a text frame the protocol does not define is dropped without complaint", async () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => undefined);
    const { sock } = openSession("text-frame");

    sock.fireMessage(JSON.stringify({ type: "screen" }));
    await drainBlobChain();

    // Server→client text frames were retired in 2026-07 (they were accepted as
    // unvalidated ServerMessages). Ignoring one is correct; routing it into a
    // binary decoder makes every stray text frame a logged decode failure.
    expect(onMessage).not.toHaveBeenCalled();
    expect(errSpy).not.toHaveBeenCalled();
  });

  it("a consumer that throws on an ArrayBuffer frame is logged, not propagated", () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => undefined);
    installConsumer({
      onMessage: () => {
        throw new Error("consumer blew up");
      },
    });
    const id = freshId("throw-ab");
    setSession(id);
    const sock = latest();
    sock.fireOpen();

    expect(() => {
      sock.fireMessage(titleFrame("boom"));
    }).not.toThrow();

    // Field observability has to be the same on both decode paths: without the
    // log this surfaces as a bare uncaught exception with no engine context,
    // and on the Blob path it is invisible.
    expect(errSpy).toHaveBeenCalledWith("vterm: dropped binary frame", expect.any(Error));
  });

  it("a consumer that throws on a Blob frame is logged and the chain keeps draining", async () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => undefined);
    let boom = true;
    const seen: string[] = [];
    installConsumer({
      onMessage: (msg) => {
        if (boom) {
          throw new Error("consumer blew up");
        }
        seen.push(msg.type);
      },
    });
    const id = freshId("throw-blob");
    setSession(id);
    const sock = latest();
    sock.fireOpen();

    sock.fireMessage(new Blob([new Uint8Array(titleFrame("first"))]));
    await drainBlobChain();
    expect(errSpy).toHaveBeenCalledWith("vterm: dropped binary (blob) frame", expect.any(Error));

    boom = false;
    sock.fireMessage(new Blob([new Uint8Array(titleFrame("second"))]));
    await drainBlobChain();

    // A rejected chain skips every later .then, so one throwing callback would
    // drop all binary frames until reconnect.
    expect(seen).toEqual(["title"]);
  });

  it("a Blob frame that resolves after its socket was replaced is dropped", async () => {
    const id = freshId("blob-stale");
    setSession(id);
    const first = latest();
    first.fireOpen();

    // The conversion is queued, then the socket dies and is replaced. The close
    // path does NOT abort the listeners, so this frame's `.then` is still live.
    first.fireMessage(new Blob([new Uint8Array(titleFrame("stale"))]));
    first.fireClose(1006);
    connect();
    const second = latest();
    expect(second).not.toBe(first);
    second.fireOpen();

    await drainBlobChain();

    // Handled, this frame's resumeAck would upgrade, reset or retransmit
    // against the REPLACEMENT socket's server; even a title is a fact about a
    // connection that no longer exists.
    expect(onMessage).not.toHaveBeenCalled();
  });

  it("an error event on the socket is inert", () => {
    const { sock } = openSession("err");

    expect(() => {
      sock.fireError();
    }).not.toThrow();
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe("connection: an incompatible server stops the socket", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    installConsumer();
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
  });

  it("a below-floor revision is reported, and the socket delivers nothing after it", () => {
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const { sock } = openSession("below-floor", {
      version: MIN_SUPPORTED_SERVER_WIRE_VERSION - 1,
    });

    expect(warnSpy).toHaveBeenCalledWith(
      "vterm: refusing incompatible server wire protocol",
      expect.stringContaining("below client minimum"),
    );

    // Refusal has to detach the socket, not merely latch a flag: this decoder
    // cannot safely consume that server's frames, so any that follow are
    // exactly what the refusal exists to keep out.
    sock.fireMessage(titleFrame("after refusal"));
    expect(onMessage).not.toHaveBeenCalled();
  });

  it("a 4002 close carries the server's own reason to the consumer", () => {
    const { sock } = openSession("close-4002");

    sock.fireClose(WIRE_INCOMPATIBLE_CLOSE_CODE, "server needs wire 9");

    // The server's reason names the actual mismatch; the built-in sentence is
    // the fallback for a close that carries none, not a replacement for it.
    expect(onWireIncompatible).toHaveBeenCalledWith(
      expect.objectContaining({ source: "server-close", reason: "server needs wire 9" }),
    );
  });

  it("a 4002 close with no reason falls back to the client's own sentence", () => {
    const { sock } = openSession("close-4002-bare");

    sock.fireClose(WIRE_INCOMPATIBLE_CLOSE_CODE, "");

    expect(onWireIncompatible).toHaveBeenCalledWith(
      expect.objectContaining({ reason: expect.stringContaining("reload or upgrade") }),
    );
  });
});

describe("connection: an initialSize provider that misbehaves", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    installConsumer(); // leave a sane consumer for later files
  });

  it("a throwing provider degrades to announcing nothing, and says so", () => {
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    installConsumer({
      initialSize: () => {
        throw new Error("measuring mid-transition");
      },
    });
    const id = freshId("size-throw");
    setSession(id);
    const sock = latest();

    sock.fireOpen();

    // This runs inside the open handler after the connect timeout was cleared
    // and before the resume is sent, so an escaping throw leaves the resume
    // unsent and the state stuck at "connecting" with nothing left to rescue
    // it. Degrading silently instead would hide a broken provider forever.
    expect(controlsOfType(sock, "resize")).toEqual([]);
    expect(controlsOfType(sock, "resume")).toHaveLength(1);
    expect(warnSpy).toHaveBeenCalledWith(
      "vterm: initialSize provider threw; announcing no size",
      expect.any(Error),
    );
  });
});

describe("connection: the module's own defaults, on a pristine instance", () => {
  // `managed`, `wsPath` and `cb` are module-scope and one-way: setSession latches
  // managed forever, init latches wsPath, and no export resets either. The
  // unmanaged contract therefore cannot be observed on a module some other suite
  // has already switched sessions on (vitest runs these files with
  // isolate:false), so these tests import a FRESH module instance instead.
  let mod: typeof ConnectionModule;

  beforeEach(async () => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    vi.resetModules();
    mod = await import("./connection.js");
  });

  afterEach(() => {
    mod.disconnect();
    vi.useRealTimers();
  });

  it("an unmanaged resume carries the bare per-tab id, not a per-sender composite", () => {
    sessionStorage.clear();
    mod.init({
      onMessage: () => undefined,
      onOpen: () => undefined,
      onClose: () => undefined,
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
    mod.connect();
    const sock = latest();
    sock.fireOpen();

    // The per-sender "<id>#<instance>" key exists because a MANAGED session id
    // is shared by every device attached to it. An unmanaged id is already per
    // tab, so composing it would key a second server-side ledger on every
    // reload and lose the resume the sessionStorage id exists to provide.
    const resume = controlsOfType(sock, "resume");
    expect(resume).toHaveLength(1);
    expect(resume[0]!["sessionId"]).toBe(mod.currentSessionId());
    expect(String(resume[0]!["sessionId"])).not.toContain("#");
  });

  it("an unmanaged socket connects to the bare path with no session query", () => {
    mod.init({
      onMessage: () => undefined,
      onOpen: () => undefined,
      onClose: () => undefined,
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
    mod.connect();

    // The single-terminal path matches its session purely by the resume frame's
    // id. A ?session= the consumer never chose routes the socket through the
    // manager's dispatch instead, which answers with 4004 for an id it has
    // never issued.
    expect(latest().url).not.toContain("session=");
  });

  it("a consumer's wsPath replaces the default endpoint", () => {
    mod.init({
      onMessage: () => undefined,
      onOpen: () => undefined,
      onClose: () => undefined,
      computeSize: () => ({ cols: 80, rows: 24 }),
      wsPath: "/api/shell/ws",
    });
    mod.connect();

    // vibekit serves its shell at /api/shell/ws. Ignoring the override sends
    // every consumer to /ws, where nothing is listening.
    expect(new URL(latest().url).pathname).toBe("/api/shell/ws");
  });

  it("a socket opened before any consumer registered asks for a full retained replay", () => {
    // connect() before init() is reachable (a consumer wiring order bug, or a
    // wake event during startup) and the resume still has to be a valid one.
    mod.connect();
    const sock = latest();
    sock.fireOpen();

    // -1 is "I hold nothing, replay everything you retain". Any other value
    // claims lines this client does not have, and the server then replays from
    // above them — leaving a permanent hole no later frame fills.
    const resume = controlsOfType(sock, "resume");
    expect(resume).toHaveLength(1);
    expect(resume[0]!["haveThrough"]).toBe(-1);
  });
});
