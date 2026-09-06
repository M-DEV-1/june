/** @vitest-environment jsdom */
/** The overlay page driven end to end: a payload arrives from Rust, the page places it, and the layer is left holding the right ink.
 * jsdom has no Web Animations and no SVG geometry, so both are faked here, thinly: an animation is a timer that fires onfinish and, with fill forwards, freezes its last opacity; a path is 100 units long and every point on it is the origin. The page only ever asks those three things of either, so nothing more is needed, and the fakes run on the same fake clock as the page's own timers.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/** One animation as the page uses it: something that can be cancelled and that calls onfinish when its time is up. */
type FakeAnimation = { cancel: () => void; onfinish: (() => void) | null };

/** Every animation now running, against the element it was started on, so getAnimations can answer for an element and its subtree. */
const running = new Map<FakeAnimation, Element>();

/** What Rust's overlay_layout answers. Each test sets it before the page starts. */
let layoutAnswer: unknown = { origin_x: 0, origin_y: 0, scale: 1, monitors: [{ x: 0, y: 0, w: 1920, h: 1080, scale: 1 }] };

/** How many times the page has asked for the layout, which is what says whether it re-read after drawing nothing. */
let layoutCalls = 0;

/** The listener the page registered for Rust's events, and when it registered relative to the layout call. */
let pageListener: ((e: { payload: string }) => void) | null = null;
let listenedBeforeLayout = false;

vi.mock("@tauri-apps/api/core", () => ({
  invoke: vi.fn(async () => {
    layoutCalls += 1;
    return layoutAnswer;
  }),
}));

vi.mock("@tauri-apps/api/event", () => ({
  listen: vi.fn(async (_name: string, handler: (e: { payload: string }) => void) => {
    listenedBeforeLayout = layoutCalls === 0;
    pageListener = handler;
    return () => {};
  }),
}));

/** Installs the fakes jsdom is missing and gives the page the four elements overlay.html gives it. Input: none. Output: nothing. */
function stage(): void {
  document.body.innerHTML = `<div id="layer"><div id="shapes"></div><svg id="ink"></svg><div id="pointer"></div></div>`;
  window.matchMedia = ((query: string) => ({
    matches: query.includes("reduce"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;

  Element.prototype.animate = function (this: Element, frames: unknown, options: unknown): Animation {
    const element = this;
    const list = (Array.isArray(frames) ? frames : []) as Record<string, unknown>[];
    const opts = (options ?? {}) as { duration?: number; delay?: number; fill?: string };
    const animation: FakeAnimation = { cancel: () => {}, onfinish: null };
    const timer = window.setTimeout(
      () => {
        running.delete(animation);
        const last = list[list.length - 1];
        if ((opts.fill === "forwards" || opts.fill === "both") && last && last.opacity !== undefined) {
          (element as HTMLElement).style.opacity = String(last.opacity);
        }
        animation.onfinish?.();
      },
      (opts.delay ?? 0) + (opts.duration ?? 0),
    );
    animation.cancel = (): void => {
      window.clearTimeout(timer);
      running.delete(animation);
    };
    running.set(animation, element);
    return animation as unknown as Animation;
  } as typeof Element.prototype.animate;

  Element.prototype.getAnimations = function (this: Element, options?: { subtree?: boolean }): Animation[] {
    const element = this;
    const out: Animation[] = [];
    for (const [animation, on] of running) {
      if (on === element || (options?.subtree === true && element.contains(on))) out.push(animation as unknown as Animation);
    }
    return out;
  } as typeof Element.prototype.getAnimations;

  const svg = SVGElement.prototype as unknown as Record<string, unknown>;
  svg.getTotalLength = (): number => 100;
  svg.getPointAtLength = (): { x: number; y: number } => ({ x: 0, y: 0 });
}

/** Starts the page and lets its two awaits settle. Input: none. Output: nothing; the page's listener is registered by the time this returns. */
async function startPage(): Promise<void> {
  await import("./main");
  await vi.waitFor(() => expect(pageListener).not.toBeNull());
  await Promise.resolve();
  await Promise.resolve();
}

/** Hands the page one overlay event off the daemon's stream. Input: the ask's id and the body of the POST /overlay. Output: nothing. */
function send(askID: string, body: string): void {
  pageListener?.({ payload: JSON.stringify({ id: askID, type: "overlay", text: body }) });
}

/** One ring, as the daemon sends it. */
const ring = (ttl: number): string => `{"kind":"ring","rects":[{"x":100,"y":100,"w":200,"h":80}],"ttl_ms":${ttl}}`;

beforeEach(() => {
  vi.resetModules();
  running.clear();
  layoutCalls = 0;
  pageListener = null;
  listenedBeforeLayout = false;
  layoutAnswer = { origin_x: 0, origin_y: 0, scale: 1, monitors: [{ x: 0, y: 0, w: 1920, h: 1080, scale: 1 }] };
  vi.useFakeTimers();
  stage();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("the overlay page", () => {
  it("registers its listener before it asks for the layout, and says so once", async () => {
    const said = vi.spyOn(console, "log").mockImplementation(() => {});
    await startPage();
    expect(listenedBeforeLayout).toBe(true);
    expect(said.mock.calls.flat().join(" ")).toContain("ora: overlay page listening");
  });

  it("keeps a second drawing from the same ask that lands while the first is fading", async () => {
    await startPage();
    const ink = document.getElementById("ink") as unknown as SVGSVGElement;

    send("ask-1", ring(1000));
    expect(ink.querySelectorAll("path.stroke").length).toBe(1);

    // The ttl starts once the ink has landed, which under reduced motion is one millisecond after the event; a moment past that the fade is running.
    vi.advanceTimersByTime(1001 + 100);
    send("ask-1", ring(1000));

    // The whole fade is 400ms, so by here the fade that was running when the second ring arrived would have finished and taken it off.
    vi.advanceTimersByTime(400);
    expect(ink.querySelectorAll("path.stroke").length).toBe(1);
    expect(ink.style.opacity === "" || Number(ink.style.opacity) > 0).toBe(true);
  });

  it("says so and re-reads the layout when a drawing places no shapes at all", async () => {
    layoutAnswer = { origin_x: 0, origin_y: 0, scale: 1, monitors: [] };
    await startPage();
    const complained = vi.spyOn(console, "error").mockImplementation(() => {});
    const before = layoutCalls;

    send("ask-1", ring(1000));
    await vi.waitFor(() => expect(complained).toHaveBeenCalled());

    expect(layoutCalls).toBe(before + 1);
    expect(complained.mock.calls.flat().join(" ")).toContain("ora: overlay");
  });
});
