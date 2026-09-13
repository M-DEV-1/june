import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { actAnswer, actPauseResume, actStart, actStop, ask, context, devToken, endpoint, events, matters, probe, setEventSourceCtor, setPort, setToken, voiceStart, voiceStatus, voiceStop } from "./daemon";

/** Minimal fake EventSource: tests trigger messages/errors by calling the instance's own methods. */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
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
  setEventSourceCtor(FakeEventSource as unknown as new (url: string) => EventSource);
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
  it("posts the question and context, and hands back the ids the daemon answered with", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "abc123", conversation_id: "70" }) });
    vi.stubGlobal("fetch", fetchMock);

    const res = await ask("is the venue sorted?", "Mail · Vexil Zelbrak");

    expect(res).toEqual({ id: "abc123", conversationId: "70" });
    expect(fetchMock).toHaveBeenCalledWith(
      "http://127.0.0.1:6942/ask",
      expect.objectContaining({
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question: "is the venue sorted?", context: "Mail · Vexil Zelbrak", conversation_id: "" }),
      }),
    );
    vi.unstubAllGlobals();
  });

  it("names the conversation to append to when it is given one, which is what makes a question a follow-up", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "abc124", conversation_id: "70" }) });
    vi.stubGlobal("fetch", fetchMock);

    await ask("again", "", "70");

    const [, opts] = fetchMock.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ question: "again", context: "", conversation_id: "70" });
    vi.unstubAllGlobals();
  });

  // An older daemon answers with the turn id alone, and a hover that read undefined as a conversation would then name "undefined" on the next ask.
  it("reads a missing conversation as none rather than as one", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "abc125" }) });
    vi.stubGlobal("fetch", fetchMock);
    expect(await ask("q", "ctx")).toEqual({ id: "abc125", conversationId: "" });
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

describe("events", () => {
  it("parses messages and forwards them", () => {
    const onEvent = vi.fn();
    events(onEvent);
    const src = FakeEventSource.instances[0];
    src.emit({ id: "1", type: "status", text: "reading mail" });
    expect(onEvent).toHaveBeenCalledWith({ id: "1", type: "status", text: "reading mail" });
  });

  it("ignores a malformed message", () => {
    const onEvent = vi.fn();
    events(onEvent);
    const src = FakeEventSource.instances[0];
    src.emitRaw("{not json");
    expect(onEvent).not.toHaveBeenCalled();
  });

  it("stop closes the source", () => {
    const stop = events(vi.fn());
    const src = FakeEventSource.instances[0];
    stop();
    expect(src.closed).toBe(true);
  });

  it("reconnects with a 2s backoff after the stream drops", () => {
    events(vi.fn());
    const first = FakeEventSource.instances[0];
    first.fail();
    expect(first.closed).toBe(true);
    expect(FakeEventSource.instances.length).toBe(1);

    vi.advanceTimersByTime(2000);
    expect(FakeEventSource.instances.length).toBe(2);
  });

  it("does not reconnect after stop", () => {
    const stop = events(vi.fn());
    const first = FakeEventSource.instances[0];
    stop();
    first.fail();
    vi.advanceTimersByTime(2000);
    expect(FakeEventSource.instances.length).toBe(1);
  });
});

describe("token", () => {
  it("ask sends no token header when none is set", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "x" }) });
    vi.stubGlobal("fetch", fetchMock);
    await ask("q", "ctx");
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["X-Ora-Token"]).toBeUndefined();
    vi.unstubAllGlobals();
  });

  it("ask sends the token header once set", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: "x" }) });
    vi.stubGlobal("fetch", fetchMock);
    setToken("secret123");
    await ask("q", "ctx");
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["X-Ora-Token"]).toBe("secret123");
    vi.unstubAllGlobals();
  });

  it("probe sends the token header once set", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);
    setToken("secret123");
    await probe();
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["X-Ora-Token"]).toBe("secret123");
    vi.unstubAllGlobals();
  });

  it("events puts the token in the stream URL as a query param", () => {
    setToken("secret123");
    events(vi.fn());
    const src = FakeEventSource.instances[0];
    expect(src.url).toBe("http://127.0.0.1:6942/events?token=secret123");
  });

  it("events omits the token query param when none is set", () => {
    events(vi.fn());
    const src = FakeEventSource.instances[0];
    expect(src.url).toBe("http://127.0.0.1:6942/events");
  });
});

describe("probe", () => {
  it("returns true when /status answers ok", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true }));
    await expect(probe()).resolves.toBe(true);
    vi.unstubAllGlobals();
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("no daemon")));
    await expect(probe()).resolves.toBe(false);
    vi.unstubAllGlobals();
  });
});

describe("context", () => {
  it("returns the parsed context on success", async () => {
    const body = { app: "Mail", title: "Re: venue", text: "hi" };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => body }));
    await expect(context()).resolves.toEqual(body);
    vi.unstubAllGlobals();
  });

  it("returns null when the response is not ok", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, json: async () => ({}) }));
    await expect(context()).resolves.toBeNull();
    vi.unstubAllGlobals();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("no daemon")));
    await expect(context()).resolves.toBeNull();
    vi.unstubAllGlobals();
  });

  it("sends the token header once set", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ app: "", title: "", text: "" }) });
    vi.stubGlobal("fetch", fetchMock);
    setToken("secret123");
    await context();
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["X-Ora-Token"]).toBe("secret123");
    vi.unstubAllGlobals();
  });
});

// /context reads the focused window through AT-SPI, which is the one daemon route that routinely blocks for seconds. The hover shows before these three land (see connect in main.ts), so a wedged read costs a stale chip rather than a window that never appears — but only if the read is actually given up on.
describe("the three card reads have a deadline", () => {
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

  it("gives up on a /matters and a /voice/status that never answer", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", wedged());
    try {
      const rows = matters();
      const live = voiceStatus();
      await vi.advanceTimersByTimeAsync(3000);
      await expect(rows).resolves.toBeNull();
      await expect(live).resolves.toBeNull();
    } finally {
      vi.useRealTimers();
      vi.unstubAllGlobals();
    }
  });
});

describe("matters", () => {
  it("returns the matters array on success", async () => {
    const rows = [{ id: "1", title: "a", kind: "action", status: "open", when: "2026-09-04T09:00:00Z", detail: "" }];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ matters: rows }) }));
    await expect(matters()).resolves.toEqual(rows);
    vi.unstubAllGlobals();
  });

  it("returns null when the response is not ok", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, json: async () => ({}) }));
    await expect(matters()).resolves.toBeNull();
    vi.unstubAllGlobals();
  });

  it("returns null when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("no daemon")));
    await expect(matters()).resolves.toBeNull();
    vi.unstubAllGlobals();
  });
});

describe("voice", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("voiceStart posts and returns the session id", async () => {
    setToken("secret");
    const calls = fakeFetch({ status: 202, body: { id: "voice-2" } });
    expect(await voiceStart()).toBe("voice-2");
    expect(calls[0].url).toBe("http://127.0.0.1:6942/voice/start");
    expect((calls[0].init?.headers as Record<string, string>)["X-Ora-Token"]).toBe("secret");
  });

  it("voiceStart gives nothing back when a session is already running", async () => {
    fakeFetch({ status: 409 });
    expect(await voiceStart()).toBeNull();
  });

  it("voiceStop posts to /voice/stop", async () => {
    const calls = fakeFetch({ status: 200, body: { active: false } });
    await voiceStop();
    expect(calls[0].url).toBe("http://127.0.0.1:6942/voice/stop");
    expect(calls[0].init?.method).toBe("POST");
  });

  it("voiceStatus reports the session the daemon already has open", async () => {
    fakeFetch({ status: 200, body: { active: true, id: "voice-1", state: "speaking" } });
    expect(await voiceStatus()).toEqual({ active: true, id: "voice-1", state: "speaking" });
  });

  it("voiceStatus is null when the daemon cannot be reached", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("offline"); }));
    expect(await voiceStatus()).toBeNull();
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
    expect((calls[0].init?.headers as Record<string, string>)["X-Ora-Token"]).toBe("secret");
  });

  it("actStart gives nothing back when the daemon refuses", async () => {
    fakeFetch({ status: 400 });
    expect(await actStart("")).toBeNull();
  });

  it("actStart gives nothing back when the daemon cannot be reached", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("offline"); }));
    expect(await actStart("book the venue")).toBeNull();
  });

  it("actStop posts to /act/{id}/stop", async () => {
    const calls = fakeFetch({ status: 204 });
    await actStop("act-1");
    expect(calls[0].url).toBe("http://127.0.0.1:6942/act/act-1/stop");
    expect(calls[0].init?.method).toBe("POST");
  });

  it("actPauseResume posts to /pause when told to pause", async () => {
    const calls = fakeFetch({ status: 204 });
    await actPauseResume("act-1", true);
    expect(calls[0].url).toBe("http://127.0.0.1:6942/act/act-1/pause");
  });

  it("actPauseResume posts to /resume when told to carry on", async () => {
    const calls = fakeFetch({ status: 204 });
    await actPauseResume("act-1", false);
    expect(calls[0].url).toBe("http://127.0.0.1:6942/act/act-1/resume");
  });

  it("actAnswer posts the text to /act/{id}/answer", async () => {
    const calls = fakeFetch({ status: 204 });
    await actAnswer("act-1", "the green room");
    expect(calls[0].url).toBe("http://127.0.0.1:6942/act/act-1/answer");
    expect(JSON.parse(calls[0].init?.body as string)).toEqual({ text: "the green room" });
  });

  it("actStop, actPauseResume and actAnswer do nothing when the daemon cannot be reached", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("offline"); }));
    await expect(actStop("act-1")).resolves.toBeUndefined();
    await expect(actPauseResume("act-1", true)).resolves.toBeUndefined();
    await expect(actAnswer("act-1", "x")).resolves.toBeUndefined();
  });
});

describe("endpoint", () => {
  it("hands out the base URL and token the dictation calls need", () => {
    setPort("7000");
    setToken("secret");
    expect(endpoint()).toEqual({ base: "http://127.0.0.1:7000", token: "secret" });
    setPort("6942");
  });
});

describe("the dev-only token in the URL", () => {
  it("is taken on the Vite dev server, so a browser tab can read live data", () => {
    expect(devToken({ port: "1420", search: "?token=abc123&screen=days" })).toBe("abc123");
  });
  it("is refused on any other origin, so a packaged window can never be handed one", () => {
    expect(devToken({ port: "", search: "?token=abc123" })).toBeUndefined();
    expect(devToken({ port: "8080", search: "?token=abc123" })).toBeUndefined();
  });
  it("is undefined when the dev server was opened without one", () => {
    expect(devToken({ port: "1420", search: "?screen=days" })).toBeUndefined();
  });
});
