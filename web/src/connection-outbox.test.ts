// The connection layer's RELIABLE-INPUT half: the outbox, the acked-byte window
// and what a reconnect replays over the new socket.
//
// Every assertion here is about bytes the user typed. The outbox is the only
// copy of input the server has not confirmed, and applyAck is the only thing
// that removes bytes from it, so an off-by-one there is silent: keystrokes
// vanish on a reconnect (trimmed too far) or are executed twice (trimmed too
// little). The observable is deliberately the WIRE — the frames the replacement
// socket is asked to send after its resumeAck — never the module's counters.
//
// Drives the REAL connection module with a fake global WebSocket, the same shape
// connection.test.ts and connection-paging.test.ts use.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  MAX_OUTBOX_BYTES,
  disconnect,
  init,
  reconnectNow,
  sendBinary,
  sendResize,
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

/**
 * A resumeAck frame, mirroring encodeResumeAck (wire_binary.go): 35 bytes, so
 * it carries the version byte that arms the typed-framing upgrade. `version`
 * exists for the v3 cases — a server that stays below TYPED_FRAMING_MIN_VERSION
 * leaves the socket in sentinel mode, which is where the leading-NUL split
 * lives.
 */
function resumeAckFrame(opts: { received?: number; version?: number | undefined }): ArrayBuffer {
  const buf = new ArrayBuffer(35);
  const v = new DataView(buf);
  v.setUint8(0, 2); // MSG_RESUME_ACK
  v.setBigUint64(1, BigInt(opts.received ?? 0), true);
  v.setBigUint64(9, 0n, true); // serverEpoch 0: no restart detection
  v.setBigUint64(17, 0n, true); // committed
  v.setBigUint64(25, 0n, true); // oldestIndex
  v.setUint8(33, opts.version ?? WIRE_PROTOCOL_VERSION);
  v.setUint8(34, 0); // ackFlags: no ledgerLost, no paging
  return buf;
}

/** A bare ack, mirroring MSG_ACK_ONLY: [1B type=7][8B inputAck]. */
function ackOnlyFrame(inputAck: number): ArrayBuffer {
  const buf = new ArrayBuffer(9);
  const v = new DataView(buf);
  v.setUint8(0, 7);
  v.setBigUint64(1, BigInt(inputAck), true);
  return buf;
}

/**
 * The PTY-input frames the socket was asked to send, decoded as text.
 *
 * The encoding is the discriminator, and it is exact rather than incidental:
 * `controlFrame` sends a Uint8Array (the 0x00-sentinel form) and `textControl` a
 * string, while every input path — live send and retransmit alike — sends an
 * ArrayBuffer. So this sees keystrokes and nothing else, including a solitary
 * NUL frame, which a "first byte is 0x00 means control" filter would swallow.
 */
function inputSent(sock: MockWS): string[] {
  const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
  const out: string[] = [];
  for (const c of calls) {
    if (c[0] instanceof ArrayBuffer) {
      out.push(new TextDecoder().decode(c[0]));
    }
  }
  return out;
}

/** Byte lengths of the input frames sent, so a test can pin FRAMING, not text. */
function inputFrameSizes(sock: MockWS): number[] {
  const calls = (sock.send as unknown as { mock: { calls: unknown[][] } }).mock.calls;
  const out: number[] = [];
  for (const c of calls) {
    if (c[0] instanceof ArrayBuffer) {
      out.push(c[0].byteLength);
    }
  }
  return out;
}

const enc = new TextEncoder();

let outboxFull: ReturnType<typeof vi.fn<() => void>>;
let serverRestart: ReturnType<typeof vi.fn<() => void>>;
let computeSize: ReturnType<typeof vi.fn<() => { cols: number; rows: number }>>;
let session = 0;

/** Bring up a socket for a FRESH session and take it through its resumeAck. */
function openSession(opts: { version?: number | undefined } = {}): MockWS {
  session++;
  setSession(`outbox-${String(session)}`);
  const sock = sockets[sockets.length - 1]!;
  sock.fireOpen();
  sock.fireMessage(resumeAckFrame({ received: 0, version: opts.version }));
  return sock;
}

/** Reconnect and answer the new socket's resume with `received`. */
function reconnectAcking(received: number, opts: { version?: number | undefined } = {}): MockWS {
  reconnectNow();
  const sock = sockets[sockets.length - 1]!;
  sock.fireOpen();
  sock.fireMessage(resumeAckFrame({ received, version: opts.version }));
  return sock;
}

describe("connection: the outbox and the acked-byte window", () => {
  beforeEach(() => {
    sockets.length = 0;
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", makeMockWebSocket());
    outboxFull = vi.fn<() => void>();
    serverRestart = vi.fn<() => void>();
    computeSize = vi.fn<() => { cols: number; rows: number }>(() => ({ cols: 80, rows: 24 }));
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
      computeSize,
      onOutboxFull: outboxFull,
      onServerRestart: serverRestart,
    });
  });

  afterEach(() => {
    disconnect();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  describe("the cap", () => {
    it("accepts input that fills the outbox to exactly the cap", () => {
      openSession();

      // The boundary is `>`, not `>=`: a send that lands exactly on the cap is
      // the largest legal one, and refusing it would drop input the client can
      // still hold.
      expect(sendBinary(new Uint8Array(MAX_OUTBOX_BYTES))).toBe(true);
      expect(outboxFull).not.toHaveBeenCalled();
    });

    it("refuses the byte that would exceed the cap, reports it, and sends nothing", () => {
      const sock = openSession();
      sendBinary(new Uint8Array(MAX_OUTBOX_BYTES));
      const framesBefore = inputFrameSizes(sock).length;

      expect(sendBinary(enc.encode("x"))).toBe(false);
      expect(outboxFull).toHaveBeenCalledTimes(1);
      // Refused means refused on the wire too: a byte the outbox will not hold
      // must not reach the server, or the server's ledger runs ahead of ours
      // and every later ack trims bytes we never had confirmed.
      expect(inputFrameSizes(sock).length).toBe(framesBefore);
    });
  });

  describe("what a reconnect replays", () => {
    it("replays exactly the bytes the resume ack left unacked", () => {
      openSession();
      sendBinary(enc.encode("hello"));

      // The server confirms 2 of the 5 bytes: "he" is applied, "llo" is not.
      const next = reconnectAcking(2);

      expect(inputSent(next)).toEqual(["llo"]);
    });

    it("trims a second ack from the already-trimmed head", () => {
      openSession();
      sendBinary(enc.encode("hello"));
      const first = sockets[sockets.length - 1]!;
      first.fireMessage(ackOnlyFrame(2));
      first.fireMessage(ackOnlyFrame(4));

      // Both acks landed inside the same chunk, so the second one has to
      // measure its drop against the REMAINING head, not the original.
      const next = reconnectAcking(4);

      expect(inputSent(next)).toEqual(["o"]);
    });

    it("drops every chunk an ack spans and keeps the partial remainder", () => {
      openSession();
      sendBinary(enc.encode("ab"));
      sendBinary(enc.encode("cde"));

      // 4 of 5 bytes acked: the whole first chunk plus two bytes of the second.
      const next = reconnectAcking(4);

      expect(inputSent(next)).toEqual(["e"]);
    });

    it("replays nothing once every sent byte is acked", () => {
      const errors = vi.spyOn(console, "error").mockImplementation(() => {
        /* keep the log quiet; the assertion is that it stays empty */
      });
      openSession();
      sendBinary(enc.encode("ab"));
      sendBinary(enc.encode("cd"));
      const first = sockets[sockets.length - 1]!;
      first.fireMessage(ackOnlyFrame(4));

      const next = reconnectAcking(4);

      // A fully-acked outbox that still holds a chunk is duplicate input: the
      // server already ran those keystrokes.
      expect(inputSent(next)).toEqual([]);
      // And the walk that emptied it has to TERMINATE. A throw inside the ack
      // handling is swallowed by the frame listener's try/catch, so a walk that
      // runs off the end of the outbox leaves no trace except this log line —
      // and skips the retransmit that follows it.
      expect(errors).not.toHaveBeenCalled();
    });

    it("does not let a stale ack reopen the window a later ack closed", () => {
      openSession();
      sendBinary(enc.encode("hello"));
      const first = sockets[sockets.length - 1]!;
      first.fireMessage(ackOnlyFrame(3));
      // A late duplicate from the flush sweep, below the high-water mark. It
      // must not move the window backwards: the received=0 resume below is the
      // server telling us it no longer holds this ledger, and the client can
      // only tell that from a bytesAcked the stale ack did not zero.
      first.fireMessage(ackOnlyFrame(0));

      const next = reconnectAcking(0);

      expect(inputSent(next)).toEqual([]);
      expect(serverRestart).toHaveBeenCalledTimes(1);
    });

    it("clamps an ack that claims more bytes than were sent, so later acks still trim", () => {
      openSession();
      sendBinary(enc.encode("hello"));
      const first = sockets[sockets.length - 1]!;
      // A server that counts bytes we never sent (its own control frames, say)
      // would otherwise park bytesAcked above every future ack, and the outbox
      // would never trim again.
      first.fireMessage(ackOnlyFrame(99));
      sendBinary(enc.encode("more"));
      first.fireMessage(ackOnlyFrame(7));

      const next = reconnectAcking(7);

      expect(inputSent(next)).toEqual(["re"]);
    });
  });

  describe("v3 framing of replayed input", () => {
    it("splits leading NULs into solitary frames with no empty frame after them", () => {
      // A server below the typed-framing floor never latches, so every frame
      // the client sends stays parseable as a v3 control unless each leading
      // 0x00 goes out alone. The trailing byte count matters as much as the
      // split: an empty frame is not a control and not input.
      const sock = openSession({ version: 3 });

      sendBinary(new Uint8Array([0x00, 0x00]));

      expect(inputFrameSizes(sock)).toEqual([1, 1]);
    });

    it("sends the remainder after a leading NUL as one frame", () => {
      const sock = openSession({ version: 3 });

      sendBinary(new Uint8Array([0x00, 0x41, 0x42]));

      expect(inputFrameSizes(sock)).toEqual([1, 2]);
    });
  });

  describe("resize coalescing", () => {
    it("announces a resize that changed only the row count", () => {
      const sock = openSession();
      computeSize.mockReturnValue({ cols: 80, rows: 24 });
      sendResize();
      const before = controlsOfType(sock, "resize").length;
      computeSize.mockReturnValue({ cols: 80, rows: 30 });

      sendResize();

      // Both dimensions have to match for a resize to be redundant. Suppressing
      // on either one alone loses a real geometry change, and the server keeps
      // rendering at the old size.
      expect(controlsOfType(sock, "resize").length).toBe(before + 1);
      expect(controlsOfType(sock, "resize").at(-1)).toEqual({ type: "resize", cols: 80, rows: 30 });
    });

    it("announces a resize that changed only the column count", () => {
      const sock = openSession();
      computeSize.mockReturnValue({ cols: 80, rows: 24 });
      sendResize();
      const before = controlsOfType(sock, "resize").length;
      computeSize.mockReturnValue({ cols: 100, rows: 24 });

      sendResize();

      // The mirror of the row-only case: each dimension has to be compared on
      // its own, and a column-only change is the common one (a rotated phone).
      expect(controlsOfType(sock, "resize").length).toBe(before + 1);
      expect(controlsOfType(sock, "resize").at(-1)).toEqual({
        type: "resize",
        cols: 100,
        rows: 24,
      });
    });

    it("does not ask the consumer to measure while there is no socket", () => {
      openSession();
      disconnect();
      computeSize.mockClear();

      sendResize();

      // Measuring forces layout in a real consumer, and the answer could not be
      // sent anywhere: the dedup baseline is reset on every open, so a size
      // recorded now would only be a lie about what the server has been told.
      expect(computeSize).not.toHaveBeenCalled();
    });
  });
});

/** Control frames of one type, decoded from either encoding, in order. */
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
