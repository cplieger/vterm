// The bounds and the pacing around a history request, and the replay bound the
// resume carries. Three edges the paging suite leaves open, all of them arithmetic
// that decides what the server sends back:
//
// 1. requestHistory's argument guards at their exact boundaries — index 0 is the
//    oldest line there is, a 1-line page is a legal page, and a range ending
//    exactly on MAX_SAFE_INTEGER has not overflowed.
// 2. The token bucket's refill rate and the wait it returns. The client's bucket
//    is sized to agree with the server's, so the refill is a contract, not a
//    heuristic: a client that refills faster burns the server's bucket and gets
//    its requests dropped, and one that under-waits re-asks into an empty bucket
//    forever.
// 3. The resume's replayMax at 1 and at 0. The store predicts the replay jump
//    from the value the socket SENT, so a bound the client mis-clamps strands
//    the band it predicted wrong (docs/paged-scrollback.md §4.5).

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  MAX_REPLAY_LINES,
  disconnect,
  historyRequestInFlight,
  init,
  requestHistory,
  setSession,
} from "./connection.js";
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
    } as unknown as MockWS;
    Object.setPrototypeOf(sock, Mock.prototype);
    sockets.push(sock);
    return sock;
  } as unknown as typeof WebSocket;
  class Mock {}
  ctor.prototype = Mock.prototype as unknown as WebSocket;
  return ctor;
}

/** A 35-byte resumeAck: bounds tail, version byte, and the paging flag (bit 1). */
function resumeAckFrame(
  opts: { paging?: boolean; version?: number | undefined } = {},
): ArrayBuffer {
  const buf = new ArrayBuffer(35);
  const v = new DataView(buf);
  v.setUint8(0, 2); // MSG_RESUME_ACK
  v.setBigUint64(1, 0n, true); // received
  v.setBigUint64(9, 0n, true); // serverEpoch
  v.setBigUint64(17, BigInt(Number.MAX_SAFE_INTEGER), true); // committed
  v.setBigUint64(25, 0n, true); // oldestIndex
  v.setUint8(33, opts.version ?? WIRE_PROTOCOL_VERSION);
  v.setUint8(34, opts.paging === false ? 0 : 2);
  return buf;
}

/** A scroll frame of `count` one-run rows starting at `firstIndex`. */
function scrollFrame(firstIndex: number, count: number): ArrayBuffer {
  const rows: number[] = [];
  const text = new TextEncoder().encode("x");
  const one = [
    1,
    0, // one run (u16 LE)
    text.length & 0xff,
    (text.length >> 8) & 0xff,
    ...text,
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
    0, // url length 0
  ];
  rows.push(...one, ...one);
  const buf = new ArrayBuffer(19 + rows.length);
  const v = new DataView(buf);
  v.setUint8(0, 1); // MSG_SCROLL
  v.setBigUint64(1, 0n, true); // inputAck
  v.setBigUint64(9, BigInt(firstIndex), true);
  v.setUint16(17, count, true);
  new Uint8Array(buf).set(rows, 19);
  return buf;
}

/** Control frames the socket sent, decoded from either encoding, in order. */
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

function historyControls(sock: MockWS): { fromAbs: number; maxLines: number }[] {
  return controlsSent(sock)
    .filter((m) => m["type"] === "history")
    .map((m) => ({ fromAbs: m["fromAbs"] as number, maxLines: m["maxLines"] as number }));
}

let retries: number;
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
    onHistoryReply: () => {
      /* no-op */
    },
    onHistoryRetry: () => {
      retries++;
    },
  };
}

/** A fresh session whose socket is open, acked and paging. */
function openPagingSocket(opts: { version?: number | undefined } = {}): MockWS {
  session++;
  setSession(`bounds-${String(session)}`);
  const sock = sockets[sockets.length - 1]!;
  sock.fireOpen();
  sock.fireMessage(resumeAckFrame({ paging: true, version: opts.version }));
  return sock;
}

/**
 * Spend the whole burst: four grants, each with its reply, because single-flight
 * allows only one outstanding request at a time.
 */
function spendBurst(sock: MockWS): void {
  requestHistory(1000, 2);
  sock.fireMessage(scrollFrame(1000, 2));
  requestHistory(2000, 2);
  sock.fireMessage(scrollFrame(2000, 2));
  requestHistory(3000, 2);
  sock.fireMessage(scrollFrame(3000, 2));
  requestHistory(4000, 2);
  sock.fireMessage(scrollFrame(4000, 2));
}

describe("connection: history request bounds and pacing", () => {
  beforeEach(() => {
    sockets.length = 0;
    retries = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    init(baseCallbacks());
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  describe("the argument guards at their edges", () => {
    it("requests absolute index 0, the oldest line a server can hold", () => {
      const sock = openPagingSocket();

      expect(requestHistory(0, 500)).toBe(true);
      // Index 0 is the top of history, and the frontier walk arrives at it on
      // every session that scrolls far enough. Rejecting it would make the
      // first screen of output the one page paging cannot fetch.
      expect(historyControls(sock)).toEqual([{ fromAbs: 0, maxLines: 500 }]);
    });

    it("requests a single line", () => {
      const sock = openPagingSocket();

      expect(requestHistory(1000, 1)).toBe(true);
      // A one-line gap is the last thing a byte-short continuation needs to
      // heal; refusing it leaves that gap permanently marked.
      expect(historyControls(sock)).toEqual([{ fromAbs: 1000, maxLines: 1 }]);
    });

    it("requests a range that ends exactly on the safe-integer ceiling", () => {
      const sock = openPagingSocket();
      const fromAbs = Number.MAX_SAFE_INTEGER - 500;

      expect(requestHistory(fromAbs, 500)).toBe(true);
      // fromAbs + maxLines lands ON MAX_SAFE_INTEGER, which has not overflowed:
      // the guard exists to reject a range that would, not the largest that
      // would not.
      expect(historyControls(sock)).toEqual([{ fromAbs, maxLines: 500 }]);
    });
  });

  describe("the socket the request needs", () => {
    it("declines on a socket that never negotiated typed framing, without holding the slot", () => {
      // A v3 server parses the 0x00 sentinel only while unlatched, so a history
      // control on a socket that never upgraded is written to the PTY instead —
      // no reply ever comes back. Declining is the whole defence, and it has to
      // decline WITHOUT taking the single-flight slot: a request marked in
      // flight that no reply can release would freeze paging for the socket's
      // life.
      const sock = openPagingSocket({ version: 3 });

      expect(requestHistory(1000, 500)).toBe(false);
      expect(historyControls(sock)).toEqual([]);
      expect(historyRequestInFlight()).toBe(false);
    });
  });

  describe("the pacing bucket", () => {
    it("does not grant a request on a half-refilled bucket, and arms the retry for the exact remainder", () => {
      const sock = openPagingSocket();
      spendBurst(sock);
      expect(historyControls(sock)).toHaveLength(4);

      // Half a refill interval: half a token, which is not a token.
      vi.advanceTimersByTime(1000);
      expect(requestHistory(5000, 2)).toBe(false);
      expect(historyControls(sock)).toHaveLength(4);

      // The denial armed ONE coalesced retry for exactly the remaining wait:
      // half a token still owed is 1000ms, not 1ms and not 3000ms.
      vi.advanceTimersByTime(999);
      expect(retries).toBe(0);
      vi.advanceTimersByTime(1);
      expect(retries).toBe(1);
    });

    it("coalesces a burst of denials into one retry", () => {
      const sock = openPagingSocket();
      spendBurst(sock);
      vi.advanceTimersByTime(1000);

      // Three triggers denied on the same empty bucket. A scroll gesture fires
      // the trigger repeatedly, so arming a timer per denial would answer one
      // gesture with a burst of requests the moment the bucket refills — into a
      // server bucket that is just as empty.
      expect(requestHistory(5000, 2)).toBe(false);
      expect(requestHistory(6000, 2)).toBe(false);
      expect(requestHistory(7000, 2)).toBe(false);

      vi.advanceTimersByTime(1000);
      expect(retries).toBe(1);
    });
  });

  describe("the resume's replay bound", () => {
    it("sends a consumer's bound of exactly 1", () => {
      init({ ...baseCallbacks(), getReplayMax: () => 1 });
      const sock = openPagingSocket();

      // 1 is the smallest legal bound, not a nonsensical one: a client that
      // holds everything still asks for a floor of one line so the server's
      // clamp and the client's prediction agree.
      expect(controlsSent(sock).find((m) => m["type"] === "resume")?.["replayMax"]).toBe(1);
    });

    it("falls back to the protocol ceiling for a bound of 0", () => {
      init({ ...baseCallbacks(), getReplayMax: () => 0 });
      const sock = openPagingSocket();

      // 0 would ask the server to replay nothing while the server clamps to its
      // own ceiling anyway — the two sides would disagree about whether a
      // replay jump happened, which is the stranded-band case.
      expect(controlsSent(sock).find((m) => m["type"] === "resume")?.["replayMax"]).toBe(
        MAX_REPLAY_LINES,
      );
    });
  });
});
