/** The window's own size and position, remembered between runs and always kept on the screen. Tauri's window-state plugin lives on the Rust side and this window has none, so the geometry is read off the window with the JS API, kept in the same localStorage both windows already use for the theme, and put back the next time the page loads — fitted to the work area of the monitor it lands on, so a window can never come back bigger than the screen or with its title bar out of reach. Nothing here runs outside Tauri: every call is behind a try, and in a plain browser tab reading fails and the module does nothing. */

import { usableArea, type MonitorLike, type Rect } from "../winplace";

/** The key the geometry is kept under, alongside "june-theme" and "june-hover-position". */
const GEOMETRY_KEY = "june-window-place";

/** The key the first version kept its geometry under. It stored the window's outer size and put it back as the inner size, so the window grew by its own frame on every start until it ran off the screen; what is under it is thrown away rather than read. */
const OLD_GEOMETRY_KEY = "june-window-geometry";

/** Where the window was and how big, in physical pixels on the whole desktop: x and y are the outer top-left corner, which is what setPosition places, and width and height are the inside, which is what setSize sets. maximized is set when the window was maximized, and then the rest is the size it goes back to when un-maximized. */
export type Geometry = { x: number; y: number; width: number; height: number; maximized?: boolean };

/** The smallest window worth restoring. Anything under this came from a minimised or half-mapped window and putting it back would leave a sliver on screen. */
const MIN = 320;

/** How much of the work area a window that has never been placed may take, so the first open on a small laptop screen is not wall to wall. */
const FIRST_SHARE = 0.92;

/** How far, in logical pixels, a window may sit from where fitting would put it before it is moved. */
const SLACK = 16;

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
  const out: Geometry = { x: g.x as number, y: g.y as number, width: g.width as number, height: g.height as number };
  if (out.width < MIN || out.height < MIN) return undefined;
  if (g.maximized === true) out.maximized = true;
  return out;
}

/** Reads the geometry this machine last stored. Input: none. Output: it, or undefined when nothing valid is stored or storage is blocked. */
function storedGeometry(): Geometry | undefined {
  try {
    localStorage.removeItem(OLD_GEOMETRY_KEY);
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

/** Fits a window inside one work area. Input: the window (outer top-left corner, inside size), the frame the window draws around its inside, and the work area, all in physical pixels. Output: the window moved, and shrunk only where it does not fit, so its whole outer rectangle lies inside the work area. */
function fitGeometry(g: Geometry, frame: { width: number; height: number }, area: Rect): Geometry {
  const width = Math.max(1, Math.min(g.width, area.width - frame.width));
  const height = Math.max(1, Math.min(g.height, area.height - frame.height));
  const outerW = width + frame.width;
  const outerH = height + frame.height;
  const x = Math.min(Math.max(g.x, area.x), area.x + area.width - outerW);
  const y = Math.min(Math.max(g.y, area.y), area.y + area.height - outerH);
  return { ...g, x: Math.round(x), y: Math.round(y), width: Math.round(width), height: Math.round(height) };
}

/** How much of a rectangle lies on a monitor's work area. Input: the rectangle and the area. Output: the overlap in square pixels, 0 when they do not meet. */
function overlap(r: Rect, area: Rect): number {
  const w = Math.min(r.x + r.width, area.x + area.width) - Math.max(r.x, area.x);
  const h = Math.min(r.y + r.height, area.y + area.height) - Math.max(r.y, area.y);
  return w > 0 && h > 0 ? w * h : 0;
}

/** The monitor a window belongs on: the one most of it lies on, or, when it lies on none — a monitor unplugged since it was last open — the fallback. Input: the monitors, the window's outer rectangle, and the fallback. Output: the monitor, or undefined when there is none at all. */
function monitorFor(monitors: MonitorLike[], r: Rect, fallback: MonitorLike | null): MonitorLike | undefined {
  let best: MonitorLike | undefined;
  let most = 0;
  for (const m of monitors) {
    const share = overlap(r, usableArea(m));
    if (share > most) {
      best = m;
      most = share;
    }
  }
  return best ?? fallback ?? monitors[0];
}

/** Puts the window back where it was, fitted to the screen it lands on, and starts remembering where it goes next. Input: none. Output: nothing.
 *
 * Every number is physical: the size set is the inside of the window, the position set is its outer corner, and the frame between the two is measured off the window itself, so nothing depends on the display scale being 100%. A window that never had a place stored keeps the size and spot Tauri opened it at, unless that does not fit the screen, and is then shrunk and centred.
 *
 * The size is applied before the position, because a window manager clamps a position against the size it currently has, and once more after it, because moving onto a monitor with another scale makes Windows rescale the window as it arrives.
 *
 * Moves and resizes arrive in a stream while a drag is in progress, so the write is put off until a fifth of a second after the last one rather than run on every frame. A minimised window is not recorded at all, and a maximized one only marks the last normal size as maximized.
 */
export async function rememberWindow(): Promise<void> {
  type Win = ReturnType<typeof import("@tauri-apps/api/window").getCurrentWindow>;
  let win: Win;
  let monitors: () => Promise<MonitorLike[]>;
  let current: () => Promise<MonitorLike | null>;
  try {
    const api = await import("@tauri-apps/api/window");
    win = api.getCurrentWindow();
    monitors = api.availableMonitors;
    current = api.currentMonitor;
  } catch {
    // Not inside Tauri; a browser tab has no window to place.
    return;
  }

  /** The window as it is now, as a Geometry. */
  const measure = async (): Promise<Geometry> => {
    const [size, position] = await Promise.all([win.innerSize(), win.outerPosition()]);
    return { x: position.x, y: position.y, width: size.width, height: size.height };
  };

  /** Fits a geometry to the screen it belongs on. Input: it, and whether it is a first placement (which takes only FIRST_SHARE of the area and is centred when it had to shrink). Output: the fitted geometry, or the same one when the desktop cannot be read. */
  const fit = async (g: Geometry, first: boolean): Promise<Geometry> => {
    const [outer, inner, scale, all, here] = await Promise.all([win.outerSize(), win.innerSize(), win.scaleFactor(), monitors().catch(() => []), current().catch(() => null)]);
    const rawFrame = { width: outer.width - inner.width, height: outer.height - inner.height };
    const monitor = monitorFor(all, { x: g.x, y: g.y, width: g.width + rawFrame.width, height: g.height + rawFrame.height }, here);
    if (!monitor) return g;
    // The frame was measured at the scale the window has now; on a monitor with another scale it is drawn that much bigger or smaller.
    const ratio = scale > 0 && monitor.scaleFactor > 0 ? monitor.scaleFactor / scale : 1;
    const frame = { width: Math.min(400, Math.max(0, rawFrame.width * ratio)), height: Math.min(400, Math.max(0, rawFrame.height * ratio)) };
    const area = usableArea(monitor);
    if (!first) return fitGeometry(g, frame, area);
    const room = { width: area.width * FIRST_SHARE, height: area.height * FIRST_SHARE };
    if (g.width + frame.width <= room.width && g.height + frame.height <= room.height) return fitGeometry(g, frame, area);
    const width = Math.min(g.width, room.width - frame.width);
    const height = Math.min(g.height, room.height - frame.height);
    const centred = { ...g, width, height, x: area.x + (area.width - width - frame.width) / 2, y: area.y + (area.height - height - frame.height) / 2 };
    return fitGeometry(centred, frame, area);
  };

  /** Moves and sizes the window to a fitted geometry, unless it is already within a few pixels of it. Input: where the window is now, where it should be, and whether to move it however close it already is. Output: nothing. The slack is there because Windows draws an invisible resize border outside every window, so one snapped to the edge of the screen already sits a few pixels past it, and pulling it in would unsnap it. */
  const place = async (now: Geometry, g: Geometry, always = false) => {
    const slack = SLACK * ((await win.scaleFactor()) || 1);
    const off = [g.x - now.x, g.y - now.y, g.width - now.width, g.height - now.height].some((d) => Math.abs(d) > slack);
    if (!off && !always) return;
    const { PhysicalPosition, PhysicalSize } = await import("@tauri-apps/api/dpi");
    await win.setSize(new PhysicalSize(g.width, g.height));
    await win.setPosition(new PhysicalPosition(g.x, g.y));
    await win.setSize(new PhysicalSize(g.width, g.height));
  };

  // The last size the window had while it was neither maximized nor minimised, which is what a maximized window is stored with.
  let normal: Geometry | undefined;
  // Maximizing a hidden window shows it on Windows, so a window that was maximized is maximized again the first time it is in front instead.
  let maximizeOnFocus = false;

  try {
    const saved = storedGeometry();
    const now = await measure();
    const target = await fit(saved ?? now, !saved);
    // The size and spot stored are put back even when they are only a few pixels from where the window opened; place's slack is for a window that is already where the person left it.
    await place(now, target, Boolean(saved));
    normal = { x: target.x, y: target.y, width: target.width, height: target.height };
    maximizeOnFocus = Boolean(saved?.maximized);
  } catch {
    /* the compositor refused the placement, which Wayland does; the window keeps whatever it opened at */
  }

  let timer: ReturnType<typeof setTimeout> | undefined;
  const record = () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(async () => {
      try {
        if (await win.isMinimized()) return;
        if (await win.isMaximized()) {
          if (normal) storeGeometry({ ...normal, maximized: true });
          return;
        }
        normal = await measure();
        // A window still waiting to be maximized again is not maximized yet, and the moves the placement above queued can land here before it is ever shown; storing it plain would forget the maximize for the next start.
        storeGeometry(maximizeOnFocus ? { ...normal, maximized: true } : normal);
      } catch {
        /* the window went away */
      }
    }, 200);
  };

  try {
    await win.onMoved(record);
    await win.onResized(record);
    // A screen that changed under a running window — a monitor unplugged, the display scale changed — can leave it bigger than the screen or off it, so each time it comes to the front it is fitted again. A maximized or full-screen window is the window manager's to place.
    await win.onFocusChanged(async ({ payload: focused }) => {
      if (!focused) return;
      try {
        if (maximizeOnFocus) {
          maximizeOnFocus = false;
          await win.maximize();
          return;
        }
        if ((await win.isMaximized()) || (await win.isMinimized()) || (await win.isFullscreen())) return;
        const now = await measure();
        await place(now, await fit(now, false));
      } catch {
        /* nothing to fit against */
      }
    });
  } catch {
    /* no event stream; the window simply is not remembered */
  }
}
