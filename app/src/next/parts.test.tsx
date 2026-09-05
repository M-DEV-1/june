// @vitest-environment jsdom

/** Tests for the small pieces more than one screen draws: the Scroller's stick-to-bottom behaviour, and the usage bars the brain picker draws under a row. */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import { useState } from "react";

import type { Brain, UsageLimit } from "./api";
import { BrainPicker, Scroller, UsageBar } from "./parts";
import { makeStore } from "./store";
import { stubBrowser } from "./testing";

afterEach(cleanup);

/** Gives the scrolling region the size jsdom will not: a viewport 800px tall holding 4,000px of thread. Mirrors the same helper in chats.test.tsx. Input: the region. Output: nothing; it is mutated in place. */
function tall(view: HTMLElement): void {
  Object.defineProperty(view, "scrollHeight", { value: 4000, configurable: true });
  Object.defineProperty(view, "clientHeight", { value: 800, configurable: true });
}

describe("Scroller staying put", () => {
  it("does not undo a reader's scroll-up when the thread re-renders for a reason that added nothing to it", async () => {
    // A stand-in for what happens on Chats in a wide pane: the rail beside the thread picks a new active reply as the reader scrolls, which re-renders the thread with the very same turns in it.
    function Harness() {
      const [tick, setTick] = useState(0);
      return (
        <div>
          <button onClick={() => setTick((t) => t + 1)}>rerender, same content</button>
          <Scroller newest="c1">
            <div data-testid="content">turn {tick}</div>
          </Scroller>
        </div>
      );
    }

    stubBrowser();
    render(<Harness />);
    const view = screen
      .getByTestId("content")
      .closest("[data-slot=scroll-area-viewport]") as HTMLElement;
    tall(view);

    // Seed the region at its newest turn, the way a thread opens.
    await userEvent.click(screen.getByText("rerender, same content"));
    await waitFor(() => expect(view.scrollTop).toBe(3200));

    // The reader scrolls up a little, staying within the 120px band that is meant only to mean "pull them along if the next answer lands" — not "snap them back on the next unrelated render".
    view.scrollTop = 3150;
    view.dispatchEvent(new Event("scroll"));
    await waitFor(() => expect(view.scrollTop).toBe(3150));

    // Something unrelated re-renders the thread — the rail's active reply, in the real bug — without adding anything to it.
    await userEvent.click(screen.getByText("rerender, same content"));

    expect(view.scrollTop).toBe(3150);
  });
});

describe("UsageBar", () => {
  const limit = (used_fraction: number, resets_at: string, window = "5h"): UsageLimit => ({ window, used_fraction, resets_at, source: "test" });

  it("names the window in sentence case and gives the relative reset with its percent when it turns over within a day", () => {
    const now = new Date();
    const resets = new Date(now.getTime() + (3 * 60 + 3) * 60000);
    render(<UsageBar limit={limit(0.92, resets.toISOString())} now={now} />);
    expect(screen.getByText("5-hour")).toBeDefined();
    expect(screen.getByText("Resets in 3 hr 3 min · 92%")).toBeDefined();
  });

  it("names weekly, daily and monthly windows in sentence case", () => {
    const now = new Date();
    const soon = new Date(now.getTime() + 60000).toISOString();
    const { unmount: u1 } = render(<UsageBar limit={limit(0.1, soon, "weekly")} now={now} />);
    expect(screen.getByText("Weekly")).toBeDefined();
    u1();
    const { unmount: u2 } = render(<UsageBar limit={limit(0.1, soon, "daily")} now={now} />);
    expect(screen.getByText("Daily")).toBeDefined();
    u2();
    render(<UsageBar limit={limit(0.1, soon, "monthly")} now={now} />);
    expect(screen.getByText("Monthly")).toBeDefined();
  });

  it("drops the percent and gives the weekday and clock once the reset is more than a day away", () => {
    const now = new Date();
    const resets = new Date(now.getTime() + 30 * 3600000);
    const weekday = resets.toLocaleDateString(undefined, { weekday: "short" });
    const clock = `${String(resets.getHours()).padStart(2, "0")}:${String(resets.getMinutes()).padStart(2, "0")}`;
    render(<UsageBar limit={limit(0.1, resets.toISOString(), "weekly")} now={now} />);
    expect(screen.getByText(`Resets ${weekday} ${clock}`)).toBeDefined();
  });

  it("turns the fill the danger colour at or above 90% used, and keeps it the accent colour below that", () => {
    const now = new Date();
    const resets = new Date(now.getTime() + 3600000).toISOString();
    const { container: over, unmount } = render(<UsageBar limit={limit(0.9, resets, "monthly")} now={now} />);
    expect(over.querySelector(".bg-destructive")).not.toBeNull();
    expect(over.querySelector(".bg-primary")).toBeNull();
    unmount();
    const { container: under } = render(<UsageBar limit={limit(0.89, resets, "monthly")} now={now} />);
    expect(under.querySelector(".bg-primary")).not.toBeNull();
    expect(under.querySelector(".bg-destructive")).toBeNull();
  });
});

describe("the brain picker's usage rows", () => {
  const brains: Brain[] = [
    {
      id: "claude",
      name: "Claude",
      signed_in: true,
      account: "max",
      models: ["opus"],
      model: "opus",
      note: "",
      default: true,
      limits: [
        { window: "5h", used_fraction: 0.42, resets_at: new Date(Date.now() + 3600000).toISOString(), source: "test" },
        { window: "weekly", used_fraction: 0.95, resets_at: new Date(Date.now() + 3600000).toISOString(), source: "test" },
      ],
    },
    { id: "codex", name: "Codex", signed_in: true, account: "plus", models: ["gpt"], model: "gpt", note: "", default: false },
    { id: "gemini", name: "Gemini", signed_in: false, account: "", models: [], model: "", note: "", default: false },
  ];

  function renderPicker() {
    stubBrowser();
    const store = makeStore({ ui: { place: "chats" } });
    return render(
      <Provider store={store}>
        <BrainPicker current="claude" brains={brains} />
      </Provider>,
    );
  }

  it("draws one row per limit under a brain that reports them, labelled in sentence case", async () => {
    renderPicker();
    await userEvent.click(screen.getByRole("button", { name: /Brain: Claude/ }));
    const menu = within(await screen.findByRole("menu"));
    const claudeItem = (await menu.findByRole("menuitem", { name: /Claude/ })) as HTMLElement;
    expect(within(claudeItem).getByText("5-hour")).toBeDefined();
    expect(within(claudeItem).getByText("Weekly")).toBeDefined();
  });

  it("shows 'No usage data' for a signed-in brain with no limits, and 'Not signed in' for one that isn't", async () => {
    renderPicker();
    await userEvent.click(screen.getByRole("button", { name: /Brain: Claude/ }));
    const menu = within(await screen.findByRole("menu"));
    const codexItem = menu.getByRole("menuitem", { name: /Codex/ }) as HTMLElement;
    expect(within(codexItem).getByText("No usage data")).toBeDefined();
    expect(codexItem.querySelector(".bg-muted")).toBeNull();
    const geminiItem = menu.getByRole("menuitem", { name: /Gemini/ }) as HTMLElement;
    expect(within(geminiItem).getByText("Not signed in")).toBeDefined();
  });

  it("separates each brain's section from the next with a hairline", async () => {
    renderPicker();
    await userEvent.click(screen.getByRole("button", { name: /Brain: Claude/ }));
    const menu = await screen.findByRole("menu");
    expect(menu.querySelectorAll("[role=separator]").length).toBe(brains.length - 1);
  });

  it("stays keyboard navigable: opening focuses the first row, with the rows adding no focusable stop of their own, and the arrow key walks to the next row", async () => {
    renderPicker();
    const trigger = screen.getByRole("button", { name: /Brain: Claude/ });
    trigger.focus();
    await userEvent.keyboard("{Enter}");
    const menu = within(await screen.findByRole("menu"));
    expect(document.activeElement).toBe(menu.getByRole("menuitem", { name: /Claude/ }));
    await userEvent.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(menu.getByRole("menuitem", { name: /Codex/ }));
  });
});
