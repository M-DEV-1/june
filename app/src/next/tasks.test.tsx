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
  return within(screen.getByRole("listbox", { name: "Tasks" }));
}

const when = new Date().toISOString();

/** One task the user typed in and two an agent noticed in the same meeting. */
const tasks: Task[] = [
  { id: "task-1", title: "Book the flight", source: "you", when, done: false, conversation_id: "c1", detail: "" },
  { id: "12", title: "Send the TCFD file", source: "noticed", when, done: false, conversation_id: "", detail: "TCFD call" },
  { id: "13", title: "Book the room", source: "noticed", when, done: false, conversation_id: "", detail: "TCFD call" },
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
    expect(list().getByRole("option", { name: /Book the flight/ }).className).toContain("focus-visible:ring-2");
  });

  it("moves the keyboard's own focus onto the row the arrow just selected, so the ring on screen follows it", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    const first = list().getByRole("option", { name: /Book the flight/ });
    first.focus();
    // The store has no task selected yet — the fallback in TasksScreen is only what the page shows, not what App.tsx's walker has — so the first press just confirms row one and the second is what actually steps to row two.
    await userEvent.keyboard("{ArrowDown}{ArrowDown}");
    await waitFor(() => expect(document.activeElement).toBe(list().getByRole("option", { name: /Send the TCFD file/ })));
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

  it("says nothing has been said about a task that has no conversation yet", async () => {
    renderApp({ tasks }, { place: "tasks" });
    await screen.findByRole("checkbox", { name: "Mark Book the flight done" });
    await userEvent.click(list().getByText("Send the TCFD file"));
    expect(await screen.findByText("Nothing said about “Send the TCFD file” yet.")).toBeDefined();
  });
});
