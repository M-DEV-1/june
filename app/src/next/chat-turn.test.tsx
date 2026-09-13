// @vitest-environment jsdom

/** Tests for the turn list's face chips: a finished reply opens with "done" or "refused", a run still streaming opens with "thinking", and a finished tool step carries its own "done" face. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, within } from "@testing-library/react";

import type { ConversationSummary, ConversationView } from "./api";
import { progress } from "./store";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const when = new Date().toISOString();

const summary: ConversationSummary[] = [
  { id: "c1", title: "Flights to Zurich", brain: "claude", last: "booked", updated: when },
];

const view: ConversationView = {
  id: "c1",
  title: "Flights to Zurich",
  brain: "claude",
  turns: [
    { id: "t1", role: "you", text: "what did she say about the deadline?", kind: "ask", evidence: [], tools: [], when, reason: "" },
    { id: "t2", role: "ora", text: "She said Friday.", kind: "ask", evidence: [], tools: [], when, reason: "" },
    { id: "t3", role: "ora", text: "the provider returned 529", kind: "error", evidence: [], tools: [], when, reason: "The model was too busy to answer." },
  ],
};

describe("a finished reply's face", () => {
  it("leaves an ordinary answer bare — a face on every reply is decoration, not a signal", async () => {
    renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    await screen.findByText("She said Friday.");
    const reply = document.getElementById("turn-t2")!;
    expect(within(reply).queryByRole("img", { name: "ora is done" })).toBeNull();
  });

  it("opens a failed ask with the refused face", async () => {
    renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    await screen.findByText("The model was too busy to answer.");
    expect(screen.getByRole("img", { name: "ora is refused" })).toBeDefined();
  });

  it("shows exactly one face for a failed ask, on the reply that failed", async () => {
    renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    await screen.findByText("The model was too busy to answer.");
    expect(screen.getAllByRole("img", { name: "ora is refused" })).toHaveLength(1);
  });
});

describe("a run in flight", () => {
  it("opens a streaming answer with the thinking face", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: { ...view, turns: [] } } }, { conversationId: "c1" });
    store.dispatch(progress.askSent({ conversationId: "c1", question: "what next?" }));
    store.dispatch(
      progress.eventArrived({ type: "answer", text: "Working on it" } as never),
    );
    const answer = await screen.findByText("Working on it");
    // The sidebar carries its own permanent face chip too, so this looks only at the one beside the streamed answer.
    expect(within(answer.parentElement!).getByRole("img", { name: "ora is thinking" })).toBeDefined();
  });

  it("leaves a finished tool step bare — every step finishing is the normal case", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: { ...view, turns: [] } } }, { conversationId: "c1" });
    store.dispatch(progress.askSent({ conversationId: "c1", question: "what next?" }));
    store.dispatch(
      progress.eventArrived({ type: "tool", text: "search_memory", detail: "read 3 notes" } as never),
    );
    expect(await screen.findByText("search_memory")).toBeDefined();
    expect(screen.queryByRole("img", { name: "ora is done" })).toBeNull();
  });

  it("gives a failed tool step the refused face instead of done", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: { ...view, turns: [] } } }, { conversationId: "c1" });
    store.dispatch(progress.askSent({ conversationId: "c1", question: "what next?" }));
    store.dispatch(
      progress.eventArrived({ type: "tool", text: "click", detail: "error: element not found", failed: true } as never),
    );
    expect(await screen.findByText("click")).toBeDefined();
    expect(screen.getByRole("img", { name: "ora is refused" })).toBeDefined();
  });
});
