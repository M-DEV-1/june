/** The window's own size and position, remembered between runs. Tauri's window-state plugin lives on the Rust side and this window has none, so the geometry is read off the window with the JS API, kept in the same localStorage both windows already use for the theme, and put back the next time the page loads. Nothing here runs outside Tauri: every call is behind a try, and in a plain browser tab reading fails and the module does nothing. */

/** The key the geometry is kept under, alongside "ora-theme" and "ora-hover-position". */
const GEOMETRY_KEY = "ora-window-geometry";

/** Where the window was and how big, in physical pixels on the whole desktop. */
export type Geometry = { x: number; y: number; width: number; height: number };

/** The smallest window worth restoring. Anything under this came from a minimised or half-mapped window and putting it back would leave a sliver on screen. */
const MIN = 320;

/** Reads a stored geometry back. Input: the stored string, which is what localStorage hands over, or null when nothing is stored. Output: the geometry, or undefined when there is none, when it does not parse, or when it is too small or not finite to be worth restoring. Exported on its own because the parsing is the only part worth a test. */
export function parseGeometry(raw: string | null): Geometry | undefined {
  if (!raw) return undefined;
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return undefined;
  }
  if (typeof value !== "object" || value === null) return undefined;
  const g = value as Record<string, unknown>;
  const nums = [g.x, g.y, g.width, g.height];
  if (!nums.every((n) => typeof n === "number" && Number.isFinite(n))) return undefined;
  const out = { x: g.x as number, y: g.y as number, width: g.width as number, height: g.height as number };
  if (out.width < MIN || out.height < MIN) return undefined;
  return out;
}

/** Reads the geometry this machine last stored. Input: none. Output: it, or undefined when nothing valid is stored or storage is blocked. */
function storedGeometry(): Geometry | undefined {
  try {
    return parseGeometry(localStorage.getItem(GEOMETRY_KEY));
  } catch {
    return undefined;
  }
}

/** Writes a geometry down. Input: it. Output: nothing; blocked storage is ignored, because the window is already where it is and the only cost is forgetting. */
function storeGeometry(g: Geometry): void {
  try {
    localStorage.setItem(GEOMETRY_KEY, JSON.stringify(g));
  } catch {
    /* storage blocked */
  }
}

/** Puts the window back where it was and starts remembering where it goes next. Input: none. Output: nothing.
 *
 * The size is applied before the position, because a window manager clamps a position against the size it currently has. Nothing is applied when nothing is stored, so the first run keeps whatever size Tauri opened at and only starts recording from there — which is also why a reload never appears to move the window: what is stored is what it already is.
 *
 * Moves and resizes arrive in a stream while a drag is in progress, so the write is put off until a fifth of a second after the last one rather than run on every frame.
 */
export async function rememberWindow(): Promise<void> {
  let win: Awaited<ReturnType<typeof import("@tauri-apps/api/window").getCurrentWindow>> | undefined;
  try {
    const { getCurrentWindow } = await import("@tauri-apps/api/window");
    win = getCurrentWindow();
  } catch {
    // Not inside Tauri; a browser tab has no window to place.
    return;
  }

  const saved = storedGeometry();
  if (saved) {
    try {
      const { PhysicalPosition, PhysicalSize } = await import("@tauri-apps/api/dpi");
      await win.setSize(new PhysicalSize(saved.width, saved.height));
      await win.setPosition(new PhysicalPosition(saved.x, saved.y));
    } catch {
      /* the compositor refused the placement, which Wayland does; the window keeps whatever it opened at */
    }
  }

  let timer: ReturnType<typeof setTimeout> | undefined;
  const record = () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(async () => {
      try {
        const [size, position] = await Promise.all([win!.outerSize(), win!.outerPosition()]);
        storeGeometry({ x: position.x, y: position.y, width: size.width, height: size.height });
      } catch {
        /* the window went away */
      }
    }, 200);
  };

  try {
    await win.onMoved(record);
    await win.onResized(record);
  } catch {
    /* no event stream; the window simply is not remembered */
  }
}
