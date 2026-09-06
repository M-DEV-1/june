/** Light and dark for the React window: the choice is kept in the one localStorage key both windows share, and the answer is stamped as data-theme on the root element, which is what src/next/index.css switches every colour token on. */

import { THEME_KEY as KEY, themeChoice, systemTheme, type Theme } from "../shared/theme";

/** Reads the stored theme choice. Input: none. Output: the stored choice, or "system" when nothing valid is stored or storage is blocked. */
export function readTheme(): Theme {
  try {
    return themeChoice(localStorage.getItem(KEY));
  } catch {
    return "system";
  }
}

/** Stores the theme choice. Input: the choice. Output: nothing; a blocked storage is ignored, since the stamp below has already taken effect. */
export function storeTheme(theme: Theme): void {
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    /* storage blocked */
  }
}

// Which call was made last, so an earlier "system" resolution still on its way back from Rust cannot land after a later theme choice and stamp the wrong colour on the root — the same guard the hover's own applyThemeChoice keeps in main.ts, for the same race (System, then a fast second pick, while the first invoke("system_theme") is still in flight).
let themeAsk = 0;

/** Stamps a theme choice on the root element. Input: the choice and the element to stamp. Output: the light or dark actually stamped, or undefined when a later call to this function has started since — its own answer, not this one, is what the root should show. "system" is resolved by asking the desktop first, so the attribute is never left off and the page never falls back to a media query the webview gets wrong. */
export async function applyTheme(
  theme: Theme,
  root: HTMLElement = document.documentElement,
): Promise<"light" | "dark" | undefined> {
  const ask = ++themeAsk;
  const resolved = theme === "system" ? await systemTheme() : theme;
  if (ask !== themeAsk) return undefined;
  root.dataset.theme = resolved;
  return resolved;
}
