// Regression test for the duplicate-output-on-reconnect bug. When the
// browser fired both `visibilitychange` and `pageshow` on iPad wake,
// each event triggered connection.reconnectNow(); the old WebSocket
// was .close()'d but its addEventListener('message', ...) handler was
// NOT removed. Frames the server delivered between the close request
// and the close-handshake completion were processed by both the
// orphaned old sock AND the freshly-created new sock — every WS frame
// produced two handleScreen / handleScroll calls, leading to
// duplicated DOM rows. The fix uses an AbortController per sock and
// passes its signal to every addEventListener so the listeners
// auto-detach when the controller is aborted (which the new connect()
// does before creating the replacement sock).

import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import {
  adoptPersistedEpoch,
  connect,
  currentSessionId,
  disconnect,
  forgetSession,
  generateSessionId,
  init,
  reconnectNow,
  sendBinary,
  sendResize,
  serverEpochOf,
  setSession,
} from "./connection.js";
import * as modes from "./modes.js";
import * as render from "./render.js";
import { LineStore } from "./store.js";
import { mapKeyboardEvent } from "./keyboard.js";
import type { ServerMessage } from "./types.js";

interface MockWS {
  url: string;
  binaryType: string;
  readyState: number;
  closed: boolean;
  listeners: Map<string, ((ev: unknown) => void)[]>;
  signals: Map<string, AbortSignal | undefined>;
  send: (...args: unknown[]) => unknown;
  close: (...args: unknown[]) => unknown;
  addEventListener: (
    type: string,
    handler: (ev: unknown) => void,
    opts?: { signal?: AbortSignal },
  ) => void;
  // Test helpers:
  fireOpen: () => void;
  fireMessage: (data: ArrayBuffer | Blob | string) => void;
  fireClose: (code?: number) => void;
}

const allMockWebSockets: MockWS[] = [];

function makeMockWebSocket(): typeof WebSocket {
  // Constructor returning MockWS; we type it as `unknown as typeof WebSocket`
  // so it can stand in for the global.
  const ctor = function (url: string): MockWS {
    const sock: MockWS = {
      url,
      binaryType: "blob",
      readyState: 0, // CONNECTING
      closed: false,
      listeners: new Map(),
      signals: new Map(),
      send: vi.fn(),
      close: vi.fn(function (this: MockWS) {
        this.closed = true;
        this.readyState = 3; // CLOSED
      }) as unknown as ReturnType<typeof vi.fn>,
      addEventListener(
        this: MockWS,
        type: string,
        handler: (ev: unknown) => void,
        opts?: { signal?: AbortSignal },
      ): void {
        if (!this.listeners.has(type)) {
          this.listeners.set(type, []);
        }
        this.listeners.get(type)!.push(handler);
        // Honor signal: when aborted, remove this listener.
        if (opts?.signal) {
          const list = this.listeners.get(type)!;
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
        for (const fn of this.listeners.get("open") ?? []) {
          fn({});
        }
      },
      fireMessage(this: MockWS, data: ArrayBuffer | Blob | string) {
        for (const fn of this.listeners.get("message") ?? []) {
          fn({ data });
        }
      },
      fireClose(this: MockWS, code = 1000) {
        this.readyState = 3;
        for (const fn of this.listeners.get("close") ?? []) {
          fn({ code });
        }
      },
    } as unknown as MockWS;
    Object.setPrototypeOf(sock, MockWebSocket.prototype);
    allMockWebSockets.push(sock);
    return sock;
  } as unknown as typeof WebSocket;

  // Stamp a prototype so `instanceof` checks work if any. Not used here
  // but keeps TS happy.
  class MockWebSocket {}
  ctor.prototype = MockWebSocket.prototype as unknown as WebSocket;
  return ctor;
}

// Decode a 0x00-prefixed control frame (the JSON the client sends for
// resume/resize) back to an object. Returns null for raw-input frames or
// non-frame sends, so callers can filter the send log down to control frames.
interface DecodedControlFrame {
  type?: string;
  haveThrough?: number;
  sentBytes?: number;
  sessionId?: string;
  cols?: number;
  rows?: number;
}

function decodeControlFrame(arg: unknown): DecodedControlFrame | null {
  let bytes: Uint8Array | null = null;
  if (arg instanceof Uint8Array) {
    bytes = arg;
  } else if (arg instanceof ArrayBuffer) {
    bytes = new Uint8Array(arg);
  }
  if (!bytes || bytes.length === 0 || bytes[0] !== 0x00) {
    return null;
  }
  return JSON.parse(new TextDecoder().decode(bytes.subarray(1))) as DecodedControlFrame;
}

// Every control frame the mock socket was asked to send, decoded, in order.
function controlFramesSent(sock: MockWS): DecodedControlFrame[] {
  const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
  return calls
    .map((c) => decodeControlFrame(c[0]))
    .filter((m): m is DecodedControlFrame => m !== null);
}

// A binary pong frame as the server sends it: [1B type=5 pong][8B ack=0].
// Mirrors encodePongMsg (Go) / MSG_PONG (wire-binary.ts). Decodes to null —
// the client treats its mere arrival as proof of liveness.
function pongFrame(): ArrayBuffer {
  return new Uint8Array([5, 0, 0, 0, 0, 0, 0, 0, 0]).buffer;
}

describe("connection: a socket superseded by a reconnect delivers no duplicate messages", () => {
  // Drives the REAL connection module (init + connect + reconnectNow) with
  // a fake global WebSocket, asserting the observable contract: once a
  // socket is superseded, frames still arriving on it never reach the
  // onMessage callback. This is the duplicate-output-on-iPad-wake bug —
  // the fix is the AbortController-per-socket whose signal detaches every
  // listener when the replacement socket is created.
  let onMessage: ReturnType<typeof vi.fn<(msg: ServerMessage) => void>>;

  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers(); // neutralize the 10s connect-timeout / reconnect backoff
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    onMessage = vi.fn<(msg: ServerMessage) => void>();
    init({
      onMessage,
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  // Binary title frame ([1B type=4][8B ack][2B len][utf8 title]) mirroring
  // encodeTitleMsg — the server-sent text-JSON form no longer exists (the
  // dormant unvalidated string branch was removed 2026-07).
  function titleFrame(title: string): ArrayBuffer {
    const body = new TextEncoder().encode(title);
    const buf = new ArrayBuffer(11 + body.length);
    const v = new DataView(buf);
    v.setUint8(0, 4); // MSG_TITLE
    v.setBigUint64(1, 0n, true);
    v.setUint16(9, body.length, true);
    new Uint8Array(buf).set(body, 11);
    return buf;
  }

  it("reconnecting while still connecting orphans the first socket so its late frames are ignored", () => {
    connect();
    const first = allMockWebSockets[0]!;

    // iPad wake fires visibilitychange + pageshow almost together; the
    // second reconnect supersedes `first` before it ever opened.
    reconnectNow();
    const second = allMockWebSockets[1]!;
    expect(second).toBeDefined();
    expect(second).not.toBe(first);

    // A frame the server already had in flight is delivered to the
    // orphaned socket after it was superseded — it must NOT propagate.
    first.fireOpen();
    first.fireMessage(titleFrame("ghost"));
    expect(onMessage).not.toHaveBeenCalled();

    // The live socket delivers normally.
    second.fireOpen();
    second.fireMessage(titleFrame("live"));
    expect(onMessage).toHaveBeenCalledTimes(1);
  });

  it("a frame in flight on the previous socket is not double-delivered after a fresh connect", () => {
    connect();
    const first = allMockWebSockets[0]!;
    first.fireOpen();
    first.fireMessage(titleFrame("one"));
    expect(onMessage).toHaveBeenCalledTimes(1);

    // A new connect() while connected supersedes `first` (its double-call
    // guard aborts the existing socket before creating the replacement).
    connect();
    const second = allMockWebSockets[1]!;
    expect(second).not.toBe(first);

    first.fireMessage(titleFrame("dup"));
    expect(onMessage).toHaveBeenCalledTimes(1); // orphan can't re-deliver

    second.fireOpen();
    second.fireMessage(titleFrame("two"));
    expect(onMessage).toHaveBeenCalledTimes(2);
  });
});

describe("connection: wake reconnect resumes from the held index (bug 2)", () => {
  // Bug 2: on iOS wake the socket is frequently a zombie that still reads
  // OPEN, so the old reconnectNow() early-returned on "connected" and never
  // resynced — anything printed during sleep stayed missing until a manual
  // refresh. These pin the two halves of the fix: (a) reconnectNow()
  // unconditionally tears down a healthy socket and opens a fresh one, and
  // (b) the resume frame carries the highest absolute line index the client
  // holds (getHaveThrough), so the server backfills exactly the missed lines.
  let onMessage: ReturnType<typeof vi.fn<(msg: ServerMessage) => void>>;

  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    onMessage = vi.fn<(msg: ServerMessage) => void>();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function baseCallbacks() {
    return {
      onMessage,
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    };
  }

  it("tears down a healthy connected socket and opens a fresh one", () => {
    init(baseCallbacks());
    connect();
    const first = allMockWebSockets[0]!;
    first.fireOpen(); // status === "connected": the zombie-looking-but-healthy state
    expect(first.closed).toBe(false);

    // Simulate iOS wake. The old code returned early here (status was
    // "connected") and never resynced — the exact bug-2 smoking gun.
    reconnectNow();

    expect(first.closed).toBe(true); // the (possibly zombie) socket is torn down ...
    expect(allMockWebSockets.length).toBe(2); // ... and a fresh socket is opened
    expect(allMockWebSockets[1]!).not.toBe(first);
  });

  it("sends a resume frame carrying the haveThrough the consumer reports", () => {
    init({ ...baseCallbacks(), getHaveThrough: () => 42 });
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    const resume = controlFramesSent(sock).find((m) => m.type === "resume");
    expect(resume).toBeDefined();
    expect(resume!.haveThrough).toBe(42); // server replays only lines after 42
  });

  it("sends -1 with nothing wired AND nothing held, because that is the boundary", () => {
    // Reads as the old "no callback means -1" case and is no longer that. A
    // consumer supplying no getHaveThrough now gets the engine's own answer, and
    // for an empty store that answer IS -1: nothing is held, so nothing can be
    // claimed. The next test is the one that distinguishes the two.
    init(baseCallbacks());
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    const resume = controlFramesSent(sock).find((m) => m.type === "resume");
    expect(resume).toBeDefined();
    expect(resume!.haveThrough).toBe(-1);
  });

  it("claims the engine's own boundary when the consumer wires nothing", () => {
    // The point of the engine defaults. Before them, a consumer that supplied no
    // getHaveThrough asked for a full bounded replay on every attach, and one that
    // supplied the wrong accessor claimed rows the application had merely drawn.
    // Neither is now reachable by omission: the transport answers from the store.
    //
    // Fails if engineDefaults() loses its getHaveThrough member, which is the
    // regression this guards.
    // render.bind rebuilds DOM rows, so the renderer needs its two elements. Two
    // divs is the whole requirement; nothing here depends on layout or geometry.
    const output = document.createElement("div");
    const termWrap = document.createElement("div");
    termWrap.appendChild(output);
    document.body.appendChild(termWrap);
    render.init({ output, termWrap });

    const store = new LineStore();
    store.applyScroll({
      type: "scroll",
      firstIndex: 0,
      lines: Array.from({ length: 100 }, (_, i) => [
        { t: `h${String(i)}`, f: -1, b: -1, a: 0, uc: -1 },
      ]),
    });
    render.bind(store);

    init(baseCallbacks());
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    const resume = controlFramesSent(sock).find((m) => m.type === "resume");
    expect(resume).toBeDefined();
    expect(resume!.haveThrough).toBe(99);
  });

  it("announces the initial size BEFORE the resume so the snapshot comes back at it", () => {
    // Order is the point: the server applies a resize the moment it decodes it,
    // so a resize preceding the resume makes the resume's window snapshot and
    // history replay arrive at this client's geometry. If it arrives afterwards,
    // the client is answered at whatever size the session last held and a
    // program that redraws on SIGWINCH then reprints INTO the replay it just
    // applied (measured: the restore's row count collapsed mid-flight).
    init({ ...baseCallbacks(), initialSize: () => ({ cols: 100, rows: 40 }) });
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    const frames = controlFramesSent(sock);
    const resizeAt = frames.findIndex((m) => m.type === "resize");
    const resumeAt = frames.findIndex((m) => m.type === "resume");
    expect(resizeAt).toBeGreaterThanOrEqual(0);
    expect(resumeAt).toBeGreaterThanOrEqual(0);
    expect(resizeAt).toBeLessThan(resumeAt);
    expect(frames[resizeAt]!.cols).toBe(100);
    expect(frames[resizeAt]!.rows).toBe(40);
  });

  it("announces nothing when the consumer cannot measure yet, leaving the resume first", () => {
    // null is the explicit "not yet" answer (fonts still loading, viewport mid
    // transition). Announcing an untrustworthy measurement is worse than
    // announcing none: it costs a second resize and therefore a second redraw.
    init({ ...baseCallbacks(), initialSize: () => null });
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    const frames = controlFramesSent(sock);
    expect(frames.some((m) => m.type === "resize")).toBe(false);
    expect(frames[0]!.type).toBe("resume");
  });

  it("keeps the resume as message one when no initialSize is wired at all", () => {
    init(baseCallbacks());
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    expect(controlFramesSent(sock)[0]!.type).toBe("resume");
  });

  it("refuses a malformed announced size rather than putting it on the wire", () => {
    // The server floors an out-of-range resize rather than dropping it, so a 0
    // or a fraction would silently resize the session to the minimum instead of
    // being ignored. Validate at the boundary that owns the frame.
    for (const bad of [
      { cols: 0, rows: 24 },
      { cols: 80, rows: 0 },
      { cols: -80, rows: 24 },
      { cols: 80.5, rows: 24 },
      { cols: Number.NaN, rows: 24 },
    ]) {
      allMockWebSockets.length = 0;
      init({ ...baseCallbacks(), initialSize: () => bad });
      connect();
      const sock = allMockWebSockets[0]!;
      sock.fireOpen();
      expect(controlFramesSent(sock).some((m) => m.type === "resize")).toBe(false);
    }
  });

  it("survives a provider that throws, still sending the resume", () => {
    // The provider runs inside the open handler, after the connect timeout has
    // been cleared and before the resume is sent, so a throw that escaped would
    // leave the resume unsent and the connection stuck at "connecting" with no
    // timeout left to rescue it: a permanently dead socket from a consumer bug.
    init({
      ...baseCallbacks(),
      initialSize: () => {
        throw new Error("consumer bug");
      },
    });
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    const frames = controlFramesSent(sock);
    expect(frames.some((m) => m.type === "resize")).toBe(false);
    expect(frames[0]!.type).toBe("resume");
    // And the socket is live enough to carry input afterwards.
    sendResize();
    expect(controlFramesSent(sock).some((m) => m.type === "resize")).toBe(true);
  });

  it("does not re-send the announced size when the consumer calls sendResize at the same size", () => {
    // The announce seeds the deduplication baseline, so a consumer that also
    // measures on open (the usual shape) costs one resize, not two.
    init({ ...baseCallbacks(), initialSize: () => ({ cols: 100, rows: 40 }) });
    connect();
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();
    sendResize(); // computeSize reports 80x24 in baseCallbacks, so this one lands
    const resizes = controlFramesSent(sock).filter((m) => m.type === "resize");
    expect(resizes.length).toBe(2);

    sendResize(); // same size again: deduplicated
    expect(controlFramesSent(sock).filter((m) => m.type === "resize").length).toBe(2);
  });

  it("probes a silent socket with a ping and reconnects when the probe goes unanswered", () => {
    init(baseCallbacks());
    connect();
    const first = allMockWebSockets[0]!;
    first.fireOpen();

    // No inbound frames at all. After IDLE_BEFORE_PROBE_MS of silence the
    // heartbeat actively probes with a ping (10s; one tick lands at 10s).
    vi.advanceTimersByTime(10_000);
    expect(controlFramesSent(first).some((m) => m.type === "ping")).toBe(true);

    // The probe draws no pong (nor any other frame): the socket is stale.
    // Past the grace window the heartbeat tears it down and opens a fresh one.
    vi.advanceTimersByTime(11_000);
    expect(first.closed).toBe(true);
    expect(allMockWebSockets.length).toBe(2);
    expect(allMockWebSockets[1]!).not.toBe(first);
  });

  it("keeps a socket whose probe is answered by a pong (and the pong stays out of the UI)", () => {
    init(baseCallbacks());
    connect();
    const first = allMockWebSockets[0]!;
    first.fireOpen();

    vi.advanceTimersByTime(10_000); // triggers a probe ping
    expect(controlFramesSent(first).some((m) => m.type === "ping")).toBe(true);

    // The pong arrives. It refreshes the liveness clock (so the socket is not
    // declared stale) and decodes to null (so it never reaches onMessage).
    first.fireMessage(pongFrame());
    expect(onMessage).not.toHaveBeenCalled();

    // Advance past the point where an unanswered probe WOULD have reconnected:
    // because the pong reset the clock, the socket is kept.
    vi.advanceTimersByTime(11_000);
    expect(first.closed).toBe(false);
    expect(allMockWebSockets.length).toBe(1);
  });
});

describe("connection: per-session resume state (tab switch)", () => {
  // The reconnect-on-switch model runs N server sessions but one live socket,
  // swapped on switch. Each session owns its resume state (outbox, byte
  // counters, boot-epoch), so a switch must not replay one tab's unacked bytes
  // onto another or fire a false server-restart reset (design sections 5, 8, 18).
  let onMessage: ReturnType<typeof vi.fn<(msg: ServerMessage) => void>>;
  let onServerRestart: ReturnType<typeof vi.fn<() => void>>;

  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    onMessage = vi.fn<(msg: ServerMessage) => void>();
    onServerRestart = vi.fn<() => void>();
    init({
      onMessage,
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      onServerRestart,
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
  });

  afterEach(() => {
    disconnect(); // drop the live socket so state doesn't leak into later tests
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function resumeAckFrame(fields: {
    received: number;
    serverEpoch?: number;
    committed?: number;
    oldestIndex?: number;
  }): ArrayBuffer {
    // Binary resumeAck (33-byte pre-tail form) mirroring encodeResumeAck; the
    // server-sent text-JSON form no longer exists (dormant branch removed
    // 2026-07). serverEpoch 0 = "absent" (skips restart detection), matching
    // the epoch-gating in handleDecoded.
    const buf = new ArrayBuffer(33);
    const v = new DataView(buf);
    v.setUint8(0, 2); // MSG_RESUME_ACK
    v.setBigUint64(1, BigInt(fields.received), true);
    v.setBigUint64(9, BigInt(fields.serverEpoch ?? 0), true);
    v.setBigUint64(17, BigInt(fields.committed ?? 0), true);
    v.setBigUint64(25, BigInt(fields.oldestIndex ?? 0), true);
    return buf;
  }

  // The non-control (raw input) frames a socket was asked to send.
  function rawInputSends(sock: MockWS): unknown[] {
    const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
    return calls.map((c) => c[0]).filter((arg) => decodeControlFrame(arg) === null);
  }

  // Module state (the `sessions` map, activeId, managed) persists across tests
  // in this file (vitest isolate:false), so each test uses its own session ids
  // to stay isolated rather than relying on a reset that the module does not
  // expose.

  it("treats a hydrated session's first resumeAck as a restart when the epoch moved", () => {
    // The corruption case persistence exists to avoid. Absolute line indices are
    // only meaningful within one server process, and restart detection is
    // otherwise in-memory: the FIRST resumeAck of a page load records the epoch
    // with nothing to compare against. So a store restored from disk after the
    // server restarted would be presented as live, and the new session's output
    // — whose indices start again near 0 — would then be refused by the store's
    // staleness guard. Seeding the persisted epoch is what gives that first
    // resumeAck something to compare against.
    adoptPersistedEpoch("epoch-restored", 111);
    setSession("epoch-restored");
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 222 }));

    expect(onServerRestart).toHaveBeenCalledTimes(1);
  });

  it("stays quiet when a hydrated session reconnects to the SAME server process", () => {
    // The other half: a wake that finds the server still running must not throw
    // away the content that was just restored.
    adoptPersistedEpoch("epoch-same", 333);
    setSession("epoch-same");
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 333 }));

    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("ignores an absent persisted epoch rather than seeding a sentinel", () => {
    // 0 means "no epoch was ever recorded", which is indistinguishable from a
    // version-silent server. Seeding it would make the next real epoch look like
    // a restart and wipe a perfectly good restore.
    adoptPersistedEpoch("epoch-zero", 0);
    adoptPersistedEpoch("epoch-zero", Number.NaN);
    setSession("epoch-zero");
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 444 }));

    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("treats a hydrated session as restarted when the server reports NO epoch", () => {
    // The one hole the epoch comparison alone leaves, and it is the design's worst
    // case rather than a nuisance: a server downgraded to reporting no epoch between
    // the save and the load never contradicts the seeded value, so the stale store
    // would be presented as live and would then refuse the new session's low
    // absolute indices — wrong, and then permanently blank. A seeded epoch is a
    // CLAIM about restored content; a resume that cannot confirm it is handled as a
    // restart.
    adoptPersistedEpoch("epoch-silent-server", 111);
    setSession("epoch-silent-server");
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0 })); // no serverEpoch at all

    expect(onServerRestart).toHaveBeenCalledTimes(1);
  });

  it("does NOT treat a session whose epoch it LEARNED as restarted by a silent ack", () => {
    // The other half, and the reason the seeded/learned distinction exists: an
    // epoch learned from an earlier ack is an observation, and a server that reports
    // none is ordinary operation for it. Resetting live sessions here would be a
    // catastrophic false positive.
    setSession("epoch-learned");
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    first.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 222 }));
    expect(onServerRestart).not.toHaveBeenCalled();

    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckFrame({ received: 0 })); // silent this time

    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("refuses to seed an epoch over one a server already reported", () => {
    // "Seed" is the whole contract: it exists to give the FIRST resumeAck something
    // to compare against. Overwriting a value a server reported would make a genuine
    // restart look like agreement, or a live session look restarted — so a caller
    // that reaches here twice, or after connecting, is refused rather than obeyed.
    setSession("epoch-no-clobber");
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 555 }));
    expect(serverEpochOf("epoch-no-clobber")).toBe(555);

    adoptPersistedEpoch("epoch-no-clobber", 111);

    expect(serverEpochOf("epoch-no-clobber")).toBe(555);
    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("reports the epoch a session's content belongs to, so a snapshot can record it", () => {
    // The read half. Without it a consumer persisting a store has no way to
    // record WHICH server process its absolute indices came from, and the seeding
    // above has nothing to be seeded with.
    expect(serverEpochOf("epoch-read")).toBe(0);
    setSession("epoch-read");
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 555 }));

    expect(serverEpochOf("epoch-read")).toBe(555);
  });

  it("reports 0 for a session it has never seen rather than throwing", () => {
    // A save can be attempted for a tab that never completed a resume; 0 is the
    // same "unknown" value adoptPersistedEpoch ignores, so the pair composes.
    expect(serverEpochOf("never-connected-at-all")).toBe(0);
  });

  it("resolves a stable unmanaged session id, so the untabbed path has a key", () => {
    // The unmanaged single-terminal path's identity is minted inside this module
    // from sessionStorage. A consumer keying persisted scrollback by session has
    // nothing else to key it by, and it must be resolvable BEFORE connecting.
    // Module state persists across tests in this file, so drop the active
    // session first: "unmanaged" is defined by there being no session set.
    forgetSession(currentSessionId());
    const first = currentSessionId();
    expect(first).not.toBe("");
    expect(first).toBe(sessionStorage.getItem("vterm-session-id"));
    // Stable within the page: a second read must not mint a second id, or every
    // reader would key its storage differently.
    expect(currentSessionId()).toBe(first);
  });

  it("reports the MANAGED session once one is set, not the unmanaged id", () => {
    setSession("managed-id-read");
    expect(currentSessionId()).toBe("managed-id-read");
  });

  it("routes the socket to ?session=<id> and resumes with a per-sender key derived from it", () => {
    setSession("route-A");
    const sock = allMockWebSockets[0]!;
    // The URL carries the BARE routing id (the server routes on it)...
    expect(sock.url).toContain("session=route-A");
    expect(sock.url).not.toContain("#");

    sock.fireOpen();
    const resume = controlFramesSent(sock).find((m) => m.type === "resume");
    expect(resume).toBeDefined();
    // ...while the resume frame carries the per-sender ledger key (P1):
    // routing id + "#" + a crypto-random per-client instance id, so two
    // devices on one session never share a server-side bytesReceived ledger
    // (a shared ledger acked device B's bytes to device A, whose applyAck
    // then trimmed unacked bytes the server never got from A).
    expect(resume!.sessionId).toMatch(/^route-A#[0-9a-f-]{16,}$/);
    expect(resume!.sessionId).not.toBe("route-A");
    expect(resume!.sentBytes).toBe(0);
  });

  it("keeps ONE instance id for the page lifetime (stable across sessions and reconnects)", () => {
    setSession("stable-A");
    allMockWebSockets.at(-1)!.fireOpen();
    const keyA = controlFramesSent(allMockWebSockets.at(-1)!).find((m) => m.type === "resume")!
      .sessionId as string;

    setSession("stable-B");
    allMockWebSockets.at(-1)!.fireOpen();
    const keyB = controlFramesSent(allMockWebSockets.at(-1)!).find((m) => m.type === "resume")!
      .sessionId as string;

    reconnectNow();
    allMockWebSockets.at(-1)!.fireOpen();
    const keyB2 = controlFramesSent(allMockWebSockets.at(-1)!).find((m) => m.type === "resume")!
      .sessionId as string;

    const instanceOf = (k: string): string => k.slice(k.indexOf("#") + 1);
    // Same page = same sender: the instance suffix is identical everywhere
    // (the server-side ledger key changes only with the session), and a
    // reconnect resumes the SAME ledger rather than minting a new one.
    expect(instanceOf(keyA)).toBe(instanceOf(keyB));
    expect(keyB2).toBe(keyB);
    expect(keyA.startsWith("stable-A#")).toBe(true);
    expect(keyB.startsWith("stable-B#")).toBe(true);
  });

  it("does not replay session A's unacked bytes onto session B after a switch", () => {
    setSession("noreplay-A");
    const a = allMockWebSockets[0]!;
    a.fireOpen();
    // Type into A while connected: the bytes go out on A and into A's outbox.
    sendBinary(new Uint8Array([104, 105])); // "hi"
    expect(rawInputSends(a).length).toBe(1);

    // Switch to B. A's socket is torn down; B connects fresh.
    setSession("noreplay-B");
    const b = allMockWebSockets[1]!;
    expect(b).not.toBe(a);
    b.fireOpen();

    // B resumes with ITS OWN counters (0 sent), and its socket never receives
    // A's queued input — no cross-tab replay.
    const resumeB = controlFramesSent(b).find((m) => m.type === "resume");
    expect(resumeB!.sessionId).toMatch(/^noreplay-B#/); // per-sender key (P1)
    expect(resumeB!.sentBytes).toBe(0);
    expect(rawInputSends(b).length).toBe(0);
  });

  it("replays a session's own unacked outbox when switched back to it", () => {
    setSession("own-A");
    const a1 = allMockWebSockets[0]!;
    a1.fireOpen();
    sendBinary(new Uint8Array([120])); // "x" into A, unacked (no resumeAck)

    setSession("own-B"); // leave A with an unacked byte
    allMockWebSockets[1]!.fireOpen();

    setSession("own-A"); // back to A: its outbox must replay on the fresh socket
    const a2 = allMockWebSockets[2]!;
    expect(a2).not.toBe(a1);
    a2.fireOpen();
    a2.fireMessage(resumeAckFrame({ received: 0 })); // server acked nothing yet
    // The unacked "x" is retransmitted on A's new socket.
    expect(rawInputSends(a2).length).toBe(1);
    const resumeA = controlFramesSent(a2).find((m) => m.type === "resume");
    expect(resumeA!.sentBytes).toBe(1);
  });

  it("compares each session's boot-epoch only against its own (no false restart on switch)", () => {
    setSession("epoch-A");
    const a = allMockWebSockets[0]!;
    a.fireOpen();
    a.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 111 }));

    // B is a different session with a different process epoch. Switching to it
    // must not read A's epoch and declare a restart.
    setSession("epoch-B");
    const b = allMockWebSockets[1]!;
    b.fireOpen();
    b.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 222 }));
    expect(onServerRestart).not.toHaveBeenCalled();

    // Back to A: A's epoch is unchanged (111), so still no restart.
    setSession("epoch-A");
    const a2 = allMockWebSockets[2]!;
    a2.fireOpen();
    a2.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 111 }));
    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("still fires a server-restart reset when the SAME session's boot-epoch changes", () => {
    setSession("restart-A");
    const a1 = allMockWebSockets[0]!;
    a1.fireOpen();
    a1.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 111 }));

    // A reconnects and the server reports a different epoch: A's process was
    // replaced. That is a genuine restart for this session.
    reconnectNow();
    const a2 = allMockWebSockets[1]!;
    a2.fireOpen();
    a2.fireMessage(resumeAckFrame({ received: 0, serverEpoch: 999 }));
    expect(onServerRestart).toHaveBeenCalledTimes(1);
  });

  it("forgetSession drops the active session and tears down its socket", () => {
    setSession("forget-A");
    const a = allMockWebSockets[0]!;
    a.fireOpen();
    expect(a.closed).toBe(false);

    forgetSession("forget-A");
    expect(a.closed).toBe(true);
    // No reconnect is scheduled for a forgotten session.
    vi.advanceTimersByTime(20_000);
    expect(allMockWebSockets.length).toBe(1);
  });
});

describe("generateSessionId: session token is cryptographically random", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("delegates to crypto.randomUUID when available", () => {
    vi.stubGlobal("crypto", {
      randomUUID: () => "11111111-2222-3333-4444-555555555555",
      getRandomValues: () => {
        throw new Error("should not be called when randomUUID exists");
      },
    });
    expect(generateSessionId()).toBe("11111111-2222-3333-4444-555555555555");
  });

  it("falls back to crypto.getRandomValues (a CSPRNG) when randomUUID is absent", () => {
    // randomUUID requires a secure context; getRandomValues does not, so
    // this is the path taken on a plain-HTTP origin.
    const getRandomValues = vi.fn((arr: Uint8Array) => {
      for (let i = 0; i < arr.length; i++) {
        arr[i] = i;
      }
      return arr;
    });
    vi.stubGlobal("crypto", { getRandomValues });

    const id = generateSessionId();

    // The output IS the CSPRNG bytes rendered as hex, so it proves the fallback
    // consulted getRandomValues (asserting the exact call count would pin an
    // internal detail — reading 16 bytes in one call vs several is not part of
    // the contract). 16 bytes -> 32 lowercase hex chars; deterministic here.
    expect(getRandomValues).toHaveBeenCalled();
    expect(id).toBe("000102030405060708090a0b0c0d0e0f");
  });

  it("produces a 128-bit lowercase-hex token via the getRandomValues fallback", () => {
    // Use the environment's real CSPRNG, asserting only on shape so the
    // test stays deterministic without pinning the bytes.
    const realGet = globalThis.crypto.getRandomValues.bind(globalThis.crypto);
    vi.stubGlobal("crypto", { getRandomValues: realGet });

    const a = generateSessionId();
    const b = generateSessionId();

    expect(a).toMatch(/^[0-9a-f]{32}$/);
    expect(b).toMatch(/^[0-9a-f]{32}$/);
    expect(a).not.toBe(b);
  });

  it("never falls back to Math.random", () => {
    const mathRandom = vi.spyOn(Math, "random");
    vi.stubGlobal("crypto", {
      getRandomValues: (arr: Uint8Array) => arr,
    });

    generateSessionId();

    expect(mathRandom).not.toHaveBeenCalled();
  });

  it("fails closed when no Web Crypto RNG is available (no guessable token)", () => {
    // crypto present but without either method, e.g. a stripped global.
    vi.stubGlobal("crypto", {});
    expect(() => generateSessionId()).toThrow(/secure RNG/);
  });
});

describe("connection: a process-exited close (4001) is definitive, not transient", () => {
  // The server closes with the application code 4001 (statusProcessExited)
  // when the session's child process has ended. Reconnecting such a session
  // can only replay its final screen and earn another 4001, so when the
  // consumer wires onProcessExit the module must treat the close as an END —
  // no onClose, no backoff reconnect. Without the callback, the legacy
  // transient treatment must be preserved bit-for-bit (existing consumers).
  let onClose: ReturnType<typeof vi.fn<() => void>>;
  let onProcessExit: ReturnType<typeof vi.fn<() => void>>;

  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    onClose = vi.fn<() => void>();
    onProcessExit = vi.fn<() => void>();
  });

  afterEach(() => {
    disconnect(); // drop any live socket so state doesn't leak into later tests
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function initWith(cb: { withProcessExit: boolean }): void {
    init({
      onMessage: vi.fn(),
      onOpen: () => {
        /* no-op */
      },
      onClose,
      ...(cb.withProcessExit ? { onProcessExit } : {}),
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
  }

  it("routes a 4001 close to onProcessExit and schedules no reconnect", () => {
    initWith({ withProcessExit: true });
    setSession("procexit-A");
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    sock.fireClose(4001);

    expect(onProcessExit).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
    // No backoff reconnect for a definitive close: even far past the maximum
    // backoff (8s + jitter), no replacement socket is created.
    vi.advanceTimersByTime(60_000);
    expect(allMockWebSockets.length).toBe(1);
  });

  it("keeps the legacy transient treatment for 4001 when onProcessExit is not wired", () => {
    initWith({ withProcessExit: false });
    setSession("procexit-B");
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    sock.fireClose(4001);

    expect(onClose).toHaveBeenCalledTimes(1);
    // The backoff reconnect fires (500ms initial + <250ms jitter).
    vi.advanceTimersByTime(10_000);
    expect(allMockWebSockets.length).toBe(2);
  });

  it("keeps the transient treatment for a non-4001 close even with onProcessExit wired", () => {
    initWith({ withProcessExit: true });
    setSession("procexit-C");
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    sock.fireClose(1006); // abnormal closure: a genuine transient drop

    expect(onProcessExit).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(10_000);
    expect(allMockWebSockets.length).toBe(2);
  });

  it("a reconnect explicitly requested after a 4001 still works (re-viewing a dead session)", () => {
    initWith({ withProcessExit: true });
    setSession("procexit-D");
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();
    sock.fireClose(4001);
    expect(allMockWebSockets.length).toBe(1);

    // The consumer may still re-attach deliberately (a tab switch back onto
    // the dead session calls setSession -> reconnectNow): the module must not
    // have latched anything that blocks an explicit reconnect.
    reconnectNow();
    expect(allMockWebSockets.length).toBe(2);
    const again = allMockWebSockets[1]!;
    expect(again.url).toContain("session=procexit-D");
  });

  it("routes an unknown-session close (4004) to the same definitive path as 4001", () => {
    // The server accepts a WS to an id it does not know and closes 4004
    // (statusUnknownSession) precisely so the client can READ the reason (a
    // pre-upgrade 404 is an opaque 1006). Reconnecting can only earn another
    // 4004, so it must take the ended path, never the backoff loop.
    initWith({ withProcessExit: true });
    setSession("unknown-A");
    const sock = allMockWebSockets[0]!;
    sock.fireOpen();

    sock.fireClose(4004);

    expect(onProcessExit).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
    vi.advanceTimersByTime(60_000);
    expect(allMockWebSockets.length).toBe(1);
  });
});

describe("connection: ledger-loss signal, ackOnly trimming, wire-version surface", () => {
  // Pins the resume-reliability additions from the session-transport /
  // wire-protocol judgement portfolio (P5 + P8): an explicit resumeAck
  // ledgerLost flag replaces the client-side guess for the bytesAcked === 0
  // duplicate-replay branch (drop-and-notify, never replay), a bare ackOnly
  // frame trims the outbox when input produced no output frame, and the
  // resumeAck's serverWireVersion tail surfaces a protocol skew through
  // onWireVersionMismatch. Frames are hand-built byte-mirrors of the Go
  // encoders (wire-golden.node.test.ts pins the exact cross-language bytes).
  let onMessage: ReturnType<typeof vi.fn<(msg: ServerMessage) => void>>;
  let onServerRestart: ReturnType<typeof vi.fn<() => void>>;
  let onWireVersionMismatch: ReturnType<typeof vi.fn<(server: number, client: number) => void>>;

  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    onMessage = vi.fn<(msg: ServerMessage) => void>();
    onServerRestart = vi.fn<() => void>();
    onWireVersionMismatch = vi.fn<(server: number, client: number) => void>();
    init({
      onMessage,
      onServerRestart,
      onWireVersionMismatch,
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  // resumeAck frame bytes, mirroring encodeResumeAck (wire_binary.go).
  // 33-byte form = pre-tail server; 35-byte form adds serverWireVersion +
  // ackFlags (bit0 = ledgerLost). epoch defaults to 0 so the restart
  // detector stays out of these tests' way.
  function resumeAckFrame(opts: {
    received: number;
    version?: number;
    ledgerLost?: boolean;
    legacy33?: boolean;
  }): ArrayBuffer {
    const len = opts.legacy33 ? 33 : 35;
    const buf = new ArrayBuffer(len);
    const v = new DataView(buf);
    v.setUint8(0, 2); // MSG_RESUME_ACK
    v.setBigUint64(1, BigInt(opts.received), true);
    v.setBigUint64(9, 0n, true); // serverEpoch: 0 = skip restart detection
    v.setBigUint64(17, 0n, true); // committed
    v.setBigUint64(25, 0n, true); // oldestIndex
    if (!opts.legacy33) {
      v.setUint8(33, opts.version ?? 3);
      v.setUint8(34, opts.ledgerLost ? 1 : 0);
    }
    return buf;
  }

  // ackOnly frame bytes, mirroring encodeAckOnly: [1B type=7][8B ack].
  function ackOnlyFrame(ack: number): ArrayBuffer {
    const buf = new ArrayBuffer(9);
    const v = new DataView(buf);
    v.setUint8(0, 7); // MSG_ACK_ONLY
    v.setBigUint64(1, BigInt(ack), true);
    return buf;
  }

  // Raw (non-control) frames handed to sock.send — i.e. PTY input bytes,
  // including retransmitOutbox replays. Control frames start with 0x00.
  function rawInputSends(sock: MockWS): Uint8Array[] {
    const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
    const out: Uint8Array[] = [];
    for (const c of calls) {
      const a = c[0];
      const bytes =
        a instanceof Uint8Array ? a : a instanceof ArrayBuffer ? new Uint8Array(a) : null;
      if (bytes && bytes.length > 0 && bytes[0] !== 0x00) {
        out.push(bytes);
      }
    }
    return out;
  }

  it("ledgerLost resumeAck drops the unacked outbox (no duplicate replay) and notifies", () => {
    setSession(generateSessionId()); // opens a socket via reconnectNow
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3])); // applied by the server, never acked

    // Reconnect: the resume claims sentBytes=3; the server's ledger is gone
    // (idle GC) and says so explicitly.
    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    expect(second).not.toBe(first);
    second.fireOpen();
    expect(controlFramesSent(second).at(-1)?.sentBytes).toBe(3);

    second.fireMessage(resumeAckFrame({ received: 0, ledgerLost: true }));

    expect(rawInputSends(second)).toEqual([]); // outbox dropped, NOT replayed
    expect(onServerRestart).toHaveBeenCalledTimes(1); // drop-and-notify
  });

  it("ledgerLost resumeAck with a fully-acked outbox resets silently (no false restart banner)", () => {
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3]));
    first.fireMessage(ackOnlyFrame(3)); // the server received and acked all 3

    // The socket drops (a phone screen sleeping), the server's idle GC reclaims
    // the ledger, and the reconnect's resume key misses. sentBytes === the GC'd
    // ledger's bytes_received, so nothing was ever at risk.
    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckFrame({ received: 0, ledgerLost: true }));

    expect(rawInputSends(second)).toEqual([]); // nothing to replay
    expect(onServerRestart).not.toHaveBeenCalled(); // nothing lost, so nothing to announce
  });

  it("resets the byte counters on a forgotten ledger so later acks still trim", () => {
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3]));
    first.fireMessage(ackOnlyFrame(3));

    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckFrame({ received: 0, ledgerLost: true }));

    // The server's replacement ledger counts from zero. If the client had kept
    // bytesAcked=3, this ack would read as stale in applyAck and the outbox
    // would never trim again.
    sendBinary(new Uint8Array([9]));
    second.fireMessage(ackOnlyFrame(1));
    reconnectNow();
    const third = allMockWebSockets.at(-1)!;
    third.fireOpen();
    expect(controlFramesSent(third).at(-1)?.sentBytes).toBe(1);
    third.fireMessage(resumeAckFrame({ received: 1 }));
    expect(rawInputSends(third)).toEqual([]); // acked, so nothing replayed
  });

  it("a clear ledgerLost flag keeps the legacy in-transit-loss replay (received=0, bytesAcked=0)", () => {
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3])); // lost in transit, never applied

    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckFrame({ received: 0, ledgerLost: false }));

    // Key hit with a genuinely-zero ledger: replay is CORRECT and must survive.
    expect(rawInputSends(second).map((b) => Array.from(b))).toEqual([[1, 2, 3]]);
    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("ackOnly trims the outbox so a later reconnect retransmits nothing", () => {
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3]));

    first.fireMessage(ackOnlyFrame(3)); // quiet-input ack (no content frame)
    expect(onMessage).not.toHaveBeenCalled(); // transport-internal, not forwarded

    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckFrame({ received: 3 }));

    expect(rawInputSends(second)).toEqual([]); // nothing unacked to replay
    expect(onServerRestart).not.toHaveBeenCalled();
  });

  it("warns on a future server revision but keeps the socket usable", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {
      /* silence */
    });
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();

    sock.fireMessage(resumeAckFrame({ received: 0, version: 99 }));
    expect(onWireVersionMismatch).toHaveBeenCalledWith(99, 4);
    expect(warn).toHaveBeenCalled();
    expect(sock.close).not.toHaveBeenCalled();

    // Supported-range servers (v3 sentinel peer, v4 current) are normal
    // pairings and produce no mismatch noise.
    onWireVersionMismatch.mockClear();
    sock.fireMessage(resumeAckFrame({ received: 0, version: 3 }));
    expect(onWireVersionMismatch).not.toHaveBeenCalled();
    sock.fireMessage(resumeAckFrame({ received: 0, version: 4 }));
    expect(onWireVersionMismatch).not.toHaveBeenCalled();

    // Version-silent old server (33-byte resumeAck, no tail): tolerated.
    sock.fireMessage(resumeAckFrame({ received: 0, legacy33: true }));
    expect(onWireVersionMismatch).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  it("refuses an explicit below-floor server revision and blocks reconnects", () => {
    const onWireIncompatible = vi.fn();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {
      /* silence */
    });
    init({
      onMessage,
      onServerRestart,
      onWireVersionMismatch,
      onWireIncompatible,
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();

    sock.fireMessage(resumeAckFrame({ received: 0, version: 2 }));

    expect(onWireVersionMismatch).toHaveBeenCalledWith(2, 4);
    expect(onWireIncompatible).toHaveBeenCalledWith(
      expect.objectContaining({
        source: "server-version",
        serverVersion: 2,
        clientVersion: 4,
        minimumServerVersion: 3,
      }),
    );
    expect(sock.close).toHaveBeenCalledWith(4002, expect.stringContaining("upgrade the server"));
    vi.advanceTimersByTime(60_000);
    reconnectNow(); // wake/manual reconnect cannot bypass the terminal state
    expect(allMockWebSockets).toHaveLength(1);
    warn.mockRestore();
  });

  it("treats a server 4002 rejection as definitive and surfaces incompatibility", () => {
    const onClose = vi.fn();
    const onWireIncompatible = vi.fn();
    init({
      onMessage,
      onOpen: () => {
        /* no-op */
      },
      onClose,
      onWireIncompatible,
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();

    sock.fireClose(4002);

    expect(onWireIncompatible).toHaveBeenCalledWith(
      expect.objectContaining({ source: "server-close", clientVersion: 4 }),
    );
    expect(onClose).not.toHaveBeenCalled();
    vi.advanceTimersByTime(60_000);
    reconnectNow();
    expect(allMockWebSockets).toHaveLength(1);
  });
});

describe("connection: v4 typed-framing negotiation (see WIRE_PROTOCOL_VERSION in wire-compatibility.ts)", () => {
  // Pins the client half of the three-phase handshake: binary bootstrap
  // resume first on every socket (F4), text `upgrade` as the FIRST message
  // after proof and before any retransmit (F1), text controls only after
  // upgrade, v3 mode against old servers, per-socket state (fresh sockets
  // re-bootstrap), the stale-Blob guard (F2), and the single input encoder
  // (F5: v3-mode leading-NUL split vs v4 verbatim).
  let onMessage: ReturnType<typeof vi.fn<(msg: ServerMessage) => void>>;
  let onServerRestart: ReturnType<typeof vi.fn<() => void>>;

  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    onMessage = vi.fn<(msg: ServerMessage) => void>();
    onServerRestart = vi.fn<() => void>();
    init({
      onMessage,
      onServerRestart,
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function resumeAckV4(received = 0): ArrayBuffer {
    const buf = new ArrayBuffer(35);
    const v = new DataView(buf);
    v.setUint8(0, 2);
    v.setBigUint64(1, BigInt(received), true);
    v.setBigUint64(9, 0n, true);
    v.setBigUint64(17, 0n, true);
    v.setBigUint64(25, 0n, true);
    v.setUint8(33, 4); // serverWireVersion = 4 (proof)
    v.setUint8(34, 0);
    return buf;
  }

  function resumeAckV3(received = 0): ArrayBuffer {
    const buf = new ArrayBuffer(33); // tail-absent old server
    const v = new DataView(buf);
    v.setUint8(0, 2);
    v.setBigUint64(1, BigInt(received), true);
    v.setBigUint64(9, 0n, true);
    v.setBigUint64(17, 0n, true);
    v.setBigUint64(25, 0n, true);
    return buf;
  }

  // All sends on a socket, classified: "text" (string), "control" (binary
  // 0x00-sentinel), or "input" (other binary).
  function sendLog(sock: MockWS): { kind: string; text?: string; bytes?: number[] }[] {
    const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
    return calls.map((c) => {
      const a = c[0];
      if (typeof a === "string") {
        return { kind: "text", text: a };
      }
      const bytes =
        a instanceof Uint8Array ? a : a instanceof ArrayBuffer ? new Uint8Array(a) : null;
      if (!bytes) {
        return { kind: "other" };
      }
      // A binary frame is a v3 control iff 0x00 + valid control JSON — first
      // byte alone cannot distinguish it from v4 full-alphabet input (that
      // ambiguity is exactly what the typed migration removes), so classify
      // by parseability like the server's fallback does.
      if (bytes.length > 1 && bytes[0] === 0x00) {
        try {
          JSON.parse(new TextDecoder().decode(bytes.subarray(1)));
          return { kind: "control", bytes: Array.from(bytes) };
        } catch {
          // fall through: 0x00-leading input (e.g. the P2 fallback class)
        }
      }
      return { kind: "input", bytes: Array.from(bytes) };
    });
  }

  it("bootstrap resume is message one even when onOpen sends immediately", () => {
    init({
      onMessage,
      onOpen: () => {
        sendResize();
        sendBinary(new Uint8Array([65]));
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();

    const first = controlFramesSent(sock)[0];
    expect(first?.type).toBe("resume");
    const log = sendLog(sock);
    expect(log[0]?.kind).toBe("control"); // the binary bootstrap resume
  });

  it("upgrades on v4 proof: text upgrade precedes the retransmit, later controls are text", () => {
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3])); // unacked at proof time

    const sendsBefore = sendLog(sock).length;
    sock.fireMessage(resumeAckV4(0));

    const after = sendLog(sock).slice(sendsBefore);
    expect(after[0]?.kind).toBe("text"); // the upgrade transition, FIRST
    expect(JSON.parse(after[0]!.text!)).toEqual({ type: "upgrade" });
    expect(after[1]?.kind).toBe("input"); // retransmit follows the latch
    expect(after[1]?.bytes).toEqual([1, 2, 3]);

    sendResize();
    const last = sendLog(sock).at(-1)!;
    expect(last.kind).toBe("text");
    expect((JSON.parse(last.text!) as { type: string }).type).toBe("resize");
  });

  it("stays in v3 mode against old servers (tail-absent resumeAck)", () => {
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();
    sock.fireMessage(resumeAckV3(0));

    sendResize();
    const last = sendLog(sock).at(-1)!;
    expect(last.kind).toBe("control"); // still binary sentinel
    expect(sendLog(sock).some((s) => s.kind === "text")).toBe(false);
  });

  it("a fresh socket re-bootstraps in v3 mode after an upgraded one", () => {
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    first.fireMessage(resumeAckV4(0));
    expect(sendLog(first).some((s) => s.kind === "text")).toBe(true); // upgraded

    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    const log = sendLog(second);
    expect(log[0]?.kind).toBe("control"); // binary bootstrap again
    sendResize();
    expect(sendLog(second).at(-1)?.kind).toBe("control"); // no proof yet → binary
  });

  it("v3-mode input splits leading NULs; upgraded input goes verbatim", () => {
    setSession(generateSessionId());
    const sock = allMockWebSockets.at(-1)!;
    sock.fireOpen();

    // v3 mode (no proof yet): leading NULs go out as solitary frames.
    sendBinary(new Uint8Array([0, 0, 65]));
    let inputs = sendLog(sock).filter((s) => s.kind === "input");
    expect(inputs.map((s) => s.bytes)).toEqual([[0], [0], [65]]);

    // Upgrade, then the same bytes go out as ONE full-alphabet frame.
    sock.fireMessage(resumeAckV4(3)); // acks the 3 bytes already sent
    sendBinary(new Uint8Array([0, 66]));
    inputs = sendLog(sock).filter((s) => s.kind === "input");
    expect(inputs.at(-1)?.bytes).toEqual([0, 66]);
  });

  it("v3 reconnect replay splits a leading-NUL chunk (the F5 regression)", () => {
    // The original F5 defect class: replay bypassing the live-send encoder.
    // A leading-NUL chunk left unacked across a reconnect to a v3 server
    // (tail-absent resumeAck) must be retransmitted SPLIT — solitary [0]
    // then [65] — with no text frame anywhere on the socket.
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([0, 65]));

    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckV3(0)); // old server: no proof, no upgrade

    const log = sendLog(second);
    expect(log.some((s) => s.kind === "text")).toBe(false);
    expect(log.filter((s) => s.kind === "input").map((s) => s.bytes)).toEqual([[0], [65]]);
  });

  it("retransmit uses the socket's framing mode (split in v3, verbatim after upgrade)", () => {
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([0, 65])); // v3 mode live send: split into [0],[65]

    // Reconnect to a v4 server with the chunk still unacked: the retransmit
    // happens AFTER the upgrade, so it goes out verbatim as one frame.
    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();
    second.fireMessage(resumeAckV4(0));
    const log = sendLog(second);
    const firstText = log.findIndex((s) => s.kind === "text");
    const firstInput = log.findIndex((s) => s.kind === "input");
    expect(firstText).toBeGreaterThanOrEqual(0);
    expect(firstInput).toBeGreaterThan(firstText); // latch precedes retransmit
    expect(log[firstInput]?.bytes).toEqual([0, 65]); // verbatim, not split
  });

  it("a stale Blob resumeAck from a superseded socket cannot upgrade or retransmit on the new one", async () => {
    vi.useRealTimers(); // Blob.arrayBuffer() is a real microtask hop
    setSession(generateSessionId());
    const first = allMockWebSockets.at(-1)!;
    first.fireOpen();
    sendBinary(new Uint8Array([1, 2, 3]));

    // Deliver the proof as a Blob (iOS path) and supersede the socket before
    // the async conversion resolves.
    first.fireMessage(new Blob([new Uint8Array(resumeAckV4(0))]));
    reconnectNow();
    const second = allMockWebSockets.at(-1)!;
    second.fireOpen();

    await new Promise((r) => setTimeout(r, 10)); // let the Blob chain drain

    // The stale proof must not have upgraded the NEW socket…
    sendResize();
    expect(sendLog(second).at(-1)?.kind).toBe("control");
    // …nor triggered a retransmit on it (only the bootstrap resume + resize).
    expect(sendLog(second).filter((s) => s.kind === "input")).toEqual([]);
    expect(onServerRestart).not.toHaveBeenCalled();
  });
});

describe("connection: per-session DEC-mode mirror (P3)", () => {
  // Mode state was page-global while every neighboring state was
  // session-scoped: a keystroke in the tab-switch window encoded under the
  // OLD session's modes (vim's DECCKM arrows bleeding into a shell tab, wrong
  // paste bracketing, stale kitty flags) and was delivered to the NEW
  // session's outbox. setSession now restores the target's cached snapshot
  // synchronously; these tests drive the full path a real switch takes:
  // binary modes frames in, keyboard encoding out.
  beforeEach(() => {
    allMockWebSockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    init({
      onMessage: vi.fn(),
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
    });
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    // Leave the shared modes singleton at power-on for later test files
    // (vitest isolate:false).
    modes.applySnapshot(modes.POWER_ON_MODES);
  });

  // Binary MSG_MODES frame mirroring wire_binary.go: type(1) + inputAck(8) +
  // flags(1) + mouseMode(2) + kbdFlags(1).
  function modesFrame(fields: {
    bracketed?: boolean;
    appCursor?: boolean;
    kbdFlags?: number;
    mouseMode?: number;
  }): ArrayBuffer {
    const buf = new ArrayBuffer(13);
    const v = new DataView(buf);
    v.setUint8(0, 3); // MSG_MODES
    v.setBigUint64(1, 0n, true);
    let flags = 0;
    if (fields.bracketed ?? false) {
      flags |= 1;
    }
    if (fields.appCursor ?? false) {
      flags |= 2;
    }
    v.setUint8(9, flags);
    v.setUint16(10, fields.mouseMode ?? 0, true);
    v.setUint8(12, fields.kbdFlags ?? 0);
    return buf;
  }

  it("a keydown in the switch window encodes under the TARGET session's modes, never the old tab's", () => {
    // Session A announces DECCKM + kitty disambiguate (a vim-like app).
    setSession("modes-A");
    const a = allMockWebSockets.at(-1)!;
    a.fireOpen();
    a.fireMessage(modesFrame({ bracketed: true, appCursor: true, kbdFlags: 1 }));
    expect(modes.isApplicationCursor()).toBe(true);
    expect(modes.getKeyboardFlags()).toBe(1);

    // Switch to a session the page has never seen: SYNCHRONOUSLY at power-on
    // defaults — an ArrowUp fired before B's modes frame arrives encodes as
    // legacy CSI A, not A's SS3/kitty form.
    setSession("modes-B");
    expect(modes.isApplicationCursor()).toBe(false);
    expect(modes.getKeyboardFlags()).toBe(0);
    const up = mapKeyboardEvent(new KeyboardEvent("keydown", { key: "ArrowUp" }), modes);
    expect(up).toEqual({ kind: "send", bytes: "\x1b[A" });

    // B announces its own modes (mouse tracking, no DECCKM).
    allMockWebSockets.at(-1)!.fireOpen();
    allMockWebSockets.at(-1)!.fireMessage(modesFrame({ bracketed: true, mouseMode: 1000 }));
    expect(modes.getMouseMode()).toBe(1000);

    // Switching BACK to A restores A's snapshot synchronously: the same
    // ArrowUp now encodes under A's DECCKM... except kitty disambiguate
    // supersedes it (CSI form) — exactly what A's live encoder did.
    setSession("modes-A");
    expect(modes.isApplicationCursor()).toBe(true);
    expect(modes.getKeyboardFlags()).toBe(1);
    expect(modes.getMouseMode()).toBe(0); // B's mouse mode did not bleed into A
    const upOnA = mapKeyboardEvent(new KeyboardEvent("keydown", { key: "ArrowUp" }), modes);
    expect(upOnA).toEqual({ kind: "send", bytes: "\x1b[A" }); // kitty CSI form
    // Escape makes the kitty restoration visible unambiguously.
    const esc = mapKeyboardEvent(new KeyboardEvent("keydown", { key: "Escape" }), modes);
    expect(esc).toEqual({ kind: "send", bytes: "\x1b[27u" });
  });

  it("a modes frame updates the singleton AND the session's cached snapshot (single writer)", () => {
    setSession("modes-C");
    const c = allMockWebSockets.at(-1)!;
    c.fireOpen();
    c.fireMessage(modesFrame({ bracketed: false, appCursor: true }));
    expect(modes.isBracketedPaste()).toBe(false);

    // Bounce away and back with no new frames: the cache round-trips.
    setSession("modes-D");
    expect(modes.isBracketedPaste()).toBe(true); // D: power-on defaults
    setSession("modes-C");
    expect(modes.isBracketedPaste()).toBe(false);
    expect(modes.isApplicationCursor()).toBe(true);
  });
});
