/** @vitest-environment jsdom */
/** Regression test for the "stuttering" status line: main.ts used to rebuild the whole card's innerHTML on every daemon event while an ask was running (and once a second besides, from the elapsed-time ticker), tearing down and recreating the live step row on every one of them. A CSS animation restarts from its first frame whenever the element carrying it is removed and recreated, so the shimmer and breathe never got to run a full pass — that restart, not the animations themselves, was the stutter. patchLiveSteps (see main.ts) now patches that row in place instead. This checks the fix holds: the row survives a run of daemon events as the same DOM node instead of being swapped for a fresh one. */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { LogicalSize, PhysicalPosition, PhysicalSize } from "@tauri-apps/api/dpi";
import type { DaemonEvent } from "./shared/wire";
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
  // The real post, minus the base URL and the token, so a test sees each call on the stubbed fetch.
  post: vi.fn(async (path: string, body?: unknown) => {
    try {
      return await fetch(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) });
    } catch {
      return null;
    }
  }),
  voiceStart: vi.fn(),
  voiceStatus: vi.fn().mockResolvedValue(null),
  voiceStop: vi.fn(),
  actStart: vi.fn().mockReturnValue(new Promise(() => {})),
  actStop: vi.fn(),
  actPauseResume: vi.fn(),
  actAnswer: vi.fn(),
}));

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

// The waveform itself (the smoothing and the braille rows) is tested in waveform.test.ts. These check the voice-mode surface built around it (see voiceSurfaceHtml in main.ts): while a session runs it replaces the input and the thread entirely, the way Gemini Live and ChatGPT's own voice mode take over the screen.
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

// What the daemon puts on a task notice: the same five buttons its desktop banner offers, in the same order (noticeActions in internal/proactive/notify.go). A card draws the actions its notice names and nothing else, so a fixture that presses a button has to carry them.
const TASK_ACTIONS = [
  { key: "default", label: "Open in Ora" },
  { key: "done", label: "Done" },
  { key: "hour", label: "In an hour" },
  { key: "evening", label: "This evening" },
  { key: "tomorrow", label: "Tomorrow" },
];

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
});

// An answer to a question, or a meeting's Start recording, comes back with its action set to the pressed key, which has no line of its own to show (see Act in internal/proactive/notify.go). It means the card is dealt with, so the card goes, rather than being drawn again as a fresh card with its title, body and an Open button.
describe("a notice answered with a button of its own", () => {
  beforeEach(() => {
    vi.resetModules();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("takes the card down", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    const question = { title: "Still open", body: "Send the invoice", place: "tasks", id: "7", kind: "stale" };
    dispatch({ kind: "notice", notice: { ...question, actions: [{ key: "dropped", label: "Not happening" }] }, hoverOpen: true });
    expect(document.getElementById("n")?.hidden).toBe(false);

    dispatch({ kind: "notice", notice: { ...question, action: "dropped", actions: [{ key: "default", label: "Open in Ora" }] }, hoverOpen: true });

    expect(document.getElementById("n")?.hidden).toBe(true);
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
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", actions: TASK_ACTIONS },
        hoverOpen: true,
      });
      const bubble = document.getElementById("n")!;
      const acts = [...bubble.querySelectorAll<HTMLButtonElement>("button.na")].map((b) => b.dataset.act);
      expect(acts).toEqual(["default", "done", "hour", "evening", "tomorrow"]);
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

  it("closes on its own cross without opening the app window or telling the daemon anything", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({
        kind: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", actions: TASK_ACTIONS },
        hoverOpen: true,
      });
      const bubble = document.getElementById("n")!;
      fetchMock.mockClear();

      bubble.querySelector<HTMLButtonElement>("button.nx")!.click();

      expect(bubble.hidden).toBe(true);
      // Neither the notice route nor the window route: the cross is the user saying they have seen it, not an answer and not a request to open anything.
      expect(fetchMock.mock.calls).toEqual([]);
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
      dispatch({ kind: "notice", notice: { title: "Morning brief", body: "Nothing urgent.", place: "", id: "", kind: "brief", actions: TASK_ACTIONS }, hoverOpen: true });
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="done"]')!.click();
      expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith("/notices/brief/-/action"))).toBe(true);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  // The daily stale-task question used to go to a GNOME banner with its own three buttons because the card only ever knew the Done/snooze set. A notice that names its answers now gets exactly those, on the card, in the order it named them.
  it("renders a notice's own actions instead of the default set and posts the pressed key", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({
        kind: "notice",
        notice: {
          title: "Still open",
          body: "Vexil — send the invoice. Open since 28 Aug. Any progress?",
          place: "tasks",
          id: "42",
          kind: "task",
          actions: [
            { key: "done", label: "Done" },
            { key: "dropped", label: "Not happening" },
            { key: "later", label: "Not urgent" },
          ],
        },
        hoverOpen: true,
      });
      const buttons = [...document.querySelectorAll<HTMLButtonElement>("#n button.na")];
      expect(buttons.map((b) => b.textContent)).toEqual(["Done", "Not happening", "Not urgent"]);
      expect(buttons.map((b) => b.dataset.act)).toEqual(["done", "dropped", "later"]);
      buttons[1]!.click();
      const call = fetchMock.mock.calls.find(([url]) => String(url).endsWith("/notices/task/42/action"));
      expect(call).toBeDefined();
      expect(JSON.parse(call![1].body)).toEqual({
        title: "Still open",
        body: "Vexil — send the invoice. Open since 28 Aug. Any progress?",
        action: "dropped",
      });
      // The card is kept up until the daemon's follow-up event, the same as every other answer.
      expect(document.getElementById("n")?.hidden).toBe(false);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("leaves the buttons up and says so when the daemon refuses a custom action", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 500 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({
        kind: "notice",
        notice: {
          title: "Still open",
          body: "Any progress?",
          place: "tasks",
          id: "42",
          kind: "task",
          actions: [{ key: "later", label: "Not urgent" }],
        },
        hoverOpen: true,
      });
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="later"]')!.click();
      await new Promise((r) => setTimeout(r, 0));
      expect(document.querySelector("#n .nf")?.textContent).toBe("Could not do that");
      expect(document.querySelectorAll("#n button.na").length).toBe(1);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  // A card offering a button the notice cannot answer is what put Done and three snoozes on "Transcribing meeting", where pressing one answered "Could not do that". The daemon says what each notice can answer (see openOnlyActions and noticeActions in internal/proactive), and the card draws that and nothing else.
  it("draws the buttons the notice names and nothing else, and opens on the desktop's own open key", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({
        kind: "notice",
        notice: {
          title: "Transcribing meeting",
          body: "Ora is transcribing the recording in the background.",
          place: "",
          id: "",
          kind: "note",
          actions: [{ key: "default", label: "Open in Ora" }],
        },
        hoverOpen: true,
      });
      expect([...document.querySelectorAll<HTMLButtonElement>("#n button.na")].map((b) => b.dataset.act)).toEqual(["default"]);

      // "default" is what the desktop calls a press on the notification body, and it is the key the daemon's own Open button carries. It opens the window here rather than being posted back as an action the daemon would refuse.
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="default"]')!.click();
      await new Promise((r) => setTimeout(r, 0));
      expect(fetchMock.mock.calls.map(([u]) => String(u))).toEqual(["/window?action=open"]);

      dispatch({
        kind: "notice",
        notice: { title: "Recording saved", body: "Ora will transcribe it once you plug in.", place: "", id: "", kind: "note" },
        hoverOpen: true,
      });
      expect(document.querySelectorAll("#n button.na").length).toBe(0);
    } finally {
      vi.unstubAllGlobals();
    }
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

// The hover used to render every turn in the matter, folded but still on screen, which flooded the card the longer a conversation ran. Older turns stay in the matter's own state; only what the hover draws changed.
describe("the hover shows only the newest turn", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("renders just the third question and answer, not the first two", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));

    dispatch({ kind: "type", value: "first question" });
    dispatch({ kind: "enter" });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "answer", text: "first answer" } });

    dispatch({ kind: "type", value: "second question" });
    dispatch({ kind: "enter" });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "answer", text: "second answer" } });

    dispatch({ kind: "type", value: "third question" });
    dispatch({ kind: "enter" });
    dispatch({ kind: "daemonEvent", ev: { id: "", type: "answer", text: "third answer" } });

    const thread = document.querySelector(".thread")!;
    expect(thread.querySelector(".qq")!.textContent).toContain("third question");
    expect(thread.querySelector(".a")!.textContent).toContain("third answer");
    expect(thread.textContent).not.toContain("first question");
    expect(thread.textContent).not.toContain("second question");
    expect(thread.querySelector(".prev")).toBeNull();
  });
});

// A snooze the daemon could not write answers 500 (see internal/ipc/notices.go). The card used to come down at the press, before the request had even gone out, so a refusal read as done and the item was never seen again.
describe("a notice the daemon refuses", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("says the same when the daemon cannot be reached at all", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("connection refused")));
    try {
      const { dispatch } = await import("./main");
      await new Promise((r) => setTimeout(r, 0));
      dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", actions: TASK_ACTIONS }, hoverOpen: true });
      document.querySelector<HTMLButtonElement>('#n button.na[data-act="done"]')!.click();
      await new Promise((r) => setTimeout(r, 0));

      expect(document.querySelector("#n .nf")?.textContent).toBe("Could not do that");
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

    dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", actions: TASK_ACTIONS }, hoverOpen: true });
    const bubble = document.getElementById("n")!;
    // The pointer is on the card; no fresh pointerenter fires for the notice that lands under it.
    vi.spyOn(bubble, "matches").mockReturnValue(true);
    dispatch({ kind: "notice", notice: { title: "Also open", body: "Book the flight", place: "tasks", id: "43", kind: "task", actions: TASK_ACTIONS }, hoverOpen: true });

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
    await vi.waitFor(() => expect(document.querySelector(".face")).toBeTruthy());

    vi.mocked(probe).mockResolvedValue(false);
    pressHotkey();
    await vi.waitFor(() => expect(probe).toHaveBeenCalledTimes(2));
    expect(document.querySelector(".face")).toBeTruthy();

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
    expect(calls).not.toContain("raise");
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
    dispatch({ kind: "type", value: "what did Vexil say" });
    dispatch({ kind: "enter" });
    dispatch({
      kind: "daemonEvent",
      ev: { id: "", type: "answer", text: "She said yes.", evidence: [{ title: "Re: venue", meta: "Vexil · 08:40", body: "Tuesday works." }] },
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

// Space with an empty input opens the microphone, and the capture-phase listener that does it sees every key on the card first. The fold headers were exempted by their role="button"; every other control on the card is a native <button>, which carries no role attribute at all.
describe("Space on the card's own buttons", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("leaves the microphone shut when Space presses a notice button", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", actions: TASK_ACTIONS }, hoverOpen: true });

    const button = document.querySelector<HTMLButtonElement>('#n button.na[data-act="done"]')!;
    const pressed = button.dispatchEvent(new KeyboardEvent("keydown", { key: " ", bubbles: true, cancelable: true }));

    // Not cancelled means the browser goes on to turn this Space into the button's own click.
    expect(pressed).toBe(true);
    expect(document.querySelector(".in.holding")).toBeNull();
  });
});

// The context read is what a question about what is on screen is answered from, and it is fired off behind the show rather than before it, so a question sent in the first seconds after the hotkey used to go with nothing.
describe("a question asked while the screen context is still being read", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe, matters } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    vi.mocked(matters).mockResolvedValue(null);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("waits for the read and sends the screen text with the question", async () => {
    const { context, ask } = await import("./daemon");
    let land: () => void = () => {};
    vi.mocked(context).mockReturnValue(
      new Promise((resolve) => {
        land = () => resolve({ app: "Brave", title: "Invoice 42", text: "Invoice 42 is due on Friday." });
      }),
    );
    vi.mocked(ask).mockReturnValue(new Promise(() => {}));

    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    dispatch({ kind: "type", value: "when is this due" });
    dispatch({ kind: "enter" });

    // Nothing has gone to the daemon yet: the read the question needs is still out.
    expect(ask).not.toHaveBeenCalled();

    land();
    await new Promise((r) => setTimeout(r, 0));
    expect(vi.mocked(ask).mock.calls[0][1]).toBe("Invoice 42 is due on Friday.");
  });
});

// Every render rebuilds the card, and the rebuild used to put the keyboard back in the input whatever the user was doing — including a render landing seconds after the show, when the context read finally answers.
describe("what a render does to the keyboard", () => {
  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    const { probe } = await import("./daemon");
    vi.mocked(probe).mockResolvedValue(true);
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });

  it("leaves the keyboard on a notice button a render lands under", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    dispatch({ kind: "notice", notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task", actions: TASK_ACTIONS }, hoverOpen: true });
    const button = document.querySelector<HTMLButtonElement>('#n button.na[data-act="hour"]')!;
    button.focus();

    dispatch({ kind: "contextLoaded", ctx: { app: "Brave", title: "Invoice 42", text: "due Friday" } });

    expect(document.activeElement).toBe(button);
  });

  it("keeps the caret where the user put it instead of throwing it to the end", async () => {
    const { dispatch } = await import("./main");
    await new Promise((r) => setTimeout(r, 0));
    const input = document.querySelector<HTMLInputElement>(".q")!;
    input.value = "invoice from vexil";
    dispatch({ kind: "type", value: input.value });
    input.setSelectionRange(7, 7);

    dispatch({ kind: "contextLoaded", ctx: { app: "Brave", title: "Invoice 42", text: "due Friday" } });

    const after = document.querySelector<HTMLInputElement>(".q")!;
    expect(document.activeElement).toBe(after);
    expect(after.selectionStart).toBe(7);
  });
});

// A question's card lives exactly as long as its answer window. The daemon stamps the moment the goroutine waiting on the answer gives up; past it the button 400s, so the card must be gone by then and must say how long is left while it is up.
describe("a question's card counts down and goes when its answer window closes", () => {
  const question = (expiresInMs: number) => ({
    title: "In a meeting?",
    body: "Online Voice Recorder is using your microphone.",
    place: "",
    id: "",
    kind: "meeting",
    expires: new Date(Date.now() + expiresInMs).toISOString(),
    actions: [{ key: "record", label: "Start recording" }],
  });

  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    vi.useFakeTimers();
    document.body.innerHTML = `<div class="N" id="n" hidden></div><div class="W" id="w"></div>`;
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  // A bar rather than a number: the card is glanced at, not read. It is a CSS animation timed from what is left, so nothing ticks and the browser keeps it true on its own.
  it("draws a bar that empties over the time left", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);

    dispatch({ kind: "notice", notice: question(15000), hoverOpen: true });
    const bar = document.getElementById("n")!.querySelector<HTMLElement>(".np > i");
    expect(bar).not.toBeNull();
    expect(bar!.style.animationDuration).toBe("15000ms");
  });

  it("takes itself off screen when the answer window closes", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);

    dispatch({ kind: "notice", notice: question(15000), hoverOpen: true });
    const bubble = document.getElementById("n")!;
    expect(bubble.hidden).toBe(false);

    // Still up well past the six seconds an ordinary notice gets, because this card's life is its answer window and not that timer.
    await vi.advanceTimersByTimeAsync(7000);
    expect(bubble.hidden).toBe(false);

    await vi.advanceTimersByTimeAsync(8000);
    expect(bubble.hidden).toBe(true);
  });

  // A pointer resting on the card pauses the ordinary six seconds. It must not pause this one: the answer window closes on the daemon's clock whatever the pointer is doing, and a card held open past it is a button that no longer works.
  it("goes even while the pointer is on it", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);

    dispatch({ kind: "notice", notice: question(15000), hoverOpen: true });
    const bubble = document.getElementById("n")!;
    vi.spyOn(bubble, "matches").mockReturnValue(true);
    dispatch({ kind: "noticeHold" });

    await vi.advanceTimersByTimeAsync(15000);
    expect(bubble.hidden).toBe(true);
  });

  // An ordinary notice carries no expiry and keeps the six seconds it always had.
  it("leaves a notice with no answer window on its own timer", async () => {
    const { dispatch } = await import("./main");
    await vi.advanceTimersByTimeAsync(0);

    dispatch({ kind: "notice", notice: { title: "Transcribing meeting", body: "Standup", place: "", id: "", kind: "meeting" }, hoverOpen: true });
    const bubble = document.getElementById("n")!;
    expect(bubble.querySelector(".np")).toBeNull();

    await vi.advanceTimersByTimeAsync(7000);
    expect(bubble.hidden).toBe(true);
  });
});
