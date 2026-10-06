import { afterEach, describe, expect, it, vi } from "vitest";
import { DictationRefused, stopDictation } from "./daemon";
import { dictationKey } from "./dictate";

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

describe("stopDictation", () => {
  it("throws when whisper failed", async () => {
    fakeFetch({ status: 500 });
    await expect(stopDictation("dictate-3")).rejects.toBeInstanceOf(DictationRefused);
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
});
