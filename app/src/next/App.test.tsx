// @vitest-environment jsdom

/** Tests for App.tsx's own behaviour: the arrow keys that walk whichever list a screen has on its left, and the keys that must be left alone because a caret or a draft owns them instead. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ConversationSummary, ConversationView } from "./api";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const when = new Date().toISOString();

const summary: ConversationSummary[] = [
  { id: "c1", title: "Flights to Zurich", brain: "claude", last: "booked", updated: when },
  { id: "c2", title: "The old one", brain: "", last: "", updated: when },
];

const view: ConversationView = { id: "c1", title: "Flights to Zurich", brain: "claude", turns: [] };

describe("the arrow keys that walk a list", () => {
  it("moves to the next chat when nothing has the keyboard's caret", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    await screen.findByRole("button", { name: /^Flights to Zurich/ });
    document.body.focus();
    await userEvent.keyboard("{ArrowDown}");
    await waitFor(() => expect(store.getState().ui.conversationId).toBe("c2"));
  });

  it("leaves the caret in the composer alone rather than walking the chat list underneath it", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    const box = await screen.findByLabelText("Ask Ora");
    await userEvent.type(box, "hello");
    await userEvent.keyboard("{ArrowUp}{ArrowUp}{ArrowDown}");
    // The composer is a multi-line textarea, not an <input>, so a caret move in it must not also walk the conversation list open behind it.
    expect(store.getState().ui.conversationId).toBe("c1");
  });
});

describe("the jump-to palette", () => {
  it("opens on Ctrl+K even with the caret in the composer, and keeps what was half-typed there", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    const box = await screen.findByLabelText("Ask Ora");
    await userEvent.type(box, "a half-typed question");
    await userEvent.keyboard("{Control>}k{/Control}");
    expect(store.getState().ui.paletteOpen).toBe(true);
    expect((box as HTMLTextAreaElement).value).toBe("a half-typed question");
  });
});
