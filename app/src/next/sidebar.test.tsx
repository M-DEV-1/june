// @vitest-environment jsdom

/** Tests for the rail every screen shares and the three things that sit above every screen: the conversations under their date headings, the menu on a row, the rename box, the confirmation in front of a delete, and the jump-to-a-chat palette. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ConversationSummary } from "./api";
import { progress } from "./store";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** Two conversations, one touched today and one a month ago, so the rail has two headings to draw. */
function conversations(): ConversationSummary[] {
  const today = new Date();
  const then = new Date(today.getTime() - 40 * 86400000);
  return [
    { id: "c1", title: "Flights to Zurich", brain: "claude", last: "booked", updated: today.toISOString() },
    { id: "c2", title: "The old one", brain: "", last: "", updated: then.toISOString() },
  ];
}

/** The row one conversation has in the rail, which is a button whose name opens with the conversation's title — the "…" beside it names the same conversation, so the match is anchored to the start. Input: the title. Output: the row, once the daemon's answer has arrived. */
function row(title: string) {
  return screen.findByRole("button", { name: new RegExp(`^${title}`) });
}

describe("the rail", () => {
  it("heads the conversations by when they were last touched", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    expect(screen.getByText("Today")).toBeDefined();
    // The second heading is whatever month that day fell in, so the assertion is that both groups are drawn rather than what the month is called.
    expect(screen.getAllByRole("group")).toHaveLength(2);
    expect(await row("The old one")).toBeDefined();
  });

  it("leaves only what the search matches, and says so when nothing does", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    const field = screen.getByLabelText("Search chats");
    await userEvent.type(field, "zurich");
    expect(screen.queryByRole("button", { name: /^The old one/ })).toBeNull();
    await userEvent.clear(field);
    await userEvent.type(field, "nothing like this");
    expect(await screen.findByText(/Nothing matches/)).toBeDefined();
  });

  it("says the daemon is not connected rather than that there are no chats", async () => {
    renderApp({ fails: ["GET /conversations"] });
    expect(await screen.findByText("Not connected.")).toBeDefined();
  });

  it("moves the keyboard's own focus onto the chat the arrow just selected", async () => {
    renderApp({ conversations: conversations() }, { conversationId: "c1" });
    const first = await row("Flights to Zurich");
    first.focus();
    await userEvent.keyboard("{ArrowDown}");
    await waitFor(async () => expect(document.activeElement).toBe(await row("The old one")));
  });

  it("opens a chat and comes back to Chats from another screen", async () => {
    const { store } = renderApp({ conversations: conversations() }, { place: "days" });
    await userEvent.click(await row("The old one"));
    expect(store.getState().ui).toMatchObject({ place: "chats", conversationId: "c2" });
  });

  it("shows each place from its foot row and comes back to Chats from the row that is lit", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await userEvent.click(await screen.findByRole("button", { name: "Tasks" }));
    expect(store.getState().ui.place).toBe("tasks");
    await userEvent.click(screen.getByRole("button", { name: "Tasks" }));
    expect(store.getState().ui.place).toBe("chats");
  });

  it("opens an unsaved draft when New chat is clicked, posting nothing and adding nothing to the list", async () => {
    const { calls, store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "New chat" }));
    expect(store.getState().ui.chatDraft).toBe(true);
    expect(screen.getByRole("heading", { name: "Ora" })).toBeDefined();
    expect(calls.some((c) => c.method === "POST" && c.path === "/conversations")).toBe(false);
  });

  it("leads to the four places that are not a chat, and to no ledger of its own", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    for (const place of ["Tasks", "Meetings", "Days", "Settings"]) expect(screen.getByRole("button", { name: place })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Usage" })).toBeNull();
  });

  it("pins the width of the box Radix draws around the conversation list, so a long subtitle truncates against the rail's own edge rather than growing past it with no ellipsis", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    const scrollArea = document.querySelector('[data-slot="sidebar-content"] [data-slot="scroll-area"]');
    expect(scrollArea).not.toBeNull();
    // Radix's own viewport box is `display: table`, which sizes to its widest row rather than the rail; without a fixed table layout and a width pinned to zero (floored back up to the full rail by min-w-full), a long subtitle grows that box past the rail and is cut by the rail's own edge instead of by the truncated span's, which is what left the reported line stopping mid-word with no "…".
    expect(scrollArea?.className).toContain("[&>[data-radix-scroll-area-viewport]>div]:table-fixed");
    expect(scrollArea?.className).toContain("[&>[data-radix-scroll-area-viewport]>div]:w-0");
    expect(scrollArea?.className).toContain("[&>[data-radix-scroll-area-viewport]>div]:min-w-full");
  });
});

describe("the menu on a row", () => {
  it("renames a chat through the box the menu opens", async () => {
    const { calls } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
    const box = await screen.findByLabelText("New title");
    await userEvent.clear(box);
    await userEvent.type(box, "Zurich, September");
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/conversations/c1/title")?.body).toEqual({ title: "Zurich, September" }));
  });

  it("will not take a blank title", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
    await userEvent.clear(await screen.findByLabelText("New title"));
    expect(screen.getByRole("button", { name: "Rename" }).hasAttribute("disabled")).toBe(true);
  });

  it("asks before a delete, and falls to the next chat when the one that was open is removed", async () => {
    const { store, calls } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.path === "/conversations/c1")).toBe(true));
    await waitFor(() => expect(store.getState().ui.conversationId).toBe("c2"));
  });

  it("leaves the row where it is and says so when a delete does not go through", async () => {
    renderApp({ conversations: conversations(), fails: ["DELETE /conversations/c1"] }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Delete" }));
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Could not delete");
    expect(await row("Flights to Zurich")).toBeDefined();
  });

  it("keeps the chat when the confirmation is dismissed", async () => {
    const { calls } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Keep it" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
  });
});

describe("the jump-to-a-chat palette", () => {
  it("opens on Ctrl+K and opens the chat that is chosen", async () => {
    const { store } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.keyboard("{Control>}k{/Control}");
    const palette = await screen.findByRole("dialog");
    await userEvent.click(within(palette).getByRole("option", { name: /The old one/ }));
    expect(store.getState().ui.conversationId).toBe("c2");
    expect(store.getState().ui.paletteOpen).toBe(false);
  });
});

// A live notice — one that reached the window with no action yet — offers its own Done/1h/Evening/Tomorrow row, wired through POST /notices/{kind}/{id}/action (see internal/proactive/notify.go's Act, the same code a desktop notification's own buttons call).
describe("a live notice's own buttons", () => {
  it("posts the pressed button's action for that notice", async () => {
    const { store, calls } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task" },
      }),
    );

    await userEvent.click(await screen.findByRole("button", { name: "1 h" }));

    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.path === "/notices/task/task-42/action")?.body).toEqual({
        title: "Still open",
        body: "Send the invoice",
        action: "hour",
      }),
    );
  });

  it("offers Done alongside the snooze buttons for a task, but only the snooze buttons for a routine", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task" },
      }),
    );
    expect(await screen.findByRole("button", { name: "Done" })).toBeDefined();

    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Routine", body: "Priya replied about the venue.", place: "", id: "7", kind: "routine" },
      }),
    );
    expect(await screen.findByRole("button", { name: "Evening" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Done" })).toBeNull();
  });

  it("replaces the buttons with the rail line's own text once the daemon answers", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task" },
      }),
    );
    await screen.findByRole("button", { name: "1 h" });

    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", action: "done", until: "" },
      }),
    );

    expect(await screen.findByText("Send the invoice: Done")).toBeDefined();
    expect(screen.queryByRole("button", { name: "1 h" })).toBeNull();
  });
});
