// Tests for the session status-stream client (connectStatusStream).
//
// The client is a thin EventSource wrapper, so the tests inject a fake
// EventSourceLike via the `make` parameter and drive events by hand. Behaviors
// tested:
// 1. A well-formed "message" event is parsed and fanned out to onStatus with
//    every field (id/status/title/createdAt/removed) preserved.
// 2. A malformed "message" frame (invalid JSON) is skipped without throwing and
//    without calling onStatus; a later valid frame still fans out.
// 3. onOpen fires on every "open" event (initial connect and each reconnect),
//    so the consumer can resync after a gap.
// 4. onError fires on an "error" event.
// 5. close() closes the underlying EventSource exactly once.
// 6. Optional callbacks absent: the module ALWAYS registers both the OPEN
//    listener (to reset reconnect backoff on every successful reopen) and the
//    ERROR listener (to drive its own reconnect); only the onOpen callback
//    inside the open listener is gated. A stray event does not throw.
// 7. On a permanent close (readyState CLOSED) the module re-establishes the
//    stream after a capped backoff, fires onOpen on the reopen, and close()
//    cancels a pending reconnect.
// 8. OSC 9 wire floor (per the OSC 9 spec brief): the "failed" (progress state
//    2) and "warning" (state 4) statuses parse, the progress percentage is
//    carried with -1 meaning absent, a notification is delivered as event data
//    rather than as a status, and an UNKNOWN status string from a newer server is
//    forwarded rather than dropped or thrown on.
// 9. The session-end split: "exited" (an ordinary end) and "crashed" (a non-zero
//    or signalled exit) are both admitted by the status union and both parse.

import { describe, it, expect, vi } from "vitest";

import { connectStatusStream, type EventSourceLike, type SessionStatus } from "./status-stream.js";

// FakeEventSource records registered listeners and lets a test dispatch events
// synchronously. It mirrors only the surface connectStatusStream uses.
class FakeEventSource implements EventSourceLike {
  readonly url: string;
  closed = 0;
  readyState = 0; // 0 CONNECTING, 1 OPEN, 2 CLOSED; a test sets 2 to drive reconnect
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

  // emit dispatches a MessageEvent-shaped payload to every listener for `type`.
  emit(type: string, data?: string): void {
    for (const l of this.listeners.get(type) ?? []) {
      l({ data } as MessageEvent);
    }
  }

  hasListener(type: string): boolean {
    return (this.listeners.get(type)?.length ?? 0) > 0;
  }
}

// mountFake wires a FakeEventSource into connectStatusStream and returns both so
// a test can drive events and assert on the wrapper.
function mountFake(cb: Parameters<typeof connectStatusStream>[1]) {
  let fake: FakeEventSource | undefined;
  const stream = connectStatusStream("/api/sessions/events", cb, (url) => {
    fake = new FakeEventSource(url);
    return fake;
  });
  if (!fake) {
    throw new Error("factory not invoked");
  }
  return { fake, stream };
}

const sample: SessionStatus = {
  id: "abc123",
  status: "input",
  title: "kiro-cli",
  createdAt: "2026-07-01T12:00:00Z",
  removed: false,
};

describe("connectStatusStream", () => {
  it("parses a well-formed message and fans out every field", () => {
    const onStatus = vi.fn();
    const { fake } = mountFake({ onStatus });

    fake.emit("message", JSON.stringify(sample));

    expect(onStatus).toHaveBeenCalledTimes(1);
    expect(onStatus).toHaveBeenCalledWith(sample);
  });

  it("passes the factory the requested path", () => {
    const { fake } = mountFake({ onStatus: vi.fn() });
    expect(fake.url).toBe("/api/sessions/events");
  });

  it("skips a malformed frame without throwing, then delivers the next valid one", () => {
    const onStatus = vi.fn();
    const { fake } = mountFake({ onStatus });

    expect(() => fake.emit("message", "{not json")).not.toThrow();
    expect(onStatus).not.toHaveBeenCalled();

    fake.emit("message", JSON.stringify(sample));
    expect(onStatus).toHaveBeenCalledTimes(1);
    expect(onStatus).toHaveBeenCalledWith(sample);
  });

  it("fires onOpen on every open event (connect and each reconnect)", () => {
    const onOpen = vi.fn();
    const { fake } = mountFake({ onStatus: vi.fn(), onOpen });

    fake.emit("open");
    fake.emit("open");

    expect(onOpen).toHaveBeenCalledTimes(2);
  });

  it("fires onError on an error event", () => {
    const onError = vi.fn();
    const { fake } = mountFake({ onStatus: vi.fn(), onError });

    fake.emit("error");

    expect(onError).toHaveBeenCalledTimes(1);
  });

  it("close() closes the underlying EventSource", () => {
    const { fake, stream } = mountFake({ onStatus: vi.fn() });

    stream.close();

    expect(fake.closed).toBe(1);
  });

  it("always registers open+error listeners; gates only the onOpen callback", () => {
    const { fake } = mountFake({ onStatus: vi.fn() });

    // The open listener is ALWAYS registered so it can reset the reconnect
    // backoff on every successful (re)open; only the onOpen callback inside it
    // is gated on the consumer supplying one.
    expect(fake.hasListener("open")).toBe(true);
    // The module always listens for errors so it can re-establish a permanent
    // close even when the consumer supplies no onError callback.
    expect(fake.hasListener("error")).toBe(true);
    // A stray open/error event must not throw with no consumer callback set;
    // readyState stays CONNECTING (0), so no reconnect is scheduled either.
    expect(() => fake.emit("open")).not.toThrow();
    expect(() => fake.emit("error")).not.toThrow();
  });

  it("re-establishes the stream on a permanent close and cancels reconnect on close()", () => {
    vi.useFakeTimers();
    // The reconnect delay is now jittered (base + Math.random()*250, matching
    // the WS reconnect). Pin Math.random to 0.5 so the first attempt's delay is
    // exactly 500 + 0.5*250 = 625ms and the test stays deterministic.
    const randomSpy = vi.spyOn(Math, "random").mockReturnValue(0.5);
    const onOpen = vi.fn();
    const fakes: FakeEventSource[] = [];
    const stream = connectStatusStream(
      "/api/sessions/events",
      { onStatus: vi.fn(), onOpen },
      (url) => {
        const f = new FakeEventSource(url);
        fakes.push(f);
        return f;
      },
    );

    // Initial connect opens exactly one source.
    expect(fakes).toHaveLength(1);
    fakes[0]!.emit("open");
    expect(onOpen).toHaveBeenCalledTimes(1);

    // A permanent close (readyState CLOSED) schedules a reconnect after the
    // jittered backoff (625ms with Math.random pinned to 0.5).
    fakes[0]!.readyState = 2;
    fakes[0]!.emit("error");
    expect(fakes).toHaveLength(1); // not yet: waits for the backoff delay
    vi.advanceTimersByTime(625);
    expect(fakes).toHaveLength(2); // re-established

    // The reopen fires onOpen again so the consumer resyncs after the gap.
    fakes[1]!.emit("open");
    expect(onOpen).toHaveBeenCalledTimes(2);

    // close() cancels any pending reconnect and does not reopen.
    fakes[1]!.readyState = 2;
    fakes[1]!.emit("error");
    stream.close();
    vi.advanceTimersByTime(10_000);
    expect(fakes).toHaveLength(2);
    expect(fakes[1]!.closed).toBe(1);

    randomSpy.mockRestore();
    vi.useRealTimers();
  });

  // OSC 9 wire-floor cases, written against the OSC 9 spec brief.
  //
  // Two things the brief makes the client's problem. (1) Progress state 2 is an
  // error and state 4 is a warning (iTerm2 semantics), so "failed" and "warning"
  // are statuses the server can send and the union has to admit; the percentage
  // rides along, with -1 meaning absent (which is NOT 0%). (2) A notification is
  // an EVENT, so it arrives as event data rather than as a status, and a consumer
  // with no server-side classifier still sees it.
  describe("OSC 9 progress and notification fields", () => {
    const cases: { name: string; status: SessionStatus["status"]; progressValue: number }[] = [
      { name: "failed with a percentage", status: "failed", progressValue: 75 },
      { name: "failed indeterminate", status: "failed", progressValue: -1 },
      { name: "warning with a percentage", status: "warning", progressValue: 25 },
      { name: "working with a percentage", status: "working", progressValue: 50 },
      { name: "idle with no percentage", status: "idle", progressValue: -1 },
    ];
    for (const tc of cases) {
      it(`parses ${tc.name}`, () => {
        const onStatus = vi.fn();
        const { fake } = mountFake({ onStatus });
        const ev: SessionStatus = { ...sample, status: tc.status, progressValue: tc.progressValue };

        fake.emit("message", JSON.stringify(ev));

        expect(onStatus).toHaveBeenCalledTimes(1);
        expect(onStatus).toHaveBeenCalledWith(ev);
        const got = onStatus.mock.calls[0]![0] as SessionStatus;
        expect(got.status).toBe(tc.status);
        expect(got.progressValue).toBe(tc.progressValue);
      });
    }

    it("delivers a notification and its sequence without it being a status", () => {
      const onStatus = vi.fn();
      const { fake } = mountFake({ onStatus });
      const ev: SessionStatus = {
        ...sample,
        status: "idle",
        notification: "Response complete",
        notificationSeq: 7,
        progressValue: -1,
      };

      fake.emit("message", JSON.stringify(ev));

      const got = onStatus.mock.calls[0]![0] as SessionStatus;
      expect(got.notification).toBe("Response complete");
      expect(got.notificationSeq).toBe(7);
      // An event, not a state: the status is whatever the session's state is.
      expect(got.status).toBe("idle");
    });

    it("forwards an UNKNOWN status from a newer server without throwing", () => {
      const onStatus = vi.fn();
      const { fake } = mountFake({ onStatus });
      // A server one release ahead adds a status this client has never heard of.
      // The stream is parsed, not validated: dropping the frame would lose the
      // session's title and removal signal too, so the value is forwarded as-is
      // and the consumer falls back on its own default rendering.
      const raw = JSON.stringify({
        id: "abc123",
        status: "throttled",
        title: "kiro-cli",
        createdAt: "2026-07-01T12:00:00Z",
        progressValue: 40,
        futureField: { nested: true },
      });

      expect(() => {
        fake.emit("message", raw);
      }).not.toThrow();

      expect(onStatus).toHaveBeenCalledTimes(1);
      const got = onStatus.mock.calls[0]![0] as SessionStatus;
      expect(got.status).toBe("throttled");
      expect(got.id).toBe("abc123");
      expect(got.progressValue).toBe(40);
    });
  });

  // The session-end split. The server reports "exited" for an ordinary end —
  // status 0, or any exit the server itself caused (a closed session, the idle
  // reaper, a shutdown) — and "crashed" only for a non-zero or signalled exit,
  // so a routine restart never arrives as a failure. Both are terminal: no
  // progress state or notification latch outranks them.
  describe("session-end statuses", () => {
    const cases: SessionStatus["status"][] = ["exited", "crashed"];
    for (const status of cases) {
      it(`accepts and parses "${status}"`, () => {
        const onStatus = vi.fn();
        const { fake } = mountFake({ onStatus });
        // Typed as SessionStatus, so this is also the type-level assertion that
        // the union admits the value: tsc fails the build if it does not.
        const ev: SessionStatus = { ...sample, status, progressValue: -1 };

        fake.emit("message", JSON.stringify(ev));

        expect(onStatus).toHaveBeenCalledTimes(1);
        const got = onStatus.mock.calls[0]![0] as SessionStatus;
        expect(got.status).toBe(status);
      });
    }

    it("carries a crashed session's removal marker like any other status", () => {
      const onStatus = vi.fn();
      const { fake } = mountFake({ onStatus });
      const ev: SessionStatus = { ...sample, status: "crashed", removed: true };

      fake.emit("message", JSON.stringify(ev));

      const got = onStatus.mock.calls[0]![0] as SessionStatus;
      expect(got.status).toBe("crashed");
      expect(got.removed).toBe(true);
    });
  });
});
