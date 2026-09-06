/** Light, dark or system, and the one localStorage key both windows keep that choice under: the app window's Settings screen writes it and the hover reads it, the same way the hover position is shared (see HOVER_POSITION_KEY in winplace.ts). */

import { invoke } from "@tauri-apps/api/core";

/** "system" means follow the desktop, which still resolves to a light or dark stamp on the root element. */
export type Theme = "light" | "dark" | "system";

/** The localStorage key the theme choice is kept under. */
export const THEME_KEY = "ora-theme";

/** Turns a stored value into a Theme. Input: whatever was read back from THEME_KEY, including null when nothing was ever stored or storage was blocked. Output: "light" or "dark" when that is exactly what was stored, and "system" for everything else, including the word "system" itself, garbage, or nothing at all. */
export function themeChoice(stored: string | null | undefined): Theme {
  return stored === "light" || stored === "dark" ? stored : "system";
}

/** Asks the desktop whether it is in dark mode right now. Input: none. Output: "dark" or "light", read from the Rust system_theme command, falling back to the browser's own prefers-color-scheme media query when that command is unavailable, which is the case in a plain browser tab with no Tauri behind it. WebKitGTK's media query does not follow GNOME's own setting, which is why the Rust command exists at all; both windows resolve "system" this same way so they never disagree about what it means. */
export async function systemTheme(): Promise<"light" | "dark"> {
  try {
    const v = await invoke<string>("system_theme");
    return v === "dark" ? "dark" : "light";
  } catch {
    return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
}
