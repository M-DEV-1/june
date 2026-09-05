// @vitest-environment jsdom

/** Tests for the small pieces more than one screen draws: today just the Scroller's stick-to-bottom behaviour, since that is where the bug lives. */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { Scroller } from "./parts";
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
