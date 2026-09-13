// @vitest-environment jsdom

/** Tests for the rail beside a wide thread: it caps its own height and scrolls inside itself instead of overfilling past the pane, and a reply with many sources folds them behind a "show all" control rather than listing every one. */

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
  it("caps its own height and scrolls inside itself, so it never grows past the pane", () => {
    renderRail(turnWith(evidenceList(2)));
    const aside = screen.getByRole("complementary", { name: "What this answer is built on" });
    expect(aside.className).toContain("overflow-y-auto");
    expect(aside.className).toContain("max-h-[calc(100vh-3rem)]");
  });

  it("shows the first six of many sources with a 'show all' control, which reveals the rest when clicked", async () => {
    renderRail(turnWith(evidenceList(40)));
    expect(screen.getAllByText(/^quote \d+$/)).toHaveLength(6);
    const button = screen.getByRole("button", { name: "show all 40" });

    await userEvent.click(button);

    expect(screen.getAllByText(/^quote \d+$/)).toHaveLength(40);
  });

  it("shows no 'show all' control when there are six or fewer sources", () => {
    renderRail(turnWith(evidenceList(6)));
    expect(screen.getAllByText(/^quote \d+$/)).toHaveLength(6);
    expect(screen.queryByRole("button", { name: /show all/ })).toBeNull();
  });
});
