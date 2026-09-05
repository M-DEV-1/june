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

// The waveform itself (buildVariation, the smoothing, the braille rows) is tested in waveform.test.ts. These check the voice-mode surface built around it (see voiceSurfaceHtml in main.ts): while a session runs it replaces the input and the thread entirely, the way Gemini Live and ChatGPT's own voice mode take over the screen.
describe("live voice mode", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("replaces the input with the voice surface, and brings the input back once the stop control ends the session", async () => {
    const { voiceStop } = await import("./daemon");
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    expect(document.querySelector(".q")).toBeTruthy();
    expect(document.querySelector(".voicebox")).toBeNull();

    dispatch({ kind: "voiceOn", id: "voice-1" });

    expect(document.querySelector(".q")).toBeNull();
    expect(document.querySelector(".voicebox")).toBeTruthy();
    expect(document.querySelector(".vs-state")?.textContent).toBe("Listening");
    expect(document.querySelectorAll(".vw-spk .vw-row").length).toBe(4);
    const stopBtn = document.querySelector<HTMLButtonElement>(".vs-stop");
    expect(stopBtn).toBeTruthy();

    stopBtn?.click();
    await new Promise((r) => setTimeout(r, 0));

    expect(voiceStop).toHaveBeenCalled();
    expect(document.querySelector(".voicebox")).toBeNull();
    expect(document.querySelector(".q")).toBeTruthy();
  });

  it("shows the state word the daemon's 'state' events report", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "voiceOn", id: "voice-1" });
    dispatch({
      kind: "voiceEvent",
      ev: { id: "voice-1", type: "state", text: "thinking" },
    });
    expect(document.querySelector(".vs-state")?.textContent).toBe("Thinking");

    dispatch({
      kind: "voiceEvent",
      ev: { id: "voice-1", type: "state", text: "speaking" },
    });
    expect(document.querySelector(".vs-state")?.textContent).toBe("Speaking");
  });

  it("breathes gently while Ora is silent, and lets a real level take over once she speaks", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "voiceOn", id: "voice-1" });
    const rowText = () =>
      Array.from(document.querySelectorAll(".vw-spk .vw-row")).map(
        (el) => el.textContent ?? "",
      );
    const silentNearTop = rowText()[1];

    // A silent ("speaker": 0) level tick still moves the grid — the idle breathing — instead of leaving it flat.
    dispatch({
      kind: "voiceEvent",
      ev: {
        id: "voice-1",
        type: "level",
        detail: JSON.stringify({ mic: 0, speaker: 0 }),
      },
    });
    await new Promise((r) => requestAnimationFrame(r));
    const breathingNearTop = rowText()[1];
    expect(breathingNearTop).not.toBe(silentNearTop);

    // A real speaker reading takes over from the breathing once she actually speaks.
    dispatch({
      kind: "voiceEvent",
      ev: {
        id: "voice-1",
        type: "level",
        detail: JSON.stringify({ mic: 0, speaker: 0.8 }),
      },
    });
    await new Promise((r) => requestAnimationFrame(r));
    expect(rowText()[1]).not.toBe(breathingNearTop);
  });

  it("shows the last thing the user said and the last thing Ora said", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "voiceOn", id: "voice-1" });
    expect(document.querySelector(".vs-you")).toBeNull();
    expect(document.querySelector(".vs-ora")).toBeNull();

    dispatch({
      kind: "voiceEvent",
      ev: { id: "voice-1", type: "heard", text: "what's the weather" },
    });
    expect(document.querySelector(".vs-you")?.textContent).toBe(
      "what's the weather",
    );
    expect(document.querySelector(".vs-ora")).toBeNull();

    dispatch({
      kind: "voiceEvent",
      ev: { id: "voice-1", type: "said", text: "Sunny today." },
    });
    expect(document.querySelector(".vs-ora")?.textContent).toBe(
      "Sunny today.",
    );
  });
});

// A notice sent back with its action filled in is the desktop notification's own Done/snooze buttons reaching this window (see internal/proactive/notify.go); it draws as one line instead of the title and body a fresh notice shows, reusing the same bubble and the same .nt/.nb markup.
describe("a notice's action line replaces its title and body in the bubble", () => {
  beforeEach(() => {
    vi.resetModules();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("shows 'Done' alone for a notice dismissed from its own notification", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({
      kind: "notice",
      notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", action: "done", until: "" },
      hoverOpen: true,
    });

    const bubble = document.getElementById("n");
    expect(bubble?.querySelector(".nt")?.textContent).toBe("Done");
    expect(bubble?.querySelector(".nb")).toBeNull();
  });

  it("shows the snooze time for a notice snoozed from its own notification", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    const until = new Date();
    until.setHours(until.getHours() + 1, 0, 0, 0);
    dispatch({
      kind: "notice",
      notice: { title: "Routine", body: "Priya replied about the venue.", place: "", id: "7", kind: "routine", action: "snoozed", until: until.toISOString() },
      hoverOpen: true,
    });

    const bubble = document.getElementById("n");
    expect(bubble?.querySelector(".nt")?.textContent).toContain("Snoozed until");
    expect(bubble?.querySelector(".nb")).toBeNull();
  });
});

// C6 from the design review: the status dot told its four states apart by colour alone, with idle (a near-invisible grey) reading the same as "the dot is missing", and the daemon-down red carrying no words for a new user to go on.
describe("the status dot names its own state", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("gives the resting dot an aria-label and title once the daemon has answered", async () => {
    await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    const dot = document.querySelector(".dot");
    expect(dot?.getAttribute("aria-label")).toBe("Ready");
    expect(dot?.getAttribute("title")).toBe("Ready");
  });

  it("gives the dot a different label while a question is running", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });

    const dot = document.querySelector(".dot");
    expect(dot?.getAttribute("aria-label")).toBe("Working");
  });

  it("replaces the dot with the words 'Not connected' when the daemon does not answer its probe", async () => {
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(false);
    await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    expect(document.querySelector(".dot")).toBeNull();
    expect(document.querySelector(".in")?.textContent).toContain("Not connected");
  });
});

// Same review: the resting placeholder used to carry the two shortcut hints inline ("Space to dictate · Shift+Space for voice"), which vanished the moment there was anything else to say and were never announced any other way. They now sit permanently beside the input instead.
describe("the shortcut hints beside the input", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("shows the resting placeholder as just 'Ask Ora', with the dictate and voice hints in the context-chip slot instead", async () => {
    await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    const input = document.querySelector<HTMLInputElement>(".q");
    expect(input?.placeholder).toBe("Ask Ora");

    const ctx = document.querySelector(".ctx");
    expect(ctx?.textContent).toBe("⎵ dictate · ⇧⎵ voice");
  });
});

// A fresh notice is a card with its answers on it: the user deals with it where it appears instead of going to the app window or to a desktop banner asking the same thing.
describe("a notice card carries its own buttons", () => {
  beforeEach(() => {
    vi.resetModules();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("offers Done, the three snoozes and Open, and sends a pressed snooze to the daemon's notice route", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({
        kind: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task" },
        hoverOpen: true,
      });
      const bubble = document.getElementById("n")!;
      const acts = [...bubble.querySelectorAll<HTMLButtonElement>("button.na")].map((b) => b.dataset.act);
      expect(acts).toEqual(["done", "hour", "evening", "tomorrow", "open"]);
      expect(bubble.querySelector(".nt")?.textContent).toBe("Still open");

      bubble.querySelector<HTMLButtonElement>('button.na[data-act="hour"]')!.click();
      const call = fetchMock.mock.calls.find(([url]) => String(url).endsWith("/notices/task/42/action"));
      expect(call).toBeDefined();
      expect(call![1].method).toBe("POST");
      expect(JSON.parse(call![1].body)).toEqual({ title: "Still open", body: "Send the invoice", action: "hour" });
      expect(document.getElementById("n")?.hidden).toBe(true);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("uses '-' for a notice with no row of its own, the way the daemon's route expects", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({ kind: "notice", notice: { title: "Morning brief", body: "Nothing urgent.", place: "", id: "", kind: "brief" }, hoverOpen: true });
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="done"]')!.click();
      expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith("/notices/brief/-/action"))).toBe(true);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
