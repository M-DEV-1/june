import { describe, expect, it } from "vitest";
import {
  applyToolEvent,
  ASK_PLACEHOLDER,
  RESTING_PLACEHOLDER,
  askConversation,
  CONVERSATION_MS,
  chipLabel,
  dotLabel,
  faceState,
  failRunningStep,
  isJobLive,
  jobGoal,
  placeholder,
  sourceMeta,
  step,
  stepIconKind,
  stepLabel,
  noticeActionLine,
  stepsCollapsed,
  stepsSummaryLine,
  stepSeconds,
  THEME_KEY,
  themeChoice,
  themeFromStorage,
  type ContextInfo,
  type DaemonEvent,
  type JobMeta,
  type Matter,
  type MatterRow,
  type Notice,
  type ToolStep,
  type View,
} from "./state";

function matter(partial: Partial<Matter> = {}): Matter {
  return { id: "m1", title: "m1", context: "ctx", turns: [], ...partial };
}

function view(partial: Partial<View> = {}): View {
  return {
    matters: [matter()],
    current: 0,
    evidenceOpen: false,
    input: "",
    state: "empty",
    contextChip: "",
    contextText: "",
    dictating: false,
    hint: "",
    voice: "",
    voiceState: "idle",
    ...partial,
  };
}

describe("jobGoal", () => {
  it("strips the do: prefix and any space after it", () => {
    expect(jobGoal("do: reload the page")).toBe("reload the page");
    expect(jobGoal("do:reload the page")).toBe("reload the page");
  });

  it("is case-insensitive", () => {
    expect(jobGoal("Do: reload the page")).toBe("reload the page");
  });

  it("is undefined for an ordinary question", () => {
    expect(jobGoal("is the venue sorted?")).toBeUndefined();
  });

  it("is undefined for do: with nothing after it", () => {
    expect(jobGoal("do:")).toBeUndefined();
    expect(jobGoal("do:   ")).toBeUndefined();
  });
});

describe("isJobLive", () => {
  it("is true for every state a job may still take a step from", () => {
    for (const s of ["planning", "stepping", "verifying", "paused", "stuck"])
      expect(isJobLive(s)).toBe(true);
  });

  it("is false for the three ends and for no job at all", () => {
    for (const s of ["done", "stopped", "failed", ""])
      expect(isJobLive(s)).toBe(false);
  });
});

describe("submit", () => {
  it("moves empty->asking and clears input", () => {
    const v = view({ input: "is the venue sorted?" });
    const { view: next } = step(v, { kind: "submit" });
    expect(next.state).toBe("asking");
    expect(next.input).toBe("");
  });

  it("does nothing on empty input", () => {
    const v = view({ input: "   " });
    const { view: next } = step(v, { kind: "submit" });
    expect(next.state).toBe("empty");
  });

  it("a do: question starts a job instead of an ask", () => {
    const v = view({ input: "do: reload the page" });
    const { view: next, effect } = step(v, { kind: "submit" });
    expect(next.state).toBe("asking");
    expect(next.input).toBe("");
    expect(next.matters[0].turns[0]).toEqual({
      q: "reload the page",
      a: "",
      at: expect.any(Number),
    });
    expect(effect).toEqual({ kind: "startJob", goal: "reload the page" });
  });

  it("text sent while a job is stuck on a question answers it instead of asking something new", () => {
    const job: JobMeta = {
      id: "act-1",
      state: "stuck",
      startedAt: 0,
      question: "which venue?",
    };
    const v = view({
      input: "the green room",
      matters: [matter({ turns: [{ q: "book a venue", a: "", job }] })],
    });
    const { view: next, effect } = step(v, { kind: "submit" });
    expect(effect).toEqual({
      kind: "answerJob",
      id: "act-1",
      text: "the green room",
    });
    expect(next.matters[0].turns[0].job?.question).toBeUndefined();
    expect(next.input).toBe("");
  });
});

describe("tab", () => {
  it("cycles current across matters", () => {
    const v = view({
      matters: [matter({ id: "a" }), matter({ id: "b" }), matter({ id: "c" })],
      current: 0,
    });
    const { view: v1 } = step(v, { kind: "tab" });
    expect(v1.current).toBe(1);
    const { view: v2 } = step(v1, { kind: "tab" });
    expect(v2.current).toBe(2);
    const { view: v3 } = step(v2, { kind: "tab" });
    expect(v3.current).toBe(0);
  });
});

describe("escape", () => {
  it("closes evidence first and only then signals close", () => {
    const v = view({ evidenceOpen: true });
    const { view: v1, effect: e1 } = step(v, { kind: "escape" });
    expect(v1.evidenceOpen).toBe(false);
    expect(e1).toBeUndefined();

    const { view: v2, effect: e2 } = step(v1, { kind: "escape" });
    expect(v2.evidenceOpen).toBe(false);
    expect(e2).toEqual({ kind: "close" });
  });

  it("asks for confirmation instead of closing while a job is live, then stops it on the next escape", () => {
    const job: JobMeta = { id: "act-1", state: "stepping", startedAt: 0 };
    const v = view({
      matters: [matter({ turns: [{ q: "reload the page", a: "", job }] })],
    });

    const { view: v1, effect: e1 } = step(v, { kind: "escape" });
    expect(v1.confirmStopJob).toBe(true);
    expect(e1).toBeUndefined();

    const { view: v2, effect: e2 } = step(v1, { kind: "escape" });
    expect(v2.confirmStopJob).toBe(false);
    expect(e2).toEqual({ kind: "stopJob", id: "act-1" });
  });

  it("closes at once once the job has ended", () => {
    const job: JobMeta = { id: "act-1", state: "done", startedAt: 0 };
    const v = view({
      matters: [matter({ turns: [{ q: "reload the page", a: "done.", job }] })],
    });
    const { effect } = step(v, { kind: "escape" });
    expect(effect).toEqual({ kind: "close" });
  });

  it("any other event drops a pending confirmation instead of leaving it armed", () => {
    const job: JobMeta = { id: "act-1", state: "stepping", startedAt: 0 };
    const v = view({
      confirmStopJob: true,
      matters: [matter({ turns: [{ q: "reload the page", a: "", job }] })],
    });
    const { view: next } = step(v, { kind: "type", value: "x" });
    expect(next.confirmStopJob).toBe(false);
  });
});

describe("enter", () => {
  it("closes the window when the input is empty, because there is nothing to ask", () => {
    const { effect } = step(view({ input: "   " }), { kind: "enter" });
    expect(effect).toEqual({ kind: "close" });
  });

  it("submits instead when there is text in the input, which is what sends the question", () => {
    const v = view({ input: "hello" });
    const { view: next, effect } = step(v, { kind: "enter" });
    expect(next.state).toBe("asking");
    expect(effect).toEqual({
      kind: "ask",
      question: "hello",
      conversation: undefined,
    });
  });
});

describe("daemonEvent", () => {
  function askedView(): View {
    return view({
      matters: [matter({ turns: [{ q: "is the venue sorted?", a: "" }] })],
      state: "asking",
    });
  }

  it("status sets the pending turn's status text", () => {
    const ev: DaemonEvent = { id: "1", type: "status", text: "reading mail" };
    const { view: next } = step(askedView(), { kind: "daemonEvent", ev });
    expect(next.matters[0].turns[0].status).toBe("reading mail");
  });

  it("tool appends the tool name to the pending turn's tools", () => {
    const v1 = step(askedView(), {
      kind: "daemonEvent",
      ev: { id: "1", type: "tool", text: "search_mail" },
    }).view;
    const v2 = step(v1, {
      kind: "daemonEvent",
      ev: { id: "1", type: "tool", text: "read_thread" },
    }).view;
    expect(v2.matters[0].turns[0].tools).toEqual([
      "search_mail",
      "read_thread",
    ]);
  });

  it("tool sets the pending turn's progress to the event's Detail, most recent wins", () => {
    const v1 = step(askedView(), {
      kind: "daemonEvent",
      ev: { id: "1", type: "tool", text: "click", detail: "clicking Reload" },
    }).view;
    expect(v1.matters[0].turns[0].progress).toBe("clicking Reload");
    const v2 = step(v1, {
      kind: "daemonEvent",
      ev: { id: "1", type: "tool", text: "click", detail: "done" },
    }).view;
    expect(v2.matters[0].turns[0].progress).toBe("done");
  });

  it("tool with no Detail keeps the last progress line rather than blanking it", () => {
    const v1 = step(askedView(), {
      kind: "daemonEvent",
      ev: { id: "1", type: "tool", text: "click", detail: "clicking Reload" },
    }).view;
    const v2 = step(v1, {
      kind: "daemonEvent",
      ev: { id: "1", type: "tool", text: "click" },
    }).view;
    expect(v2.matters[0].turns[0].progress).toBe("clicking Reload");
  });

  // The daemon reports one tool call as two "tool" events with the same name — no id ties them together — so the reducer tells them apart by whether a step is still open, the same way the live step-list card needs to.
  it("tool opens a step on the first event and closes it on the second", () => {
    const v1 = step(
      askedView(),
      {
        kind: "daemonEvent",
        ev: { id: "1", type: "tool", text: "query_memory", detail: '"venue"' },
      },
      1000,
    ).view;
    expect(v1.matters[0].turns[0].steps).toEqual([
      { name: "query_memory", detail: '"venue"', startedAt: 1000 },
    ]);

    const v2 = step(
      v1,
      {
        kind: "daemonEvent",
        ev: { id: "1", type: "tool", text: "query_memory", detail: "3 hits" },
      },
      1500,
    ).view;
    expect(v2.matters[0].turns[0].steps).toEqual([
      {
        name: "query_memory",
        detail: '"venue"',
        startedAt: 1000,
        finishedAt: 1500,
      },
    ]);
  });

  it("a second tool call opens its own step once the first has closed", () => {
    let v = step(
      askedView(),
      { kind: "daemonEvent", ev: { id: "1", type: "tool", text: "click" } },
      1000,
    ).view;
    v = step(
      v,
      { kind: "daemonEvent", ev: { id: "1", type: "tool", text: "click" } },
      1200,
    ).view;
    v = step(
      v,
      { kind: "daemonEvent", ev: { id: "1", type: "tool", text: "scroll_to" } },
      1300,
    ).view;
    expect(v.matters[0].turns[0].steps).toEqual([
      { name: "click", detail: "", startedAt: 1000, finishedAt: 1200 },
      { name: "scroll_to", detail: "", startedAt: 1300 },
    ]);
  });

  // The whole ask fails while a tool call is in flight — that call never gets a finishing "tool" event of its own — so the error event has to close its step out, spinner turned cross, instead of leaving it stuck open.
  it("error fails the step still running, if any", () => {
    const v1 = step(
      askedView(),
      { kind: "daemonEvent", ev: { id: "1", type: "tool", text: "click" } },
      1000,
    ).view;
    const v2 = step(
      v1,
      {
        kind: "daemonEvent",
        ev: { id: "1", type: "error", text: "could not click that" },
      },
      1400,
    ).view;
    expect(v2.matters[0].turns[0].steps).toEqual([
      {
        name: "click",
        detail: "",
        startedAt: 1000,
        finishedAt: 1400,
        error: "could not click that",
      },
    ]);
  });

  it("error with no step running leaves the (empty) steps alone", () => {
    const v = step(
      askedView(),
      {
        kind: "daemonEvent",
        ev: { id: "1", type: "error", text: "no tools ran" },
      },
      1000,
    ).view;
    expect(v.matters[0].turns[0].steps).toEqual([]);
  });

  it("answer sets text and the full evidence list", () => {
    const ev: DaemonEvent = {
      id: "1",
      type: "answer",
      text: "Nearly. Priya said yes for Tuesday.",
      evidence: [
        { title: "Re: venue", meta: "Priya · 08:40", body: "..." },
        { title: "Re: parking", meta: "Priya · 08:41", body: "..." },
      ],
    };
    const { view: next } = step(askedView(), { kind: "daemonEvent", ev });
    expect(next.matters[0].turns[0].a).toBe(
      "Nearly. Priya said yes for Tuesday.",
    );
    expect(next.matters[0].turns[0].evidence).toEqual([
      { title: "Re: venue", meta: "Priya · 08:40", body: "..." },
      { title: "Re: parking", meta: "Priya · 08:41", body: "..." },
    ]);
  });

  it("done moves state to answered", () => {
    const { view: next } = step(askedView(), {
      kind: "daemonEvent",
      ev: { id: "1", type: "done" },
    });
    expect(next.state).toBe("answered");
  });

  it("error sets the answer text and moves to answered", () => {
    const ev: DaemonEvent = {
      id: "1",
      type: "error",
      text: "daemon lost the thread",
    };
    const { view: next } = step(askedView(), { kind: "daemonEvent", ev });
    expect(next.matters[0].turns[0].a).toBe("daemon lost the thread");
    expect(next.state).toBe("answered");
    expect(next.matters[0].turns[0].detail).toBeUndefined();
  });
});

describe("jobStarted and act daemon events", () => {
  /** A view holding one turn a "do:" question opened, its job id already attached — the shape the reducer is in once main.ts's POST /act reply lands. */
  function jobView(job: Partial<JobMeta> = {}): View {
    return view({
      state: "asking",
      matters: [
        matter({
          turns: [
            {
              q: "reload the page",
              a: "",
              job: { id: "act-1", state: "planning", startedAt: 0, ...job },
            },
          ],
        }),
      ],
    });
  }

  function act(kind: string, extra: Record<string, unknown> = {}): DaemonEvent {
    return {
      id: "act-1",
      type: "act",
      text: "",
      detail: JSON.stringify({ kind, state: "stepping", ...extra }),
    };
  }

  it("jobStarted attaches the job id to the pending turn", () => {
    const v = view({
      state: "asking",
      matters: [matter({ turns: [{ q: "reload the page", a: "" }] })],
    });
    const { view: next } = step(v, { kind: "jobStarted", id: "act-1" }, 500);
    expect(next.matters[0].turns[0].job).toEqual({
      id: "act-1",
      state: "planning",
      startedAt: 500,
    });
  });

  it("an act event for a different job, or one that beats jobStarted, changes nothing", () => {
    const noJob = view({
      state: "asking",
      matters: [matter({ turns: [{ q: "reload the page", a: "" }] })],
    });
    expect(
      step(noJob, {
        kind: "daemonEvent",
        ev: act("step", { text: "clicking Reload" }),
      }).view,
    ).toEqual(noJob);

    const otherJob = jobView();
    const ev = { ...act("step"), id: "act-2" };
    expect(step(otherJob, { kind: "daemonEvent", ev }).view).toEqual(otherJob);
  });

  it("step appends a row naming the tool and its expected change", () => {
    const ev = act("step", {
      text: "clicking Reload",
      expect: "page title contains 'Success'",
    });
    const { view: next } = step(jobView(), { kind: "daemonEvent", ev }, 1000);
    expect(next.matters[0].turns[0].steps).toEqual([
      {
        name: "clicking Reload — expecting page title contains 'Success'",
        detail: "",
        startedAt: 1000,
      },
    ]);
  });

  it("verified marks the running row passed, ready for the next step", () => {
    let full = jobView();
    full = step(
      full,
      {
        kind: "daemonEvent",
        ev: act("step", { text: "clicking Reload", expect: "loaded" }),
      },
      1000,
    ).view;
    full = step(
      full,
      {
        kind: "daemonEvent",
        ev: act("verified", { outcome: "pass", text: "it loaded" }),
      },
      1500,
    ).view;
    expect(full.matters[0].turns[0].steps).toEqual([
      {
        name: "clicking Reload — expecting loaded",
        detail: "",
        startedAt: 1000,
        finishedAt: 1500,
      },
    ]);
  });

  it("verified marks the running row failed, with why as its error line", () => {
    let full = jobView();
    full = step(
      full,
      {
        kind: "daemonEvent",
        ev: act("step", { text: "clicking Reload", expect: "loaded" }),
      },
      1000,
    ).view;
    full = step(
      full,
      {
        kind: "daemonEvent",
        ev: act("verified", {
          outcome: "fail",
          text: "the page did not change",
        }),
      },
      1500,
    ).view;
    expect(full.matters[0].turns[0].steps).toEqual([
      {
        name: "clicking Reload — expecting loaded",
        detail: "",
        startedAt: 1000,
        finishedAt: 1500,
        error: "the page did not change",
      },
    ]);
  });

  it("question sets the job's question and leaves the steps alone", () => {
    const { view: next } = step(jobView(), {
      kind: "daemonEvent",
      ev: act("question", { state: "stuck", text: "which venue?" }),
    });
    expect(next.matters[0].turns[0].job?.question).toBe("which venue?");
    expect(next.matters[0].turns[0].job?.state).toBe("stuck");
  });

  it("answered clears the question", () => {
    const { view: next } = step(jobView({ question: "which venue?" }), {
      kind: "daemonEvent",
      ev: act("answered", { text: "the green room" }),
    });
    expect(next.matters[0].turns[0].job?.question).toBeUndefined();
  });

  it("done sets the closing text, the spend, and moves to answered", () => {
    const ev = act("done", {
      state: "done",
      text: "Booked the green room.",
      spend: { rounds: 4, input: 8000, cached: 500, output: 300 },
    });
    const { view: next } = step(jobView(), { kind: "daemonEvent", ev });
    expect(next.state).toBe("answered");
    expect(next.matters[0].turns[0].a).toBe("Booked the green room.");
    expect(next.matters[0].turns[0].job).toEqual({
      id: "act-1",
      state: "done",
      startedAt: 0,
      question: undefined,
      spend: { rounds: 4, input: 8000, cached: 500, output: 300 },
    });
  });
});

// On 2026-09-04 an ask failed on the free tier's daily quota and the provider's whole error — a thousand characters of JSON, map literals and a URL — went into the answer slot, which is 20px type in a window that sizes itself to its content. The card grew past the screen and pushed the input and the footer out of view. Whatever a provider sends, the answer slot takes one line of it and the rest is kept for the fold.
describe("an error that is a wall of text", () => {
  const asked = (): View =>
    view({
      matters: [matter({ turns: [{ q: "is the venue sorted?", a: "" }] })],
      state: "asking",
    });
  const WALL =
    `ask text: generate (iteration 0): Error 429, Message: You exceeded your current quota, please check your plan and billing details. For more information on this error, head to: https://ai.google.dev/gemini-api/docs/rate-limits.\nmap[@type:type.googleapis.com/google.rpc.QuotaFailure violations:[map[quotaDimensions:map[location:global model:gemini-3.5-flash]]]]`.repeat(
      20,
    );

  it("puts one line in the answer and keeps the whole message beside it", () => {
    const { view: next } = step(asked(), {
      kind: "daemonEvent",
      ev: { id: "1", type: "error", text: WALL },
    });
    const turn = next.matters[0].turns[0];
    expect(turn.a.length).toBeLessThanOrEqual(160);
    expect(turn.a).not.toContain("\n");
    expect(turn.a).not.toContain("map[");
    expect(turn.a).toContain("You exceeded your current quota");
    expect(turn.detail).toBe(WALL);
  });

  it("cuts a message that is one unbroken run of characters just the same", () => {
    const unbroken = "x".repeat(5000);
    const { view: next } = step(asked(), {
      kind: "daemonEvent",
      ev: { id: "1", type: "error", text: unbroken },
    });
    expect(next.matters[0].turns[0].a.length).toBeLessThanOrEqual(160);
    expect(next.matters[0].turns[0].detail).toBe(unbroken);
  });
});

describe("mattersLoaded", () => {
  function row(partial: Partial<MatterRow> = {}): MatterRow {
    return {
      id: "a",
      title: "a",
      kind: "action",
      status: "open",
      when: "2026-09-04T09:00:00Z",
      detail: "",
      ...partial,
    };
  }

  it("replaces the matters list with rows mapped to Matter, carrying status and kind", () => {
    const v = view({ matters: [matter({ id: "old" })] });
    const rows = [
      row({ id: "a", title: "Send the invoice" }),
      row({ id: "b", kind: "meeting", status: "watching" }),
    ];
    const { view: next } = step(v, { kind: "mattersLoaded", rows });
    expect(next.matters).toEqual([
      {
        id: "a",
        title: "Send the invoice",
        context: "",
        turns: [],
        status: "open",
        kind: "action",
      },
      {
        id: "b",
        title: "a",
        context: "",
        turns: [],
        status: "watching",
        kind: "meeting",
      },
    ]);
  });

  it("keeps the current index valid when the list shrinks", () => {
    const v = view({
      matters: [matter({ id: "a" }), matter({ id: "b" })],
      current: 1,
    });
    const { view: next } = step(v, {
      kind: "mattersLoaded",
      rows: [row({ id: "a" })],
    });
    expect(next.current).toBe(0);
  });

  it("keeps one placeholder matter when the daemon lists none, so the next question still has somewhere to go", () => {
    const v = view({
      matters: [matter({ id: "now" })],
      input: "what changed?",
    });
    const { view: emptied } = step(v, { kind: "mattersLoaded", rows: [] });
    expect(emptied.matters.length).toBe(1);
    const { view: asked, effect } = step(emptied, { kind: "submit" });
    expect(asked.matters[0].turns.map((t) => t.q)).toEqual(["what changed?"]);
    expect(effect?.kind).toBe("ask");
  });

  // The list is read again every time the hover is shown. It used to keep only questions still waiting for an answer, so asking something, switching to another window to check on it and coming back threw the answer away.
  it("keeps an answered exchange through a reload", () => {
    const v = view({
      matters: [matter({ id: "a", turns: [{ q: "q", a: "an answer" }] })],
      state: "answered",
    });
    const { view: next } = step(v, {
      kind: "mattersLoaded",
      rows: [row({ id: "a" })],
    });
    expect(next.matters[0].turns).toEqual([{ q: "q", a: "an answer" }]);
    expect(next.state).toBe("answered");
  });

  it("stays in the asking state when the matter it kept is still mid-question", () => {
    const v = view({
      matters: [matter({ id: "a", turns: [{ q: "q", a: "" }] })],
      state: "asking",
    });
    const { view: next } = step(v, {
      kind: "mattersLoaded",
      rows: [row({ id: "a" })],
    });
    expect(next.state).toBe("asking");
  });

  it("keeps the same matter open when the reload returns the list in a different order", () => {
    // "a" is the one on screen (current points at it by index 1); the reload comes back with "a" first, so a raw index carried across the merge would land on "b" instead.
    const v = view({
      matters: [matter({ id: "b" }), matter({ id: "a" })],
      current: 1,
    });
    const { view: next } = step(v, {
      kind: "mattersLoaded",
      rows: [row({ id: "a" }), row({ id: "b" })],
    });
    expect(next.matters[next.current].id).toBe("a");
  });

  it("keeps a matter that is mid-question instead of replacing it from the row", () => {
    const asking = matter({
      id: "a",
      title: "a",
      turns: [{ q: "is the venue sorted?", a: "" }],
    });
    const v = view({ matters: [asking], current: 0 });
    const { view: next } = step(v, {
      kind: "mattersLoaded",
      rows: [row({ id: "a", title: "renamed" })],
    });
    expect(next.matters).toEqual([asking]);
  });
});

describe("contextLoaded", () => {
  it("stores the chip text as app · title and the raw text for the next ask", () => {
    const ctx: ContextInfo = {
      app: "Mail",
      title: "Re: venue",
      text: "full body text",
    };
    const { view: next } = step(view(), { kind: "contextLoaded", ctx });
    expect(next.contextChip).toBe("Re: venue · Mail");
    expect(next.contextText).toBe("full body text");
  });

  it("delegates a multi-segment title to chipLabel", () => {
    const ctx: ContextInfo = {
      app: "Google Chrome",
      title: "Chat | Akshay Rathod | Microsoft Teams",
      text: "",
    };
    const { view: next } = step(view(), { kind: "contextLoaded", ctx });
    expect(next.contextChip).toBe("Akshay Rathod · Teams");
  });
});

describe("chipLabel", () => {
  it("pairs the specific segment with the shortened site name for a multi-segment title", () => {
    expect(
      chipLabel("Google Chrome", "Chat | Akshay Rathod | Microsoft Teams"),
    ).toBe("Akshay Rathod · Teams");
  });

  it("shows the app alone when the title is empty", () => {
    expect(chipLabel("Finder", "")).toBe("Finder");
  });

  it.each([
    [
      "Google Chrome",
      "Chat | Priya Shah | Microsoft Teams - High memory usage - 920 MB",
      "Priya Shah · Teams",
    ],
    ["Google Chrome", "Q3 hiring plan — Docs", "Q3 hiring plan · Docs"],
    ["Thunderbird", "Inbox (3) - Mail", "Inbox · Mail"],
    ["Code", "upload.go — ora — Visual Studio Code", "upload.go · Code"],
    [
      "Brave-browser",
      "Meet - abc-defg-hij - Microphone recording - Brave",
      "Meet · Brave",
    ],
    ["Finder", "", "Finder"],
    ["Mail", "Re: venue", "Re: venue · Mail"],
  ])("turns %s / %s into the chip", (app, title, want) => {
    expect(chipLabel(app, title)).toBe(want);
  });

  it("caps the label at 34 characters with a trailing ellipsis", () => {
    const label = chipLabel("Mail", "x".repeat(40));
    const runes = Array.from(label);
    expect(runes.length).toBe(34);
    expect(runes[runes.length - 1]).toBe("…");
  });
});

describe("sourceMeta", () => {
  it("drops the part that only repeats the title and shortens the timestamp", () => {
    expect(sourceMeta("note", "note · 2026-08-31T06:25:01Z")).toEqual([
      "31 Aug 2026",
    ]);
  });

  it("keeps a kind that says something the title does not", () => {
    expect(
      sourceMeta("Re: venue for the 12th", "mail · 2026-08-31T06:25:01Z"),
    ).toEqual(["mail", "31 Aug 2026"]);
  });

  it("returns nothing when the meta is only the title", () => {
    expect(sourceMeta("note", "note")).toEqual([]);
  });

  it("leaves a meta part that is not a timestamp alone", () => {
    expect(sourceMeta("Re: venue", "mail · Priya Nair 08:40")).toEqual([
      "mail",
      "Priya Nair 08:40",
    ]);
  });
});

describe("dictation", () => {
  it("dictating turns the listening look on and clears any leftover hint", () => {
    const { view: next } = step(view({ hint: "Heard nothing." }), {
      kind: "dictating",
    });
    expect(next.dictating).toBe(true);
    expect(next.hint).toBe("");
  });

  it("dictated puts the trimmed text in the input and closes the listening look", () => {
    const held = step(view(), { kind: "dictating" }).view;
    const { view: next } = step(held, {
      kind: "dictated",
      text: "  book the venue for Tuesday  ",
    });
    expect(next.input).toBe("book the venue for Tuesday");
    expect(next.dictating).toBe(false);
    expect(next.hint).toBe("");
  });

  it("dictated with nothing said leaves the input alone and hints", () => {
    const held = step(view({ input: "" }), { kind: "dictating" }).view;
    const { view: next } = step(held, { kind: "dictated", text: "   " });
    expect(next.input).toBe("");
    expect(next.dictating).toBe(false);
    expect(next.hint).toBe("Heard nothing.");
  });

  it("dictationFailed hints and closes the listening look", () => {
    const held = step(view(), { kind: "dictating" }).view;
    const { view: next } = step(held, { kind: "dictationFailed" });
    expect(next.dictating).toBe(false);
    expect(next.hint).toBe("Dictation failed.");
  });

  it("hint clears the hint again once the second is up", () => {
    const v = view({ hint: "Heard nothing." });
    expect(step(v, { kind: "hint", text: "" }).view.hint).toBe("");
  });
});

describe("placeholder", () => {
  // Neither key is written anywhere on the hover, so a user who does not already know about Space and Shift+Space has no way to find out that dictation and voice exist. The resting placeholder is the one line that is on screen before anybody acts.
  it("names the two voice keys while the input is empty and nothing is running", () => {
    expect(placeholder(view())).toBe(
      "Ask about this window, or anything. Space to dictate · Shift+Space for voice",
    );
  });

  it("drops the key hint once there is something in the input", () => {
    expect(placeholder(view({ input: "what is this" }))).toBe(
      ASK_PLACEHOLDER,
    );
  });

  it("drops the key hint while an ask is in flight", () => {
    expect(placeholder(view({ state: "asking" }))).toBe(ASK_PLACEHOLDER);
  });

  it("says it is listening while the key is held", () => {
    expect(placeholder(view({ dictating: true }))).toBe(
      "Listening… Space or Enter to stop",
    );
  });

  it("shows the hint after a dictation that gave nothing back", () => {
    expect(placeholder(view({ hint: "Heard nothing." }))).toBe(
      "Heard nothing.",
    );
  });

  it("says how to stop while a voice session is live", () => {
    expect(
      placeholder(view({ voice: "voice-1", voiceState: "listening" })),
    ).toBe("Live voice on · Shift+Space to stop");
  });
});

describe("faceState", () => {
  // The face beside the input names the same state the dot did, so the two never disagree.
  it("names the state the input is in", () => {
    expect(faceState(view({ dictating: true }), true)).toBe("listening");
    expect(faceState(view({ voice: "voice-1", voiceState: "speaking" }), true)).toBe("speaking");
    expect(faceState(view({ voice: "voice-1", voiceState: "thinking" }), true)).toBe("thinking");
    expect(faceState(view({ voice: "voice-1", voiceState: "listening" }), true)).toBe("listening");
    expect(faceState(view({ state: "asking" }), true)).toBe("thinking");
    expect(faceState(view({ state: "answered" }), true)).toBe("done");
    expect(faceState(view(), true)).toBe("watching");
    expect(faceState(view(), false)).toBe("asleep");
  });
});

describe("dotLabel", () => {
  // The dot's five states differ only by colour, so the label is the only thing a screen reader gets and the only thing a hover tooltip can say. It has to walk the same branches in the same order as faceState, or the face and the words disagree.
  it("names every state faceState distinguishes, in the same order", () => {
    expect(dotLabel(view({ dictating: true }), true)).toBe("Listening");
    expect(
      dotLabel(view({ voice: "voice-1", voiceState: "listening" }), true),
    ).toBe("Listening");
    expect(
      dotLabel(view({ voice: "voice-1", voiceState: "thinking" }), true),
    ).toBe("Thinking");
    expect(
      dotLabel(view({ voice: "voice-1", voiceState: "speaking" }), true),
    ).toBe("Speaking");
    expect(dotLabel(view({ state: "asking" }), true)).toBe("Working");
    expect(dotLabel(view({ state: "answered" }), true)).toBe("Answered");
    expect(dotLabel(view(), true)).toBe("Ready");
  });

  // The review asked for these exact words, and the footer tag beside the dot uses them too.
  it("says Not connected when the daemon is unreachable", () => {
    expect(dotLabel(view(), false)).toBe("Not connected");
  });
});

describe("voice", () => {
  function live(): View {
    return step(view(), { kind: "voiceOn", id: "voice-1" }).view;
  }

  function say(v: View, type: DaemonEvent["type"], text: string): View {
    return step(v, { kind: "voiceEvent", ev: { id: "voice-1", type, text } })
      .view;
  }

  it("voiceOn records the session id and starts it listening", () => {
    const v = live();
    expect(v.voice).toBe("voice-1");
    expect(v.voiceState).toBe("listening");
  });

  // Shift+Space and Space alone are one key apart, so a slipped modifier can open a live session without anyone meaning to; the hint has to say so the first time, once, not on every toggle.
  it("the first voiceOn shows a one-second hint that live voice started", () => {
    const v = live();
    expect(v.hint).toBe("Live voice started");
    expect(v.voiceHintShown).toBe(true);
  });

  it("a later voiceOn in the same window does not repeat the hint", () => {
    const off = step(live(), { kind: "voiceOff" }).view;
    const second = step(off, { kind: "voiceOn", id: "voice-2" }).view;
    expect(second.hint).toBe("");
    expect(second.voice).toBe("voice-2");
  });

  it("voiceOff clears the session", () => {
    const v = step(live(), { kind: "voiceOff" }).view;
    expect(v.voice).toBe("");
    expect(v.voiceState).toBe("idle");
  });

  // main.ts smooths these into the braille bar itself; the reducer's whole job is to hand the raw reading back out unchanged.
  it("a level event stores the mic and speaker reading", () => {
    const v = step(live(), {
      kind: "voiceEvent",
      ev: {
        id: "voice-1",
        type: "level",
        detail: JSON.stringify({ mic: 0.6, speaker: 0.2 }),
      },
    }).view;
    expect(v.voiceLevel).toEqual({ mic: 0.6, speaker: 0.2 });
  });

  it("a malformed level reading is stored as silence rather than thrown", () => {
    const v = step(live(), {
      kind: "voiceEvent",
      ev: { id: "voice-1", type: "level", detail: "not json" },
    }).view;
    expect(v.voiceLevel).toEqual({ mic: 0, speaker: 0 });
  });

  it("voiceOff clears the last level reading too", () => {
    const withLevel = step(live(), {
      kind: "voiceEvent",
      ev: {
        id: "voice-1",
        type: "level",
        detail: JSON.stringify({ mic: 0.6, speaker: 0.2 }),
      },
    }).view;
    const v = step(withLevel, { kind: "voiceOff" }).view;
    expect(v.voiceLevel).toBeUndefined();
  });

  // The session outlives the window: hiding it is not a way to end it, because the user hides the window to get on with what they were doing while Ora is still listening.
  it("escape hides the window and leaves the session running", () => {
    const { view: next, effect } = step(live(), { kind: "escape" });
    expect(effect).toEqual({ kind: "close" });
    expect(next.voice).toBe("voice-1");
    expect(next.voiceState).toBe("listening");
  });

  // The daemon ends the session itself when the user says "stop", and this event is the only thing that tells the window about it.
  it("an idle state event ends the session", () => {
    const v = say(live(), "state", "idle");
    expect(v.voice).toBe("");
    expect(v.voiceState).toBe("idle");
  });

  it("the placeholder and the face go back to their resting look once the session is idle", () => {
    const v = say(live(), "state", "idle");
    expect(placeholder(v)).toBe(RESTING_PLACEHOLDER);
    expect(faceState(v, true)).toBe("watching");
  });

  it("a state event moves the face", () => {
    expect(say(live(), "state", "speaking").voiceState).toBe("speaking");
  });

  it("heard opens a you turn and said fills its answer", () => {
    let v = say(live(), "heard", "is the venue sorted?");
    v = say(v, "said", "Nearly.");
    v = say(v, "said", " Priya said yes.");
    expect(v.matters[0].turns).toEqual([
      { q: "is the venue sorted?", a: "Nearly. Priya said yes." },
    ]);
  });

  it("a run of heard chunks joins into one turn", () => {
    let v = say(live(), "heard", "is the venue");
    v = say(v, "heard", "sorted?");
    expect(v.matters[0].turns).toEqual([{ q: "is the venue sorted?", a: "" }]);
  });

  it("the next heard after an answer starts a new turn", () => {
    let v = say(live(), "heard", "first");
    v = say(v, "said", "ok");
    v = say(v, "heard", "second");
    expect(v.matters[0].turns.map((t) => t.q)).toEqual(["first", "second"]);
  });

  it("said with nothing heard yet still opens a turn", () => {
    expect(say(live(), "said", "Morning.").matters[0].turns).toEqual([
      { q: "", a: "Morning." },
    ]);
  });

  it("tool hangs the tool name off the turn on screen", () => {
    let v = say(live(), "heard", "what is on today?");
    v = say(v, "tool", "search_memory");
    expect(v.matters[0].turns[0].tools).toEqual(["search_memory"]);
  });
});

describe("toggleSteps", () => {
  it("flips stepsOpen", () => {
    const v1 = step(view(), { kind: "toggleSteps" }).view;
    expect(v1.stepsOpen).toBe(true);
    const v2 = step(v1, { kind: "toggleSteps" }).view;
    expect(v2.stepsOpen).toBe(false);
  });

  it("tab closes it, same as it closes the evidence fold", () => {
    const v = step(
      view({ stepsOpen: true, matters: [matter(), matter({ id: "m2" })] }),
      { kind: "tab" },
    ).view;
    expect(v.stepsOpen).toBe(false);
  });
});

// applyToolEvent, failRunningStep and stepSeconds back the live step list (see the "tool" and "error" cases of the daemonEvent reducer above); these pin their behaviour directly so a change to the shape of a step is caught here rather than only through the reducer's own tests.
describe("applyToolEvent", () => {
  it("opens a step with no finish time", () => {
    const steps = applyToolEvent(
      [],
      { id: "1", type: "tool", text: "click", detail: "" },
      1000,
    );
    expect(steps).toEqual([{ name: "click", detail: "", startedAt: 1000 }]);
  });

  it("closes the open step without touching its name or detail", () => {
    const opened: ToolStep[] = [{ name: "click", detail: "", startedAt: 1000 }];
    const steps = applyToolEvent(
      opened,
      { id: "1", type: "tool", text: "click", detail: "done" },
      1200,
    );
    expect(steps).toEqual([
      { name: "click", detail: "", startedAt: 1000, finishedAt: 1200 },
    ]);
  });

  it("a closed step does not get reopened by the next event; a new one opens instead", () => {
    const closed: ToolStep[] = [
      { name: "click", detail: "", startedAt: 1000, finishedAt: 1200 },
    ];
    const steps = applyToolEvent(
      closed,
      { id: "1", type: "tool", text: "scroll_to", detail: "" },
      1300,
    );
    expect(steps).toEqual([
      ...closed,
      { name: "scroll_to", detail: "", startedAt: 1300 },
    ]);
  });
});

describe("failRunningStep", () => {
  it("marks the open step failed", () => {
    const steps = failRunningStep(
      [{ name: "click", detail: "", startedAt: 1000 }],
      "could not click",
      1400,
    );
    expect(steps).toEqual([
      {
        name: "click",
        detail: "",
        startedAt: 1000,
        finishedAt: 1400,
        error: "could not click",
      },
    ]);
  });

  it("no open step, nothing changes", () => {
    const closed: ToolStep[] = [
      { name: "click", detail: "", startedAt: 1000, finishedAt: 1200 },
    ];
    expect(failRunningStep(closed, "too late", 1400)).toEqual(closed);
    expect(failRunningStep([], "nothing ran", 1400)).toEqual([]);
  });
});

describe("stepSeconds", () => {
  it("against finishedAt once the step is done", () => {
    expect(
      stepSeconds(
        { name: "click", detail: "", startedAt: 1000, finishedAt: 3500 },
        9999999,
      ),
    ).toBe(2.5);
  });

  it("against now while still running", () => {
    expect(
      stepSeconds({ name: "click", detail: "", startedAt: 1000 }, 4200),
    ).toBe(3.2);
  });
});

describe("stepIconKind and stepLabel", () => {
  it("maps the eight named kinds", () => {
    expect(stepIconKind("observe_screen")).toBe("look");
    expect(stepIconKind("point_at")).toBe("point");
    expect(stepIconKind("click")).toBe("click");
    expect(stepIconKind("type_text")).toBe("type");
    expect(stepIconKind("scroll_to")).toBe("scroll");
    expect(stepIconKind("save_note")).toBe("note");
    expect(stepIconKind("query_memory")).toBe("search");
    expect(stepIconKind("open_url")).toBe("open");
  });

  it("an unlisted tool falls back to a generic step icon and its own name", () => {
    expect(stepIconKind("shell_exec")).toBe("step");
    expect(stepLabel("frobnicate_widget", "")).toBe("frobnicate widget");
  });

  it("splices an unquoted Detail into the label", () => {
    expect(stepLabel("query_memory", '"venue"')).toBe(
      "Searching memory for venue",
    );
    expect(stepLabel("observe_screen", "")).toBe("Looking at the screen");
  });

  it("a tool with no Detail falls back to the bare verb", () => {
    expect(stepLabel("click", "")).toBe("Clicking");
    expect(stepLabel("scroll_to", "")).toBe("Scrolling");
  });
});

describe("stepsSummaryLine", () => {
  it("counts steps and spans first start to last finish", () => {
    const steps: ToolStep[] = [
      { name: "click", detail: "", startedAt: 1000, finishedAt: 1500 },
      { name: "scroll_to", detail: "", startedAt: 1600, finishedAt: 6200 },
    ];
    expect(stepsSummaryLine(steps)).toBe("2 steps · 5.2 s");
  });

  it("one step reads singular", () => {
    expect(
      stepsSummaryLine([
        { name: "click", detail: "", startedAt: 0, finishedAt: 900 },
      ]),
    ).toBe("1 step · 0.9 s");
  });

  it("no steps at all renders nothing to collapse to", () => {
    expect(stepsSummaryLine([])).toBe("");
  });
});

// stepsCollapsed decides whether a finished turn's step list shows as the one-line summary or stays as the full list — the one bit of logic behind the collapse-on-answer visual (see main.ts's collapseStepsThenRender), so it gets its own tests rather than the CSS transition that plays it.
describe("stepsCollapsed", () => {
  const done: ToolStep[] = [
    { name: "click", detail: "", startedAt: 0, finishedAt: 500 },
    { name: "scroll_to", detail: "", startedAt: 500, finishedAt: 900 },
  ];

  it("collapses once every step is done and none failed", () => {
    expect(stepsCollapsed(done, false)).toBe(true);
    expect(stepsCollapsed(done, undefined)).toBe(true);
  });

  it("stays expanded if the user has clicked it back open", () => {
    expect(stepsCollapsed(done, true)).toBe(false);
  });

  it("stays expanded when a step failed, however stepsOpen is set", () => {
    const failed: ToolStep[] = [
      ...done.slice(0, 1),
      { ...done[1], finishedAt: 900, error: "could not scroll" },
    ];
    expect(stepsCollapsed(failed, false)).toBe(false);
    expect(stepsCollapsed(failed, true)).toBe(false);
  });

  it("nothing to collapse when no tool ran at all", () => {
    expect(stepsCollapsed([], false)).toBe(false);
  });
});

// The notice card: one of Ora's own moments — the morning brief, the evening close, a meeting prep — arriving on the daemon's event stream and shown as a speech bubble above the hover. It is not an answer to anything, so it has to leave whatever the user was in the middle of exactly as it was.
describe("notice", () => {
  const brief: Notice = {
    title: "Morning brief",
    body: "Two things are still open.",
    place: "tasks",
    id: "",
    kind: "brief",
  };
  const close: Notice = {
    title: "Day's written down",
    body: "A long day on the diary seam.",
    place: "days",
    id: "2026-09-05",
    kind: "close",
  };

  it("puts the card up on its own when the hover was not already open", () => {
    const { view: next } = step(view(), {
      kind: "notice",
      notice: brief,
      hoverOpen: false,
    });
    expect(next.notice).toEqual(brief);
    expect(next.noticeAlone).toBe(true);
  });

  // The user asked something and the answer is on screen; the notice stacks above that card rather than taking its place.
  it("stacks above the card when the hover is already open", () => {
    const asked = view({
      matters: [
        matter({ turns: [{ q: "is the venue sorted?", a: "Nearly." }] }),
      ],
      state: "answered",
    });
    const { view: next } = step(asked, {
      kind: "notice",
      notice: brief,
      hoverOpen: true,
    });
    expect(next.noticeAlone).toBe(false);
    expect(next.state).toBe("answered");
    expect(next.matters).toEqual(asked.matters);
  });

  it("leaves an ask in flight alone", () => {
    const asking = view({
      matters: [matter({ turns: [{ q: "q", a: "" }] })],
      state: "asking",
      input: "half a thought",
    });
    const { view: next } = step(asking, {
      kind: "notice",
      notice: brief,
      hoverOpen: true,
    });
    expect(next.state).toBe("asking");
    expect(next.input).toBe("half a thought");
    expect(next.matters).toEqual(asking.matters);
  });

  it("a second notice replaces the first and starts its timer again", () => {
    let v = step(view(), {
      kind: "notice",
      notice: brief,
      hoverOpen: false,
    }).view;
    v = step(v, { kind: "noticeHold" }).view;
    v = step(v, { kind: "notice", notice: close, hoverOpen: false }).view;
    expect(v.notice).toEqual(close);
    expect(v.noticeHeld).toBe(false);
  });

  it("the pointer going over the card holds it, and leaving lets it go again", () => {
    let v = step(view(), {
      kind: "notice",
      notice: brief,
      hoverOpen: false,
    }).view;
    v = step(v, { kind: "noticeHold" }).view;
    expect(v.noticeHeld).toBe(true);
    v = step(v, { kind: "noticeRelease" }).view;
    expect(v.noticeHeld).toBe(false);
  });

  it("noticeGone takes the card away", () => {
    const up = step(view(), {
      kind: "notice",
      notice: brief,
      hoverOpen: false,
    }).view;
    const { view: next } = step(up, { kind: "noticeGone" });
    expect(next.notice).toBeUndefined();
    expect(next.noticeAlone).toBe(false);
  });

  // The six seconds are counted in main.ts, but a timer that fires while the pointer is over the card must not be what takes it away: reading it is the reason it is being held.
  it("noticeGone leaves a held card up", () => {
    let v = step(view(), {
      kind: "notice",
      notice: brief,
      hoverOpen: false,
    }).view;
    v = step(v, { kind: "noticeHold" }).view;
    expect(step(v, { kind: "noticeGone" }).view.notice).toEqual(brief);
  });

  it("clicking it asks for the app window at the notice's own place and row, and takes the card away", () => {
    const up = step(view(), {
      kind: "notice",
      notice: close,
      hoverOpen: false,
    }).view;
    const { view: next, effect } = step(up, { kind: "noticeClick" });
    expect(effect).toEqual({
      kind: "openNotice",
      place: "days",
      id: "2026-09-05",
    });
    expect(next.notice).toBeUndefined();
  });

  it("a click with no card up asks for nothing", () => {
    expect(step(view(), { kind: "noticeClick" }).effect).toBeUndefined();
  });

  // The daily stale-task question comes with its own three answers instead of the usual Done/snooze set, so the reducer has to carry them from the wire to the card unchanged, and hand the pressed key back for the POST.
  it("keeps a notice's own actions and sends the pressed key to the daemon", () => {
    const ask: Notice = {
      title: "Still open",
      body: "Priya — send the invoice. Open since 28 Aug. Any progress?",
      place: "tasks",
      id: "42",
      kind: "task",
      actions: [
        { key: "done", label: "Done" },
        { key: "dropped", label: "Not happening" },
        { key: "later", label: "Not urgent" },
      ],
    };
    const up = step(view(), { kind: "notice", notice: ask, hoverOpen: false })
      .view;
    expect(up.notice?.actions).toEqual(ask.actions);
    const { view: next, effect } = step(up, {
      kind: "noticeAct",
      act: "dropped",
    });
    expect(effect).toEqual({ kind: "noticeAct", notice: ask, act: "dropped" });
    // The card stays up until the daemon's follow-up event, exactly as a snooze does.
    expect(next.notice).toEqual(ask);
  });
});

// noticeActionLine is what the desktop notification's own Done/snooze buttons turn a notice into, once the daemon sends the same notice back with that button's choice on it (see internal/proactive/notify.go's chose/snooze/markDone).
describe("noticeActionLine", () => {
  const now = new Date("2026-09-05T12:00:00");
  const base: Notice = {
    title: "Still open",
    body: "Send the invoice",
    place: "tasks",
    id: "42",
    kind: "task",
  };

  it("is undefined for a notice arriving fresh, which draws as it always has", () => {
    expect(noticeActionLine({ ...base, action: "" }, now)).toBeUndefined();
  });

  it("is 'Done' for one dismissed outright", () => {
    expect(noticeActionLine({ ...base, action: "done" }, now)).toBe("Done");
  });

  it("says the clock time for a snooze landing later the same day", () => {
    const n = { ...base, action: "snoozed", until: "2026-09-05T18:00:00" };
    expect(noticeActionLine(n, now)).toBe("Snoozed until 18:00");
  });

  it("says 'tomorrow' for a snooze that crosses midnight", () => {
    const n = { ...base, action: "snoozed", until: "2026-09-06T09:00:00" };
    expect(noticeActionLine(n, now)).toBe("Snoozed until tomorrow 09:00");
  });

  it("falls back to a bare 'Snoozed' for an until the daemon sent malformed", () => {
    const n = { ...base, action: "snoozed", until: "not-a-date" };
    expect(noticeActionLine(n, now)).toBe("Snoozed");
  });
});

// The hover is shown and hidden rather than reloaded, so the only way a theme picked in the app window after this page loaded can reach it is the storage event, which fires in every other page of the same origin when one of them writes. These are that event's two halves: what a stored value means, and which events are about the theme at all.
describe("the theme the app window stored", () => {
  it("takes light and dark as they are", () => {
    expect(themeChoice("light")).toBe("light");
    expect(themeChoice("dark")).toBe("dark");
  });

  it("counts nothing stored, and anything unrecognised, as system", () => {
    expect(themeChoice(null)).toBe("system");
    expect(themeChoice("")).toBe("system");
    expect(themeChoice("sepia")).toBe("system");
    expect(themeChoice("system")).toBe("system");
  });
});

describe("following a theme change through the storage event", () => {
  it("takes the choice the app window just wrote", () => {
    expect(themeFromStorage({ key: THEME_KEY, newValue: "light" })).toBe(
      "light",
    );
    expect(themeFromStorage({ key: THEME_KEY, newValue: "dark" })).toBe("dark");
  });

  // "system" comes back as "system" rather than as a colour, because resolving it is asking the desktop, which main.ts does and this file cannot.
  it("hands system back to be resolved against the desktop again", () => {
    expect(themeFromStorage({ key: THEME_KEY, newValue: "system" })).toBe(
      "system",
    );
  });

  it("ignores a write to any other key the two windows share", () => {
    expect(
      themeFromStorage({ key: "ora-hover-position", newValue: "top" }),
    ).toBeUndefined();
    expect(
      themeFromStorage({
        key: "ora-open-at",
        newValue: '{"place":"tasks","id":"k1"}',
      }),
    ).toBeUndefined();
  });

  it("falls back to system when the key is emptied or holds something unrecognised", () => {
    expect(themeFromStorage({ key: THEME_KEY, newValue: null })).toBe("system");
    expect(themeFromStorage({ key: THEME_KEY, newValue: "sepia" })).toBe(
      "system",
    );
  });

  // A clear() of the whole store arrives with no key at all, and it has taken the theme choice with it.
  it("goes back to system when the whole store is cleared", () => {
    expect(themeFromStorage({ key: null, newValue: null })).toBe("system");
  });
});

// Every question the hover asked used to open a conversation of its own, so "show me" came back as "Show you what?" and "again" as "Hey — I'm here": the model was handed an empty thread each time and had nothing to refer to. The daemon appends to a thread when the ask names one, so the reducer holds the id of the thread in hand and hands it to the next question.
describe("the conversation the hover asks in", () => {
  const T = 1_700_000_000_000;

  /** One matter with one answered turn in it, the shape the card is in after a question has been answered. */
  function answered(): View {
    return view({
      matters: [matter({ turns: [{ q: "show me", a: "Show you what?" }] })],
      state: "answered",
    });
  }

  it("names no conversation on the first question, which is what asks the daemon to open one", () => {
    const { view: next, effect } = step(
      view({ input: "show me" }),
      { kind: "submit" },
      T,
    );
    expect(effect).toEqual({
      kind: "ask",
      question: "show me",
      conversation: undefined,
    });
    expect(next.state).toBe("asking");
  });

  it("keeps the conversation the daemon opened, which comes back with the ask's own id", () => {
    const asking = step(view({ input: "show me" }), { kind: "submit" }, T).view;
    const next = step(
      asking,
      { kind: "asked", conversationId: "70" },
      T + 900,
    ).view;
    expect(next.conversationId).toBe("70");
  });

  it("names that conversation on the next question, so the follow-up has the earlier turns to read", () => {
    let v = step(view({ input: "show me" }), { kind: "submit" }, T).view;
    v = step(v, { kind: "asked", conversationId: "70" }, T + 900).view;
    v = step(
      v,
      { kind: "daemonEvent", ev: { id: "1", type: "done" } },
      T + 4000,
    ).view;
    v = step(v, { kind: "type", value: "again" }, T + 5000).view;
    const { effect } = step(v, { kind: "submit" }, T + 6000);
    expect(effect).toEqual({
      kind: "ask",
      question: "again",
      conversation: "70",
    });
  });

  it("keeps it across a window the user dismissed, which is the whole point of the few minutes", () => {
    let v = step(
      view({ conversationId: "70", conversationAt: T }),
      { kind: "escape" },
      T + 1000,
    ).view;
    v = step(v, { kind: "type", value: "again" }, T + 60_000).view;
    expect(step(v, { kind: "submit" }, T + 60_000).effect).toEqual({
      kind: "ask",
      question: "again",
      conversation: "70",
    });
  });

  it("still names it at the very edge of that window", () => {
    const v = view({ input: "again", conversationId: "70", conversationAt: T });
    expect(step(v, { kind: "submit" }, T + CONVERSATION_MS).effect).toEqual({
      kind: "ask",
      question: "again",
      conversation: "70",
    });
  });

  it("opens a new one once the thread has been quiet for longer than that", () => {
    const v = view({
      input: "and the other one?",
      conversationId: "70",
      conversationAt: T,
    });
    const { view: next, effect } = step(
      v,
      { kind: "submit" },
      T + CONVERSATION_MS + 1,
    );
    expect(effect).toEqual({
      kind: "ask",
      question: "and the other one?",
      conversation: undefined,
    });
    expect(next.conversationId).toBeUndefined();
  });

  // The clock runs from the last thing that happened in the thread, not from the question that started it, so a slow answer read slowly does not age the thread out from under the follow-up it invites.
  it("counts the quiet from the last answer, not from the first question", () => {
    let v = view({ input: "show me", conversationId: "70", conversationAt: T });
    v = step(v, { kind: "submit" }, T).view;
    v = step(
      v,
      { kind: "daemonEvent", ev: { id: "1", type: "done" } },
      T + CONVERSATION_MS,
    ).view;
    v = step(v, { kind: "type", value: "again" }, T + CONVERSATION_MS).view;
    expect(
      step(v, { kind: "submit" }, T + CONVERSATION_MS + 1000).effect,
    ).toEqual({ kind: "ask", question: "again", conversation: "70" });
  });

  it("starts a fresh thread on demand, without waiting the few minutes out", () => {
    const v = view({
      input: "something else entirely",
      conversationId: "70",
      conversationAt: T,
    });
    const { view: next, effect } = step(
      v,
      { kind: "enter", fresh: true },
      T + 1000,
    );
    expect(effect).toEqual({
      kind: "ask",
      question: "something else entirely",
      conversation: undefined,
    });
    expect(next.conversationId).toBeUndefined();
  });

  it("drops the thread when Tab moves to another matter, because that is another subject", () => {
    const v = view({
      matters: [matter({ id: "m1" }), matter({ id: "m2" })],
      conversationId: "70",
      conversationAt: T,
    });
    expect(
      step(v, { kind: "tab" }, T + 1000).view.conversationId,
    ).toBeUndefined();
  });

  it("asks for a thread only while one is in hand and still current", () => {
    expect(askConversation(view(), T)).toBeUndefined();
    expect(
      askConversation(
        view({ conversationId: "70", conversationAt: T }),
        T + CONVERSATION_MS,
      ),
    ).toBe("70");
    expect(
      askConversation(
        view({ conversationId: "70", conversationAt: T }),
        T + CONVERSATION_MS + 1,
      ),
    ).toBeUndefined();
  });

  it("leaves an answered card alone, since the thread is not what is on screen", () => {
    const { view: next } = step(
      answered(),
      { kind: "asked", conversationId: "71" },
      T,
    );
    expect(next.matters).toEqual(answered().matters);
    expect(next.state).toBe("answered");
  });
});

// The notice card's own buttons: Done and the three snoozes go to the daemon and leave the card standing, because the daemon can refuse them (a locked store answers 500) and a card taken down at the press would show a refusal as done; the daemon's follow-up event, the same notice with its action filled in, is what replaces it with the one-line confirmation. Open is the one that takes the card down at the press, because it leaves for the app window.
describe("noticeAct", () => {
  const task = { title: "Still open", body: "Send the invoice", place: "tasks", id: "42", kind: "task" };

  it("hands Done and the snoozes to the daemon and leaves the card up until the daemon answers", () => {
    const up = step(view(), { kind: "notice", notice: task, hoverOpen: false }).view;
    const { view: next, effect } = step(up, { kind: "noticeAct", act: "hour" });
    expect(effect).toEqual({ kind: "noticeAct", notice: task, act: "hour" });
    expect(next.notice).toEqual(task);
    expect(next.noticeAlone).toBe(true);
  });

  it("treats Open as the click it always was, and takes the card down with it", () => {
    const up = step(view(), { kind: "notice", notice: task, hoverOpen: false }).view;
    const { view: next, effect } = step(up, { kind: "noticeAct", act: "open" });
    expect(effect).toEqual({ kind: "openNotice", place: "tasks", id: "42" });
    expect(next.notice).toBeUndefined();
  });

  it("does nothing with no card up", () => {
    expect(step(view(), { kind: "noticeAct", act: "done" }).effect).toBeUndefined();
  });
});
