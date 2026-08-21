// Tests for the status stream's RECONNECT DECISION and its default EventSource
// adapter — the two halves status-stream.test.ts leaves open.
//
// The decision is a three-part guard: re-establish only on a PERMANENT close
// (readyState CLOSED), only while the consumer has not closed the stream, and
// only when no reconnect is already pending. Each part defends a different
// failure: reconnecting on a transient drop fights native EventSource
// auto-reconnect and doubles the streams; reconnecting after close() resurrects
// a stream the consumer dropped; and re-arming while a timer is pending turns
// one drop into a reconnect storm.
//
// The adapter is the factory that actually ships (tests inject a fake, browsers
// get this one), and it was reached by no test at all.

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

import { connectStatusStream, type EventSourceLike, type SessionStatus } from "./status-stream.js";

/** Records listeners and lets a test dispatch events synchronously. */
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

/** The jittered first backoff with Math.random pinned to 0.5: 500 + 0.5*250. */
const FIRST_BACKOFF_MS = 625;

describe("connectStatusStream: the reconnect decision", () => {
  let sources: FakeEventSource[];
  let randomSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.useFakeTimers();
    // Pin the jitter so a delay assertion is exact.
    randomSpy = vi.spyOn(Math, "random").mockReturnValue(0.5);
    sources = [];
  });

  afterEach(() => {
    randomSpy.mockRestore();
    vi.useRealTimers();
  });

  function mount() {
    return connectStatusStream("/api/sessions/events", { onStatus: vi.fn() }, (url) => {
      const f = new FakeEventSource(url);
      sources.push(f);
      return f;
    });
  }

  it("schedules no reconnect for an error on a stream that is still connecting", () => {
    mount();

    // readyState CONNECTING: the browser's own auto-reconnect owns this case,
    // and re-establishing on top of it would run two streams for one consumer.
    sources[0]!.readyState = 0;
    sources[0]!.emit("error");
    vi.advanceTimersByTime(60_000);

    expect(sources).toHaveLength(1);
  });

  it("schedules no reconnect for an error on an open stream", () => {
    mount();

    sources[0]!.readyState = 1;
    sources[0]!.emit("error");
    vi.advanceTimersByTime(60_000);

    expect(sources).toHaveLength(1);
  });

  it("arms exactly one reconnect when a permanent close reports twice", () => {
    mount();
    sources[0]!.readyState = 2;

    // A permanent close can surface as several error events (the proxy's 502
    // and the readyState transition). One pending timer must absorb them all:
    // each extra arm both leaks a timer and, when the backoff has grown, fires
    // its own reconnect later on.
    sources[0]!.emit("error");
    sources[0]!.emit("error");
    sources[0]!.emit("error");
    vi.advanceTimersByTime(60_000);

    expect(sources).toHaveLength(2);
  });

  it("does not reopen after close(), even when an error arrives later", () => {
    const stream = mount();
    sources[0]!.readyState = 2;

    // close() with no timer pending: the consumer is gone, so the error that
    // follows must not start the stream up again behind its back.
    stream.close();
    sources[0]!.emit("error");
    vi.advanceTimersByTime(60_000);

    expect(sources).toHaveLength(1);
  });

  it("re-establishes after the jittered backoff, not before it", () => {
    mount();
    sources[0]!.readyState = 2;
    sources[0]!.emit("error");

    vi.advanceTimersByTime(FIRST_BACKOFF_MS - 1);
    expect(sources).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(sources).toHaveLength(2);
  });
});

describe("connectStatusStream: the default EventSource adapter", () => {
  class StubEventSource {
    static instances: StubEventSource[] = [];
    readonly url: string;
    readyState = 0;
    closed = 0;
    readonly handlers = new Map<string, EventListener[]>();

    constructor(url: string) {
      this.url = url;
      StubEventSource.instances.push(this);
    }

    addEventListener(type: string, listener: EventListener): void {
      const list = this.handlers.get(type) ?? [];
      list.push(listener);
      this.handlers.set(type, list);
    }

    close(): void {
      this.closed++;
    }

    emit(type: string, data?: string): void {
      for (const l of this.handlers.get(type) ?? []) {
        l({ data } as unknown as Event);
      }
    }
  }

  beforeEach(() => {
    vi.useFakeTimers();
    StubEventSource.instances = [];
    vi.stubGlobal("EventSource", StubEventSource);
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("subscribes on the EventSource it constructs, at the requested path", () => {
    const onStatus = vi.fn();

    connectStatusStream("/api/sessions/events", { onStatus });
    StubEventSource.instances[0]!.emit("message", JSON.stringify(sample));

    // The default factory is the one that ships. If it registered the listener
    // on nothing, a browser consumer would see an open stream and no events.
    expect(StubEventSource.instances[0]!.url).toBe("/api/sessions/events");
    expect(onStatus).toHaveBeenCalledWith(sample);
  });

  it("closes the EventSource it constructed", () => {
    const stream = connectStatusStream("/api/sessions/events", { onStatus: vi.fn() });

    stream.close();

    // An EventSource that is never closed keeps the HTTP request open and keeps
    // reconnecting on its own for the life of the page.
    expect(StubEventSource.instances[0]!.closed).toBe(1);
  });

  it("reports the EventSource's readyState live, so a permanent close is seen", () => {
    const stream = connectStatusStream("/api/sessions/events", { onStatus: vi.fn() });
    // CLOSED after construction: a snapshot taken when the adapter was built
    // would still read CONNECTING here and the reconnect would never fire.
    StubEventSource.instances[0]!.readyState = 2;

    StubEventSource.instances[0]!.emit("error");
    vi.advanceTimersByTime(60_000);

    expect(StubEventSource.instances).toHaveLength(2);
    stream.close();
  });
});
