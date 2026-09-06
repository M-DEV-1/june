// @vitest-environment jsdom

/** Tests for the Tasks screen: the one wide list with no rail beside the sidebar, the picker in the header that names the task the composer is aimed at, the two status changes the daemon takes, and the composer that sends the task itself along with the question. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ConversationView, Task } from "./api";
import { openPicker, renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** The list on the page, so a title is looked for among the rows rather than in the header, which names the same task. */
function list() {
  return within(screen.getByRole("list", { name: "Tasks" }));
}

const when = new Date().toISOString();

/** One task the user typed in and two an agent noticed in the same meeting, all three the user's own — the tests below that predate the owner split expect one flat list, so all three stay in Mine. */
const tasks: Task[] = [
  { id: "task-1", title: "Book the flight", source: "you", when, done: false, conversation_id: "c1", detail: "", owner: "me" },
  { id: "12", title: "Send the TCFD file", source: "noticed", when, done: false, conversation_id: "", detail: "TCFD call", owner: "me" },
  { id: "13", title: "Book the room", source: "noticed", when, done: false, conversation_id: "", detail: "TCFD call", owner: "me" },
];

const conversation: ConversationView = {
  id: "c1",
  title: "Book the flight",
  brain: "claude",
  turns: [{ id: "t1", role: "you", text: "which airline?", kind: "ask", evidence: [], tools: [], when, reason: "" }],
};

describe("the list", () => {
  it("draws one wide list with no headings and no rail, saying where a noticed task came from", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    expect(list().getByText("Book the flight")).toBeDefined();
    expect(screen.getAllByText(/from TCFD call/)).toHaveLength(2);
    expect(screen.queryByText("You set")).toBeNull();
    expect(screen.queryByText("Ora noticed")).toBeNull();
  });

  it("gives every row and its tick a visible ring of its own for a keyboard user", async () => {
    renderApp({ tasks }, { place: "tasks" });
    const tick = await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    expect(tick.className).toContain("focus-visible:ring-2");
    expect(list().getByRole("button", { name: "Book the flight" }).className).toContain("focus-visible:ring-2");
  });

  it("draws the rows as a plain list, so the tick and the menus a row holds are controls in their own right rather than parts of one option", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    // An option must not hold interactive descendants, and every row holds at least two: the tick and the More menu. So there are no options on the page at all, only listitems, and the selectable part of each is a button of its own.
    expect(screen.queryAllByRole("option")).toHaveLength(0);
    expect(list().getAllByRole("listitem")).toHaveLength(3);
    const row = list().getByRole("button", { name: "Book the flight" });
    expect(row.getAttribute("data-row-id")).toBe("task-1");
    expect(within(row).queryByRole("checkbox")).toBeNull();
  });

  it("opens the task from anywhere on the row that is not one of its controls", async () => {
    const { store } = renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    // The row's own padding, the gaps and the column saying when it was raised are all part of the target: a row with no detail line is eight pixels tall, and only its title being clickable loses a third of its width.
    await userEvent.click(list().getAllByRole("listitem")[1]);
    await waitFor(() => expect(store.getState().ui.taskId).toBe("12"));

    // A click on a control is that control's, and nothing else's.
    await userEvent.click(screen.getByRole("checkbox", { name: "Mark Book the flight done" }));
    expect(store.getState().ui.taskId).toBe("12");
  });

  it("marks the picked row with aria-current rather than aria-selected, since the list is no longer a listbox", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(list().getByText("Book the room"));
    // A noticed row reads as its title and where it came from, which is what the button is named after; only the title carries for one the user typed in.
    await waitFor(() => expect(list().getByRole("button", { name: "Book the room from TCFD call" }).getAttribute("aria-current")).toBe("true"));
    expect(list().getByRole("button", { name: "Book the flight" }).getAttribute("aria-current")).toBeNull();
  });

  it("moves the keyboard's own focus onto the row the arrow just selected, so the ring on screen follows it", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    const first = list().getByRole("button", { name: "Book the flight" });
    first.focus();
    // The store has no task selected yet — the fallback in TasksScreen is only what the page shows, not what App.tsx's walker has — so the first press just confirms row one and the second is what actually steps to row two.
    await userEvent.keyboard("{ArrowDown}{ArrowDown}");
    await waitFor(() => expect(document.activeElement?.getAttribute("data-row-id")).toBe("12"));
  });

  it("says there is nothing to do rather than showing an empty list", async () => {
    renderApp({}, { place: "tasks" });
    expect(await screen.findByText("Nothing to do.")).toBeDefined();
  });
});

describe("the picker in the header", () => {
  it("closes on Escape and gives the keyboard back to the button that opened it", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    const trigger = screen.getByRole("button", { name: /Choose a task/ });
    await userEvent.click(trigger);
    await screen.findByRole("dialog");
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });

  it("names the task the composer is aimed at and picks another", async () => {
    const { store } = renderApp({ tasks, turns: { c1: conversation } }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    const picker = await openPicker("Choose a task");
    await userEvent.click(picker.getByRole("option", { name: /Book the room/ }));
    expect(store.getState().ui.taskId).toBe("13");
    expect(await screen.findByRole("button", { name: /Choose a task.*Book the room/ })).toBeDefined();
  });

  it("narrows both the picker and the list behind it as the search is typed", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    const picker = await openPicker("Choose a task");
    await userEvent.type(screen.getByLabelText("Search tasks"), "room");
    await waitFor(() => expect(list().queryByText("Book the flight")).toBeNull());
    expect(list().getByText("Book the room")).toBeDefined();
    expect(picker.getAllByRole("option")).toHaveLength(1);
  });
});

describe("changing a task's status", () => {
  it("ticks one done and tells the daemon which status it now has", async () => {
    const { calls } = renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(screen.getByRole("checkbox", { name: "Mark Book the flight done" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/task-1/done")?.body).toEqual({ status: "done" }));
    expect(await screen.findByRole("checkbox", { name: "Reopen Book the flight" })).toBeDefined();
  });

  it("drops an action item Ora noticed, and takes it out of the list", async () => {
    const { calls } = renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("button", { name: "More for Send the TCFD file" });
    await userEvent.click(screen.getByRole("button", { name: "More for Send the TCFD file" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Drop it" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/12/done")?.body).toEqual({ status: "dropped" }));
    await waitFor(() => expect(list().queryByText("Send the TCFD file")).toBeNull());
  });

  it("does not offer to drop a task the user typed in, because the daemon refuses it", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("button", { name: "More for Book the flight" });
    await userEvent.click(screen.getByRole("button", { name: "More for Book the flight" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Mark done" })).toBeDefined();
    expect(within(menu).queryByRole("menuitem", { name: "Drop it" })).toBeNull();
  });

  it("fills the circle at once but waits for the undo window before telling the daemon", async () => {
    const { calls } = renderApp({ tasks }, { place: "tasks" });
    const tick = await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(tick);
    expect(await screen.findByRole("checkbox", { name: "Reopen Book the flight" })).toBeDefined();
    expect(calls.find((c) => c.path === "/tasks/task-1/done")).toBeUndefined();
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/task-1/done")?.body).toEqual({ status: "done" }));
  });

  it("cancels for nothing when a second click lands inside the undo window", async () => {
    const { calls } = renderApp({ tasks }, { place: "tasks" });
    const tick = await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(tick);
    await userEvent.click(await screen.findByRole("checkbox", { name: "Reopen Book the flight" }));
    expect(await screen.findByRole("checkbox", { name: "Mark Book the flight done" })).toBeDefined();
    await new Promise((r) => setTimeout(r, 500));
    expect(calls.find((c) => c.path === "/tasks/task-1/done")).toBeUndefined();
  });

  it("holds the circle filled while the change is in flight, says the tick is disabled for as long as that lasts, and sends nothing when it is clicked again in that time", async () => {
    const { calls } = renderApp({ tasks }, { place: "tasks" });
    const tick = await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    // The fake daemon answers at once, so the round trip is held open here: the status change waits for `answer` to be called, everything else the window reads answers as usual, and `sent` counts the status changes that actually went out.
    const daemon = globalThis.fetch;
    const sent: string[] = [];
    let answer = () => {};
    const held = new Promise<void>((r) => {
      answer = r;
    });
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(typeof input === "object" && "url" in input ? input.url : String(input)).pathname;
      if (path !== "/tasks/task-1/done") return daemon(input, init);
      sent.push(path);
      return held.then(() => daemon(input, init));
    });

    await userEvent.click(tick);
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(await screen.findByRole("checkbox", { name: "Reopen Book the flight" })).toBeDefined();
    // The click below does nothing whatever the tick looks like, so the tick has to say so rather than swallow it in silence.
    await waitFor(() => expect(screen.getByRole("checkbox", { name: "Reopen Book the flight" }).getAttribute("aria-disabled")).toBe("true"));
    await userEvent.click(screen.getByRole("checkbox", { name: "Reopen Book the flight" }));
    await new Promise((r) => setTimeout(r, 500));
    expect(sent).toHaveLength(1);

    answer();
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/task-1/done")?.body).toEqual({ status: "done" }));
    expect(await screen.findByRole("checkbox", { name: "Reopen Book the flight" })).toBeDefined();
    await waitFor(() => expect(screen.getByRole("checkbox", { name: "Reopen Book the flight" }).getAttribute("aria-disabled")).not.toBe("true"));
  });

  it("moves the tick back and says so when the daemon refuses the change", async () => {
    renderApp({ tasks, fails: ["POST /tasks/task-1/done"] }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(screen.getByRole("checkbox", { name: "Mark Book the flight done" }));
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Could not change that task");
    expect(await screen.findByRole("checkbox", { name: "Mark Book the flight done" })).toBeDefined();
  });

  it("adds a task of your own and aims the composer at what the daemon named after it", async () => {
    const { calls, store } = renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.type(screen.getByLabelText("Give Ora something to do"), "call the hotel{Enter}");
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/tasks")?.body).toEqual({ title: "call the hotel" }));
    await waitFor(() => expect(store.getState().ui.taskId).toBe("task-9"));
  });
});

describe("talking to a task", () => {
  it("shows the picked task's conversation under the list", async () => {
    renderApp({ tasks, turns: { c1: conversation } }, { place: "tasks" });
    expect(await screen.findByText("which airline?")).toBeDefined();
  });

  it("sends the task and where it came from as the question's context", async () => {
    const { calls } = renderApp({ tasks, turns: { c1: conversation } }, { place: "tasks" });
    await screen.findByText("which airline?");
    await userEvent.type(await screen.findByLabelText("Ask Ora"), "which airline did she book?{Enter}");
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/ask")?.body).toMatchObject({
        question: "which airline did she book?",
        conversation_id: "c1",
        context: 'This is about one thing on the user\'s list: "Book the flight". The user set it themselves.',
      }),
    );
  });

  it("opens a conversation for a noticed task on the first question, and asks in that one", async () => {
    const { calls, store } = renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(list().getByText("Send the TCFD file"));
    await userEvent.type(await screen.findByLabelText("Ask Ora"), "what did she want?{Enter}");
    // The conversation is opened through the same POST /conversations the rail's New chat makes, and the question goes into it.
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/conversations")).toBe(true));
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/ask")?.body).toMatchObject({
        conversation_id: "new",
        context: 'This is about one thing on the user\'s list: "Send the TCFD file". Ora noticed it in TCFD call.',
      }),
    );
    // The pairing is remembered, so the next question about the same task does not open a second conversation.
    await waitFor(() => expect(store.getState().ui.taskChats["12"]).toBe("new"));
  });

  it("shows only the composer, with the empty line as its placeholder, for a task that has no conversation yet", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(list().getByText("Send the TCFD file"));
    expect(await screen.findByPlaceholderText("Nothing said about “Send the TCFD file” yet.")).toBeDefined();
    expect(screen.queryByText("Pick a task above to ask about it.")).toBeNull();
  });
});

/** Two of the user's own and two a meeting raised for someone else or for nobody named — owner "them" and "unclear" are both watched rather than assumed onto the user's own list. */
const withWatched: Task[] = [
  { id: "task-1", title: "Book the flight", source: "you", when, done: false, conversation_id: "c1", detail: "you said", owner: "me" },
  { id: "20", title: "Send the file", source: "noticed", when, done: true, conversation_id: "", detail: "TCFD call", owner: "me" },
  { id: "21", title: "Re-run the source data", source: "noticed", when, done: false, conversation_id: "", detail: "TCFD call", owner: "them" },
  { id: "22", title: "Write up the findings", source: "noticed", when, done: true, conversation_id: "", detail: "Standup", owner: "unclear" },
];

describe("Mine and Theirs", () => {
  it("shows only the user's own by default, with what a meeting raised for someone else collapsed behind a count", async () => {
    renderApp({ tasks: withWatched }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    expect(list().getByText("Book the flight")).toBeDefined();
    expect(list().getByText("Send the file")).toBeDefined();
    expect(list().queryByText("Re-run the source data")).toBeNull();
    expect(list().queryByText("Write up the findings")).toBeNull();
    const disclosure = screen.getByRole("button", { name: /Theirs, watching/ });
    expect(disclosure.textContent).toContain("2");
    expect(disclosure.getAttribute("aria-expanded")).toBe("false");
  });

  it("opens the theirs section on its own disclosure and shows where each one came from", async () => {
    renderApp({ tasks: withWatched }, { place: "tasks" });
    await userEvent.click(await screen.findByRole("button", { name: /Theirs, watching/ }));
    const watched = within(screen.getByRole("list", { name: "Theirs, watching" }));
    expect(watched.getByText("Re-run the source data")).toBeDefined();
    expect(watched.getByText(/from TCFD call/)).toBeDefined();
    expect(watched.getByText("Write up the findings")).toBeDefined();
  });

  it("says nothing of yours is open when every task is someone else's, naming the filter rather than showing an empty list", async () => {
    const allWatched = withWatched.filter((t) => t.owner !== "me");
    renderApp({ tasks: allWatched }, { place: "tasks" });
    expect(await screen.findByText("Nothing of yours open.")).toBeDefined();
    expect(screen.getByRole("button", { name: /Theirs, watching/ })).toBeDefined();
  });

  it("says nothing is being watched at all, rather than showing an empty section, when nothing was raised for anyone else", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    expect(screen.queryByRole("button", { name: /Theirs, watching/ })).toBeNull();
  });

  it("moves a watched row to Mine from its owner menu, and tells the daemon which class it is now", async () => {
    const { calls } = renderApp({ tasks: withWatched }, { place: "tasks" });
    await userEvent.click(await screen.findByRole("button", { name: /Theirs, watching/ }));
    await userEvent.click(screen.getByRole("button", { name: /Re-run the source data.*change who owns it/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Mine" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH" && c.path === "/tasks/21")?.body).toEqual({ owner: "me" }));
  });

  it("offers the same owner menu on a noticed row already in Mine, since a meeting's guess at Mine is still only a guess", async () => {
    const { calls } = renderApp({ tasks: withWatched }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Reopen Send the file" });
    await userEvent.click(screen.getByRole("button", { name: /Send the file.*change who owns it/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Unclear" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH" && c.path === "/tasks/20")?.body).toEqual({ owner: "unclear" }));
  });

  it("pins the owner even when the pick matches the meeting's guess, so a later identity change cannot move it", async () => {
    const { calls } = renderApp({ tasks: withWatched }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Reopen Send the file" });
    await userEvent.click(screen.getByRole("button", { name: /Send the file.*change who owns it/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Mine" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH" && c.path === "/tasks/20")?.body).toEqual({ owner: "me" }));
  });

  it("does not offer an owner control on a task the user typed in, since there is nothing to correct", async () => {
    renderApp({ tasks: withWatched }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    expect(screen.queryByRole("button", { name: /Book the flight.*change who owns it/ })).toBeNull();
  });

  it("says so and leaves the row where it was when the daemon refuses the owner change", async () => {
    renderApp({ tasks: withWatched, fails: ["PATCH /tasks/21"] }, { place: "tasks" });
    await userEvent.click(await screen.findByRole("button", { name: /Theirs, watching/ }));
    await userEvent.click(screen.getByRole("button", { name: /Re-run the source data.*change who owns it/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Mine" }));
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Could not change who owns that task");
  });
});
