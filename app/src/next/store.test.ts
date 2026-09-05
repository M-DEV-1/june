/** Tests for the reducers: what the window is showing, what is half-typed, what Escape gives up, and how a message off the daemon's stream folds into the question in flight. Every test builds its own store, so nothing leaks from one to the next. */

import { describe, expect, it, vi } from "vitest";

import type { DaemonEvent } from "./api";
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
