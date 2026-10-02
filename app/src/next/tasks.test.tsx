// @vitest-environment jsdom

/** Tests for what the Tasks screen sends the daemon: the tick after its undo window, the question's context, and the owner change. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ConversationView, Task } from "./api";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const when = new Date().toISOString();

/** One task the user typed in and two an agent noticed in the same meeting. */
const tasks: Task[] = [
  { id: "task-1", title: "Book the flight", source: "you", when, done: false, conversation_id: "c1", detail: "", owner: "me" },
  { id: "12", title: "Send the Meridian file", source: "noticed", when, done: false, conversation_id: "", detail: "Meridian call", owner: "me" },
  { id: "13", title: "Book the room", source: "noticed", when, done: false, conversation_id: "", detail: "Meridian call", owner: "me" },
];

describe("changing a task's status", () => {
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
});

const conversation: ConversationView = {
  id: "c1",
  title: "Book the flight",
  brain: "claude",
  turns: [{ id: "t1", role: "you", text: "which airline?", kind: "ask", evidence: [], tools: [], when, reason: "" }],
};

describe("talking to a task", () => {
  it("sends the task and where it came from as the question's context", async () => {
    const { calls } = renderApp({ tasks, turns: { c1: conversation } }, { place: "tasks" });
    await screen.findByText("which airline?");
    await userEvent.type(await screen.findByLabelText("Ask June"), "which airline did she book?{Enter}");
    await waitFor(() =>
      expect(calls.find((c) => c.path === "/ask")?.body).toMatchObject({
        question: "which airline did she book?",
        conversation_id: "c1",
        context: 'This is about one thing on the user\'s list: "Book the flight". The user set it themselves.',
      }),
    );
  });
});

/** A task a meeting raised for someone else, which sits in the collapsed Watching section. */
const watched: Task[] = [
  ...tasks,
  { id: "21", title: "Re-run the source data", source: "noticed", when, done: false, conversation_id: "", detail: "Meridian call", owner: "them" },
];

describe("Mine and Theirs", () => {
  it("moves a watched row to Mine from its owner menu, and tells the daemon which class it is now", async () => {
    const { calls } = renderApp({ tasks: watched }, { place: "tasks" });
    await userEvent.click(await screen.findByRole("button", { name: /Watching/ }));
    await userEvent.click(screen.getByRole("button", { name: /Re-run the source data.*change who owns it/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Mine" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH" && c.path === "/tasks/21")?.body).toEqual({ owner: "me" }));
  });
});
