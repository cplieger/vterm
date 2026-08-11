// @vitest-environment happy-dom
//
// The connection layer's DEMAND-PAGING half (docs/paged-scrollback.md §4 and
// §5.1): reading the server's capability off the resumeAck, capturing the values
// this socket SENT so the store can predict the replay start, the single-flight
// history request with its token bucket and data timeout, the containment rule
// that decides whether a reply may take control effects, the RFC-5681-shaped
// budget ladder, and the pre-ack suppression that keeps a paging client from
// rendering history the resume batch is about to discard.
//
// Drives the REAL connection module with a fake global WebSocket, the same shape
// connection.test.ts uses.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  MAX_REPLAY_LINES,
  connect,
  disconnect,
  historyBudget,
  historyPagingAvailable,
  historyRequestInFlight,
  init,
  requestHistory,
  setSession,
} from "./connection.js";
import { LineStore } from "./store.js";
import { WIRE_PROTOCOL_VERSION } from "./wire-compatibility.js";
import type { ScrollMessage, ServerMessage } from "./types.js";

interface MockWS {
  readyState: number;
  listeners: Map<string, ((ev: unknown) => void)[]>;
  send: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  addEventListener: (type: string, handler: (ev: unknown) => void, opts?: { signal?: AbortSignal }) => void;
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
      fireClose(this: MockWS, code = 1000) {
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

/**
 * A resumeAck frame, mirroring encodeResumeAck (wire_binary.go). The 35-byte
 * form carries the bounds tail plus serverWireVersion + ackFlags, where bit0 is
 * ledgerLost and BIT 1 IS THE PAGING DECLARATION.
 *
 * `noTail` truncates to 17 bytes — a server old enough to report an epoch and
 * nothing else, which is the compatibility path where the client must not
 * invent bounds. The version defaults to the client's own protocol version,
 * because anything lower leaves the socket un-upgraded and history requests are
 * refused on a socket that never negotiated typed framing.
 */
function resumeAckFrame(opts: {
  received?: number;
  committed?: number;
  oldest?: number;
  paging?: boolean;
  ledgerLost?: boolean;
  noTail?: boolean;
}): ArrayBuffer {
  const len = opts.noTail ? 17 : 35;
  const buf = new ArrayBuffer(len);
  const v = new DataView(buf);
  v.setUint8(0, 2); // MSG_RESUME_ACK
  v.setBigUint64(1, BigInt(opts.received ?? 0), true);
  v.setBigUint64(9, 0n, true); // serverEpoch 0: no restart detection
  if (!opts.noTail) {
    v.setBigUint64(17, BigInt(opts.committed ?? 0), true);
    v.setBigUint64(25, BigInt(opts.oldest ?? 0), true);
    v.setUint8(33, WIRE_PROTOCOL_VERSION);
    v.setUint8(34, (opts.ledgerLost ? 1 : 0) | (opts.paging ? 2 : 0));
  }
  return buf;
}

/** A scroll frame: [1B type=1][8B ack][8B firstIndex][2B count][rows...]. */
function scrollFrame(firstIndex: number, count: number): ArrayBuffer {
  const rows: number[] = [];
  for (let i = 0; i < count; i++) {
    const text = new TextEncoder().encode(`L${firstIndex + i}`);
    rows.push(1, 0); // one run (u16 LE)
    rows.push(text.length & 0xff, (text.length >> 8) & 0xff);
    rows.push(...text);
    rows.push(0xff, 0xff, 0xff, 0xff); // f = -1
    rows.push(0xff, 0xff, 0xff, 0xff); // b = -1
    rows.push(0, 0); // a = 0
    rows.push(0xff, 0xff, 0xff, 0xff); // uc = -1
    rows.push(0, 0); // url length 0
  }
  const buf = new ArrayBuffer(19 + rows.length);
  const v = new DataView(buf);
  v.setUint8(0, 1); // MSG_SCROLL
  v.setBigUint64(1, 0n, true); // inputAck
  v.setBigUint64(9, BigInt(firstIndex), true);
  v.setUint16(17, count, true);
  new Uint8Array(buf).set(rows, 19);
  return buf;
}

/**
 * A minimal screen frame with one changed row, optionally carrying ED3. Header
 * layout mirrors encodeScreenMsg: type, inputAck, base, cursorRow, cursorCol,
 * screenHeight, numChanged, cursorStyle, cursorFlags — then one entry per
 * changed row (row index + runs).
 */
function screenFrame(base: number, opts: { scrollbackCleared?: boolean } = {}): ArrayBuffer {
  const text = new TextEncoder().encode("W");
  const row: number[] = [];
  row.push(0, 0); // row index 0 (u16 LE)
  row.push(1, 0); // one run
  row.push(text.length & 0xff, (text.length >> 8) & 0xff);
  row.push(...text);
  row.push(0xff, 0xff, 0xff, 0xff); // f = -1
  row.push(0xff, 0xff, 0xff, 0xff); // b = -1
  row.push(0, 0); // a = 0
  row.push(0xff, 0xff, 0xff, 0xff); // uc = -1
  row.push(0, 0); // url length 0
  const header = 27;
  const buf = new ArrayBuffer(header + row.length);
  const v = new DataView(buf);
  v.setUint8(0, 0); // MSG_SCREEN
  v.setBigUint64(1, 0n, true); // inputAck
  v.setBigUint64(9, BigInt(base), true);
  v.setUint16(17, 0, true); // cursorRow
  v.setUint16(19, 0, true); // cursorCol
  v.setUint16(21, 1, true); // screenHeight
  v.setUint16(23, 1, true); // numChanged
  v.setUint8(25, 0); // cursorStyle
  v.setUint8(26, opts.scrollbackCleared ? 16 : 0); // cursorFlags: bit4 = ED3
  new Uint8Array(buf).set(row, header);
  return buf;
}

interface Harness {
  messages: ServerMessage[];
  replies: { msg: ScrollMessage; raiseFloorTo: number | null }[];
  transitions: {
    epochChanged: boolean;
    committed: number | null;
    serverOldest: number | null;
    paging: boolean;
    sentHaveThrough: number;
    sentReplayMax: number | null;
  }[];
  solicited: [number, number][];
  cleared: number;
  retries: number;
  /**
   * An optional REAL store wired into the reply callbacks, for the one test that
   * has to observe the ORDER of two of them rather than their counts. Null for
   * every other test, which only needs the counts.
   */
  store: LineStore | null;
}

let h: Harness;

/**
 * Just past the 8s history data timeout. Deliberately NOT tens of seconds: the
 * heartbeat's own ping deadline lives out there, and letting it fire closes the
 * socket and RESETS every history counter, which would make a budget assertion
 * pass or fail for the wrong reason.
 */
const HISTORY_TIMEOUT_SLACK = 9_000;

/** Control frames the socket was asked to send, decoded from their JSON body. */
/**
 * Every control the client sent, in EITHER encoding — a JSON text frame (what a
 * post-upgrade socket uses) or a v3 0x00-sentinel binary frame (the pre-upgrade
 * bootstrap).
 *
 * Decoding only the binary form is how 29 tests in this file passed against a
 * client that sent its history requests in the wrong encoding entirely: the
 * string sends were invisible here, so the harness saw the frames the code
 * should not have been sending and nothing saw the ones it should. A harness
 * that ignores a whole encoding cannot notice a client choosing it.
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
    const bytes = a instanceof Uint8Array ? a : a instanceof ArrayBuffer ? new Uint8Array(a) : null;
    if (bytes === null || bytes.length === 0 || bytes[0] !== 0x00) {
      continue;
    }
    out.push(JSON.parse(new TextDecoder().decode(bytes.subarray(1))) as Record<string, unknown>);
  }
  return out;
}

/** The RAW frames sent, so a test can assert the encoding and not just the payload. */
function rawSent(sock: MockWS): unknown[] {
  return (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls.map((c) => c[0]);
}

function historyControls(sock: MockWS): { fromAbs: number; maxLines: number }[] {
  return controlsSent(sock)
    .filter((m) => m["type"] === "history")
    .map((m) => ({ fromAbs: m["fromAbs"] as number, maxLines: m["maxLines"] as number }));
}

/**
 * Bring a socket up and (optionally) declare paging, which is what makes
 * `requestHistory` willing to send at all.
 */
function openSocket(opts: { paging?: boolean; committed?: number; oldest?: number; ack?: boolean } = {}): MockWS {
  connect();
  const sock = sockets[sockets.length - 1]!;
  sock.fireOpen();
  if (opts.ack !== false) {
    sock.fireMessage(
      resumeAckFrame({ paging: opts.paging ?? true, committed: opts.committed ?? 5000, oldest: opts.oldest ?? 0 }),
    );
  }
  return sock;
}

describe("connection: history paging", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    h = { messages: [], replies: [], transitions: [], solicited: [], cleared: 0, retries: 0, store: null };
    init({
      onMessage: (msg) => h.messages.push(msg),
      onOpen: () => {
        /* no-op */
      },
      onClose: () => {
        /* no-op */
      },
      computeSize: () => ({ cols: 80, rows: 24 }),
      onHistoryReply: (msg, raiseFloorTo) => {
        h.replies.push({ msg, raiseFloorTo });
        h.store?.applyHistoryScroll(msg, msg.firstIndex);
      },
      onResumeTransition: (t) => h.transitions.push(t),
      noteSolicited: (lo, hi) => {
        h.solicited.push([lo, hi]);
        h.store?.noteSolicited(lo, hi);
      },
      clearSolicited: () => {
        h.cleared++;
        h.store?.clearSolicited();
      },
      onHistoryRetry: () => {
        h.retries++;
      },
    });
    setSession("paging-tests");
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  describe("capability", () => {
    it("is read from the ack's flag bit, not assumed", () => {
      const sock = openSocket({ paging: true });
      expect(historyPagingAvailable()).toBe(true);
      expect(h.transitions.at(-1)?.paging).toBe(true);
      expect(sock.close).not.toHaveBeenCalled();
    });

    it("stays off for a server that does not declare it", () => {
      const sock = openSocket({ paging: false });
      expect(historyPagingAvailable()).toBe(false);
      expect(requestHistory(0, 100)).toBe(false);
      expect(historyControls(sock)).toEqual([]);
    });

    it("stays off for a server too old to carry the flags byte", () => {
      const sock = openSocket({ ack: false });
      sock.fireMessage(resumeAckFrame({ noTail: true }));
      expect(historyPagingAvailable()).toBe(false);
      // The transition still fires, so the store runs its epoch reset; the
      // bounds are null because inventing zeros would forge a replay jump.
      const t = h.transitions.at(-1);
      expect(t?.paging).toBe(false);
      expect(t?.committed).toBeNull();
      expect(t?.serverOldest).toBeNull();
    });

    it("reports the bounds tail to the store when the ack carries it", () => {
      openSocket({ paging: true, committed: 9000, oldest: 4000 });
      const t = h.transitions.at(-1);
      expect(t?.committed).toBe(9000);
      expect(t?.serverOldest).toBe(4000);
    });

    it("does not survive the socket that declared it", () => {
      const sock = openSocket({ paging: true });
      expect(historyPagingAvailable()).toBe(true);
      sock.fireClose(1006);
      expect(historyPagingAvailable()).toBe(false);
      expect(historyRequestInFlight()).toBe(false);
    });
  });

  describe("the resume send", () => {
    it("carries the client's own replayMax and reports the SENT value", () => {
      init({
        onMessage: (msg) => h.messages.push(msg),
        onOpen: () => {
          /* no-op */
        },
        onClose: () => {
          /* no-op */
        },
        computeSize: () => ({ cols: 80, rows: 24 }),
        getReplayMax: () => 750,
        onResumeTransition: (t) => h.transitions.push(t),
      });
      setSession("paging-tests");
      const sock = openSocket({ paging: true });
      const resume = controlsSent(sock).find((m) => m["type"] === "resume");
      expect(resume?.["replayMax"]).toBe(750);
      // The store predicts the replay start from the value the SOCKET sent, so
      // the same number has to come back on the transition.
      expect(h.transitions.at(-1)?.sentReplayMax).toBe(750);
    });

    it("clamps the client's request to the protocol maximum on BOTH sides", () => {
      // The server clamps too. Sending an unclamped value and letting the
      // server silently reduce it would desynchronize the jump prediction,
      // which is computed from the sent value.
      init({
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
        getReplayMax: () => MAX_REPLAY_LINES * 10,
        onResumeTransition: (t) => h.transitions.push(t),
      });
      setSession("paging-tests");
      const sock = openSocket({ paging: true });
      expect(controlsSent(sock).find((m) => m["type"] === "resume")?.["replayMax"]).toBe(MAX_REPLAY_LINES);
      expect(h.transitions.at(-1)?.sentReplayMax).toBe(MAX_REPLAY_LINES);
    });

    it("sends the protocol ceiling when the consumer offers no bound", () => {
      // Never omitted. The server bounds the replay whether or not the client
      // asks, so a client that sent nothing would predict no replay jump where
      // the server made one — and the stranded band would stay classified as
      // live tail for the cap to eat. The two sides agree by construction.
      const sock = openSocket({ paging: true });
      const resume = controlsSent(sock).find((m) => m["type"] === "resume");
      expect(resume).toBeDefined();
      expect(resume!["replayMax"]).toBe(MAX_REPLAY_LINES);
      expect(h.transitions.at(-1)?.sentReplayMax).toBe(MAX_REPLAY_LINES);
    });
  });

  describe("requestHistory", () => {
    it("sends one request and marks the window solicited", () => {
      const sock = openSocket({ paging: true });
      expect(requestHistory(1000, 500)).toBe(true);
      expect(historyControls(sock)).toEqual([{ fromAbs: 1000, maxLines: 500 }]);
      expect(h.solicited).toEqual([[1000, 1500]]);
      expect(historyRequestInFlight()).toBe(true);
    });

    it("is single-flight: a second request while one is open is refused", () => {
      const sock = openSocket({ paging: true });
      expect(requestHistory(1000, 500)).toBe(true);
      expect(requestHistory(2000, 500)).toBe(false);
      expect(historyControls(sock).length).toBe(1);
    });

    it("refuses a request before the ack has arrived", () => {
      // Until the ack the client does not know the server pages, and the
      // pre-ack window is exactly when a request would race the resume batch.
      // (`paging` cannot be true before the ack either, since only the ack
      // handler sets it — the separate `acked` check is belt-and-braces for a
      // future capability source that is not the ack.)
      const sock = openSocket({ ack: false });
      expect(requestHistory(1000, 500)).toBe(false);
      expect(historyControls(sock)).toEqual([]);
    });

    it("refuses an unsafe or nonsensical range instead of sending it", () => {
      const sock = openSocket({ paging: true });
      expect(requestHistory(-1, 100)).toBe(false);
      expect(requestHistory(0, 0)).toBe(false);
      expect(requestHistory(Number.MAX_SAFE_INTEGER, 100)).toBe(false);
      expect(requestHistory(1.5, 100)).toBe(false);
      expect(historyControls(sock)).toEqual([]);
    });

    it("clamps the page size to the current budget", () => {
      const sock = openSocket({ paging: true });
      requestHistory(1000, 10_000);
      expect(historyControls(sock)[0]?.maxLines).toBe(historyBudget());
    });

    it("spends its burst and then paces, asking the consumer to retry", () => {
      const sock = openSocket({ paging: true });
      // The bucket starts full: each grant needs the previous reply to release
      // the single-flight slot first.
      let sent = 0;
      for (let i = 0; i < 6; i++) {
        if (requestHistory(1000 + i * 500, 500)) {
          sent++;
          sock.fireMessage(scrollFrame(1000 + i * 500, 500));
        }
      }
      expect(sent).toBeGreaterThan(0);
      expect(sent).toBeLessThan(6); // the bucket ran out
      const before = historyControls(sock).length;
      // The denial armed ONE coalesced retry, which is a timer: letting the
      // clock run both fires it and refills the bucket.
      vi.advanceTimersByTime(5000);
      expect(h.retries).toBeGreaterThan(0);
      expect(requestHistory(9000, 500)).toBe(true);
      expect(historyControls(sock).length).toBe(before + 1);
    });
  });

  describe("reply correlation", () => {
    it("forwards a contained reply, releases the slot AND closes the solicited window", () => {
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      const clearedBefore = h.cleared;
      sock.fireMessage(scrollFrame(1000, 500));
      expect(h.replies.length).toBe(1);
      expect(h.replies[0]?.msg.firstIndex).toBe(1000);
      expect(h.replies[0]?.raiseFloorTo).toBeNull();
      expect(historyRequestInFlight()).toBe(false);
      // The window is the store's permission to admit lines below its
      // stale-re-send watermark. Left open with no request in flight, a later
      // duplicate or malformed frame in the same range keeps bypassing that
      // guard — so the transport releasing its slot is only half the lifecycle.
      expect(h.cleared).toBe(clearedBefore + 1);
    });

    it("sends the request as a TEXT control, never a v3 sentinel frame", () => {
      // The ENCODING, asserted separately from the payload, because getting it
      // wrong is silent on both sides. requestHistory only runs on an UPGRADED
      // socket, and a post-upgrade server parses the 0x00 sentinel only while
      // unlatched — so a binary frame here is not a control at all: it is written
      // to the PTY, typed into the user's shell, and counted in the server's
      // received-byte ledger that the client's outbox reconciles against.
      const sock = openSocket({ paging: true });
      const before = rawSent(sock).length;
      expect(requestHistory(1000, 500)).toBe(true);

      const frames = rawSent(sock).slice(before);
      expect(frames.length).toBe(1);
      const frame = frames[0];
      expect(typeof frame).toBe("string");
      expect(JSON.parse(frame as string)).toMatchObject({ type: "history", fromAbs: 1000 });
    });

    it("applies the page BEFORE closing the window, proved against a real store", () => {
      // The order of these two callbacks is load-bearing and the counts cannot
      // see it. The solicited window is the store's only exemption from its
      // stale-re-send watermark, so closing it first means a page apply with no
      // window — which the store now drops WHOLE — and the requested history is
      // silently lost while every count in this file still reads correct.
      //
      // So drive a real store: a cap-bounded fill pushes the eviction watermark
      // far above the requested range, making the exemption the only way in.
      const store = new LineStore(100);
      store.applyScroll({
        type: "scroll",
        firstIndex: 0,
        lines: Array.from({ length: 3000 }, (_, i) => [
          { t: `L${String(i)}`, f: -1, b: -1, a: 0, uc: -1 },
        ]),
      });
      expect(store.getLine(1000)).toBeUndefined(); // evicted, and below the watermark
      h.store = store;

      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      const clearedBefore = h.cleared; // the socket lifecycle clears too
      sock.fireMessage(scrollFrame(1000, 500));

      expect(h.cleared).toBe(clearedBefore + 1); // the window did close
      expect(store.getLine(1000)).toBeDefined(); // but only after the page landed
      expect(store.browseCacheSize()).toBe(500);
    });

    it("keeps the solicited window open for an overspilling reply", () => {
      // Its attempt is still open (the slot is not released either), so the
      // window that authorises the rest of the range has to stay with it.
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      const clearedBefore = h.cleared;
      sock.fireMessage(scrollFrame(1000, 900));
      expect(h.replies.length).toBe(1);
      expect(historyRequestInFlight()).toBe(true);
      expect(h.cleared).toBe(clearedBefore);
    });

    it("reads a clamped reply as proof the floor moved", () => {
      // firstIndex above the request's fromAbs means the server no longer holds
      // the bottom of the range, so nothing below the reply's start can ever be
      // served and asking again would burn a token per approach.
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      sock.fireMessage(scrollFrame(1200, 300));
      expect(h.replies[0]?.raiseFloorTo).toBe(1200);
    });

    it("reads an empty reply as the whole window being gone", () => {
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      sock.fireMessage(scrollFrame(1000, 0));
      expect(h.replies.length).toBe(1);
      expect(h.replies[0]?.raiseFloorTo).toBe(1500); // the request's end
      expect(historyRequestInFlight()).toBe(false);
    });

    it("does not correlate a frame at the window's exclusive upper edge", () => {
      // The window is half-open [fromAbs, fromAbs + maxLines): a frame starting
      // at the end index is the NEXT range, so treating it as the reply would
      // release single-flight on content the request never asked for.
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      h.messages.length = 0;
      sock.fireMessage(scrollFrame(1500, 3));
      expect(h.replies.length).toBe(0);
      expect(h.messages.some((m) => m.type === "scroll")).toBe(true);
      expect(historyRequestInFlight()).toBe(true);
    });

    it("does not correlate a frame below the window", () => {
      // The shape the server actually emits while a fetch is open: live
      // committed lines whose firstIndex is the PREVIOUS window base, strictly
      // below the requested range.
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      sock.fireMessage(scrollFrame(500, 3));
      expect(h.replies.length).toBe(0);
      expect(historyRequestInFlight()).toBe(true);
    });

    it("does correlate the window's first and last index", () => {
      // Both inclusive edges are the reply, so the guard cannot be an
      // off-by-one in the other direction either.
      const first = openSocket({ paging: true });
      requestHistory(1000, 500);
      first.fireMessage(scrollFrame(1000, 1));
      expect(h.replies.length).toBe(1);

      vi.advanceTimersByTime(5000);
      requestHistory(2000, 500);
      first.fireMessage(scrollFrame(2499, 1));
      expect(h.replies.length).toBe(2);
    });

    it("passes an uncorrelated scroll frame through as live output", () => {
      // Live committed lines keep flowing while a fetch is open; only frames
      // inside the requested window are that request's reply.
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      h.messages.length = 0;
      sock.fireMessage(scrollFrame(8000, 3));
      expect(h.replies.length).toBe(0);
      expect(h.messages.some((m) => m.type === "scroll")).toBe(true);
      expect(historyRequestInFlight()).toBe(true); // still waiting
    });

    it("does not let an overspilling reply take control effects", () => {
      // A reply wider than the current slot is a timed-out larger request
      // racing a shrunken retry. It is still forwarded (the store clips it),
      // but it must not release the slot or grow the budget, or the ladder
      // would be driven by a request that already failed.
      const sock = openSocket({ paging: true });
      requestHistory(1000, 500);
      sock.fireMessage(scrollFrame(1000, 900)); // spills past 1500
      expect(h.replies.length).toBe(1);
      expect(historyRequestInFlight()).toBe(true);
    });
  });

  describe("the budget ladder", () => {
    it("halves the ceiling and restarts small after a data timeout", () => {
      openSocket({ paging: true });
      const full = historyBudget();
      requestHistory(1000, full);
      expect(historyRequestInFlight()).toBe(true);

      vi.advanceTimersByTime(HISTORY_TIMEOUT_SLACK); // the data timer fires; the socket stays up
      // The slot is released so the reader is not stuck, the solicited window is
      // withdrawn, and the next attempt is small (RFC 5681's ssthresh shape:
      // remember half of what failed, restart at the floor).
      expect(historyRequestInFlight()).toBe(false);
      expect(h.cleared).toBeGreaterThan(0);
      expect(historyBudget()).toBeLessThan(full);
      expect(h.retries).toBeGreaterThan(0);
    });

    it("grows back toward the remembered ceiling, and never past it", () => {
      const sock = openSocket({ paging: true });
      const full = historyBudget();
      requestHistory(1000, full);
      vi.advanceTimersByTime(HISTORY_TIMEOUT_SLACK);
      const floor = historyBudget();
      expect(floor).toBeLessThan(full);

      // Each contained reply doubles the budget; the ceiling caps it. Plain
      // capped doubling would have been arithmetically dead here.
      const seen: number[] = [floor];
      for (let i = 0; i < 8; i++) {
        vi.advanceTimersByTime(5000); // refill the bucket
        if (!requestHistory(2000 + i * 200, historyBudget())) {
          break;
        }
        sock.fireMessage(scrollFrame(2000 + i * 200, 1));
        seen.push(historyBudget());
      }
      expect(seen[1]).toBeGreaterThan(floor);
      expect(Math.max(...seen)).toBeLessThan(full);
      // Monotone non-decreasing on success: no step goes backwards.
      for (let i = 1; i < seen.length; i++) {
        expect(seen[i]).toBeGreaterThanOrEqual(seen[i - 1]!);
      }
    });
  });

  describe("pre-ack suppression", () => {
    it("drops history frames that arrive before the ack", () => {
      // A paging client's resume is bounded, so anything the server sent before
      // the ack may be history the batch is about to supersede. Rendering it
      // would show rows the client then discards.
      const sock = openSocket({ ack: false });
      sock.fireMessage(scrollFrame(1000, 5));
      expect(h.messages.some((m) => m.type === "scroll")).toBe(false);

      sock.fireMessage(resumeAckFrame({ paging: true }));
      sock.fireMessage(scrollFrame(1000, 5));
      expect(h.messages.some((m) => m.type === "scroll")).toBe(true);
    });

    it("still forwards a pre-ack scrollbackCleared, without its rows", () => {
      // ED3 is a consumed one-shot: the resume batch hard-codes it false, so a
      // dropped frame loses the clear forever and the client keeps showing
      // history the application erased.
      const sock = openSocket({ ack: false });
      sock.fireMessage(screenFrame(0, { scrollbackCleared: true }));
      const screens = h.messages.filter((m) => m.type === "screen");
      expect(screens.length).toBe(1);
      const msg = screens[0] as Extract<ServerMessage, { type: "screen" }>;
      expect(msg.scrollbackCleared).toBe(true);
      expect(msg.changed.length).toBe(0); // rows withheld
    });

    it("drops a pre-ack screen frame that carries no clear", () => {
      const sock = openSocket({ ack: false });
      sock.fireMessage(screenFrame(0));
      expect(h.messages.filter((m) => m.type === "screen").length).toBe(0);
    });
  });
});
