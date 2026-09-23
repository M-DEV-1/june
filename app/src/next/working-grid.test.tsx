// @vitest-environment jsdom

/** Tests for the one row of braille dots that says "Ora is working": that it moves, that it stops moving when it leaves the page, and that it holds still for someone who asked for less motion. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { WorkingGrid } from "./working-grid";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  // jsdom does not implement matchMedia, so a test that stubs it puts the page back the way it found it.
  delete (window as { matchMedia?: unknown }).matchMedia;
});

/** Says whether every character of a string is a braille pattern, U+2800 to U+28FF. Input: the string. Output: true when it is all braille and not empty. */
function allBraille(s: string): boolean {
  return (
    s.length > 0 &&
    [...s].every((c) => c.codePointAt(0)! >= 0x2800 && c.codePointAt(0)! <= 0x28ff)
  );
}

/** Makes window.matchMedia answer that the reader asked for reduced motion. Input: whether reduce is on. Output: nothing; window is stubbed in place. */
function stubReducedMotion(reduce: boolean): void {
  window.matchMedia = ((query: string) =>
    ({
      matches: reduce && query.includes("prefers-reduced-motion"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

describe("the working grid", () => {
  it("moves as time passes", async () => {
    vi.useFakeTimers();
    render(<WorkingGrid />);
    const row = screen.getByRole("img", { name: "Ora is working" });
    const first = row.textContent;
    await act(async () => {
      vi.advanceTimersByTime(600);
    });
    expect(row.textContent).not.toBe(first);
    expect(allBraille(row.textContent ?? "")).toBe(true);
  });

  it("stops moving once it leaves the page", async () => {
    vi.useFakeTimers();
    const clear = vi.spyOn(globalThis, "clearInterval");
    const { unmount } = render(<WorkingGrid />);
    unmount();
    expect(clear).toHaveBeenCalled();
    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
    expect(screen.queryByRole("img", { name: "Ora is working" })).toBeNull();
  });

  it("holds one frame when the reader asked for less motion", async () => {
    stubReducedMotion(true);
    vi.useFakeTimers();
    render(<WorkingGrid />);
    const row = screen.getByRole("img", { name: "Ora is working" });
    const first = row.textContent;
    expect(allBraille(first ?? "")).toBe(true);
    await act(async () => {
      vi.advanceTimersByTime(1200);
    });
    expect(row.textContent).toBe(first);
  });
});
