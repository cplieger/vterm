// The client's liveness probe and the session switch — the two paths that decide
// when a socket is REPLACED.
//
// Both are timing-driven and both fail silently in opposite directions. A probe
// that never fires leaves an iOS tab looking connected while the socket is a
// frozen zombie (nothing arrives, nothing is retried). A probe that fires too
// eagerly tears down a healthy socket, and every teardown re-runs the resume and
// replays the outbox. The session switch has the same shape: a switch that
// declines to no-op reconnects a socket that was already serving the requested
// session, and one that no-ops too broadly leaves the consumer with no socket at
// all after a disconnect.
//
// The clock here is fake and every assertion lands on an exact instant:
// HEARTBEAT_INTERVAL_MS 5s, IDLE_BEFORE_PROBE_MS 10s, PONG_TIMEOUT_MS 7s.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { disconnect, init, sendBinary, setSession } from "./connection.js";
import { WIRE_PROTOCOL_VERSION } from "./wire-compatibility.js";

interface MockWS {
  readyState: number;
  listeners: Map<string, ((ev: unknown) => void)[]>;
  send: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  addEventListener: (
    type: string,
    handler: (ev: unknown) => void,
    opts?: { signal?: AbortSignal },
  ) => void;
  fireOpen: () => void;
  fireMessage: (data: ArrayBuffer) => void;
  fireClose: (code?: number) => void;
}

const sockets: MockWS[] = [];

function makeMockWebSocket(): typeof WebSocket {
  const ctor = function (): MockWS {
    const sock: MockWS = {
      readyState: 0,
      listeners: new Map(),
      send: vi.fn(),
      close: vi.fn(function (this: MockWS) {
        this.readyState = 3;
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
        for (const fn of this.listeners.get("open") ?? []) {
          fn({});
        }
      },
      fireMessage(this: MockWS, data: ArrayBuffer) {
        for (const fn of this.listeners.get("message") ?? []) {
          fn({ data });
        }
      },
      fireClose(this: MockWS, code = 1006) {
        this.readyState = 3;
        for (const fn of this.listeners.get("close") ?? []) {
          fn({ code });
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

/** A 35-byte resumeAck, which upgrades the socket to typed framing. */
function resumeAckFrame(): ArrayBuffer {
  const buf = new ArrayBuffer(35);
  const v = new DataView(buf);
  v.setUint8(0, 2); // MSG_RESUME_ACK
  v.setBigUint64(1, 0n, true); // received
  v.setBigUint64(9, 0n, true); // serverEpoch
  v.setBigUint64(17, 0n, true); // committed
  v.setBigUint64(25, 0n, true); // oldestIndex
  v.setUint8(33, WIRE_PROTOCOL_VERSION);
  v.setUint8(34, 0); // ackFlags
  return buf;
}

/** A binary pong: [1B type=5][8B ack]. Its arrival is the liveness proof. */
function pongFrame(): ArrayBuffer {
  const buf = new ArrayBuffer(9);
  new DataView(buf).setUint8(0, 5); // MSG_PONG
  return buf;
}

/** Control frames of one type, in either encoding, in order. */
function controlsOfType(sock: MockWS, type: string): Record<string, unknown>[] {
  const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
  const out: Record<string, unknown>[] = [];
  for (const c of calls) {
    const a = c[0];
    if (typeof a === "string") {
      const msg = JSON.parse(a) as Record<string, unknown>;
      if (msg["type"] === type) {
        out.push(msg);
      }
      continue;
    }
    if (!(a instanceof Uint8Array) || a.length === 0 || a[0] !== 0x00) {
      continue;
    }
    const msg = JSON.parse(new TextDecoder().decode(a.subarray(1))) as Record<string, unknown>;
    if (msg["type"] === type) {
      out.push(msg);
    }
  }
  return out;
}

let session = 0;

function baseCallbacks() {
  return {
    onMessage: () => {
      /* no-op */
    },
    onOpen: () => {
      /* no-op */
    },
    onClose: () => {
      /* no-op */
    },
    computeSize: () => ({ cols: 80, rows: 24 }),
  };
}

/** A fresh session, open and acked, with the heartbeat running. */
function openSession(): { id: string; sock: MockWS } {
  session++;
  const id = `liveness-${String(session)}`;
  setSession(id);
  const sock = sockets[sockets.length - 1]!;
  sock.fireOpen();
  sock.fireMessage(resumeAckFrame());
  return { id, sock };
}

describe("connection: the liveness probe", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    init(baseCallbacks());
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("stays silent while the socket is younger than the idle window", () => {
    const { sock } = openSession();

    vi.advanceTimersByTime(9_999);

    // The probe costs a round trip on a link that may be metered, and an early
    // one measures nothing: the socket has not been silent long enough to be
    // suspect.
    expect(controlsOfType(sock, "ping")).toEqual([]);
  });

  it("probes once the socket has been silent for the idle window", () => {
    const { sock } = openSession();

    vi.advanceTimersByTime(10_000);

    expect(controlsOfType(sock, "ping")).toEqual([{ type: "ping" }]);
  });

  it("does not probe a hidden tab", () => {
    const { sock } = openSession();
    // A backgrounded tab's timers are throttled or frozen, so its silence proves
    // nothing and its probe would be measured against a stale clock. The wake
    // path (visibilitychange/pageshow) owns this case instead.
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");

    vi.advanceTimersByTime(30_000);

    expect(controlsOfType(sock, "ping")).toEqual([]);
    expect(sockets).toHaveLength(1);
  });

  it("keeps the socket while a sent probe is still inside its grace window", () => {
    openSession();
    vi.advanceTimersByTime(10_000); // probe goes out

    vi.advanceTimersByTime(6_999);

    // Tearing down here would replace a socket whose reply is still in flight,
    // and every replacement re-runs the resume and replays the outbox.
    expect(sockets).toHaveLength(1);
  });

  it("replaces the socket when the probe goes unanswered past its grace window", () => {
    openSession();
    vi.advanceTimersByTime(10_000); // probe goes out

    vi.advanceTimersByTime(10_000);

    // 7s of grace, measured on the 5s tick: the tick at +20s is the first one
    // past the deadline.
    expect(sockets).toHaveLength(2);
  });

  it("treats any inbound frame as the answer and stops probing", () => {
    const { sock } = openSession();
    vi.advanceTimersByTime(10_000); // probe goes out

    sock.fireMessage(pongFrame());
    vi.advanceTimersByTime(9_999);

    // The pong cleared the outstanding probe AND refreshed the activity clock:
    // the socket survives well past the grace window it was inside, and the next
    // probe is a whole fresh idle window away rather than one tick.
    expect(sockets).toHaveLength(1);
    expect(controlsOfType(sock, "ping")).toEqual([{ type: "ping" }]);
  });
});

describe("connection: the reconnect schedule", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(0.5);
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    init(baseCallbacks());
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("schedules one reconnect however many close events arrive", () => {
    const { sock } = openSession();

    // A dropped link can deliver close more than once (the socket's own close
    // plus a forced one from teardown). Each extra schedule would spawn its own
    // socket when the backoff elapses, and the server would see several
    // connections resuming the same session.
    sock.fireClose(1006);
    sock.fireClose(1006);
    // Past the first backoff step (625ms with the jitter pinned) and past the
    // second (1125ms), so a stacked schedule has fired by now too.
    vi.advanceTimersByTime(1_500);

    expect(sockets).toHaveLength(2);
  });

  it("retries a connect that closed before it ever opened", () => {
    session++;
    setSession(`liveness-${String(session)}`);
    const sock = sockets[sockets.length - 1]!;

    // A CONNECTING socket that closes (refused, reset, proxy 502) is the case
    // with no fallback: the connect timeout was cleared by nothing and no open
    // handler ever ran, so if this close does not schedule the retry the tab
    // sits on "Reconnecting…" forever.
    sock.fireClose(1006);
    // Past the backoff ceiling (8000ms + 250ms of jitter) so the step lands
    // wherever the shared delay had grown to, and short of the fresh socket's
    // own 10s connect timeout so nothing cascades.
    vi.advanceTimersByTime(9_999);

    expect(sockets).toHaveLength(2);
  });
});

describe("connection: the session switch", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    init(baseCallbacks());
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("is a no-op for the session already being served", () => {
    const { id } = openSession();

    setSession(id);

    // A consumer re-selects the visible tab freely (a focus event, a re-render).
    // Reconnecting for it would replay the outbox and re-run the resume for
    // nothing.
    expect(sockets).toHaveLength(1);
  });

  it("is a no-op for the session whose socket is still connecting", () => {
    session++;
    const id = `liveness-${String(session)}`;
    setSession(id);
    // No fireOpen: the socket is CONNECTING, which is already serving this
    // session — the resume it will send names it.
    setSession(id);

    expect(sockets).toHaveLength(1);
  });

  it("reconnects the same session when there is no socket left", () => {
    const { id } = openSession();
    disconnect();

    setSession(id);

    // Same id, but nothing is serving it: the no-op is about a LIVE socket, not
    // about the id alone. Skipping the connect here leaves the consumer with a
    // terminal that never comes back.
    expect(sockets).toHaveLength(2);
  });

  it("carries a switched-to session's own outbox, not the previous session's", () => {
    const first = openSession();
    sendBinary(new TextEncoder().encode("first"));

    const second = openSession();
    sendBinary(new TextEncoder().encode("second"));

    // Per-session ledgers: the bytes queued for one tab must never leave on
    // another tab's socket.
    const sentOnSecond = (
      second.sock.send as unknown as { mock: { calls: unknown[][] } }
    ).mock.calls
      .filter((c) => c[0] instanceof ArrayBuffer)
      .map((c) => new TextDecoder().decode(c[0] as ArrayBuffer));
    expect(sentOnSecond).toEqual(["second"]);
    expect(first.sock).not.toBe(second.sock);
  });
});
