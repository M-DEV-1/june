// @vitest-environment jsdom

/** Tests for the browser-only mock mode: which URLs turn it on, and that the fake daemon keeps concurrent status changes apart. */

import { afterEach, describe, expect, it, vi } from "vitest";

import { daemonFetch, demo, wantsMock } from "./mock";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("when the mock mode turns on", () => {
  it("takes only ?mock=1, so no real window is ever taken off the real daemon by accident", () => {
    expect(wantsMock("?mock=1")).toBe(true);
    expect(wantsMock("?token=abc&mock=1")).toBe(true);
    expect(wantsMock("")).toBe(false);
    expect(wantsMock("?mock=0")).toBe(false);
    expect(wantsMock("?mock=true")).toBe(false);
    expect(wantsMock("?mocked=1")).toBe(false);
  });
});

describe("the fake daemon", () => {
  it("applies each concurrent done request's own status, even when a later request's body finishes reading before an earlier one's", async () => {
    // The fake daemon only needs "url", "method" and clone().text() off what it's handed (see the RTK-Query comment above daemonFetch's returned function) — a real Request object gives no way to control exactly when its body finishes reading, so this hand-rolled stand-in does, to force B's body to resolve before A's the way a genuinely larger or slower request body could.
    let resolveA: (body: string) => void = () => {};
    const bodyA = new Promise<string>((resolve) => {
      resolveA = resolve;
    });
    const reqA = { url: "http://127.0.0.1:6942/tasks/11/done", method: "POST", clone: () => ({ text: () => bodyA }) };
    const reqB = {
      url: "http://127.0.0.1:6942/tasks/12/done",
      method: "POST",
      clone: () => ({ text: () => Promise.resolve(JSON.stringify({ status: "dropped" })) }),
    };

    const fetching = daemonFetch(demo);
    const pA = fetching(reqA as unknown as Request);
    const pB = fetching(reqB as unknown as Request);
    await pB; // B's body resolves immediately and finishes its whole request first, well before A's does.
    resolveA(JSON.stringify({ status: "done" }));
    await pA;

    const tasksAnswer = (await (await fetching("http://127.0.0.1:6942/tasks")).json()) as { tasks: { id: string; done: boolean }[] };
    expect(tasksAnswer.tasks.some((t) => t.id === "12")).toBe(false); // dropped
    expect(tasksAnswer.tasks.find((t) => t.id === "11")?.done).toBe(true); // must reflect A's own "done", not B's "dropped"
  });
});
