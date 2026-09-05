// @vitest-environment jsdom

/** Tests for the browser-only mock mode: which URLs turn it on, and that the fake daemon it installs actually answers the routes the window reads. */

import { afterEach, describe, expect, it, vi } from "vitest";

import { daemonFetch, demo, installMock, wantsMock } from "./mock";

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

  it("leaves fetch alone on any other URL", () => {
    const real = vi.fn();
    vi.stubGlobal("fetch", real);
    expect(installMock({ search: "" })).toBe(false);
    expect(window.fetch).toBe(real);
  });

  it("puts the fake daemon in fetch's place for a mock page", async () => {
    vi.stubGlobal("fetch", vi.fn());
    expect(installMock({ search: "?mock=1" })).toBe(true);
    const answer = await window.fetch("http://127.0.0.1:6942/conversations");
    expect((await answer.json()).conversations).toHaveLength(demo.conversations!.length);
  });
});

describe("the fake daemon", () => {
  it("answers every list the window reads on start-up", async () => {
    const fetching = daemonFetch(demo);
    for (const [path, key] of [
      ["/conversations", "conversations"],
      ["/tasks", "tasks"],
      ["/days", "days"],
      ["/meetings", "meetings"],
      ["/brains", "brains"],
    ] as const) {
      const answer = await fetching(`http://127.0.0.1:6942${path}`);
      expect(answer.status).toBe(200);
      expect(Array.isArray((await answer.json())[key])).toBe(true);
    }
  });

  it("records what was asked of it, so a test can check a click wrote what it should have", async () => {
    const calls: { method: string; path: string; body?: unknown }[] = [];
    const fetching = daemonFetch(demo, calls);
    await fetching("http://127.0.0.1:6942/tasks/11/done", { method: "POST", body: JSON.stringify({ status: "done" }) });
    expect(calls).toEqual([{ method: "POST", path: "/tasks/11/done", body: { status: "done" } }]);
  });

  it("answers 404 for a route the daemon does not have, rather than an empty 200", async () => {
    expect((await daemonFetch(demo)("http://127.0.0.1:6942/nothing-here")).status).toBe(404);
  });

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
