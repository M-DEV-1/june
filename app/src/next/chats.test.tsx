// @vitest-environment jsdom

/** Tests for the Chats screen: the turns of a conversation, a failed ask and its reason, the sources folded under a reply, the question in flight with its tool steps, and the brain picker. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { Brain, ConversationSummary, ConversationView } from "./api";
import { DRAFT_CHAT, progress, ui } from "./store";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const when = new Date().toISOString();

/** The one conversation these tests open. */
const summary: ConversationSummary[] = [
  {
    id: "c1",
    title: "Flights to Zurich",
    brain: "claude",
    last: "booked",
    updated: when,
  },
];

/** That conversation with a question, an answer that read two things, and a failed ask under them. */
const view: ConversationView = {
  id: "c1",
  title: "Flights to Zurich",
  brain: "claude",
  turns: [
    {
      id: "t1",
      role: "you",
      text: "what did she say about the deadline?",
      kind: "ask",
      evidence: [],
      tools: [],
      when,
      reason: "",
    },
    {
      id: "t2",
      role: "june",
      text: "She said Friday.",
      kind: "ask",
      evidence: [
        {
          title: "Meridian call",
          meta: "meeting · 3 Sep",
          body: "the deadline is Friday",
        },
        { title: "A note", meta: "note · 2 Sep", body: "Friday, she said" },
      ],
      tools: ["search_memory"],
      when,
      reason: "",
    },
    {
      id: "t3",
      role: "june",
      text: "the provider returned 529\nand a wall of json",
      kind: "error",
      evidence: [],
      tools: [],
      when,
      reason: "The model was too busy to answer.",
    },
  ],
};

const brains: Brain[] = [
  {
    id: "claude",
    name: "Claude",
    signed_in: true,
    account: "max",
    models: ["opus", "sonnet"],
    model: "opus",
    note: "",
    default: true,
  },
  {
    id: "grok",
    name: "Grok",
    signed_in: false,
    account: "",
    models: [],
    model: "",
    note: "not set up on this machine",
    default: false,
  },
];

// A clean install, or one whose every login has lapsed, has nothing to answer with, and a question typed into the chat just failed. The chat page says so and lists every way June can be given a brain.
describe("with no brain signed in", () => {
  it("says so on the chat page and lists every way to sign one in", async () => {
    renderApp({
      conversations: summary,
      turns: { c1: view },
      brains: brains.map((b) => ({ ...b, signed_in: false })),
      settings: { data_dir: "/home/you/.local/share/june" },
    });
    expect(await screen.findByText("June cannot answer yet")).toBeDefined();
    const ways = Array.from(document.querySelectorAll("li")).map((li) => li.textContent);
    for (const way of ["claude", "/login", "codex login", "agy", "grok", "GEMINI_API_KEY", "/home/you/.local/share/june/env"])
      expect(ways.some((w) => w?.includes(way)), way).toBe(true);
  });
});

// A daemon restarted mid-question never sends that question's "done", and the composer held Send disabled until one arrived, so the window could ask nothing more until it was reloaded.
describe("a daemon that restarts mid-question", () => {
  it("lets the next question be sent once the stream is back", async () => {
    let reopen: (() => void) | undefined;
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1", open: (_onEvent, onReopen) => ((reopen = onReopen), () => {}) },
    );
    store.dispatch(progress.streamOpened());
    const box = await screen.findByLabelText("Ask June");
    await userEvent.type(box, "a question{Enter}");
    await userEvent.type(box, "the next one");
    await waitFor(() => expect(screen.getByRole("button", { name: "Send" }).hasAttribute("disabled")).toBe(true));

    reopen?.();

    await waitFor(() => expect(screen.getByRole("button", { name: "Send" }).hasAttribute("disabled")).toBe(false));
  });
});

describe("the thread", () => {
  it("draws what was said, and what June answered with the line saying how much it read", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    expect(
      await screen.findByText("what did she say about the deadline?"),
    ).toBeDefined();
    expect(screen.getByText("She said Friday.")).toBeDefined();
    expect(screen.getByRole("button", { name: /Sources/ })).toBeDefined();
    expect(screen.getByText(/search_memory/)).toBeDefined();
  });

  it("shows a failed ask as the daemon's own sentence, with the provider's whole message behind it", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    expect(
      await screen.findByText("The model was too busy to answer."),
    ).toBeDefined();
    expect(screen.queryByText(/wall of json/)).toBeNull();
    // The provider's own words are behind the same fold the sources sit behind, and open in the thread rather than beside it.
    await userEvent.click(
      screen.getByRole("button", { name: "The whole message" }),
    );
    expect(await screen.findByText(/wall of json/)).toBeDefined();
  });
});

describe("the sources under a reply", () => {
  it("counts them on a row that is shut to begin with, and folds them open under the answer", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const fold = await screen.findByRole("button", { name: /Sources/ });
    expect(fold.getAttribute("aria-expanded")).toBe("false");
    expect(fold.textContent).toContain("2");
    expect(screen.queryByText("the deadline is Friday")).toBeNull();
    await userEvent.click(fold);
    expect(await screen.findByText("the deadline is Friday")).toBeDefined();
    expect(screen.getByText("Friday, she said")).toBeDefined();
    expect(
      screen
        .getByRole("button", { name: /Sources/ })
        .getAttribute("aria-expanded"),
    ).toBe("true");
  });

  it("names the block it opens only while that block is in the document", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    // A closed fold that still points at the panel's id points at nothing: the panel is only rendered while it is open, and a screen reader offered a jump to it lands nowhere.
    const shut = await screen.findByRole("button", { name: /Sources/ });
    expect(shut.getAttribute("aria-controls")).toBeNull();

    await userEvent.click(shut);
    await screen.findByText("the deadline is Friday");
    const id = screen.getByRole("button", { name: /Sources/ }).getAttribute("aria-controls");
    expect(id).toBeTruthy();
    expect(document.getElementById(id!)).not.toBeNull();
  });

  it("offers no fold on a reply that read nothing", async () => {
    const nothing: ConversationView = {
      ...view,
      turns: [{ ...view.turns[1], id: "t9", evidence: [], tools: [] }],
    };
    renderApp(
      { conversations: summary, turns: { c1: nothing } },
      { conversationId: "c1" },
    );
    await screen.findByText(/read nothing — treat it that way/);
    expect(screen.queryByRole("button", { name: /Sources/ })).toBeNull();
  });
});

describe("the wide layout", () => {
  it("moves what a reply read out of the thread and into the rail beside it", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1", wide: true },
    );
    const rail = await screen.findByRole("complementary", {
      name: "What this answer is built on",
    });
    // The quote itself is beside the thread, and the fold that used to hold it inside the thread is gone.
    expect(within(rail).getByText("the deadline is Friday")).toBeDefined();
    expect(within(rail).getByText(/search_memory/)).toBeDefined();
    expect(screen.queryByRole("button", { name: /Sources/ })).toBeNull();
  });

  it("keeps the fold inside the thread in a narrow pane", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    expect(
      await screen.findByRole("button", { name: /Sources/ }),
    ).toBeDefined();
    expect(
      screen.queryByRole("complementary", {
        name: "What this answer is built on",
      }),
    ).toBeNull();
  });

  it("keeps the rail's column, empty, when nothing in the thread read or called anything, so the page never shifts between chats", async () => {
    const plain: ConversationView = {
      ...view,
      turns: [view.turns[0], { ...view.turns[1], evidence: [], tools: [] }],
    };
    renderApp(
      { conversations: summary, turns: { c1: plain } },
      { conversationId: "c1", wide: true },
    );
    // The reply is in the thread and again in the region a screen reader is read, which is why this counts them rather than expecting one.
    expect((await screen.findAllByText("She said Friday.")).length).toBe(2);
    expect(
      screen.queryByRole("complementary", {
        name: "What this answer is built on",
      }),
    ).toBeNull();
  });
});

describe("where a long thread opens", () => {
  /** Gives the scrolling region the size jsdom will not: a viewport 800px tall holding 4,000px of thread. Input: none — it finds the region by the turn inside it, since the sidebar's list of chats is a scrolling region too. Output: that region. */
  function tall(): HTMLElement {
    const view = screen
      .getByText("what did she say about the deadline?")
      .closest("[data-slot=scroll-area-viewport]") as HTMLElement;
    Object.defineProperty(view, "scrollHeight", {
      value: 4000,
      configurable: true,
    });
    Object.defineProperty(view, "clientHeight", {
      value: 800,
      configurable: true,
    });
    return view;
  }

  it("opens at the newest turn rather than at the top of the thread", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await screen.findByText("She said Friday.");
    const region = tall();
    // The thread only knows how tall it is once it has been laid out, so this is the first render that can act on it.
    store.dispatch(
      progress.askSent({ conversationId: "c1", question: "and the flights?" }),
    );
    await waitFor(() => expect(region.scrollTop).toBe(3200));
  });

  it("keeps a reader who scrolled up where they are when a new answer lands", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await screen.findByText("She said Friday.");
    const region = tall();
    region.scrollTop = 1000;
    region.dispatchEvent(new Event("scroll"));
    store.dispatch(
      progress.askSent({ conversationId: "c1", question: "and the flights?" }),
    );
    await screen.findByText("and the flights?");
    expect(region.scrollTop).toBe(1000);
  });

  it("follows the newest turn again for a reader who is already within 120px of the end", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await screen.findByText("She said Friday.");
    const region = tall();
    region.scrollTop = 3150;
    region.dispatchEvent(new Event("scroll"));
    store.dispatch(
      progress.askSent({ conversationId: "c1", question: "and the flights?" }),
    );
    await waitFor(() => expect(region.scrollTop).toBe(3200));
  });
});

describe("asking a question", () => {
  it("shows the question the moment it is sent and posts it to the daemon", async () => {
    const { calls, store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = await screen.findByLabelText("Ask June");
    await userEvent.type(box, "and what about the flights?{Enter}");
    expect(
      await screen.findByText("and what about the flights?"),
    ).toBeDefined();
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/ask")?.body).toMatchObject({
        question: "and what about the flights?",
        conversation_id: "c1",
      }),
    );
    await waitFor(() =>
      expect(store.getState().progress.run?.askId).toBe("ask-1"),
    );
    // What was typed is cleared once it has been sent, so the box is ready for the next question.
    await waitFor(() => expect((box as HTMLInputElement).value).toBe(""));
  });

  it("says on one line when the daemon would not take the question, stops waiting, and puts what was typed back in the box", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view }, fails: ["POST /ask"] },
      { conversationId: "c1" },
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await userEvent.type(box, "a long question worth not retyping{Enter}");
    expect(await screen.findByRole("status")).toHaveProperty(
      "textContent",
      "Could not send that question",
    );
    expect(store.getState().progress.run).toBeUndefined();
    // The run held the only other copy of the sentence and it has just been given up, so a box left empty here loses it for good.
    await waitFor(() => expect(box.value).toBe("a long question worth not retyping"));
  });

  it("sends nothing on Enter while a question is still running, the same as the Send button", async () => {
    const { calls, store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await userEvent.type(box, "and what about the flights?{Enter}");
    await waitFor(() => expect(store.getState().progress.run?.askId).toBe("ask-1"));

    await userEvent.type(box, "and the hotel?{Enter}");
    expect(calls.filter((c) => c.path === "/ask")).toHaveLength(1);
    // The second question is still there to send once the first has finished, rather than gone with the run it would have replaced.
    expect(box.value).toBe("and the hotel?");
  });

  it("puts the goal back in the box when the daemon would not start the job", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view }, fails: ["POST /act"] },
      { conversationId: "c1" },
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await userEvent.type(box, "do: reorder the slides{Enter}");
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Could not start that job");
    await waitFor(() => expect(box.value).toBe("do: reorder the slides"));
  });

  it("shows the working line, then the steps and the answer as they arrive, all in the thread", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await screen.findByText("She said Friday.");
    store.dispatch(
      progress.askSent({ conversationId: "c1", question: "and the flights?" }),
    );
    store.dispatch(
      progress.askAccepted({ askId: "ask-1", conversationId: "c1" }),
    );
    // The question in flight says it is working with the dot grid, the one shape that means June is busy, and no words of its own.
    await waitFor(() =>
      expect(screen.getAllByRole("img", { name: "June is working" })).toHaveLength(1),
    );
    expect(screen.queryByText("working…")).toBeNull();
    store.dispatch(
      progress.eventArrived({ id: "ask-1", type: "status", text: "Checking." }),
    );
    await waitFor(() =>
      expect(screen.getAllByText("Checking.").length).toBeGreaterThan(0),
    );
    store.dispatch(
      progress.eventArrived({
        id: "ask-1",
        type: "tool",
        text: "search_memory",
        detail: "looked for the flights",
      }),
    );
    const thread = await screen.findByRole("main");
    expect(within(thread).getAllByText("search_memory").length).toBeGreaterThan(
      0,
    );
    expect(within(thread).getByText("looked for the flights")).toBeDefined();
    store.dispatch(
      progress.eventArrived({
        id: "ask-1",
        type: "answer",
        text: "They are booked.",
        conversation_id: "c1",
      }),
    );
    expect(await screen.findByText("They are booked.")).toBeDefined();
    // Under the partial answer the same dot grid says the rest is still coming, in place of the old words.
    expect(screen.getAllByRole("img", { name: "June is working" })).toHaveLength(1);
    expect(screen.queryByText("still writing")).toBeNull();
  });

});

describe("a fresh chat draft", () => {
  // The front door said "Pick a chat on the left, or start a new one", which is an instruction about the furniture rather than anything June has to offer. The design he approved opens with the face, the time of day, one line saying June has been keeping track, and a few things it can actually do.
  it("greets by time of day on a new chat and offers something to do", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    store.dispatch(ui.chatDraftOpened());
    // The greeting itself is whichever one the clock says.
    expect(await screen.findByText(/^Good (morning|afternoon|evening)\.$/)).toBeDefined();
    expect(screen.getByText(/keeping track/)).toBeDefined();
    // Gone: the line about the furniture.
    expect(screen.queryByText(/Pick a chat on the left/)).toBeNull();
  });

  it("puts a suggestion in the composer rather than sending it, so it can be edited first", async () => {
    const { store, calls } = renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    store.dispatch(ui.chatDraftOpened());
    const chip = await screen.findByRole("button", { name: "Summarise my day" });
    await userEvent.click(chip);
    expect(store.getState().ui.ask[DRAFT_CHAT]).toBe("Summarise my day");
    // Nothing was asked: a suggestion is a starting point, not a command.
    expect(calls.find((c) => c.path === "/ask")).toBeUndefined();
  });

  it("takes a question on New chat, and opens the conversation the daemon names back", async () => {
    const { calls, store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await userEvent.click(screen.getByRole("button", { name: "New chat" }));
    const box = await screen.findByLabelText("Ask June");
    expect((box as HTMLTextAreaElement).disabled).toBe(false);
    await waitFor(() => expect(screen.queryByText("She said Friday.")).toBeNull());
    await userEvent.type(box, "book the flight{Enter}");
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/ask")?.body).toMatchObject({
        question: "book the flight",
        conversation_id: "",
      }),
    );
    await waitFor(() =>
      expect(store.getState().ui.conversationId).toBe("new-ask"),
    );
    expect(store.getState().ui.chatDraft).toBe(false);
  });
});

describe("giving up a draft", () => {
  it("clears a one-line draft on Escape straight away", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = await screen.findByLabelText("Ask June");
    await userEvent.type(box, "and the flights?");
    await userEvent.keyboard("{Escape}");
    expect((box as HTMLTextAreaElement).value).toBe("");
  });

  it("asks before clearing a draft that runs past one line, and leaves it when the answer is no", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = await screen.findByLabelText("Ask June");
    await userEvent.type(
      box,
      "and the flights?{Shift>}{Enter}{/Shift}book them too",
    );
    await userEvent.keyboard("{Escape}");
    expect(window.confirm).toHaveBeenCalled();
    expect((box as HTMLTextAreaElement).value).toContain("book them too");
  });
});

describe("dictating in the composer", () => {
  it("starts and stops a dictation through the daemon's mic routes, and drops the words into the box", async () => {
    const { calls } = renderApp(
      { conversations: summary, turns: { c1: view }, dictateText: "book the flight" },
      { conversationId: "c1" },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Dictate" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/dictate/start")).toBeDefined());
    const stop = await screen.findByRole("button", { name: "Stop dictation" });
    expect(screen.getByText("Listening…")).toBeDefined();
    await userEvent.click(stop);
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/dictate/stop")?.body).toEqual({ id: "dictate-1" }),
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await waitFor(() => expect(box.value).toBe("book the flight"));
  });

  it("stops a dictation on Escape as well as on a second click, without needing the box focused", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view }, dictateText: "yes" },
      { conversationId: "c1" },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Dictate" }));
    await screen.findByRole("button", { name: "Stop dictation" });
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop dictation" })).toBeNull());
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await waitFor(() => expect(box.value).toBe("yes"));
  });

  it("starts dictation on Space when the box is empty, and only types a space once there is already text in it", async () => {
    const { calls } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await userEvent.click(box);
    await userEvent.keyboard(" ");
    await waitFor(() => expect(calls.find((c) => c.path === "/dictate/start")).toBeDefined());
    await screen.findByRole("button", { name: "Stop dictation" });
    expect(box.value).toBe("");
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop dictation" })).toBeNull());
    await waitFor(() => expect(box.value).toBe("send this thought"));
    calls.length = 0;
    await userEvent.click(box);
    await userEvent.keyboard(" ");
    expect(box.value).toBe("send this thought ");
    expect(calls.find((c) => c.path === "/dictate/start")).toBeUndefined();
  });

  it("takes the words of a dictation the daemon ended by itself off the event stream, and stops listening", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Dictate" }));
    await screen.findByRole("button", { name: "Stop dictation" });
    // The daemon's silence gate closes the recording on its own and broadcasts the transcript (see finish in internal/ipc/dictate.go); nothing is going to answer a stop for it.
    store.dispatch(
      progress.eventArrived({ id: "dictate-1", type: "dictation", text: "book the flight to Zurich" }),
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await waitFor(() => expect(box.value).toBe("book the flight to Zurich"));
    expect(screen.queryByRole("button", { name: "Stop dictation" })).toBeNull();
  });

  it("stops the dictation on Escape with something already half-typed, and keeps what is typed", async () => {
    const { calls } = renderApp(
      { conversations: summary, turns: { c1: view }, dictateText: "and the hotel" },
      { conversationId: "c1" },
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await userEvent.type(box, "half a question");
    await userEvent.click(screen.getByRole("button", { name: "Dictate" }));
    await screen.findByRole("button", { name: "Stop dictation" });

    // The box is read-only rather than disabled while a recording is open, so it still takes the keypress: its own Escape, which gives up the draft, must not be what happens while the words the draft is waiting for are still being spoken.
    box.focus();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(calls.find((c) => c.path === "/dictate/stop")).toBeDefined());
    await waitFor(() => expect(box.value).toBe("half a question and the hotel"));
  });

  it("says nothing when the stop finds the daemon has already finished the dictation itself", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = (await screen.findByLabelText("Ask June")) as HTMLTextAreaElement;
    await userEvent.click(screen.getByRole("button", { name: "Dictate" }));
    await screen.findByRole("button", { name: "Stop dictation" });

    // The daemon's silence gate closes the recording between the press and the stop reaching it, and the stop route then answers 404 for a recording it no longer has; the words are already on their way as a "dictation" event, so there is nothing wrong to say.
    const daemon = globalThis.fetch;
    let stopped = false;
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(typeof input === "object" && "url" in input ? input.url : String(input)).pathname;
      if (path !== "/dictate/stop") return daemon(input, init);
      stopped = true;
      return Promise.resolve(new Response("no such recording", { status: 404 }));
    });

    await userEvent.click(screen.getByRole("button", { name: "Stop dictation" }));
    await waitFor(() => expect(stopped).toBe(true));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop dictation" })).toBeNull());
    expect(box.getAttribute("placeholder")).toBe("Ask June, or give it something to do");
  });

  it("says on the composer's own placeholder, for a few seconds, when the daemon would not start a dictation", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view }, fails: ["POST /dictate/start"] },
      { conversationId: "c1" },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Dictate" }));
    const box = await screen.findByLabelText("Ask June");
    await waitFor(() => expect(box.getAttribute("placeholder")).toBe("Could not start dictation"));
  });
});

describe("announcing a finished turn", () => {
  const oneReply: ConversationView = {
    ...view,
    turns: [view.turns[0], view.turns[1]],
  };

  it("says the answer once it is done, and stays quiet while it is still streaming in", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: oneReply } },
      { conversationId: "c1" },
    );
    const live = screen.getByLabelText("The latest reply");
    await waitFor(() => expect(live.textContent).toBe("She said Friday."));
    store.dispatch(
      progress.askSent({ conversationId: "c1", question: "and the flights?" }),
    );
    store.dispatch(
      progress.askAccepted({ askId: "ask-1", conversationId: "c1" }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "ask-1",
        type: "answer",
        text: "They",
        conversation_id: "c1",
      }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "ask-1",
        type: "answer",
        text: " are booked.",
        conversation_id: "c1",
      }),
    );
    await waitFor(() =>
      expect(screen.getByText("They are booked.")).toBeDefined(),
    );
    // The turn is still in flight — nothing has landed in the conversation itself yet — so the live region has not moved on to the streaming text token by token.
    expect(live.textContent).toBe("She said Friday.");
  });

  it("says a failed turn is a failed turn, once it is the last thing said", async () => {
    renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    await screen.findByText("The model was too busy to answer.");
    expect(screen.getByLabelText("The latest reply").textContent).toBe(
      "Could not answer that.",
    );
  });
});

describe("the brain picker", () => {
  it("names the brain that answers this conversation and writes the one that is picked", async () => {
    const { calls } = renderApp(
      { conversations: summary, turns: { c1: view }, brains },
      { conversationId: "c1" },
    );
    await userEvent.click(
      await screen.findByRole("button", { name: /Brain: Claude/ }),
    );
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByText("not signed in")).toBeDefined();
    await userEvent.click(
      within(menu).getByRole("menuitem", { name: /Claude/ }),
    );
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === "POST" && c.path === "/brains")?.body,
      ).toEqual({ brain: "claude", model: "opus" }),
    );
  });

  it("names the daemon's default brain when the conversation names none", async () => {
    renderApp(
      {
        conversations: [{ ...summary[0], brain: "" }],
        turns: { c1: { ...view, brain: "" } },
        brains,
      },
      { conversationId: "c1" },
    );
    expect(
      await screen.findByRole("button", { name: /Brain: Claude/ }),
    ).toBeDefined();
  });
});

describe("starting a job", () => {
  it("a message starting 'do:' starts a job instead of an ask, showing the goal as the turn", async () => {
    const { calls, store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    const box = await screen.findByLabelText("Ask June");
    await userEvent.type(box, "do: reorder the slides{Enter}");
    // The goal now reads in two places at once and both are wanted: the turn it was asked in, and the sidebar's running strip, which is what keeps a job visible from another screen. This one is about the turn.
    const asked = await screen.findAllByText("reorder the slides");
    expect(asked.some((el) => el.closest('[aria-label="Running now"]') === null)).toBe(true);
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/act")?.body).toEqual({
        goal: "reorder the slides",
        brain: "claude",
      }),
    );
    expect(calls.some((c) => c.path === "/ask")).toBe(false);
    await waitFor(() =>
      expect(store.getState().progress.jobs.c1.id).toBe("act-1"),
    );
  });

  // Stop and Pause fired and forgot: a daemon that refused them left the job running with nothing on screen saying the press had not taken.
  it("says so when the daemon will not stop the job", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view }, fails: ["POST /act/act-1/stop"] },
      { conversationId: "c1" },
    );
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    await userEvent.click(await screen.findByRole("button", { name: "Stop" }));
    expect(await screen.findByText("Could not stop that job")).toBeDefined();
  });

  // The plan the model writes on its first round lived only in its own prompt, so the thread opened with the goal handed back and went straight to "Step 1". Being told what it is about to do is what makes a step that wanders off it visible.
  it("shows the plan it opened with above the steps", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "plan",
          state: "stepping",
          step: 6,
          text: "open the deck, move Slide 4 up, check the order",
        }),
      }),
    );
    expect(await screen.findByText("open the deck, move Slide 4 up, check the order")).toBeDefined();
    expect(screen.getByText(/about 6 steps/)).toBeDefined();
  });

  it("shows each step as it arrives, with a tick once it checks out and a cross once it does not", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    store.dispatch(
      progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }),
    );
    store.dispatch(
      progress.jobAccepted({ id: "act-1", conversationId: "c1" }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "step",
          state: "stepping",
          text: "Clicking Slide 4",
          expect: 'an item labelled "Risk" is showing',
        }),
      }),
    );
    expect(await screen.findByText("Clicking Slide 4")).toBeDefined();
    expect(
      screen.getByText('expecting an item labelled "Risk" is showing'),
    ).toBeDefined();
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "verified",
          state: "stepping",
          text: 'the change came: found "Risk"',
          outcome: "pass",
        }),
      }),
    );
    expect(
      await screen.findByText('the change came: found "Risk"'),
    ).toBeDefined();
    expect(document.querySelector(".lucide-check")).not.toBeNull();

    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "step",
          state: "stepping",
          text: "Dragging Slide 4",
          expect: "the title contains \"Risk, Mitigation\"",
        }),
      }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "verified",
          state: "stepping",
          text: "the change did not come after 5s: still Mitigation, Risk",
          outcome: "fail",
        }),
      }),
    );
    expect(
      await screen.findByText(/the change did not come after/),
    ).toBeDefined();
    expect(document.querySelector(".lucide-x")).not.toBeNull();
  });

  it("draws a step whose check already held as neither a pass nor a fail", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({ kind: "step", state: "stepping", text: "Clicking Slide 4", expect: 'an item labelled "Risk" is showing' }),
      }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({ kind: "verified", state: "stepping", text: 'it was already showing before the click', outcome: "pass", held_before: true }),
      }),
    );

    // A check that held before the step took it proves nothing about the step, so it is neither ticked nor crossed.
    expect(await screen.findByText("it was already showing before the click")).toBeDefined();
    expect(screen.getByText("already held")).toBeDefined();
    expect(document.querySelector(".lucide-check")).toBeNull();
    expect(document.querySelector(".lucide-x")).toBeNull();
  });

  it("shows a stuck job's question, and answers it through the composer rather than asking", async () => {
    const { calls, store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    store.dispatch(
      progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }),
    );
    store.dispatch(
      progress.jobAccepted({ id: "act-1", conversationId: "c1" }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "question",
          state: "stuck",
          text: "Which deck do you mean?",
        }),
      }),
    );
    expect(await screen.findByText("Which deck do you mean?")).toBeDefined();
    const box = await screen.findByLabelText("Ask June");
    await userEvent.type(box, "the review deck{Enter}");
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/act/act-1/answer")?.body).toEqual(
        { text: "the review deck" },
      ),
    );
    expect(calls.some((c) => c.path === "/ask")).toBe(false);
  });

  it("stops and pauses a live job from its own controls", async () => {
    const { calls, store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    store.dispatch(
      progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }),
    );
    store.dispatch(
      progress.jobAccepted({ id: "act-1", conversationId: "c1" }),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Pause" }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === "POST" && c.path === "/act/act-1/pause"),
      ).toBe(true),
    );
    await userEvent.click(screen.getByRole("button", { name: "Stop" }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === "POST" && c.path === "/act/act-1/stop"),
      ).toBe(true),
    );
  });

  it("shows the closing text and what it cost once done, split by model when more than one served it", async () => {
    const { store } = renderApp(
      { conversations: summary, turns: { c1: view } },
      { conversationId: "c1" },
    );
    store.dispatch(
      progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }),
    );
    store.dispatch(
      progress.jobAccepted({ id: "act-1", conversationId: "c1" }),
    );
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({
          kind: "done",
          state: "done",
          text: "Done — Risk now sits before Mitigation.",
          spend: {
            rounds: 4,
            input: 12000,
            cached: 4000,
            output: 800,
            by_model: {
              opus: { model: "opus", input: 9000, cached: 3000, output: 600 },
              haiku: { model: "haiku", input: 3000, cached: 1000, output: 200 },
            },
          },
        }),
      }),
    );
    expect(
      await screen.findByText("Done — Risk now sits before Mitigation."),
    ).toBeDefined();
    expect(screen.getByText(/4 rounds/)).toBeDefined();
    expect(screen.getByText(/opus 9.6k/)).toBeDefined();
    expect(screen.getByText(/haiku 3.2k/)).toBeDefined();
    // A finished job offers no Stop or Pause any more.
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  });
});

describe("the wide layout for a job", () => {
  it("shows the job's plan and spend in the rail instead of sources", async () => {
    const { store } = renderApp(
      {
        conversations: summary,
        turns: { c1: view },
        jobs: {
          "act-1": {
            id: "act-1",
            goal: "reorder the slides",
            state: "stepping",
            plan: "Open the deck, then drag the slide above the one before it.",
            steps: [],
            question: "",
            say: "",
            err: "",
            spend: { rounds: 2, input: 5000, cached: 1000, output: 300 },
            elapsed_ms: 9000,
          },
        },
      },
      { conversationId: "c1", wide: true },
    );
    store.dispatch(
      progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }),
    );
    store.dispatch(
      progress.jobAccepted({ id: "act-1", conversationId: "c1" }),
    );
    const rail = await screen.findByRole("complementary", {
      name: "What this job is doing",
    });
    expect(
      within(rail).getByText(
        "Open the deck, then drag the slide above the one before it.",
      ),
    ).toBeDefined();
    expect(within(rail).getByText(/5.0k in/)).toBeDefined();
    expect(
      screen.queryByRole("complementary", {
        name: "What this answer is built on",
      }),
    ).toBeNull();
  });
});
