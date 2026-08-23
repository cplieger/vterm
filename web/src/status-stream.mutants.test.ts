// Two things the status stream owes that its own suites observe only indirectly:
// the report it makes when a frame does not parse, and the CANCELLATION of a
// pending reconnect on close().
//
// The reconnect timer is the interesting one. status-stream.test.ts and
// status-stream-reconnect.test.ts both check that no stream is reopened after
// close(), and that holds even when the timer is left armed — the callback finds
// `closed` set and does nothing. So "no reopen" cannot tell a cancelled timer
// from a leaked one, and the leak is what matters: the module is per-tab, and a
// timer that outlives the consumer keeps the whole closure (and its
// EventSource) alive until it fires.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

import { connectStatusStream, type EventSourceLike, type SessionStatus } from "./status-stream.js";

/** The listener-recording fake the other status-stream suites use. */
class FakeEventSource implements EventSourceLike {
  readonly url: string;
  closed = 0;
  readyState = 0; // 0 CONNECTING, 1 OPEN, 2 CLOSED
  private readonly listeners = new Map<string, ((event: MessageEvent) => void)[]>();

  constructor(url: string) {
    this.url = url;
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void): void {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close(): void {
    this.closed++;
  }

  emit(type: string, data?: string): void {
    for (const l of this.listeners.get(type) ?? []) {
      l({ data } as MessageEvent);
    }
  }
}

const sample: SessionStatus = {
  id: "abc123",
  status: "idle",
  title: "kiro-cli",
  createdAt: "2026-07-01T12:00:00Z",
};

describe("connectStatusStream: the malformed-frame report", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("names the dropped frame in a warning instead of failing silently", () => {
    // A status frame that does not parse means the server and this client
    // disagree about the payload — a schema change, or an error page served in
    // place of the stream. The stream deliberately survives it, so the warning
    // is the only trace: without it a consumer sees a session list that quietly
    // stops updating and nothing anywhere says why.
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const onStatus = vi.fn();
    let fake: FakeEventSource | undefined;
    connectStatusStream("/api/sessions/events", { onStatus }, (url) => {
      fake = new FakeEventSource(url);
      return fake;
    });

    fake?.emit("message", "{not json");

    expect(onStatus).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn).toHaveBeenCalledWith("vterm: dropped malformed status-stream frame");

    // And the stream keeps working, which is why the frame is only warned about.
    fake?.emit("message", JSON.stringify(sample));
    expect(onStatus).toHaveBeenCalledWith(sample);
  });
});

describe("connectStatusStream: close() cancels the pending reconnect", () => {
  let sources: FakeEventSource[];

  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(Math, "random").mockReturnValue(0.5);
    sources = [];
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  function mount() {
    return connectStatusStream("/api/sessions/events", { onStatus: vi.fn() }, (url) => {
      const f = new FakeEventSource(url);
      sources.push(f);
      return f;
    });
  }

  it("clears the armed timer, not just the reopen it would have done", () => {
    const stream = mount();

    // A permanent close (readyState CLOSED) is the case the module reconnects
    // for, so this arms the backoff timer.
    sources[0]!.readyState = 2;
    sources[0]!.emit("error");
    expect(vi.getTimerCount()).toBe(1);

    stream.close();

    // The timer is GONE, not merely disarmed by the `closed` flag: nothing of
    // this stream is left holding the event loop or its closure.
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(60_000);
    expect(sources).toHaveLength(1);
    expect(sources[0]!.closed).toBe(1);
  });

  it("closes cleanly when no reconnect is pending", () => {
    // The other side of the guard: an ordinary close with nothing armed neither
    // throws nor leaves a timer behind.
    const stream = mount();
    expect(vi.getTimerCount()).toBe(0);

    expect(() => {
      stream.close();
    }).not.toThrow();

    expect(vi.getTimerCount()).toBe(0);
    expect(sources[0]!.closed).toBe(1);
  });
});
