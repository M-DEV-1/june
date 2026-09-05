// @vitest-environment jsdom

/** Tests for theme.ts's own race guard: main.ts's hover keeps a themeAsk counter so an earlier "system" resolution still on its way back from Rust cannot land after a later theme choice and stamp the wrong colour (see applyThemeChoice in main.ts); applyTheme here now keeps the same guard, since App.tsx resolves "system" the identical way on every theme change and had nothing stopping the same race. */

import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@tauri-apps/api/core", () => ({ invoke: vi.fn() }));

import { invoke } from "@tauri-apps/api/core";
import { applyTheme } from "./theme";

afterEach(() => {
  vi.mocked(invoke).mockReset();
});

describe("applyTheme", () => {
  it("stamps light and dark straight away, with no call to the desktop", async () => {
    const root = document.createElement("div");
    expect(await applyTheme("dark", root)).toBe("dark");
    expect(root.dataset.theme).toBe("dark");
    expect(invoke).not.toHaveBeenCalled();
  });

  it("a stale 'system' resolution must not stamp the root once a newer call has already landed", async () => {
    let resolveFirst!: (v: string) => void;
    let resolveSecond!: (v: string) => void;
    vi.mocked(invoke)
      .mockImplementationOnce(() => new Promise((r) => (resolveFirst = r)))
      .mockImplementationOnce(() => new Promise((r) => (resolveSecond = r)));

    const root = document.createElement("div");
    // System, then a fast second pick to System again — the shape of the race described in main.ts's own themeAsk comment.
    const first = applyTheme("system", root);
    const second = applyTheme("system", root);

    // The newer call's answer comes back first, as it would if the first invoke was merely slow.
    resolveSecond("dark");
    await Promise.resolve();
    await Promise.resolve();
    resolveFirst("light");

    await expect(second).resolves.toBe("dark");
    await expect(first).resolves.toBeUndefined();
    expect(root.dataset.theme).toBe("dark");
  });
});
