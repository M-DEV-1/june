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

  it("labels a window that resets within a day as minutes away, rounded to the minute, with the percent only in the title", () => {
    const now = new Date();
    const resets = new Date(now.getTime() + (3 * 60 + 56) * 60000);
    render(<UsageBar limit={limit(0.42, resets.toISOString())} now={now} />);
    expect(screen.getByText("5h · resets in 3 h 56 min")).toBeDefined();
    expect(screen.getByTitle("42%")).toBeDefined();
  });

  it("labels a window past a day away by the clock time it resets at", () => {
    const now = new Date();
    const resets = new Date(now.getTime() + 30 * 3600000);
    const clock = `${String(resets.getHours()).padStart(2, "0")}:${String(resets.getMinutes()).padStart(2, "0")}`;
    render(<UsageBar limit={limit(0.1, resets.toISOString(), "weekly")} now={now} />);
    expect(screen.getByText(`weekly · resets at ${clock}`)).toBeDefined();
  });

  it("turns the fill amber once used_fraction passes 0.9, and stays the accent colour at or below it", () => {
    const now = new Date();
    const resets = new Date(now.getTime() + 3600000).toISOString();
    const { container: over, unmount } = render(<UsageBar limit={limit(0.95, resets, "daily")} now={now} />);
    expect(over.querySelector(".bg-work")).not.toBeNull();
    expect(over.querySelector(".bg-primary")).toBeNull();
    unmount();
    const { container: under } = render(<UsageBar limit={limit(0.9, resets, "daily")} now={now} />);
    expect(under.querySelector(".bg-primary")).not.toBeNull();
    expect(under.querySelector(".bg-work")).toBeNull();
  });
});

describe("the brain picker's usage bars", () => {
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

  it("draws one bar per limit under a brain that reports them, and none for a brain with no limits", async () => {
    renderPicker();
    await userEvent.click(screen.getByRole("button", { name: /Brain: Claude/ }));
    const menu = within(await screen.findByRole("menu"));
    const claudeItem = (await menu.findByRole("menuitem", { name: /Claude/ })) as HTMLElement;
    expect(within(claudeItem).getByText(/5h · resets in/)).toBeDefined();
    expect(within(claudeItem).getByText(/weekly · resets in/)).toBeDefined();
    const codexItem = menu.getByRole("menuitem", { name: /Codex/ }) as HTMLElement;
    expect(within(codexItem).queryByTitle(/%$/)).toBeNull();
  });

  it("leaves a limit-less row the same height as a bare row", async () => {
    renderPicker();
    await userEvent.click(screen.getByRole("button", { name: /Brain: Claude/ }));
    const menu = within(await screen.findByRole("menu"));
    const codexItem = menu.getByRole("menuitem", { name: /Codex/ }) as HTMLElement;
    // A row with nothing extra to draw holds only its one line of text and no bar block.
    expect(codexItem.querySelector(".bg-muted")).toBeNull();
  });

  it("stays keyboard navigable: opening focuses the first row, with the bars adding no focusable stop of their own, and the arrow key walks to the next row", async () => {
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
