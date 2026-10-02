/** Tests for events(), api.ts's SSE connection. The global EventSource is stubbed directly, the same way store.test.ts's "the stream coming back" suite already does it. invoke() is mocked to reject, the "not inside Tauri" branch refreshToken() falls back from, and location is stubbed with no dev token, so every connect() reads no token and the stream URL carries none. */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@tauri-apps/api/core", () => ({ invoke: vi.fn().mockRejectedValue(new Error("not in Tauri")) }));

import type { DaemonEvent } from "./api";
import { events } from "./api";

/** Minimal fake EventSource: tests drive it by calling onopen/onmessage/onerror themselves, plus onopen, which openStream also assigns. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }
  emit(data: unknown): void {
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent);
  }
  emitRaw(data: string): void {
    this.onmessage?.({ data } as MessageEvent);
  }
  fail(): void {
    this.onerror?.();
  }
  close(): void {
    this.closed = true;
  }
}

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  // No DOM here; devToken(location) needs a location object, and a port other than 1420 keeps it token-less.
  vi.stubGlobal("location", { port: "", search: "" });
  vi.useFakeTimers();
  // The retry delay carries up to a second of jitter; pinning random at 0 makes it exactly two seconds for every test that only needs the retry to happen.
  vi.spyOn(Math, "random").mockReturnValue(0);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** connect() awaits ensureToken() before constructing the EventSource, so every test gives that microtask a tick before touching FakeEventSource.instances. */
async function settle(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
}

describe("events", () => {
  it("opens no new stream after stop(), whether the retry was already scheduled or the stream errors afterwards", async () => {
    // The timer is already scheduled by the time stop() runs, so what saves this is connect()'s own check on the way back in.
    const stop = events(vi.fn());
    await settle();
    FakeEventSource.instances[0].fail();
    stop();
    await vi.advanceTimersByTimeAsync(2000);
    expect(FakeEventSource.instances.length).toBe(1);

    // A stream closed by stop() can still report the error of its own teardown; a timer scheduled then keeps a stopped stream reconnecting on a two-second beat for the life of the page.
    const stop2 = events(vi.fn());
    await settle();
    const src = FakeEventSource.instances[1];
    stop2();
    src.fail();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("parses and forwards each message, and ignores a malformed one without closing the stream", async () => {
    const onEvent = vi.fn();
    events(onEvent);
    await settle();
    const src = FakeEventSource.instances[0];

    const ev: DaemonEvent = { id: "1", type: "status", text: "reading mail" };
    src.emit(ev);
    expect(onEvent).toHaveBeenCalledWith(ev);

    src.emitRaw("{not json");
    expect(onEvent).toHaveBeenCalledTimes(1);
    expect(src.closed).toBe(false);
  });
});
