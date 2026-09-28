// @vitest-environment jsdom

/** Tests for the rail beside a wide thread: a reply with many sources folds them behind a "show all" control rather than listing every one. */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";

import type { Evidence, Turn } from "./api";
import { ThreadRail } from "./chat-rail";
import { makeStore } from "./store";
import { stubBrowser } from "./testing";

afterEach(cleanup);

function evidenceList(n: number): Evidence[] {
  return Array.from({ length: n }, (_, i) => ({
    title: `source ${i}`,
    meta: "meeting · 3 Sep",
    body: `quote ${i}`,
  }));
}

function turnWith(evidence: Evidence[]): Turn {
  return {
    id: "t1",
    role: "assistant",
    text: "an answer",
    kind: "reply",
    evidence,
    tools: [],
    when: new Date().toISOString(),
    reason: "",
  };
}

function renderRail(showing: Turn) {
  stubBrowser();
  const store = makeStore({ ui: { place: "chats" } });
  return render(
    <Provider store={store}>
      <ThreadRail sources showing={showing} />
    </Provider>,
  );
}

describe("ThreadRail", () => {
  it("shows the first six of many sources with a 'show all' control, which reveals the rest when clicked", async () => {
    renderRail(turnWith(evidenceList(40)));
    expect(screen.getAllByText(/^quote \d+$/)).toHaveLength(6);
    const button = screen.getByRole("button", { name: "show all 40" });

    await userEvent.click(button);

    expect(screen.getAllByText(/^quote \d+$/)).toHaveLength(40);
  });
});
