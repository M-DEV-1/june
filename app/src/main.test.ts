/** @vitest-environment jsdom */
/** Regression test for the "stuttering" status line: main.ts used to rebuild the whole card's innerHTML on every daemon event while an ask was running (and once a second besides, from the elapsed-time ticker), tearing down and recreating the live step row on every one of them. A CSS animation restarts from its first frame whenever the element carrying it is removed and recreated, so the shimmer and breathe never got to run a full pass — that restart, not the animations themselves, was the stutter. patchLiveSteps (see main.ts) now patches that row in place instead. This checks the fix holds: the row survives a run of daemon events as the same DOM node instead of being swapped for a fresh one. */
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { LogicalSize, PhysicalPosition, PhysicalSize } from "@tauri-apps/api/dpi";
import type { DaemonEvent } from "./daemon";
import type { Desktop, MonitorLike, WinLike } from "./winplace";

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
      // The card stays up until the daemon's own follow-up event replaces it: a snooze the daemon refuses must not look like it took.
      expect(document.getElementById("n")?.hidden).toBe(false);
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

// The live row's "working" state is the braille dot grid — Ora's one signature for "listening or working", the same grid live voice draws — rather than the words that used to sit there.
describe("the live step row's braille grid", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    // An earlier block leaves probe resolving false, which would send the ask down the offline path and never show a live row at all.
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  /** Whether every character of a string is a braille cell, U+2800 to U+28FF. */
  const allBraille = (t: string) =>
    t.length > 0 && [...t].every((c) => c.codePointAt(0)! >= 0x2800 && c.codePointAt(0)! <= 0x28ff);

  it("shows one row of the dot grid, not words, while a question runs with no tool call yet", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });

    const text = document.querySelector(".steps .step.run .step-label")?.textContent ?? "";
    expect(allBraille(text)).toBe(true);
    // No "Thinking", no "working" — the grid is the whole of what the row says.
    expect(document.querySelector(".steps")?.textContent).not.toMatch(/[A-Za-z]/);
  });

  it("advances the row while it is shown, without replacing the element its animations run on", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    // Installed before the ask starts, so the ticker the ask sets up is the fake one this test drives.
    vi.useFakeTimers();

    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });

    const label = document.querySelector(".steps .step.run .step-label")!;
    const before = label.textContent;
    vi.advanceTimersByTime(600);
    expect(label.textContent).not.toBe(before);
    expect(allBraille(label.textContent ?? "")).toBe(true);
    // The row is patched in place, so the element carrying the CSS animations is still the same one.
    expect(document.querySelector(".steps .step.run .step-label")).toBe(label);
  });

  it("shows a running tool call's own label instead of the grid", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "tool", text: "observe_screen", detail: "" } });
    // The label cross-fades rather than swapping instantly (see crossFadeText), so the new words land 150ms later.
    await new Promise((r) => setTimeout(r, 200));

    const label = document.querySelector(".steps .step.run .step-label")!;
    expect(label.textContent).toMatch(/[A-Za-z]/);
    expect(label.classList.contains("work")).toBe(false);
  });
});

// The daemon stops sending "level" events while nothing changes, which is exactly the silent stretch the grid's breath is for, so the breath has to run on the window's own clock.
describe("the voice grid breathes on its own while the daemon is quiet", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.useFakeTimers();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("changes the rows with no level event arriving, and stops once the session ends", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);
    dispatch({ kind: "voiceOn", id: "voice-1" });
    const rows = () => Array.from(document.querySelectorAll(".vw-spk .vw-row")).map((r) => r.textContent).join("|");
    const quiet = rows();
    await vi.advanceTimersByTimeAsync(1300);
    expect(rows()).not.toBe(quiet);

    dispatch({ kind: "voiceOff" });
    const after = document.querySelectorAll(".vw-spk .vw-row").length;
    await vi.advanceTimersByTimeAsync(1000);
    expect(document.querySelectorAll(".vw-spk .vw-row").length).toBe(after);
  });
});

// The thread grew the window without limit until it was given a max-height and its own scrollbar; a fresh element starts at scrollTop 0 and render() rebuilds the whole card's innerHTML on almost every daemon event, so without pinning, the newest turn — which is at the bottom — scrolled out of sight the moment the thread passed its ceiling.
describe("the thread stays pinned to the newest turn", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("scrolls the rebuilt thread to its bottom, where the answer being written is", async () => {
    // jsdom lays nothing out, so scrollHeight is always 0 and scrollTop refuses to be set; both are stood in for here, a 900px-tall thread whose scroll position is remembered per element.
    const height = Object.getOwnPropertyDescriptor(Element.prototype, "scrollHeight")!;
    const top = Object.getOwnPropertyDescriptor(Element.prototype, "scrollTop")!;
    const tops = new WeakMap<Element, number>();
    Object.defineProperty(Element.prototype, "scrollHeight", { configurable: true, get: () => 900 });
    Object.defineProperty(Element.prototype, "scrollTop", {
      configurable: true,
      get(this: Element) {
        return tops.get(this) ?? 0;
      },
      set(this: Element, v: number) {
        tops.set(this, v);
      },
    });
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));

      dispatch({ kind: "type", value: "what changed on this page" });
      dispatch({ kind: "enter" });
      // A full rebuild of the card, which is what used to throw the scroll position away.
      dispatch({ kind: "daemonEvent", ev: { id: "", type: "answer", text: "The page reloaded." } });

      expect(document.querySelector(".thread")!.scrollTop).toBe(900);
    } finally {
      Object.defineProperty(Element.prototype, "scrollHeight", height);
      Object.defineProperty(Element.prototype, "scrollTop", top);
    }
  });
});

// A snooze the daemon could not write answers 500 (see internal/ipc/notices.go). The card used to come down at the press, before the request had even gone out, so a refusal read as done and the item was never seen again.
describe("a notice the daemon refuses", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("keeps the card up and says on it that the button did not take", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 500 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task" }, hoverOpen: true });

      document.querySelector<HTMLButtonElement>('#n button.na[data-act="evening"]')!.click();
      await new Promise((r) => setTimeout(r, 0));

      expect(document.getElementById("n")?.hidden).toBe(false);
      expect(document.querySelector("#n .nf")?.textContent).toBe("Couldn't do that");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("says the same when the daemon cannot be reached at all", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("connection refused")));
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task" }, hoverOpen: true });
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="done"]')!.click();
      await new Promise((r) => setTimeout(r, 0));

      expect(document.querySelector("#n .nf")?.textContent).toBe("Couldn't do that");
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

// The pointer being over the card is what pauses its six seconds, and that is learnt from pointerenter — which does not fire again for a card the pointer is already on. A tick can post five notices in a row (see maybeTaskNotices in internal/proactive/notify.go), and each used to re-arm the timer under a stationary pointer.
describe("the notice timer under a pointer that is already on the card", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    vi.useFakeTimers();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("leaves a second notice up while the pointer sits on the card", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);

    dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task" }, hoverOpen: true });
    const bubble = document.getElementById("n")!;
    // The pointer is on the card; no fresh pointerenter fires for the notice that lands under it.
    vi.spyOn(bubble, "matches").mockReturnValue(true);
    dispatch({ kind: "notice", notice: { title: "Also open", body: "Book the flight", place: "tasks", id: "43", kind: "task" }, hoverOpen: true });

    await vi.advanceTimersByTimeAsync(7000);
    expect(bubble.hidden).toBe(false);
    expect(bubble.querySelector(".nt")?.textContent).toBe("Also open");
  });
});

// The step ticker wakes ten times a second for the whole length of an ask or a computer-use job, which run for minutes. Nothing it draws is on screen once the window is hidden.
describe("the step ticker rests while the window is not on screen", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    vi.useFakeTimers();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("leaves the grid where it is while the document is hidden", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);
    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });

    const label = document.querySelector(".steps .step.run .step-label")!;
    const before = label.textContent;
    const hidden = Object.getOwnPropertyDescriptor(Document.prototype, "hidden")!;
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    try {
      await vi.advanceTimersByTimeAsync(600);
      expect(label.textContent).toBe(before);
    } finally {
      Object.defineProperty(Document.prototype, "hidden", hidden);
      delete (document as unknown as { hidden?: boolean }).hidden;
    }
  });

  it("stops the ticker altogether when Escape hides the hover mid-ask", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);
    dispatch({ kind: "type", value: "what changed on this page" });
    dispatch({ kind: "enter" });
    dispatch({ kind: "escape" });

    const label = document.querySelector(".steps .step.run .step-label")!;
    const before = label.textContent;
    await vi.advanceTimersByTimeAsync(600);
    expect(label.textContent).toBe(before);
  });
});

// A live voice session deliberately survives a hide of this window, so the grid's breath — a 10Hz interval driving a waveform nobody can see — went on running for the whole session with the window off screen.
describe("the voice grid's breath rests while the window is hidden", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    vi.useFakeTimers();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("stops breathing once Escape hides the hover, with the session still running", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);
    dispatch({ kind: "voiceOn", id: "voice-1" });
    const rows = () => Array.from(document.querySelectorAll(".vw-spk .vw-row")).map((r) => r.textContent).join("|");

    const quiet = rows();
    await vi.advanceTimersByTimeAsync(1300);
    expect(rows()).not.toBe(quiet);

    // Escape only hides this window; the session goes on, and the surface is still what the card is showing when it comes back.
    dispatch({ kind: "escape" });
    const hiddenAt = rows();
    await vi.advanceTimersByTimeAsync(1300);
    expect(rows()).toBe(hiddenAt);
    expect(document.querySelector(".voicebox")).toBeTruthy();
  });
});

/** A WinLike and a Desktop that record what they were asked to do, so the whole Tauri path — the show, the notice-only window, the sizing and the thread cap — can be driven headless. Under jsdom getCurrentWindow() throws, so none of it used to be reachable from a test at all. Input: the window's own scale factor and the monitor's. Output: the shell to hand wireWindow, the call log, and a way to fire the toggle hotkey. */
function fakeShell(winScale = 1, monitorScale = 1) {
  const calls: string[] = [];
  let visible = false;
  let toggle: () => void = () => {};
  const win: WinLike = {
    async isVisible() {
      return visible;
    },
    async show() {
      visible = true;
      calls.push("show");
    },
    async hide() {
      visible = false;
      calls.push("hide");
    },
    async setPosition(pos: PhysicalPosition) {
      calls.push(`setPosition(${pos.x},${pos.y})`);
    },
    async setSize(size: LogicalSize | PhysicalSize) {
      calls.push(`setSize(${size.width}x${size.height})`);
    },
    async scaleFactor() {
      return winScale;
    },
  };
  // One 1920x1080 monitor with a 32px top bar and no dock, the same screen winplace.test.ts places against.
  const screen: MonitorLike = {
    position: { x: 0, y: 0 },
    size: { width: 1920, height: 1080 },
    workArea: { position: { x: 0, y: 32 }, size: { width: 1920, height: 1048 } },
    scaleFactor: monitorScale,
  };
  const desktop: Desktop = {
    monitors: async () => [screen],
    pointer: async () => ({ x: 100, y: 100 }),
    focused: async () => screen,
    dock: async () => ({ edge: "bottom", clearance: 0 }),
  };
  return {
    calls,
    shell: {
      win,
      desktop,
      raise: async () => {
        calls.push("raise");
      },
      onToggle: (run: () => void) => {
        toggle = run;
      },
    },
    pressHotkey: () => toggle(),
  };
}

describe("the window the hotkey shows", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
    document.body.style.removeProperty("--thread-max");
  });

  // The show used to await the context read, which goes through AT-SPI and can block for seconds; a wedged read made the hotkey look dead.
  it("shows without waiting for the context read to answer", async () => {
    const { context } = await import("./daemon");
    vi.mocked(context).mockReturnValue(new Promise(() => {}));
    const { wireWindow } = await import("./main");
    const { shell, calls, pressHotkey } = fakeShell();
    wireWindow(shell);

    pressHotkey();
    await vi.waitFor(() => expect(calls).toContain("show"));
    expect(calls.indexOf("show")).toBeLessThan(calls.indexOf("raise"));
  });

  // The probe gives the daemon 500ms; a daemon mid-decode can miss that and still be there, and the card turning to "Not connected" sends the next question to the offline sentence instead of to the daemon.
  it("keeps the daemon up through one missed probe and gives up on the second", async () => {
    const { probe } = await import("./daemon");
    const { wireWindow } = await import("./main");
    const { shell, calls, pressHotkey } = fakeShell();
    wireWindow(shell);
    await vi.waitFor(() => expect(document.querySelector(".dot")).toBeTruthy());

    vi.mocked(probe).mockResolvedValue(false);
    pressHotkey();
    await vi.waitFor(() => expect(probe).toHaveBeenCalledTimes(2));
    expect(document.querySelector(".dot")).toBeTruthy();

    // Hide, then show again: the second miss in a row is the one that is believed.
    pressHotkey();
    await vi.waitFor(() => expect(calls).toContain("hide"));
    pressHotkey();
    await vi.waitFor(() => expect(probe).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(document.querySelector(".in")?.textContent).toContain("Not connected"));
  });

  // The cap is written into the page as CSS pixels, and a CSS pixel is worth the window's own scale factor — not the pointer monitor's, which is a different number on a mixed-DPI desk.
  it("caps the thread with the window's own scale factor, not the monitor's", async () => {
    const { wireWindow } = await import("./main");
    const { shell, pressHotkey } = fakeShell(2, 1);
    wireWindow(shell);

    pressHotkey();
    // 1048 physical of work area at the window's scale of 2 is 524 CSS pixels; six tenths of that is 314. At the monitor's scale of 1 it would have been 628.
    await vi.waitFor(() => expect(document.body.style.getPropertyValue("--thread-max")).toBe("314px"));
  });
});

// A notice arriving at a shut window opens a window of its own: the notice card's width rather than the hover's, under the top bar at the right, and nothing but the bubble on it.
describe("the window a notice opens on its own", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  /** Wires a fake shell and hands back the daemon's own stream callback, which is what a notice actually arrives on. Input: none. Output: the call log, the stream callback and the shell's hotkey. */
  async function notifiable() {
    const { events } = await import("./daemon");
    let onEvent: (ev: DaemonEvent) => void = () => {};
    vi.mocked(events).mockImplementation((cb) => {
      onEvent = cb;
      return () => {};
    });
    const { wireWindow } = await import("./main");
    const shell = fakeShell();
    wireWindow(shell.shell);
    await vi.waitFor(() => expect(events).toHaveBeenCalled());
    return { ...shell, send: (ev: DaemonEvent) => onEvent(ev) };
  }

  const notice = {
    title: "Morning brief",
    body: "Nothing urgent.",
    place: "",
    id: "",
    kind: "brief",
  };

  it("is the notice card's width, is moved exactly once, and hides the card under it", async () => {
    const { calls, send } = await notifiable();
    send({ id: "", type: "notice", notice });

    await vi.waitFor(() => expect(calls).toContain("show"));
    // 456 logical: the card's own 420 maximum plus the body's 18 of padding either side.
    expect(calls.filter((c) => c.startsWith("setSize(456x"))).toHaveLength(1);
    // One move per fit, not the two the double placement used to make.
    expect(calls.filter((c) => c.startsWith("setPosition"))).toHaveLength(1);
    expect(calls.indexOf("show")).toBe(calls.length - 1);
    expect(document.body.classList.contains("alone")).toBe(true);
    expect(calls).not.toContain("raise");
  });

  // The class is a margin, and which side it goes on follows where the window actually is: a notice-only window is put under the top bar whatever hover position is stored.
  it("takes its air above the card, whatever hover position is stored", async () => {
    localStorage.setItem("ora-hover-position", "bottom");
    const { send } = await notifiable();
    send({ id: "", type: "notice", notice });
    await vi.waitFor(() => expect(document.getElementById("n")?.hidden).toBe(false));
    expect(document.getElementById("n")?.className).toBe("N up");
    localStorage.removeItem("ora-hover-position");
  });
});

// The evidence header and the collapsed step summary were divs with click handlers and a pointer cursor: no role, no tab stop, no way to open them from the keyboard.
describe("the card's fold-out headers are keyboard controls", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  /** Asks a question and answers it with one source, which is the state the evidence fold-out is drawn in. Input: main's dispatch. Output: nothing. */
  const askAndAnswer = (dispatch: (e: Parameters<typeof import("./state").step>[1]) => void) => {
    dispatch({ kind: "type", value: "what did Priya say" });
    dispatch({ kind: "enter" });
    dispatch({
      kind: "daemonEvent",
      ev: { id: "", type: "answer", text: "She said yes.", evidence: [{ title: "Re: venue", meta: "Priya · 08:40", body: "Tuesday works." }] },
    });
  };

  it("names the evidence header a button, puts it in the tab order and says whether it is open", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    askAndAnswer(dispatch);

    const h = document.querySelector<HTMLElement>(".evd .h")!;
    expect(h.getAttribute("role")).toBe("button");
    expect(h.tabIndex).toBe(0);
    expect(h.getAttribute("aria-expanded")).toBe("false");
  });

  it("opens the fold on Enter and closes it again on Space", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    askAndAnswer(dispatch);

    document.querySelector<HTMLElement>(".evd .h")!.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    expect(document.querySelector(".evd .items")).toBeTruthy();
    expect(document.querySelector(".evd .h")?.getAttribute("aria-expanded")).toBe("true");

    document.querySelector<HTMLElement>(".evd .h")!.dispatchEvent(new KeyboardEvent("keydown", { key: " ", bubbles: true }));
    expect(document.querySelector(".evd .items")).toBeNull();
  });

  // Space on the card with an empty input opens the microphone, which would swallow the press that operates the header.
  it("leaves the microphone shut when Space presses the header", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    askAndAnswer(dispatch);

    document.querySelector<HTMLElement>(".evd .h")!.dispatchEvent(new KeyboardEvent("keydown", { key: " ", bubbles: true }));
    expect(document.querySelector(".in.holding")).toBeNull();
  });
});

// The notice card is sized to itself and the window is sized to the card, with no minimum height and a clamp that pins a window too tall for its area to the top of the screen; a long title and a wrapped row of buttons had nothing stopping them running off the bottom.
describe("what the stylesheet and the page hold the notice to", () => {
  // Read off disk rather than imported: what is asserted here is what the browser will be handed, and a CSS import in a jsdom test resolves to an empty module.
  const css = readFileSync(`${process.cwd()}/src/styles.css`, "utf8");
  /** The body of one CSS rule. Input: the selector, exactly as written in the file. Output: what is between its braces. */
  const rule = (selector: string) => css.slice(css.indexOf(`${selector} {`)).slice(0, css.slice(css.indexOf(`${selector} {`)).indexOf("}"));

  it("caps the card's height and hides what runs past it", () => {
    expect(rule(".N")).toContain("max-height: 300px");
    expect(rule(".N")).toContain("overflow: hidden");
  });

  it("clamps the title to two lines, the way the body is clamped to three", () => {
    expect(rule(".N .nt")).toContain("-webkit-line-clamp: 2");
    expect(rule(".N .nb")).toContain("-webkit-line-clamp: 3");
  });

  it("makes the bubble a live region, since a notice is Ora talking first", () => {
    const html = readFileSync(`${process.cwd()}/index.html`, "utf8");
    expect(html).toContain('id="n" role="status" aria-live="polite"');
  });
});
