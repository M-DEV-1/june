/** What the tests in this folder are built on, and nothing the window itself imports: a fake daemon that answers the routes api.ts calls, the browser pieces jsdom does not have, and one function that renders the whole window against both. Kept beside the code it exercises so a screen and its test read the same types. */

import { render, screen, within, type RenderResult } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";
import { vi } from "vitest";

import App from "./App";
import type { events } from "./api";
import { daemonFetch, type Call, type Canned } from "./mock";
import { makeStore, type AppStore, type Place } from "./store";

export type { Call, Canned } from "./mock";

/** Opens the picker in the header of Tasks, Meetings or Days and hands back what is inside it. Input: the sentence that names the picker, which is the same on every screen that has one — "Choose a day", "Choose a meeting", "Choose a task". Output: the popover, so a test can look for the rows it offers without finding the page's own list instead. */
export async function openPicker(label: string): Promise<ReturnType<typeof within>> {
  await userEvent.click(await screen.findByRole("button", { name: new RegExp(label) }));
  return within(await screen.findByRole("dialog"));
}

/** Puts the fake daemon behind fetch for one test. Input: what it should answer with. Output: the list every call is recorded in, which a test reads to check that a click wrote what it should have. The daemon itself lives in mock.ts, without vitest, because the ?mock=1 browser mode serves the same fixtures from a plain page.
 */
export function mockDaemon(canned: Canned = {}): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal("fetch", daemonFetch(canned, calls));
  return calls;
}

/** The browser pieces jsdom leaves out and the components rely on: the media query the theme falls back to, the observer the scrolling and resizable panels watch their own size with, and the pointer-capture methods Radix calls on a menu. Input: none. Output: nothing; safe to call in every test. */
export function stubBrowser(): void {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }));
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.stubGlobal(
    "EventSource",
    class {
      close() {}
    },
  );
  // jsdom gives every element a zero-sized rectangle at the origin, and react-resizable-panels reads those rectangles to decide whether a pointer landed on one of its drag handles: with everything at 0,0 every click looks like a drag, and the library calls preventDefault, which stops Radix from ever opening a menu. Handing out a rectangle away from the origin is what keeps a click in a test a click.
  Element.prototype.getBoundingClientRect = () => ({ x: 500, y: 500, top: 500, left: 500, right: 600, bottom: 600, width: 100, height: 100, toJSON: () => ({}) });
  Element.prototype.scrollIntoView = () => {};
  Element.prototype.hasPointerCapture = () => false;
  Element.prototype.setPointerCapture = () => {};
  Element.prototype.releasePointerCapture = () => {};
}

/** Makes the screen's own pane report a width, so a test can exercise the wide layout in jsdom, where every element otherwise measures the one rectangle stubBrowser hands out. Only the element the screens mark with data-pane is widened, because the resizable panels read their own rectangles and break if those move. Input: the width in pixels. Output: nothing; call it after stubBrowser. */
export function widePane(width = 1672): void {
  const measured = Element.prototype.getBoundingClientRect;
  Element.prototype.getBoundingClientRect = function (this: Element) {
    const base = measured.call(this);
    if (!(this instanceof HTMLElement) || this.dataset.pane === undefined) return base;
    return { ...base, width, height: 900, toJSON: () => ({}) } as DOMRect;
  };
}

/** Renders the whole window against a fake daemon. Input: what the daemon holds, the place to open on, whether the pane should be wide enough for the rail beside the document, and what opens the event stream, for a test that plays the stream itself. Output: the render, the store, and the calls the window made. */
export function renderApp(
  canned: Canned = {},
  opened: { place?: Place; conversationId?: string; wide?: boolean; open?: typeof events } = {},
): RenderResult & { store: AppStore; calls: Call[] } {
  stubBrowser();
  if (opened.wide) widePane();
  const calls = mockDaemon(canned);
  const store = makeStore({ ui: { place: opened.place ?? "chats", conversationId: opened.conversationId } }, opened.open);
  return { ...render(<Provider store={store}>{<App />}</Provider>), store, calls };
}
