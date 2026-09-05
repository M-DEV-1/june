/** Tests for the reducers: what the window is showing, what is half-typed, what Escape gives up, and how a message off the daemon's stream folds into the question in flight. Every test builds its own store, so nothing leaks from one to the next. */

import { describe, expect, it, vi } from "vitest";

import type { ConversationSummary, DaemonEvent } from "./api";
import { events, oraApi } from "./api";
import { conversationsUi, escaped, makeStore, progress, settings, ui } from "./store";

describe("what the window is showing", () => {
  it("remembers where to come back to when Settings is opened, and Escape goes back there", () => {
    const store = makeStore({ ui: { place: "days" } });
    store.dispatch(ui.placeShown("settings"));
    expect(store.getState().ui.back).toBe("days");
    store.dispatch(escaped());
    expect(store.getState().ui.place).toBe("days");
  });

  it("never makes Settings its own back destination", () => {
    const store = makeStore({ ui: { place: "settings", back: "chats" } });
    store.dispatch(ui.placeShown("meetings"));
    expect(store.getState().ui.back).toBe("chats");
  });

  it("makes a foot row a toggle back to the chats", () => {
    const store = makeStore();
    store.dispatch(ui.footToggled("tasks"));
    expect(store.getState().ui.place).toBe("tasks");
    store.dispatch(ui.footToggled("tasks"));
    expect(store.getState().ui.place).toBe("chats");
  });

  it("opens a conversation from whichever place the rail was clicked in, and shuts the palette", () => {
    const store = makeStore({ ui: { place: "meetings", paletteOpen: true } });
    store.dispatch(ui.conversationOpened("c7"));
    expect(store.getState().ui).toMatchObject({ place: "chats", conversationId: "c7", paletteOpen: false });
    expect(store.getState().ui.back).toBe("meetings");
  });

  it("keeps each list's search apart", () => {
    const store = makeStore();
    store.dispatch(ui.searched({ list: "chats", text: "flight" }));
    store.dispatch(ui.searched({ list: "tasks", text: "file" }));
    expect(store.getState().ui.query).toEqual({ chats: "flight", tasks: "file", days: "", meetings: "" });
  });

  it("keeps what was typed per conversation, so switching chats does not lose it", () => {
    const store = makeStore();
    store.dispatch(ui.asked({ conversationId: "c1", text: "half a question" }));
    store.dispatch(ui.asked({ conversationId: "c2", text: "another" }));
    expect(store.getState().ui.ask).toEqual({ c1: "half a question", c2: "another" });
  });

  it("remembers the conversation opened for a task that had none", () => {
    const store = makeStore();
    store.dispatch(ui.taskChatOpened({ taskId: "12", conversationId: "c9" }));
    expect(store.getState().ui.taskChats).toEqual({ "12": "c9" });
  });

  it("opens and closes one reply's sources at a time", () => {
    const store = makeStore();
    store.dispatch(ui.railToggled("t1"));
    store.dispatch(ui.railToggled("t2"));
    expect(store.getState().ui.openRails).toEqual(["t1", "t2"]);
    store.dispatch(ui.railToggled("t1"));
    expect(store.getState().ui.openRails).toEqual(["t2"]);
  });

  it("toggles the palette when told nothing, and obeys when told which way", () => {
    const store = makeStore();
    store.dispatch(ui.paletteToggled(undefined));
    expect(store.getState().ui.paletteOpen).toBe(true);
    store.dispatch(ui.paletteToggled(false));
    expect(store.getState().ui.paletteOpen).toBe(false);
  });

  it("clears the notice before Escape does anything else", () => {
    const store = makeStore({ ui: { place: "settings", back: "chats" } });
    store.dispatch(ui.noticed("Could not delete"));
    store.dispatch(escaped());
    expect(store.getState().ui.notice).toBeUndefined();
    expect(store.getState().ui.place).toBe("settings");
    store.dispatch(escaped());
    expect(store.getState().ui.place).toBe("chats");
  });
});

describe("what is half-done to a conversation", () => {
  it("starts a rename with the title that is there now", () => {
    const store = makeStore();
    store.dispatch(conversationsUi.renameStarted({ id: "c1", title: "A chat" }));
    expect(store.getState().conversations).toMatchObject({ renamingId: "c1", draftTitle: "A chat" });
    store.dispatch(conversationsUi.draftTitleTyped("Another name"));
    expect(store.getState().conversations.draftTitle).toBe("Another name");
    store.dispatch(conversationsUi.renameEnded());
    expect(store.getState().conversations).toEqual({ draftTitle: "" });
  });

  it("gives up a rename and a pending delete on Escape", () => {
    const store = makeStore();
    store.dispatch(conversationsUi.renameStarted({ id: "c1", title: "A chat" }));
    store.dispatch(conversationsUi.deleteConfirmed("c2"));
    store.dispatch(escaped());
    expect(store.getState().conversations).toEqual({ draftTitle: "" });
  });
});

describe("the theme", () => {
  it("keeps the choice and what it resolved to apart", () => {
    const store = makeStore();
    store.dispatch(settings.themePicked("system"));
    store.dispatch(settings.themeResolved("dark"));
    expect(store.getState().settings).toEqual({ theme: "system", resolved: "dark" });
  });
});

/** One message off the stream, with only the fields a test cares about spelled out. */
function event(over: Partial<DaemonEvent>): DaemonEvent {
  return { id: "ask-1", type: "status", ...over } as DaemonEvent;
}

describe("the question in flight", () => {
  it("shows the question before the daemon has answered, and records the ask it turned into", () => {
    const store = makeStore();
    store.dispatch(progress.askSent({ conversationId: "c1", question: "what did she say?" }));
    expect(store.getState().progress.run).toMatchObject({ conversationId: "c1", question: "what did she say?", answer: "", steps: [] });
    store.dispatch(progress.askAccepted({ askId: "ask-1", conversationId: "c1" }));
    expect(store.getState().progress.run?.askId).toBe("ask-1");
  });

  it("takes the conversation the daemon opened when the window named none", () => {
    const store = makeStore();
    store.dispatch(progress.askSent({ conversationId: "", question: "a question" }));
    store.dispatch(progress.askAccepted({ askId: "ask-1", conversationId: "c9" }));
    expect(store.getState().progress.run?.conversationId).toBe("c9");
  });

  it("folds the working line, the tools and the answer into the run", () => {
    const store = makeStore();
    store.dispatch(progress.askSent({ conversationId: "c1", question: "what did she say?" }));
    store.dispatch(progress.askAccepted({ askId: "ask-1", conversationId: "c1" }));
    store.dispatch(progress.eventArrived(event({ type: "status", text: "Checking." })));
    expect(store.getState().progress.run?.status).toBe("Checking.");
    store.dispatch(progress.eventArrived(event({ type: "tool", text: "search_memory", detail: "looked for the deadline" })));
    expect(store.getState().progress.run?.steps).toEqual([{ name: "search_memory", detail: "looked for the deadline" }]);
    expect(store.getState().progress.run?.status).toBe("looked for the deadline");
    store.dispatch(progress.eventArrived(event({ type: "answer", text: "She said ", conversation_id: "c1", evidence: [{ title: "note", meta: "note · 3 Sep", body: "the deadline is Friday" }] })));
    store.dispatch(progress.eventArrived(event({ type: "answer", text: "Friday.", conversation_id: "c1" })));
    expect(store.getState().progress.run).toMatchObject({ answer: "She said Friday.", status: "" });
    expect(store.getState().progress.run?.evidence).toHaveLength(1);
  });

  it("ends the run when the daemon says it is done, and when it says it failed", () => {
    for (const type of ["done", "error"] as const) {
      const store = makeStore();
      store.dispatch(progress.askSent({ conversationId: "c1", question: "a question" }));
      store.dispatch(progress.eventArrived(event({ type })));
      expect(store.getState().progress.run).toBeUndefined();
    }
  });

  it("ignores a message that belongs to another question, or to no question at all", () => {
    const store = makeStore();
    store.dispatch(progress.eventArrived(event({ type: "status", text: "Checking." })));
    expect(store.getState().progress.run).toBeUndefined();

    store.dispatch(progress.askSent({ conversationId: "c1", question: "a question" }));
    store.dispatch(progress.askAccepted({ askId: "ask-1", conversationId: "c1" }));
    store.dispatch(progress.eventArrived(event({ id: "ask-2", type: "status", text: "somebody else's" })));
    store.dispatch(progress.eventArrived(event({ type: "answer", text: "not yours", conversation_id: "c2" })));
    expect(store.getState().progress.run).toMatchObject({ status: "", answer: "" });
  });

  it("gives up on a question the daemon never accepted", () => {
    const store = makeStore();
    store.dispatch(progress.askSent({ conversationId: "c1", question: "a question" }));
    store.dispatch(progress.askFailed());
    expect(store.getState().progress.run).toBeUndefined();
  });
});

describe("a computer-use job in flight", () => {
  it("folds its steps, its question and its closing spend into the job", () => {
    const store = makeStore();
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    expect(store.getState().progress.job).toMatchObject({ conversationId: "c1", goal: "reorder the slides", state: "planning", steps: [] });
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    expect(store.getState().progress.job?.id).toBe("act-1");

    store.dispatch(progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: "Clicking Slide 4", expect: "Risk showing" }) }));
    expect(store.getState().progress.job?.steps).toMatchObject([{ n: 1, text: "Clicking Slide 4", expect: "Risk showing" }]);
    expect(store.getState().progress.job?.state).toBe("stepping");

    store.dispatch(progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "verified", state: "stepping", text: "found it", outcome: "pass" }) }));
    expect(store.getState().progress.job?.steps[0]).toMatchObject({ outcome: "pass", why: "found it" });

    store.dispatch(progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "question", state: "stuck", text: "Which deck?" }) }));
    expect(store.getState().progress.job?.question).toBe("Which deck?");
    store.dispatch(progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "answered", state: "stepping", text: "the review one" }) }));
    expect(store.getState().progress.job?.question).toBeUndefined();

    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({ kind: "done", state: "done", text: "Done.", spend: { rounds: 2, input: 100, cached: 40, output: 20 } }),
      }),
    );
    expect(store.getState().progress.job).toMatchObject({ state: "done", say: "Done.", spend: { rounds: 2, input: 100, cached: 40, output: 20 } });
  });

  it("ignores an event carrying another job's id, or arriving with no job in flight", () => {
    const store = makeStore();
    store.dispatch(progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: "x" }) }));
    expect(store.getState().progress.job).toBeUndefined();

    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    store.dispatch(progress.eventArrived({ id: "act-2", type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: "somebody else's" }) }));
    expect(store.getState().progress.job?.steps).toEqual([]);
  });

  it("gives up on a job the daemon never accepted", () => {
    const store = makeStore();
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobFailed());
    expect(store.getState().progress.job).toBeUndefined();
  });

  it("patches the sidebar's own row with the job's state word while it runs, since the daemon knows nothing about a job being tied to a conversation", async () => {
    const store = makeStore();
    const rows: ConversationSummary[] = [{ id: "c1", title: "Reordering the slide deck", brain: "claude", last: "", updated: new Date().toISOString() }];
    // upsertQueryData is a thunk that writes the cache asynchronously, unlike every plain action above.
    await store.dispatch(oraApi.util.upsertQueryData("conversations", undefined, rows));

    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    expect(oraApi.endpoints.conversations.select(undefined)(store.getState()).data?.[0].last).toBe("Planning…");

    store.dispatch(progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: "Clicking Slide 4" }) }));
    expect(oraApi.endpoints.conversations.select(undefined)(store.getState()).data?.[0].last).toBe("Working…");

    // A job with nothing tying it to a row in the list — a fresh draft's, before its first message opened one — patches nothing.
    const untouched = oraApi.endpoints.conversations.select(undefined)(store.getState()).data?.[0];
    store.dispatch(progress.jobSent({ conversationId: "__draft__", goal: "x" }));
    expect(oraApi.endpoints.conversations.select(undefined)(store.getState()).data?.[0]).toEqual(untouched);
  });
});

describe("the event stream", () => {
  it("opens one stream however many times it is asked to, and feeds what arrives into the run", () => {
    const open = vi.fn((_onEvent: (ev: DaemonEvent) => void) => () => {});
    const store = makeStore(undefined, open);
    store.dispatch(progress.streamOpened());
    store.dispatch(progress.streamOpened());
    expect(open).toHaveBeenCalledTimes(1);
    expect(store.getState().progress.streaming).toBe(true);

    const onEvent = open.mock.calls[0][0];
    store.dispatch(progress.askSent({ conversationId: "c1", question: "a question" }));
    onEvent(event({ type: "status", text: "Checking." }));
    expect(store.getState().progress.run?.status).toBe("Checking.");
  });
});

// A "notice" event with its action set is the daemon sending back a task or routine notice once the user has pressed Done or a snooze button on its own desktop notification (see internal/proactive/notify.go). It says so on the sidebar's rail line, the one place every notice already surfaces (see ui.notice and its other callers in routines.tsx and App.tsx), rather than growing a second display of its own.
describe("a notice's action reaching the window", () => {
  it("says a task done from its own notification on the rail line", () => {
    const open = vi.fn((_onEvent: (ev: DaemonEvent) => void) => () => {});
    const store = makeStore(undefined, open);
    store.dispatch(progress.streamOpened());
    const onEvent = open.mock.calls[0][0];

    onEvent(
      event({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", action: "done", until: "" },
      }),
    );
    expect(store.getState().ui.notice).toBe("Send the invoice: Done");
  });

  it("tells the Tasks cache to read the list again for a task closed this way", () => {
    const invalidate = vi.spyOn(oraApi.util, "invalidateTags");
    const open = vi.fn((_onEvent: (ev: DaemonEvent) => void) => () => {});
    const store = makeStore(undefined, open);
    store.dispatch(progress.streamOpened());
    const onEvent = open.mock.calls[0][0];

    onEvent(
      event({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", action: "done", until: "" },
      }),
    );
    expect(invalidate).toHaveBeenCalledWith(["Task"]);
    invalidate.mockRestore();
  });

  it("says a snooze on the rail line, and does not touch the Tasks cache for a routine", () => {
    const invalidate = vi.spyOn(oraApi.util, "invalidateTags");
    const open = vi.fn((_onEvent: (ev: DaemonEvent) => void) => () => {});
    const store = makeStore(undefined, open);
    store.dispatch(progress.streamOpened());
    const onEvent = open.mock.calls[0][0];

    onEvent(
      event({
        id: "",
        type: "notice",
        notice: { title: "Routine", body: "Priya replied about the venue.", place: "", id: "7", kind: "routine", action: "snoozed", until: "2026-09-05T18:00:00" },
      }),
    );
    expect(store.getState().ui.notice).toBe("Priya replied about the venue.: Snoozed until 18:00");
    expect(invalidate).not.toHaveBeenCalledWith(["Task"]);
    invalidate.mockRestore();
  });

  it("reacts the same way when eventArrived is dispatched directly, the way the ?mock=1 fixture in mock.ts does it, without a live stream", () => {
    const store = makeStore();
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", action: "done", until: "" },
      }),
    );
    expect(store.getState().ui.notice).toBe("Send the invoice: Done");
  });

  it("leaves the rail line alone for a notice arriving fresh, with no action yet", () => {
    const open = vi.fn((_onEvent: (ev: DaemonEvent) => void) => () => {});
    const store = makeStore(undefined, open);
    store.dispatch(progress.streamOpened());
    const onEvent = open.mock.calls[0][0];

    onEvent(
      event({
        id: "",
        type: "notice",
        notice: { title: "Morning brief", body: "Two things are still open.", place: "tasks", id: "", kind: "brief" },
      }),
    );
    expect(store.getState().ui.notice).toBeUndefined();
  });
});

describe("the stream coming back", () => {
  it("does not call back on the first open, and calls back on every open that follows a drop", async () => {
    const opened: FakeSource[] = [];
    class FakeSource {
      onopen: (() => void) | null = null;
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(readonly url: string) {
        opened.push(this as unknown as FakeSource);
      }
      close() {}
    }
    vi.stubGlobal("EventSource", FakeSource);
    // These reducer tests run without a DOM, and reading the token falls back to the page's own URL when there is no Tauri to ask.
    vi.stubGlobal("location", { port: "", search: "" });
    vi.useFakeTimers();
    const back = vi.fn();
    const stop = events(() => {}, back);

    // The window's very first stream: every query is already fetching, so an open here is nothing to react to.
    await vi.advanceTimersByTimeAsync(1);
    opened[0].onopen?.();
    expect(back).not.toHaveBeenCalled();

    // A daemon restart: the stream drops, the retry two to three seconds later (there is up to a second of jitter) succeeds, and that is the moment the cache is stale.
    opened[0].onerror?.();
    await vi.advanceTimersByTimeAsync(3000);
    opened[1].onopen?.();
    expect(back).toHaveBeenCalledTimes(1);

    // A second restart in the same session must be caught the same way, which it only is if the drop was cleared on the way back in.
    opened[1].onerror?.();
    await vi.advanceTimersByTimeAsync(3000);
    opened[2].onopen?.();
    expect(back).toHaveBeenCalledTimes(2);

    stop();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("tells the cache to read every screen again once the stream is back", async () => {
    const invalidate = vi.spyOn(oraApi.util, "invalidateTags");
    let reopen: (() => void) | undefined;
    const store = makeStore(undefined, (_onEvent, onReopen) => {
      reopen = onReopen;
      return () => {};
    });
    store.dispatch(progress.streamOpened());
    await Promise.resolve();
    invalidate.mockClear();

    reopen?.();
    expect(invalidate).toHaveBeenCalledWith(["Conversation", "Task", "Day", "Meeting", "Settings", "Brain", "Usage", "Tracker", "Routine", "Job"]);
    invalidate.mockRestore();
  });
});
