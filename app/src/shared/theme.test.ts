// @vitest-environment jsdom

/** Tests for applyTheme's race guard: an earlier "system" resolution still on its way back from Rust must not land after a later theme choice and stamp the wrong colour. Both windows stamp their theme through applyTheme. */

import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@tauri-apps/api/core", () => ({ invoke: vi.fn() }));

import { invoke } from "@tauri-apps/api/core";
import { applyTheme } from "./theme";

afterEach(() => {
  vi.mocked(invoke).mockReset();
});

describe("applyTheme", () => {
  it("a stale 'system' resolution must not stamp the root once a newer call has already landed", async () => {
    let resolveFirst!: (v: string) => void;
    let resolveSecond!: (v: string) => void;
    vi.mocked(invoke)
      .mockImplementationOnce(() => new Promise((r) => (resolveFirst = r)))
      .mockImplementationOnce(() => new Promise((r) => (resolveSecond = r)));

    const root = document.createElement("div");
    // System, then a fast second pick to System again, while the first answer from Rust is still in flight.
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
