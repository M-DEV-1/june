// @vitest-environment jsdom

/** Tests for the browser-only mock mode: which URLs turn it on. */

import { afterEach, describe, expect, it, vi } from "vitest";

import { wantsMock } from "./mock";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("when the mock mode turns on", () => {
  it("takes only ?mock=1, so no real window is ever taken off the real daemon by accident", () => {
    expect(wantsMock("?mock=1")).toBe(true);
    expect(wantsMock("?token=abc&mock=1")).toBe(true);
    expect(wantsMock("")).toBe(false);
    expect(wantsMock("?mock=0")).toBe(false);
    expect(wantsMock("?mock=true")).toBe(false);
    expect(wantsMock("?mocked=1")).toBe(false);
  });
});

