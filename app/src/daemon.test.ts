import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { actStart, ask, context, devToken, events, setToken } from "./daemon";

/** Minimal fake EventSource: records the URL each stream was opened with. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }
  close(): void {}
}

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  setToken(undefined);
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

/** Answers every fetch with one queued response and records what was asked. Input: the responses in order. Output: the calls the code under test made. */
function fakeFetch(...responses: { status: number; body?: unknown }[]) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    const r = responses.shift() ?? { status: 200, body: {} };
    return { ok: r.status >= 200 && r.status < 300, status: r.status, json: async () => r.body } as Response;
  }));
  return calls;
}

describe("ask", () => {
  it("names the conversation to append to when it is given one, which is what makes a question a follow-up", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "abc124", conversation_id: "70" }) });
    vi.stubGlobal("fetch", fetchMock);

    await ask("again", "", "70");

    const [, opts] = fetchMock.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ question: "again", context: "", conversation_id: "70" });
    vi.unstubAllGlobals();
  });

  // react-doctor's no-fetch-response-used-without-status-check: every other call in this file already checks res.ok before parsing the body; ask() did not, so a 4xx/5xx was parsed as if it were {id, conversation_id} instead of failing like the daemon-unreachable case its own caller already handles.
  it("throws instead of parsing an error body as a success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 500, json: async () => ({ error: "boom" }) });
    vi.stubGlobal("fetch", fetchMock);
    await expect(ask("q", "ctx")).rejects.toThrow();
    vi.unstubAllGlobals();
  });
});

describe("token", () => {
  it("sends the token as a header on a request and as a query param on the stream, which cannot carry headers", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "x" }) });
    vi.stubGlobal("fetch", fetchMock);
    setToken("secret123");
    await ask("q", "ctx");
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["X-June-Token"]).toBe("secret123");

    events(vi.fn());
    await vi.advanceTimersByTimeAsync(0);
    expect(FakeEventSource.instances[0].url).toBe("http://127.0.0.1:6942/events?token=secret123");
    vi.unstubAllGlobals();
  });
});

// /context reads the focused window through AT-SPI, which is the one daemon route that routinely blocks for seconds. The hover shows before these three land (see connect in main.ts), so a wedged read costs a stale chip rather than a window that never appears — but only if the read is actually given up on.
describe("the card's screen read has a deadline", () => {
  /** A fetch that never answers unless its abort signal fires, which is what a wedged AT-SPI read looks like from here. Input: none. Output: the fake. */
  const wedged = () =>
    vi.fn(
      (_url: string, init: { signal: AbortSignal }) =>
        new Promise((_resolve, reject) => {
          init.signal.addEventListener("abort", () => reject(new Error("aborted")));
        }),
    );

  it("gives up on a /context that never answers", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", wedged());
    try {
      const pending = context();
      await vi.advanceTimersByTimeAsync(3000);
      await expect(pending).resolves.toBeNull();
    } finally {
      vi.useRealTimers();
      vi.unstubAllGlobals();
    }
  });
});

describe("act", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("actStart posts the goal and returns the job id", async () => {
    setToken("secret");
    const calls = fakeFetch({ status: 202, body: { id: "act-1" } });
    expect(await actStart("book the venue")).toBe("act-1");
    expect(calls[0].url).toBe("http://127.0.0.1:6942/act");
    expect(calls[0].init?.method).toBe("POST");
    expect(JSON.parse(calls[0].init?.body as string)).toEqual({ goal: "book the venue" });
    expect((calls[0].init?.headers as Record<string, string>)["X-June-Token"]).toBe("secret");
  });
});

describe("the dev-only token in the URL", () => {
  it("is refused on any other origin, so a packaged window can never be handed one", () => {
    expect(devToken({ port: "", search: "?token=abc123" })).toBeUndefined();
    expect(devToken({ port: "8080", search: "?token=abc123" })).toBeUndefined();
  });
});
