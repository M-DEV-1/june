import { afterEach, describe, expect, it, vi } from "vitest";
import { dictationKey, startDictation, stopDictation } from "./dictate";

/** Records every fetch and answers with a queued response. Input: the responses to hand out in order. Output: the fake, with the calls it saw. */
function fakeFetch(...responses: { status: number; body?: unknown }[]) {
  const calls: { url: string; init: RequestInit }[] = [];
  const fn = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, init });
    const r = responses.shift() ?? { status: 200, body: {} };
    return {
      ok: r.status >= 200 && r.status < 300,
      status: r.status,
      json: async () => r.body,
    } as Response;
  });
  vi.stubGlobal("fetch", fn);
  return calls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("startDictation", () => {
  it("posts to /dictate/start with the token and returns the id", async () => {
    const calls = fakeFetch({ status: 202, body: { id: "dictate-3" } });
    expect(await startDictation("http://d", "secret")).toBe("dictate-3");
    expect(calls[0].url).toBe("http://d/dictate/start");
    expect(calls[0].init.method).toBe("POST");
    expect((calls[0].init.headers as Record<string, string>)["X-Ora-Token"]).toBe("secret");
  });

  it("sends no token header when there is no token", async () => {
    const calls = fakeFetch({ status: 202, body: { id: "dictate-1" } });
    await startDictation("http://d", undefined);
    expect((calls[0].init.headers as Record<string, string>)["X-Ora-Token"]).toBeUndefined();
  });

  it("throws when the daemon refuses", async () => {
    fakeFetch({ status: 500 });
    await expect(startDictation("http://d", "secret")).rejects.toThrow(/500/);
  });
});

describe("stopDictation", () => {
  it("posts the id and returns the trimmed text", async () => {
    const calls = fakeFetch({ status: 200, body: { text: "  send this thought  " } });
    expect(await stopDictation("http://d", "secret", "dictate-3")).toBe("send this thought");
    expect(calls[0].url).toBe("http://d/dictate/stop");
    expect(JSON.parse(calls[0].init.body as string)).toEqual({ id: "dictate-3" });
  });

  it("returns nothing when the daemon has no such dictation open", async () => {
    fakeFetch({ status: 404 });
    expect(await stopDictation("http://d", "secret", "dictate-9")).toBe("");
  });

  it("throws when whisper failed", async () => {
    fakeFetch({ status: 500 });
    await expect(stopDictation("http://d", "secret", "dictate-3")).rejects.toThrow(/500/);
  });
});

describe("dictationKey", () => {
  /** Builds the part of a key event dictationKey reads. Input: the key and any flags. Output: the event stand-in. */
  function key(k: string, flags: Record<string, boolean> = {}) {
    return { key: k, ...flags };
  }

  it("starts on the space bar while the input is empty", () => {
    expect(dictationKey(key(" "), true, false)).toBe("start");
  });

  it("leaves the space bar alone once there is text to type into", () => {
    expect(dictationKey(key(" "), false, false)).toBe("");
  });

  it("stops on the space bar, on enter and on escape while it is listening", () => {
    expect(dictationKey(key(" "), true, true)).toBe("stop");
    expect(dictationKey(key("Enter"), true, true)).toBe("stop");
    expect(dictationKey(key("Escape"), true, true)).toBe("stop");
  });

  it("stops on the space bar even with text already in the input", () => {
    expect(dictationKey(key(" "), false, true)).toBe("stop");
  });

  it("ignores auto-repeat, so a key held down starts one recording and does not toggle it off", () => {
    expect(dictationKey(key(" ", { repeat: true }), true, false)).toBe("");
    expect(dictationKey(key(" ", { repeat: true }), true, true)).toBe("");
  });

  it("leaves the key to whoever bound it with a modifier", () => {
    expect(dictationKey(key(" ", { shiftKey: true }), true, false)).toBe("");
    expect(dictationKey(key(" ", { ctrlKey: true }), true, true)).toBe("");
  });

  it("says nothing about enter and escape while no dictation is running", () => {
    expect(dictationKey(key("Enter"), true, false)).toBe("");
    expect(dictationKey(key("Escape"), false, false)).toBe("");
  });

  it("ignores every other key", () => {
    expect(dictationKey(key("a"), true, false)).toBe("");
    expect(dictationKey(key("a"), true, true)).toBe("");
  });

  // A stop the daemon has not answered yet is still a dictation as far as the view is concerned, and a second stop 404s and comes back with "" — which is then taken as the transcript, so the words the first stop is still waiting for are dropped as a duplicate.
  it("says nothing while a stop is already on its way to the daemon", () => {
    expect(dictationKey(key(" "), true, true, true)).toBe("");
    expect(dictationKey(key("Enter"), true, true, true)).toBe("");
    expect(dictationKey(key("Escape"), true, true, true)).toBe("");
  });
});
