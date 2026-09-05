/** Light and dark for the React window: the choice is kept in the one localStorage key both windows share, and the answer is stamped as data-theme on the root element, which is what src/next/index.css switches every colour token on. */

import { invoke } from "@tauri-apps/api/core";

import { THEME_KEY as KEY } from "../state";
import type { Theme } from "./store";

/** Reads the stored theme choice. Input: none. Output: the stored choice, or "system" when nothing valid is stored or storage is blocked. */
export function readTheme(): Theme {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    /* storage blocked */
  }
  return "system";
}

/** Stores the theme choice. Input: the choice. Output: nothing; a blocked storage is ignored, since the stamp below has already taken effect. */
export function storeTheme(theme: Theme): void {
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    /* storage blocked */
  }
}

/** Asks the desktop whether it is in dark mode. Input: none. Output: "dark" or "light", from the Rust system_theme command, falling back to the browser's own media query when that command is unavailable, which is the case in any plain browser tab. WebKitGTK's prefers-color-scheme does not follow GNOME's setting, which is why the Rust command exists at all. */
async function systemTheme(): Promise<"light" | "dark"> {
  try {
    const v = await invoke<string>("system_theme");
    return v === "dark" ? "dark" : "light";
  } catch {
    return matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  }
}

/** Stamps a theme choice on the root element. Input: the choice and the element to stamp. Output: the light or dark actually stamped; "system" is resolved by asking the desktop first, so the attribute is never left off and the page never falls back to a media query the webview gets wrong. */
export async function applyTheme(
  theme: Theme,
  root: HTMLElement = document.documentElement,
): Promise<"light" | "dark"> {
  const resolved = theme === "system" ? await systemTheme() : theme;
  root.dataset.theme = resolved;
  return resolved;
}
