import { LogicalSize, PhysicalPosition, PhysicalSize } from "@tauri-apps/api/dpi";
import { clamp as clampRaw } from "./shared/clamp";

/**
 * The subset of a Tauri window's API this module touches, factored out so the show/hide,
 * placement and focus-timing logic can be tested with a fake window instead of a real one.
 */
export interface WinLike {
  isVisible(): Promise<boolean>;
  show(): Promise<void>;
  hide(): Promise<void>;
  setPosition(pos: PhysicalPosition): Promise<void>;
  setSize(size: LogicalSize | PhysicalSize): Promise<void>;
  /** The window's own scale factor, which is what one of its CSS pixels is worth. Not the same number as the pointer monitor's scale on a mixed-DPI desk (see threadMaxHeight). */
  scaleFactor(): Promise<number>;
}

/** A rectangle in physical pixels on the desktop, where x and y are its top-left corner relative to the whole multi-monitor desktop, not relative to the monitor it sits on. */
export interface Rect {
  x: number;
  y: number;
  width: number;
  height: number;
}

/** Where on the screen the hover opens. All three are centred left to right; they differ only in how far down the screen the window sits. */
export type HoverPosition = "top" | "center" | "bottom";

/** The position the hover opens at when nothing has been chosen. Centre, where the eye already is when the hotkey is pressed. It was bottom while the hover had to stay clear of whatever the user was reading, because a picture of the screen had Ora's own card in the middle of it; the window now steps off the screen for the moment that picture is taken (see the conceal handler in main.ts), so the hover no longer has to hide from the thing it is being asked about. */
export const DEFAULT_HOVER_POSITION: HoverPosition = "center";

/** The localStorage key the chosen position is kept under, alongside "ora-theme". Both windows are served from one origin, so the Settings screen in the app window writes it and the hover window reads it. */
export const HOVER_POSITION_KEY = "ora-hover-position";

/** Which edge of the screen the desktop's dock sits on. */
type DockEdge = "bottom" | "left" | "right" | "top";

/** Where the dock is and how much room to leave for it. `clearance` is in logical pixels and is the dock's own thickness when the dock does not reserve screen space (an auto-hiding dock), or 0 when it does reserve space, because then the monitor's work area already excludes it. */
export interface Dock {
  edge: DockEdge;
  clearance: number;
}

/** The parts of Tauri's Monitor this module reads. Every coordinate is in physical pixels relative to the whole desktop, so a second monitor to the right of a 1920-wide primary has position.x = 1920. */
export interface MonitorLike {
  position: { x: number; y: number };
  size: { width: number; height: number };
  workArea?: { position: { x: number; y: number }; size: { width: number; height: number } };
  scaleFactor: number;
}

/** What the placement needs from the desktop, as three calls, so the whole placement path can be driven from a fake in tests instead of a real desktop. */
export interface Desktop {
  monitors(): Promise<MonitorLike[]>;
  pointer(): Promise<{ x: number; y: number } | null>;
  focused(): Promise<MonitorLike | null>;
  dock(): Promise<Dock>;
}

/** One open's worth of placement facts: the work area of the monitor the hover opens on (physical pixels), that monitor's scale factor, where the dock is, and the position the user chose. Captured once when the window is shown and reused for every resize while it stays open, so a growing answer keeps sitting where it opened instead of following the pointer to another screen. */
export interface PlaceContext {
  work: Rect;
  scale: number;
  dock: Dock;
  position: HoverPosition;
}

/** Reads the chosen position out of storage. Input: a storage object, or nothing to use the browser's localStorage. Output: the stored position, or the default when nothing valid is stored or storage is blocked. */
export function storedHoverPosition(store?: { getItem(key: string): string | null }): HoverPosition {
  try {
    const value = (store ?? localStorage).getItem(HOVER_POSITION_KEY);
    if (value === "top" || value === "center" || value === "bottom") return value;
  } catch {
    /* storage blocked */
  }
  return DEFAULT_HOVER_POSITION;
}

/** How far the top and bottom positions sit in from the edge they hang off, so the hover reads as floating near that edge rather than jammed against it. A twelfth of the usable height, which is 87 physical pixels on a 1048-tall work area, with a floor for short screens. Input: the height of the area the window is placed in and the monitor's scale factor, both physical. Output: the inset in physical pixels. */
export function edgeInset(areaHeight: number, scale: number): number {
  return Math.max(Math.round(24 * scale), Math.round(areaHeight / 12));
}

/** Clamps a value into a range and rounds it to a physical pixel, returning the low end when the range is inverted, which happens when the window is larger than the space it has to fit in. Input: the value and the inclusive bounds. Output: the clamped, rounded value. */
function clamp(value: number, low: number, high: number): number {
  return clampRaw(value, low, high, true);
}

/** Takes the dock's own strip off the work area. A dock that reserves screen space is already cut out of the work area and reports 0 clearance, so this changes nothing for it; an auto-hiding dock reserves nothing, so its thickness is subtracted here and the hover stays clear of the strip the dock slides into. Input: the work area and the dock, with clearance in physical pixels. Output: the rectangle the window may occupy. */
export function dockedArea(work: Rect, dock: Dock): Rect {
  const gap = Math.max(0, dock.clearance);
  if (dock.edge === "top") return { x: work.x, y: work.y + gap, width: work.width, height: work.height - gap };
  if (dock.edge === "bottom") return { x: work.x, y: work.y, width: work.width, height: work.height - gap };
  if (dock.edge === "left") return { x: work.x + gap, y: work.y, width: work.width - gap, height: work.height };
  return { x: work.x, y: work.y, width: work.width - gap, height: work.height };
}

/**
 * Works out where to put the hover window. It is always centred left to right in the space left over once the dock is accounted for; the chosen position decides how far down it sits: `top` hangs it an inset below the top of that space, `bottom` an inset above the bottom of it, and `center` puts it halfway.
 * Anything the window cannot fit inside is resolved in favour of that space's top-left corner, so a hover taller than the screen keeps its input line on screen instead of running off the bottom.
 * Input: the work area of one monitor and the window size, both in physical pixels and both in whole-desktop coordinates so a monitor whose origin is not 0,0 lands on that monitor; the dock, with `clearance` already converted to physical pixels; the chosen position; and the inset in physical pixels.
 * Output: the window's top-left corner in physical desktop coordinates.
 */
export function hoverPlacement(work: Rect, win: { width: number; height: number }, dock: Dock, position: HoverPosition, inset: number): { x: number; y: number } {
  const area = dockedArea(work, dock);
  const x = area.x + (area.width - win.width) / 2;
  let y: number;
  if (position === "top") {
    y = area.y + inset;
  } else if (position === "center") {
    y = area.y + (area.height - win.height) / 2;
  } else {
    y = area.y + area.height - win.height - inset;
  }
  return {
    x: clamp(x, area.x, area.x + area.width - win.width),
    y: clamp(y, area.y, area.y + area.height - win.height),
  };
}

/** Finds the monitor a point falls on, treating each monitor's rectangle as including its left and top edges but not its right and bottom, so a point on the seam between two monitors belongs to exactly one of them. Input: the monitors and a point in physical desktop coordinates, or null when the point could not be read. Output: the monitor, or null if no monitor covers the point. */
export function monitorForPoint(monitors: MonitorLike[], point: { x: number; y: number } | null): MonitorLike | null {
  if (!point) return null;
  for (const m of monitors) {
    const withinX = point.x >= m.position.x && point.x < m.position.x + m.size.width;
    const withinY = point.y >= m.position.y && point.y < m.position.y + m.size.height;
    if (withinX && withinY) return m;
  }
  return null;
}

/** The area of a monitor a window may use: its work area when the desktop reports one, which already excludes any panel or dock that reserves space, and otherwise the whole monitor. Input: a monitor. Output: the usable rectangle in physical desktop coordinates. */
export function usableArea(monitor: MonitorLike): Rect {
  const area = monitor.workArea;
  if (area && area.size.width > 0 && area.size.height > 0) {
    return { x: area.position.x, y: area.position.y, width: area.size.width, height: area.size.height };
  }
  return { x: monitor.position.x, y: monitor.position.y, width: monitor.size.width, height: monitor.size.height };
}

/** The scale factor to convert this open's logical numbers with, guarding against a monitor that reports 0. Input: the context. Output: the scale factor. */
function scaleOf(ctx: PlaceContext): number {
  return ctx.scale > 0 ? ctx.scale : 1;
}

/** Converts a logical window size to the physical size to set on the window, with the same monitor scale the placement uses. Input: the context captured for this open and the window's logical size. Output: the physical size. */
export function physicalSizeFor(ctx: PlaceContext, logical: { width: number; height: number }): PhysicalSize {
  const scale = scaleOf(ctx);
  return new PhysicalSize(Math.round(logical.width * scale), Math.round(logical.height * scale));
}

/** Turns a logical window size into the physical position to put that window at, converting the size, the dock's clearance and the inset with the monitor's scale factor. Input: the context captured for this open and the window's logical size. Output: the window's top-left corner in physical desktop coordinates. */
export function placementFor(ctx: PlaceContext, logical: { width: number; height: number }): { x: number; y: number } {
  const scale = scaleOf(ctx);
  const win = { width: logical.width * scale, height: logical.height * scale };
  const dock: Dock = { edge: ctx.dock.edge, clearance: ctx.dock.clearance * scale };
  const area = dockedArea(ctx.work, dock);
  return hoverPlacement(ctx.work, win, dock, ctx.position, edgeInset(area.height, scale));
}

/** Decides which monitor this open belongs to and reads the dock, once per open. The monitor is the one the pointer is on, because the hover is opened by a keystroke while the user is looking at whatever the pointer is near; when the pointer cannot be read it falls back to the monitor the focused window is on, and then to the first monitor the desktop lists. Input: the desktop and the position the user chose. Output: the context to place with, or null when the desktop lists no monitors at all and placement has to be skipped. */
export async function resolveContext(desk: Desktop, position: HoverPosition): Promise<PlaceContext | null> {
  const [monitors, pointer, dock] = await Promise.all([
    desk.monitors().catch(() => [] as MonitorLike[]),
    desk.pointer().catch(() => null),
    desk.dock(),
  ]);
  const monitor = monitorForPoint(monitors, pointer) ?? (await desk.focused().catch(() => null)) ?? monitors[0] ?? null;
  if (!monitor) return null;
  return { work: usableArea(monitor), scale: monitor.scaleFactor || 1, dock, position };
}

/** How far under the top bar a notice-only window starts, in logical pixels. */
const NOTICE_GAP = 8;

/** Where a window showing nothing but a notice goes: flush with the right edge of the usable area and just under the top bar, beside the tray where Ora's own indicator sits, which is where the user asked for it. The dock is taken off the same way the hover's placement takes it off, so an auto-hiding dock on the right edge does not end up with the card under it, and everything the window cannot fit inside resolves to the top-left of that area. Input: the placement context captured when the window was shown, and the window's logical size. Output: the window's top-left corner in physical desktop coordinates. */
export function noticePlacement(ctx: PlaceContext, logical: { width: number; height: number }): { x: number; y: number } {
  const scale = scaleOf(ctx);
  const win = { width: logical.width * scale, height: logical.height * scale };
  const area = dockedArea(ctx.work, { edge: ctx.dock.edge, clearance: ctx.dock.clearance * scale });
  return {
    x: clamp(area.x + area.width - win.width, area.x, area.x + area.width - win.width),
    y: clamp(area.y + NOTICE_GAP * scale, area.y, area.y + area.height - win.height),
  };
}

/** How much of the work area the card's thread may take. */
const THREAD_SHARE = 0.6;

/** The tallest the card's thread may grow before it scrolls inside itself, so an answer that keeps coming grows the card to this and no further. The cap is written into the page as CSS pixels, and a CSS pixel is worth the window's own scale factor, not the pointer monitor's — so the scale passed in is win.scaleFactor(), which is the same number on a single-DPI desk and a factor of two out on a mixed one. Input: the monitor's usable area in physical pixels and the window's scale factor. Output: the cap in CSS pixels, six tenths of the usable height. */
export function threadMaxHeight(work: Rect, scale: number): number {
  const s = scale > 0 ? scale : 1;
  return Math.floor((work.height / s) * THREAD_SHARE);
}

/**
 * Resizes the window to fit new content height, and while it is visible moves it so it stays where it opened: a bottom-positioned hover that grows has to move up by the amount it grew, or it would push its own bottom edge through the dock, and a centred one has to move up by half.
 * While hidden this only records the new size, because `toggleWindow` places the window in full the next time it is shown and moving a hidden window would be wasted work.
 * The size is set in physical pixels, converted with the same monitor scale factor the position is worked out with: a LogicalSize is converted by Tauri using the *window's* current scale factor, so on a 1x + 2x desk the window would be sized against one monitor and placed against another and land half off the screen or half the size.
 * Input: the window, the target logical size, whether the size actually changed since last time, the context captured when the window was shown (null if placement was skipped), and whether to move the window as well as resize it (false for a notice-only window, which its caller places itself).
 * Output: nothing.
 */
export async function fitWindow(win: WinLike, size: { width: number; height: number }, changed: boolean, ctx: PlaceContext | null, move = true): Promise<void> {
  if (!changed) return;
  await win.setSize(ctx ? physicalSizeFor(ctx, size) : new LogicalSize(size.width, size.height));
  if (!ctx || !move) return;
  if (!(await win.isVisible())) return;
  const at = placementFor(ctx, size);
  await win.setPosition(new PhysicalPosition(at.x, at.y));
}

/**
 * Runs the show/hide sequence for the toggle hotkey. The window is sized and moved while it is still hidden and only then shown, so it is never painted at the position it had last time or at whatever position the window manager would have chosen.
 * Focus is requested exactly once per show, via `raise`, immediately after `show()` — never from placement code and never a second time (e.g. a follow-up `setFocus()`), because each focus request GNOME sees carries a fresh user-interaction time and repeating it makes GNOME treat Ora as the ongoing interaction, denying focus to whatever the user opens next. Hiding calls nothing but `hide()`, since anything else risks touching focus.
 * Input: the window; `beforeShow` (e.g. refreshing daemon context), `openContext` (deciding which monitor, dock and chosen position this open uses) and `sizeToContent` (fitting the window to its content, returning the logical size it settled on) to run before the window is shown; `raise`, the one focus request; `focusInput`, a DOM-level (not OS-level) focus to run last.
 * Output: nothing.
 */
export async function toggleWindow(
  win: WinLike,
  opts: {
    beforeShow: () => Promise<void>;
    openContext: () => Promise<PlaceContext | null>;
    sizeToContent: () => Promise<{ width: number; height: number }>;
    raise: () => Promise<void>;
    focusInput: () => void;
  },
): Promise<void> {
  if (await win.isVisible()) {
    await win.hide();
    return;
  }
  // Everything before show() is best-effort: a hover in the wrong place is still a hover, one that never appears is a dead hotkey. A throw from the daemon read, from the desktop reads or from setPosition itself falls through to show() rather than out of here.
  try {
    await opts.beforeShow();
    const ctx = await opts.openContext();
    const size = await opts.sizeToContent();
    if (ctx) {
      const at = placementFor(ctx, size);
      await win.setPosition(new PhysicalPosition(at.x, at.y));
    }
  } catch (e) {
    console.error("ora: placing the hover failed", e);
  }
  await win.show();
  await opts.raise();
  opts.focusInput();
}
