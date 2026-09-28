// @vitest-environment jsdom

/** Tests for the turn list's face chips: a failed reply and a failed tool step each carry the "refused" face. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";

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
    { id: "t2", role: "june", text: "She said Friday.", kind: "ask", evidence: [], tools: [], when, reason: "" },
    { id: "t3", role: "june", text: "the provider returned 529", kind: "error", evidence: [], tools: [], when, reason: "The model was too busy to answer." },
  ],
};

describe("a finished reply's face", () => {
  it("shows exactly one face for a failed ask, on the reply that failed", async () => {
    renderApp({ conversations: summary, turns: { c1: view } }, { conversationId: "c1" });
    await screen.findByText("The model was too busy to answer.");
    expect(screen.getAllByRole("img", { name: "june is refused" })).toHaveLength(1);
  });
});

describe("a run in flight", () => {
  it("gives a failed tool step the refused face instead of done", async () => {
    const { store } = renderApp({ conversations: summary, turns: { c1: { ...view, turns: [] } } }, { conversationId: "c1" });
    store.dispatch(progress.askSent({ conversationId: "c1", question: "what next?" }));
    store.dispatch(
      progress.eventArrived({ type: "tool", text: "click", detail: "error: element not found", failed: true } as never),
    );
    expect(await screen.findByText("click")).toBeDefined();
    expect(screen.getByRole("img", { name: "june is refused" })).toBeDefined();
  });
});
