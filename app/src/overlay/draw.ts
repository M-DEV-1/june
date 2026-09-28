/** Turning one overlay event from the daemon into shapes the overlay page can position. No DOM and no network here, so every rule about where a ring lands is testable on its own.
 *
 * The daemon sends rectangles in global desktop pixels — the same pixels the screenshot the model looked at is made of. The overlay window covers the whole desk, so a rectangle becomes a position inside that window by subtracting the window's own top-left corner and dividing by the window's scale factor, which is how many physical pixels one CSS pixel is worth.
 */

import { clamp } from "../shared/clamp";

/** One rectangle from an overlay event, in global desktop pixels. A ring's label may ride on the rect itself instead of on the event. */
export type OverlayRect = { x: number; y: number; w: number; h: number; label?: string };

/** The body of a POST /overlay, as it arrives in the text of an "overlay" event. Kind is "ring", "marks", "arrow", "line", "tap" or "clear". A ring and marks carry rects; an arrow and a line carry a run of points in the same global desktop pixels; a tap carries the one point the pointer flies to and presses. */
export type OverlaySpec = {
  kind: string;
  label?: string;
  rects?: OverlayRect[];
  points?: [number, number][];
  ttl_ms?: number;
  /** The one draw call every shape of this drawing came from. Shapes sharing it belong on screen together; empty when the drawing came from somewhere with no call to name, such as POST /overlay. */
  group?: string;
};

/** One monitor, in global desktop pixels, as the Rust side reports it. */
export type Monitor = { x: number; y: number; w: number; h: number; scale: number };

/** Where the overlay window sits on the desk and how its pixels relate to CSS pixels. origin_x and origin_y are the window's top-left corner in global desktop pixels; scale is physical pixels per CSS pixel. */
export type Layout = { origin_x: number; origin_y: number; scale: number; monitors: Monitor[] };

/** One thing to draw, already in the overlay page's own CSS pixels.
 * A stroke is a line of ink: the outline of a ring, the shaft of an arrow, and — when filled is set — the small solid head at the end of that shaft. A label is the pill that says what the ink is about, with a tail pointing at it. A mark is a numbered pill at a rectangle's top-left corner. */
export type Shape =
  | { kind: "stroke"; d: string; width: number; dashed?: boolean; filled?: boolean }
  | { kind: "label"; x: number; y: number; text: string; tail: "down" | "up" }
  | { kind: "mark"; x: number; y: number; text: string };

/** What the drawing is for, which is what colours it. Neutral is June looking, point is June showing you something, act is a press about to happen, done is a thing finished. The pointer and the label pill always carry the same one. */
export type Mood = "neutral" | "point" | "act" | "done";

/** A place on the overlay page, in its own CSS pixels. */
export type Point = { x: number; y: number };

/** One trip of the ghost pointer: a quadratic curve from where it was to where it is going, and how long the trip takes. The control point is what lifts the curve, so the pointer swings up and over instead of sliding along a straight line. */
export type Flight = { from: Point; to: Point; control: Point; ms: number };

/** How long a drawing stays up when the event names no ttl. The daemon fills this in itself, so this only covers an event that lost it on the way. */
const DEFAULT_TTL_MS = 3000;

/** How high above the straight line between the two ends the flight's curve bulges: 0.2 of the distance, never more than 80 CSS pixels. Tying the lift to the distance is what stops a hop of twenty pixels from looping absurdly high over its own target. Clicky's OverlayWindow.swift:521 lifts its own arc by the same 0.2 of the distance, capped at 80 points. */
const ARC_LIFT_RATIO = 0.2;
const ARC_LIFT_MAX = 80;

/** The shortest and the longest a flight may take, in milliseconds, and how many pixels of distance buy one millisecond in between. A trip of 800 pixels takes 1000 ms and anything past about 1120 pixels takes the full 1400. Clicky's OverlayWindow.swift:510 flies its own buddy at the same 800 points a second, clamped the same way between 600 ms and 1400 ms. */
const FLIGHT_MIN_MS = 600;
const FLIGHT_MAX_MS = 1400;
const FLIGHT_PX_PER_MS = 0.8;

/** How long the pointer takes to settle once it has touched down, in milliseconds. */
export const SETTLE_MS = 220;

/** How big the pointer swells at the middle of a flight, back to 1 at either end. It is a glide, not a swoop: the ink on the target is what the user is meant to look at, and a pointer that grows by a third on the way there takes that attention. */
const FLIGHT_SWELL = 1.12;

/** The scales the pointer passes through as it settles, and where in the settle each one falls. It gives a little under the landing, comes back a shade past its own size, and stops. */
const SETTLE_SCALES = [0.95, 1.03, 0.995, 1];
const SETTLE_OFFSETS = [0.32, 0.64, 0.86, 1];

/** The fraction of a flight the pointer spends banking into the way it is going, and the fraction after which it starts turning back upright to land. It leaves upright, faces its heading for the middle of the trip and arrives upright again, so no frame ever asks it to jump from one angle to another. */
const TURN_RISE = 0.18;
const TURN_HOLD = 0.68;

/** The direction the pointer's own drawing already faces, in degrees measured the way atan2 measures them on a screen where y grows downwards. The triangle is drawn pointing up and 40 degrees to the left, so a flight heading due right has to turn it 130 degrees clockwise. */
export const POINTER_HEADING = -130;

/** How far outside the screen edge the pointer's very first flight starts, so it slides in from off-screen rather than appearing at the edge. */
const EDGE_MARGIN = 40;

/** How long a second ring on the same rectangle still counts as a tap on the first, in milliseconds. */
const REPEAT_MS = 2000;

/** How wide a tap ripple grows compared with the thing being tapped, and the smallest and largest it may be, in CSS pixels. A ripple is a tap on that one control, so it is drawn to that control's own size: a circle far bigger than the button is a circle over somebody's work. */
const RIPPLE_RATIO = 1.3;
const RIPPLE_MIN = 24;
const RIPPLE_MAX = 96;

/** The words in a label that mean the user is being told to click something. A label carrying one of these gets a tap ripple as well as a halo. */
const TAP_WORDS = /\b(click|tap|press|push|open|select|choose|submit|send|save|toggle|check|button|menu|link)\b/i;

/** The most marks drawn for one "marks" event. show_marks lists what is on screen, and past this many the numbers are on top of each other and none of them can be read anyway. */
const MARK_CAP = 200;

/** How far the edge of the screen a label is kept away from, in CSS pixels. */
const LABEL_PAD = 8;

/** How thick a line of ink is, in CSS pixels. */
const STROKE_W = 3;

/** The corner radius of a dashed box, in CSS pixels. */
const BOX_RADIUS = 6;

/** How far outside the element its ring is drawn, in CSS pixels, so the outline sits just clear of the thing rather than on top of the border the app drew itself. */
const RING_PAD = 3;

/** The corner radius of a ring, in CSS pixels. */
const RING_RADIUS = 8;

/** The smallest a ring may be across, in CSS pixels. A fourteen-pixel icon gets an outline this size grown about its own centre, because an outline the size of the icon is a smudge rather than something the eye lands on. */
const RING_MIN = 30;

/** The small solid head at the end of an arrow: how long it is and how far its corners are swept back from the shaft, in CSS pixels and degrees. */
const HEAD_ARM = 15;
const HEAD_SPREAD = 24;

/** How long a stroke takes to draw itself on, in milliseconds, and how many pixels of its length buy one of those milliseconds. A short ring is drawn in 400 ms and anything past about 850 pixels takes the full 700. */
const DRAW_MIN_MS = 400;
const DRAW_MAX_MS = 700;
const DRAW_PX_PER_MS = 2.8;

/** How far above a ring's own outline its label sits, in CSS pixels. The pill is 26 tall, so this leaves eight pixels of clear ground between the bottom of the pill and the top of the ring: the label names the thing without sitting on the mark that points at it. */
const LABEL_LIFT = 34;

/** How far below a ring's outline the label drops instead, when there is no room for it above. The pill's tail sticks nine pixels out of its top edge, so this is what keeps the tail off the outline too. */
const LABEL_DROP = 12;

/** The band along the top of a monitor a label is never put in, in CSS pixels. The desktop's own panel lives there and is drawn over every window, so a label placed inside it cannot be seen: GNOME's top bar is 32 pixels tall on this desk, and a ring around anything in the first row of a window under it would have its label swallowed. */
const TOP_BAND = 34;

/** Reads one event's text as an overlay spec. Input: the text of an "overlay" event. Output: the spec, or null when the text is not JSON or is not an object with a kind. */
export function parseSpec(text: string): OverlaySpec | null {
  try {
    const spec = JSON.parse(text) as unknown;
    if (typeof spec !== "object" || spec === null) return null;
    if (typeof (spec as OverlaySpec).kind !== "string") return null;
    return spec as OverlaySpec;
  } catch {
    return null;
  }
}

/** How long this drawing should stay on screen. Input: the spec. Output: its ttl_ms, or 3000 when it names none; never less than 1, so a drawing is never scheduled to vanish in the past. */
export function ttlFor(spec: OverlaySpec): number {
  return Math.max(1, spec.ttl_ms || DEFAULT_TTL_MS);
}

/** The id an overlay event carries when no question drew it: a POST /overlay from another program, or a voice session's own ring. The daemon mints ask ids of the shape "ask-N", so this can never collide with one. */
export const NO_ASK = "overlay";

/** What is on the overlay layer now: the ask that drew it and the draw call it came from, either of which may be empty. */
export type Drawn = { ask: string; group: string };

/** Whether this drawing joins what is already on the layer instead of replacing it. Input: the id of the ask that drew it (NO_ASK when no ask did), what is on the layer now (null when it is empty), and the spec about to be drawn. Output: true to add these shapes to the ones already up, false to wipe the layer first.
 * Two things join. A drawing joins one already up when the same ask made both, because one answer often needs several shapes — "draw a box round each of them" is two draw calls — and those belong on screen together. And shapes join when they name the same draw call, whoever drew them: one call is one drawing, and a twenty-shape formula spoken to a voice session carries no ask at all, so without this every shape of it wiped the one before and the user saw a single stroke (2026-09-07).
 * Everything else replaces: a new ask is answering a new question and its predecessor's ink is stale, an empty layer has nothing to join, marks number their rects from 1 so a second set would put two number ones on the screen at once, and NO_ASK is shared by every caller that is not an ask, so joining on it alone would pile one program's drawing onto another's. */
export function keepsPrevious(askID: string, previous: Drawn | null, spec: OverlaySpec): boolean {
  if (previous === null || spec.kind === "marks") return false;
  if (spec.group && spec.group === previous.group) return true;
  if (askID === NO_ASK) return false;
  return askID === previous.ask;
}

/** Finds the monitor a rectangle belongs to by its centre point. Input: a rect in global desktop pixels and the monitors. Output: that monitor, or null when the centre falls on no monitor, which is what a stale rect from a screen layout that has since changed looks like. */
export function monitorFor(rect: OverlayRect, monitors: Monitor[]): Monitor | null {
  const cx = rect.x + rect.w / 2;
  const cy = rect.y + rect.h / 2;
  for (const m of monitors) {
    if (cx >= m.x && cx < m.x + m.w && cy >= m.y && cy < m.y + m.h) return m;
  }
  return null;
}

/** Turns everything an overlay event asks for into shapes placed in the overlay page's CSS pixels. Input: the spec and the window's layout. Output: the shapes, in draw order; an empty list for a "clear", for a kind nobody draws, and for rects that fall on no monitor.
 * A ring becomes one closed outline around the target and nothing else, plus, when it has a label, that label lifted above the outline, or dropped below it when the rect sits too near the top of its own monitor for the label to clear the desktop's panel. An arrow or a line becomes one stroke along the points it was given, an arrow with an open head at the end. Marks are numbered from 1 in the order the rects arrived, which is the order observe_screen listed the elements in. */
export function shapesFor(spec: OverlaySpec, layout: Layout): Shape[] {
  if (spec.kind === "clear") return [];
  const scale = layout.scale > 0 ? layout.scale : 1;
  const toX = (px: number): number => (px - layout.origin_x) / scale;
  const toY = (px: number): number => (px - layout.origin_y) / scale;

  if (spec.kind === "arrow" || spec.kind === "line" || spec.kind === "path") {
    const points = spec.points;
    if (!Array.isArray(points) || points.length < 2) return [];
    // The first point decides whether this belongs to a screen that is still there; a stroke may then run wherever it likes, including across the join between two monitors.
    if (!monitorFor({ x: points[0][0], y: points[0][1], w: 0, h: 0 }, layout.monitors)) return [];
    const here: Point[] = points.map(([x, y]) => ({ x: toX(x), y: toY(y) }));
    const shapes: Shape[] = [{ kind: "stroke", d: smoothPath(here), width: STROKE_W }];
    if (spec.kind === "arrow") {
      const head = headPath(here);
      if (head !== "") shapes.push({ kind: "stroke", d: head, width: 0, filled: true });
    }
    const text = spec.label || "";
    const start = monitorFor({ x: points[0][0], y: points[0][1], w: 0, h: 0 }, layout.monitors);
    if (text !== "") shapes.push(labelAt(here[0].x, here[0].y, here[0].y, text, layout, start));
    return shapes;
  }

  if (!Array.isArray(spec.rects)) return [];
  const shapes: Shape[] = [];
  spec.rects.slice(0, MARK_CAP).forEach((rect, i) => {
    const monitor = monitorFor(rect, layout.monitors);
    if (!monitor) return;
    const placed = placeRect(rect, layout);
    if (spec.kind === "ring" || spec.kind === "box" || spec.kind === "circle") {
      const x = placed.x;
      const y = placed.y;
      const w = placed.w;
      const h = placed.h;
      // The label is placed against the ink rather than against the target, so a pill above a ring clears the whole outline rather than sitting on it.
      let left = x;
      let top = y;
      let bottom = y + h;
      if (spec.kind === "box") {
        shapes.push({ kind: "stroke", d: boxPath(x, y, w, h), width: 2, dashed: true });
      } else if (spec.kind === "circle") {
        const r = Math.min(w, h) / 2;
        shapes.push({ kind: "stroke", d: circlePath(x + w / 2, y + h / 2, r), width: 2, dashed: true });
        top = y + h / 2 - r;
        bottom = y + h / 2 + r;
      } else {
        const round = ringRect(x, y, w, h);
        shapes.push({ kind: "stroke", d: roundedPath(round.x, round.y, round.w, round.h, RING_RADIUS), width: STROKE_W });
        left = round.x;
        top = round.y;
        bottom = round.y + round.h;
      }
      const text = rect.label || spec.label || "";
      if (text !== "") shapes.push(labelAt(left, top, bottom, text, layout, monitor));
    } else if (spec.kind === "marks") {
      shapes.push({ kind: "mark", x: placed.x, y: placed.y, text: String(i + 1) });
    }
  });
  return shapes;
}

/** Turns one rect in global desktop pixels into the overlay page's CSS pixels. Input: the rect and the window's layout. Output: the rect's left, top, width and height in this page's CSS pixels.
 * One mapping, the window's own scale, for every kind of ink: the monitor rects the layout carries are physical desktop pixels (monitor_rects and union_bounds in lib.rs), the overlay window is sized to the physical union of them, and the daemon's rects are in those same physical pixels, so one divide takes all of them into this page's grid. labelAt and the arrow, line and path branch map their points the same way, which is what keeps a ring, its label and an arrow pointing at the same element in the same place.
 */
function placeRect(rect: OverlayRect, layout: Layout): { x: number; y: number; w: number; h: number } {
  const scale = layout.scale > 0 ? layout.scale : 1;
  return {
    x: (rect.x - layout.origin_x) / scale,
    y: (rect.y - layout.origin_y) / scale,
    w: rect.w / scale,
    h: rect.h / scale,
  };
}

/** Places one label pill. Input: the left edge, the top and the bottom of the ink it names, all in this page's CSS pixels, the text, the layout, and the monitor the ink is on. Output: the label shape, sitting above the ink where there is room and below it where the desktop's own panel would swallow it, and held inside that monitor so no part of the pill runs off the screen. */
function labelAt(x: number, top: number, bottom: number, text: string, layout: Layout, monitor: Monitor | null): Shape {
  const scale = layout.scale > 0 ? layout.scale : 1;
  const toX = (px: number): number => (px - layout.origin_x) / scale;
  const toY = (px: number): number => (px - layout.origin_y) / scale;
  const above = top - LABEL_LIFT;
  const fits = monitor ? above >= toY(monitor.y) + TOP_BAND : above >= TOP_BAND;
  const left = monitor ? toX(monitor.x) : 0;
  const right = monitor ? left + monitor.w / scale : left + labelWidth(text) + LABEL_PAD * 2;
  return {
    kind: "label",
    x: clamp(x, left + LABEL_PAD, right - labelWidth(text) - LABEL_PAD),
    y: fits ? above : bottom + LABEL_DROP,
    text,
    tail: fits ? "down" : "up",
  };
}

/** The rectangle a ring is drawn on. Input: the element in this page's CSS pixels. Output: that rectangle grown by RING_PAD on every side, and then grown about its own centre until it is at least RING_MIN across each way, so a ring on a tiny icon is still something the eye lands on and a ring on a row still hugs the row.
 * A ring drawn from this is the element's own outline: no line under it, no circle beside it, one closed shape and nothing through the middle of the thing it is naming. */
export function ringRect(x: number, y: number, w: number, h: number): { x: number; y: number; w: number; h: number } {
  const rw = Math.max(w + RING_PAD * 2, RING_MIN);
  const rh = Math.max(h + RING_PAD * 2, RING_MIN);
  return { x: x + w / 2 - rw / 2, y: y + h / 2 - rh / 2, w: rw, h: rh };
}

/** The ring itself: one closed rounded outline around an element. Input: the element in this page's CSS pixels. Output: an SVG path starting at the top-left corner and going round clockwise, so the pointer tracing it starts where a hand would. */
export function ringPath(x: number, y: number, w: number, h: number): string {
  const r = ringRect(x, y, w, h);
  return roundedPath(r.x, r.y, r.w, r.h, RING_RADIUS);
}

/** A dashed rounded rectangle over a target. Input: the rect in this page's CSS pixels. Output: an SVG path that starts at the top-left corner and goes round clockwise. */
export function boxPath(x: number, y: number, w: number, h: number): string {
  return roundedPath(x, y, w, h, BOX_RADIUS);
}

/** One rounded rectangle as a path. Input: the rectangle in this page's CSS pixels and the corner radius asked for. Output: an SVG path that starts at the top-left corner, goes round clockwise and closes; the radius is cut down to half the shorter side so a corner never eats a rectangle smaller than itself. */
function roundedPath(x: number, y: number, w: number, h: number, radius: number): string {
  const r = Math.min(radius, w / 2, h / 2);
  return (
    `M ${r1(x + r)} ${r1(y)} H ${r1(x + w - r)} A ${r1(r)} ${r1(r)} 0 0 1 ${r1(x + w)} ${r1(y + r)}` +
    ` V ${r1(y + h - r)} A ${r1(r)} ${r1(r)} 0 0 1 ${r1(x + w - r)} ${r1(y + h)}` +
    ` H ${r1(x + r)} A ${r1(r)} ${r1(r)} 0 0 1 ${r1(x)} ${r1(y + h - r)}` +
    ` V ${r1(y + r)} A ${r1(r)} ${r1(r)} 0 0 1 ${r1(x + r)} ${r1(y)} Z`
  );
}

/** A circle as a path, so it can be drawn on and traced like any other stroke. Input: the centre and radius in this page's CSS pixels. Output: an SVG path starting at the left of the circle and going round once. */
export function circlePath(cx: number, cy: number, r: number): string {
  return `M ${r1(cx - r)} ${r1(cy)} A ${r1(r)} ${r1(r)} 0 1 0 ${r1(cx + r)} ${r1(cy)} A ${r1(r)} ${r1(r)} 0 1 0 ${r1(cx - r)} ${r1(cy)} Z`;
}

/** Where a path begins. Input: an SVG path string that starts with a move. Output: that point, or null when the string does not start with one; the pointer flies here before it draws. */
export function startOf(d: string): Point | null {
  const m = /^M\s*(-?[\d.]+)[ ,]+(-?[\d.]+)/.exec(d);
  return m ? { x: Number(m[1]), y: Number(m[2]) } : null;
}

/** What colour a drawing is in. Input: the event's kind and whether this one is a tap. Output: the mood; anything that points at something is a point, a tap about to happen is an act, an erase is a thing done, and numbering the screen is neutral. */
export function moodFor(kind: string, tap: boolean): Mood {
  if (kind === "clear") return "done";
  if (kind === "tap") return "act";
  if (kind === "marks") return "neutral";
  return tap ? "act" : "point";
}

/** A run of points as one smooth curve. Input: the points in this page's CSS pixels. Output: an SVG path whose corners are rounded off, using the Catmull-Rom construction: each segment becomes a cubic whose handles point along the line between the neighbours on either side, which is what turns a route typed as a handful of points into one drawn stroke. */
export function smoothPath(points: Point[]): string {
  if (points.length === 0) return "";
  if (points.length === 1) return `M ${r1(points[0].x)} ${r1(points[0].y)}`;
  let d = `M ${r1(points[0].x)} ${r1(points[0].y)}`;
  for (let i = 0; i < points.length - 1; i++) {
    const before = points[i - 1] ?? points[i];
    const from = points[i];
    const to = points[i + 1];
    const after = points[i + 2] ?? points[i + 1];
    const c1 = { x: from.x + (to.x - before.x) / 6, y: from.y + (to.y - before.y) / 6 };
    const c2 = { x: to.x - (after.x - from.x) / 6, y: to.y - (after.y - from.y) / 6 };
    d += ` C ${r1(c1.x)} ${r1(c1.y)} ${r1(c2.x)} ${r1(c2.y)} ${r1(to.x)} ${r1(to.y)}`;
  }
  return d;
}

/** The small solid head at the end of an arrow: a triangle whose point is the last point of the stroke and whose corners are swept back along it. Input: the same points the stroke was drawn from. Output: a closed SVG path, empty when there is no direction to point in. */
export function headPath(points: Point[]): string {
  const tip = points[points.length - 1];
  const before = points[points.length - 2];
  if (!tip || !before || (tip.x === before.x && tip.y === before.y)) return "";
  const heading = Math.atan2(tip.y - before.y, tip.x - before.x);
  const corner = (turn: number): string => {
    const a = heading + Math.PI + (turn * HEAD_SPREAD * Math.PI) / 180;
    return `${r1(tip.x + Math.cos(a) * HEAD_ARM)} ${r1(tip.y + Math.sin(a) * HEAD_ARM)}`;
  };
  return `M ${r1(tip.x)} ${r1(tip.y)} L ${corner(-1)} L ${corner(1)} Z`;
}

/** How long a stroke takes to draw itself on. Input: the length of the path in CSS pixels. Output: milliseconds between 400 and 700, so a small ring is quick and a long route across the screen still finishes before the user has read the label. */
export function drawMs(length: number): number {
  return Math.round(clamp(length / DRAW_PX_PER_MS, DRAW_MIN_MS, DRAW_MAX_MS));
}

/** Where a later stroke of a multi-stroke drawing should start, once the one before it has landed. Input: when the stroke just drawn finishes (shaftEnd), the delay that stroke itself started at (cursor), the most staggering may add before strokes start landing together instead (budget), and the gap normally left between one stroke and the next (gap). Output: shaftEnd plus the gap while the running delay is still under budget, so strokes still draw one after another; once budget is reached or a gap would cross it, the budget itself, unmoving, so every stroke after that starts at the same instant. This is what keeps a drawing of many strokes from pushing its last stroke's landing arbitrarily far out: however many strokes a "marks" or "box" event carries, the last one can only land at budget plus that one stroke's own draw time. */
export function nextCursor(shaftEnd: number, cursor: number, budget: number, gap: number): number {
  if (cursor >= budget) return budget;
  return Math.min(shaftEnd + gap, budget);
}

/** Cuts a number to one decimal place, which is finer than a screen pixel and keeps a path string short. */
function r1(v: number): number {
  return Math.round(v * 10) / 10;
}

/** Roughly how wide a label's callout is drawn, in CSS pixels. Input: the text. Output: its width at 13px with the padding the stylesheet gives it, guessed at 7.2 pixels a character because measuring text needs a DOM and this file has none. Only the clamping at the screen edge uses it, so being a few pixels out just moves a label a few pixels. */
export function labelWidth(text: string): number {
  return Math.round(20 + text.length * 7.2);
}

/** The centre of a rectangle, in the overlay page's CSS pixels. Input: a rect in global desktop pixels and the window's layout. Output: the point the pointer flies to and the ripple spreads from. */
export function pointFor(rect: OverlayRect, layout: Layout): Point {
  const scale = layout.scale > 0 ? layout.scale : 1;
  return {
    x: (rect.x - layout.origin_x + rect.w / 2) / scale,
    y: (rect.y - layout.origin_y + rect.h / 2) / scale,
  };
}

/** How wide a tap ripple is drawn. Input: the rect being tapped, in global desktop pixels, and the window's layout. Output: the diameter in this page's CSS pixels, 1.3 times the rect's own height, held between 24 so a tap on a small icon still shows and 96 so a tap on a whole panel is not a circle across somebody's screen. */
export function rippleSize(rect: OverlayRect, layout: Layout): number {
  const scale = layout.scale > 0 ? layout.scale : 1;
  return clamp((rect.h / scale) * RIPPLE_RATIO, RIPPLE_MIN, RIPPLE_MAX);
}

/** Where the pointer comes in from when it has never been anywhere. Input: where it is going and the size of the layer in CSS pixels. Output: a point just off whichever screen edge is nearest the target, so the shortest possible entrance is the one taken. */
export function edgeStart(to: Point, width: number, height: number): Point {
  const gaps = [to.x, width - to.x, to.y, height - to.y];
  const nearest = Math.min(...gaps);
  if (nearest === gaps[0]) return { x: -EDGE_MARGIN, y: to.y };
  if (nearest === gaps[1]) return { x: width + EDGE_MARGIN, y: to.y };
  if (nearest === gaps[2]) return { x: to.x, y: -EDGE_MARGIN };
  return { x: to.x, y: height + EDGE_MARGIN };
}

/** Plans one flight of the pointer. Input: where it is now and where it should end up, both in CSS pixels. Output: the curve and its duration; the duration grows with distance between 420 and 780 milliseconds so a short hop is quick and a trip across the desk still reads as one movement, and the curve's lift grows with distance too so a short hop stays a hop. */
export function flightFor(from: Point, to: Point): Flight {
  const distance = Math.hypot(to.x - from.x, to.y - from.y);
  const lift = Math.min(distance * ARC_LIFT_RATIO, ARC_LIFT_MAX);
  return {
    from,
    to,
    control: { x: (from.x + to.x) / 2, y: (from.y + to.y) / 2 - lift },
    ms: Math.round(Math.min(FLIGHT_MAX_MS, Math.max(FLIGHT_MIN_MS, distance / FLIGHT_PX_PER_MS))),
  };
}

/** Where the pointer is part way through a flight and which way it is going. Input: the flight and a fraction from 0 at the start to 1 at the end. Output: the point on the curve and the heading of the curve there, in degrees measured the same way atan2 measures them. */
export function flightAt(flight: Flight, t: number): { x: number; y: number; deg: number } {
  const u = 1 - t;
  const { from, to, control } = flight;
  return {
    x: u * u * from.x + 2 * u * t * control.x + t * t * to.x,
    y: u * u * from.y + 2 * u * t * control.y + t * t * to.y,
    deg:
      (Math.atan2(
        2 * u * (control.y - from.y) + 2 * t * (to.y - control.y),
        2 * u * (control.x - from.x) + 2 * t * (to.x - control.x),
      ) *
        180) /
      Math.PI,
  };
}

/** Eases from 0 to 1 with no speed and no acceleration at either end, so a movement built on it starts and stops without a visible kick. This is smootherstep, one degree gentler than the 3t squared minus 2t cubed that Clicky uses. */
function smootherstep(t: number): number {
  return t * t * t * (t * (t * 6 - 15) + 10);
}

/** Eases from 0 to 1, gently at both ends. Also the easing overlay/main.ts turns the traced pointer with. */
export function smoothstep(t: number): number {
  return t * t * (3 - 2 * t);
}

/** The whole flight and its landing written out as Web Animations keyframes. Input: the flight and how many steps to cut the flight itself into, one per screen frame by default. Output: the transforms and the offsets they sit at, over a total time of the flight plus SETTLE_MS.
 * The shape of it: the pointer moves along the curve on a smootherstep, so it accelerates out of its resting place and coasts to a stop rather than sliding at a fixed speed; it banks onto its heading over the first fifth of the trip, holds it to two thirds of the way and then turns back upright; it swells to 1.12 at the top of the arc; and on touchdown it gives a little, comes back past its own size and stops.
 * Only transform is animated, so nothing here costs a layout. */
export function flightFrames(flight: Flight, steps = Math.max(20, Math.round(flight.ms / 16))): Keyframe[] {
  const share = flight.ms / (flight.ms + SETTLE_MS);
  const frames: Keyframe[] = [];
  let last = 0;
  for (let i = 0; i <= steps; i++) {
    const p = i / steps;
    const at = flightAt(flight, smootherstep(p));
    // atan2 jumps by a full turn when a flight crosses due west, and a jump in the number is a spin on the screen, so each heading is written as the one nearest the heading before it.
    const heading = unwrap(at.deg, last);
    last = heading;
    const banked = smoothstep(clamp(p / TURN_RISE, 0, 1)) * (1 - smoothstep(clamp((p - TURN_HOLD) / (1 - TURN_HOLD), 0, 1)));
    const turn = (heading - POINTER_HEADING) * banked;
    const swell = 1 + (FLIGHT_SWELL - 1) * Math.sin(Math.PI * p);
    // Offsets keep five decimal places, not two: a flight is cut into as many frames as the screen shows, and rounding those to hundredths would drop several of them onto the same instant and turn the movement into steps.
    frames.push({ transform: transformOf(at.x, at.y, turn, swell), offset: at5(p * share) });
  }
  SETTLE_SCALES.forEach((scale, i) => {
    frames.push({
      transform: transformOf(flight.to.x, flight.to.y, 0, scale),
      offset: at5(share + (1 - share) * SETTLE_OFFSETS[i]),
    });
  });
  return frames;
}

/** One transform string, with every number cut to two decimal places so the same flight always writes the same keyframes. */
function transformOf(x: number, y: number, deg: number, scale: number): string {
  return `translate(${round(x)}px, ${round(y)}px) rotate(${round(deg)}deg) scale(${round(scale)})`;
}

/** Moves an angle by whole turns until it is within half a turn of the one before it. Input: the angle and the previous angle, in degrees. Output: the same direction written as the nearest number to the previous angle. */
export function unwrap(deg: number, previous: number): number {
  return deg + Math.round((previous - deg) / 360) * 360;
}

/** Cuts a number to two decimal places, so the transforms two runs produce for the same flight are the same string. */
function round(v: number): number {
  return Math.round(v * 100) / 100;
}

/** Cuts a number to five decimal places, which is fine enough that no two keyframes of one flight land on the same instant. */
function at5(v: number): number {
  return Math.round(v * 100000) / 100000;
}

/** The last ring the layer drew, kept so a ring that lands on the same place again can be told from a first one. */
export type LastRing = { rect: OverlayRect; at: number };

/** Whether this ring should get a tap ripple as well as a halo. Input: the ring's label, its rectangle, the ring before it if there was one, and the current time in milliseconds. Output: true when the label tells the user to click something, or when a second ring lands on the same rectangle within two seconds, which is what pointing twice at one button looks like. */
export function shouldRipple(label: string, rect: OverlayRect, last: LastRing | null, now: number): boolean {
  if (TAP_WORDS.test(label)) return true;
  if (!last || now - last.at > REPEAT_MS) return false;
  return last.rect.x === rect.x && last.rect.y === rect.y && last.rect.w === rect.w && last.rect.h === rect.h;
}
