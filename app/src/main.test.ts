/** @vitest-environment jsdom */
/** Regression test for the "stuttering" status line: main.ts used to rebuild the whole card's innerHTML on every daemon event while an ask was running (and once a second besides, from the elapsed-time ticker), tearing down and recreating the live step row on every one of them. A CSS animation restarts from its first frame whenever the element carrying it is removed and recreated, so the shimmer and breathe never got to run a full pass — that restart, not the animations themselves, was the stutter. patchLiveSteps (see main.ts) now patches that row in place instead. This checks the fix holds: the row survives a run of daemon events as the same DOM node instead of being swapped for a fresh one. */
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { DaemonEvent } from "./daemon";

// jsdom does not implement matchMedia at all; main.ts reads prefers-reduced-motion at module load.
if (!window.matchMedia) {
  window.matchMedia = ((query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

// The daemon itself is not what this test is about: probe resolves true so the ask goes down the live path,
// and ask() never resolves, so the turn stays "asking" for as long as the test needs it to.
vi.mock("./daemon", () => ({
  setPort: vi.fn(),
  setToken: vi.fn(),
  probe: vi.fn().mockResolvedValue(true),
  events: vi.fn(() => () => {}),
  context: vi.fn().mockResolvedValue(null),
  matters: vi.fn().mockResolvedValue(null),
  ask: vi.fn().mockReturnValue(new Promise(() => {})),
  endpoint: vi.fn().mockReturnValue({ base: "", token: undefined }),
  voiceStart: vi.fn(),
  voiceStatus: vi.fn().mockResolvedValue(null),
  voiceStop: vi.fn(),
  actStart: vi.fn().mockReturnValue(new Promise(() => {})),
  actStop: vi.fn(),
  actPauseResume: vi.fn(),
  actAnswer: vi.fn(),
}));

describe("the live step row's animation survives a run of daemon events", () => {
  beforeEach(() => {
    vi.resetModules();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("keeps the same row, icon and label nodes across status and tool events", async () => {
    const { dispatch } = await import("./main");
    // Lets connect()'s mocked probe/context/matters/voiceStatus promises settle before the ask starts.
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });

    const row = document.querySelector(".steps .step.run");
    const icon = row?.querySelector(".step-ic");
    const label = row?.querySelector(".step-label");
    expect(row).toBeTruthy();
    expect(icon).toBeTruthy();
    expect(label).toBeTruthy();

    dispatch({ kind: "daemonEvent", ev: { id: "", type: "status", text: "Checking." } });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "tool", text: "observe_screen", detail: "" } });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "tool", text: "observe_screen", detail: "done" } });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "status", text: "Checking." } });

    expect(document.querySelector(".steps .step.run")).toBe(row);
    expect(row?.querySelector(".step-ic")).toBe(icon);
    expect(row?.querySelector(".step-label")).toBe(label);
    // The finished tool call got its own row, appended rather than replacing anything.
    expect(document.querySelectorAll(".steps > .step").length).toBe(2);
  });
});

describe("a computer-use job started with do:", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("shows the job as a live step list built from the same step rows an ask uses, and Stop posts to the daemon", async () => {
    const { actStart, actStop } = await import("./daemon");
    vi.mocked(actStart).mockResolvedValue("act-1");
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "do: reload the page" });
    dispatch({ kind: "enter" });
    // Lets the mocked actStart's promise resolve and jobStarted attach the id.
    await new Promise((r) => setTimeout(r, 0));

    dispatch({
      kind: "daemonEvent",
      ev: { id: "act-1", type: "act", text: "", detail: JSON.stringify({ kind: "step", state: "stepping", text: "clicking Reload", expect: "page loaded" }) },
    });
    const row = document.querySelector(".steps .step.run .step-label");
    expect(row?.textContent).toBe("clicking Reload — expecting page loaded");

    const stopBtn = document.querySelector<HTMLButtonElement>(".job-stop");
    expect(stopBtn).toBeTruthy();
    stopBtn?.click();
    expect(actStop).toHaveBeenCalledWith("act-1");
  });

  it("shows a stuck job's question, and Enter there answers it instead of asking something new", async () => {
    const { actStart, actAnswer } = await import("./daemon");
    vi.mocked(actStart).mockResolvedValue("act-1");
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "do: reload the page" });
    dispatch({ kind: "enter" });
    await new Promise((r) => setTimeout(r, 0));

    dispatch({
      kind: "daemonEvent",
      ev: { id: "act-1", type: "act", text: "", detail: JSON.stringify({ kind: "question", state: "stuck", text: "which venue?" }) },
    });
    expect(document.querySelector(".jobq")?.textContent).toBe("which venue?");
    expect(document.activeElement).toBe(document.querySelector(".q"));

    dispatch({ kind: "type", value: "the green room" });
    dispatch({ kind: "enter" });
    expect(actAnswer).toHaveBeenCalledWith("act-1", "the green room");
  });

  it("Escape asks for confirmation instead of closing a live job, and stops it on the next Escape", async () => {
    const { actStart, actStop } = await import("./daemon");
    vi.mocked(actStart).mockResolvedValue("act-1");
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "do: reload the page" });
    dispatch({ kind: "enter" });
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "escape" });
    expect(document.querySelector(".confirm")).toBeTruthy();
    expect(actStop).not.toHaveBeenCalled();

    dispatch({ kind: "escape" });
    expect(actStop).toHaveBeenCalledWith("act-1");
  });
});

describe("the raw stream event needs no cast to reach the reducer", () => {
  // connect() used to read the stream through `raw as DaemonEvent`, casting into state.ts's own separately-declared type — which agreed with daemon.ts's by hand, not by the compiler. daemon.ts now declares the one DaemonEvent both sides read, so a literal built against it, with no cast, has to type-check all the way from the events() callback through to the DOM for this test to even compile.
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("carries a real 'act' event from the stream callback to the step list, and a real 'notice' event through the same callback without throwing", async () => {
    const { events, actStart } = await import("./daemon");
    vi.mocked(actStart).mockResolvedValue("act-1");
    let onEvent: (ev: DaemonEvent) => void = () => {};
    vi.mocked(events).mockImplementation((cb) => {
      onEvent = cb;
      return () => {};
    });

    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "do: reload the page" });
    dispatch({ kind: "enter" });
    // Lets the mocked actStart's promise resolve and jobStarted attach the id, so the "act" event below matches it.
    await new Promise((r) => setTimeout(r, 0));

    const act: DaemonEvent = {
      id: "act-1",
      type: "act",
      detail: JSON.stringify({ kind: "step", state: "stepping", text: "clicking Reload" }),
    };
    onEvent(act);
    expect(document.querySelector(".steps .step.run .step-label")?.textContent).toBe("clicking Reload");

    const notice: DaemonEvent = {
      id: "",
      type: "notice",
      notice: { title: "Morning brief", body: "Nothing urgent.", place: "", id: "", kind: "brief" },
    };
    expect(() => onEvent(notice)).not.toThrow();
  });
});
