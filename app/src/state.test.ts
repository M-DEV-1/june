import { describe, expect, it } from "vitest";
import {
  CONVERSATION_MS,
  step,
  themeFromStorage,
  type JobMeta,
  type Matter,
  type View,
} from "./state";
import type { MatterRow } from "./daemon";
import { THEME_KEY } from "./shared/theme";
import type { DaemonEvent, Notice } from "./shared/wire";

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

describe("submit", () => {
  it("moves empty->asking and clears input", () => {
    const v = view({ input: "is the venue sorted?" });
    const { view: next } = step(v, { kind: "submit" });
    expect(next.state).toBe("asking");
    expect(next.input).toBe("");
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

});

describe("enter", () => {
  it("closes the window when the input is empty, because there is nothing to ask", () => {
    const { effect } = step(view({ input: "   " }), { kind: "enter" });
    expect(effect).toEqual({ kind: "close" });
  });

});

describe("daemonEvent", () => {
  function askedView(): View {
    return view({
      matters: [matter({ turns: [{ q: "is the venue sorted?", a: "" }] })],
      state: "asking",
    });
  }

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

  it("answer sets text and the full evidence list", () => {
    const ev: DaemonEvent = {
      id: "1",
      type: "answer",
      text: "Nearly. Vexil said yes for Tuesday.",
      evidence: [
        { title: "Re: venue", meta: "Vexil · 08:40", body: "..." },
        { title: "Re: parking", meta: "Vexil · 08:41", body: "..." },
      ],
    };
    const { view: next } = step(askedView(), { kind: "daemonEvent", ev });
    expect(next.matters[0].turns[0].a).toBe(
      "Nearly. Vexil said yes for Tuesday.",
    );
    expect(next.matters[0].turns[0].evidence).toEqual([
      { title: "Re: venue", meta: "Vexil · 08:40", body: "..." },
      { title: "Re: parking", meta: "Vexil · 08:41", body: "..." },
    ]);
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

describe("dictation", () => {
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

  it("a malformed level reading is stored as silence rather than thrown", () => {
    const v = step(live(), {
      kind: "voiceEvent",
      ev: { id: "voice-1", type: "level", detail: "not json" },
    }).view;
    expect(v.voiceLevel).toEqual({ mic: 0, speaker: 0 });
  });

  // The session outlives the window: hiding it is not a way to end it, because the user hides the window to get on with what they were doing while June is still listening.
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

  it("heard opens a you turn and said fills its answer", () => {
    let v = say(live(), "heard", "is the venue sorted?");
    v = say(v, "said", "Nearly.");
    v = say(v, "said", " Vexil said yes.");
    expect(v.matters[0].turns).toEqual([
      { q: "is the venue sorted?", a: "Nearly. Vexil said yes." },
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

});

// The notice card: one of June's own moments — the morning brief, the evening close, a meeting prep — arriving on the daemon's event stream and shown as a speech bubble above the hover. It is not an answer to anything, so it has to leave whatever the user was in the middle of exactly as it was.
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

  // The daily stale-task question comes with its own three answers instead of the usual Done/snooze set, so the reducer has to carry them from the wire to the card unchanged, and hand the pressed key back for the POST.
  it("keeps a notice's own actions and sends the pressed key to the daemon", () => {
    const ask: Notice = {
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

describe("following a theme change through the storage event", () => {
  it("takes the choice the app window just wrote", () => {
    expect(themeFromStorage({ key: THEME_KEY, newValue: "light" })).toBe(
      "light",
    );
    expect(themeFromStorage({ key: THEME_KEY, newValue: "dark" })).toBe("dark");
  });

  it("ignores a write to any other key the two windows share", () => {
    expect(
      themeFromStorage({ key: "june-hover-position", newValue: "top" }),
    ).toBeUndefined();
    expect(
      themeFromStorage({
        key: "june-open-at",
        newValue: '{"place":"tasks","id":"k1"}',
      }),
    ).toBeUndefined();
  });

});

// Every question the hover asked used to open a conversation of its own, so "show me" came back as "Show you what?" and "again" as "Hey — I'm here": the model was handed an empty thread each time and had nothing to refer to. The daemon appends to a thread when the ask names one, so the reducer holds the id of the thread in hand and hands it to the next question.
describe("the conversation the hover asks in", () => {
  const T = 1_700_000_000_000;

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

});
