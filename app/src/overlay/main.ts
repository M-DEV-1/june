/** The drawing layer over the whole desktop. It is one transparent, click-through, always-on-top window that shows nothing until the daemon sends an "overlay" event, then flies a small triangle to where the drawing starts and draws it: an underline or a dashed spotlight on a target ("ring"), a dashed rectangle ("box"), a dashed circle ("circle"), a stroke along a run of points ("path" and "line") with a solid head on the end of an "arrow", numbers on several rectangles ("marks"), or an erase ("clear"). The ink appears from one end to the other as the triangle travels along it, so the drawing looks drawn. Everything fades again after the event's ttl_ms, and a kind this page does not know is ignored.
 *
 * This replaces the GNOME Shell extension in overlay/extension.js, which drew the same shapes but could not be reloaded on a Wayland session without logging out, so a shell holding a stale copy in memory drew nothing at all.
 *
 * The page does not talk to the daemon itself. Rust holds the connection to its event stream and hands each event over as a Tauri event, because this app's WebKit delivers a slow HTTP stream in held-back scraps: measured on 2026-09-04, a ring the daemon flushed in nine milliseconds had still not reached the page thirteen seconds later. See src-tauri/src/overlay.rs.
 */

import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import {
  edgeStart,
  flightFor,
  flightFrames,
  parseSpec,
  pointFor,
  shapesFor,
  shouldRipple,
  rippleSize,
  drawMs,
  moodFor,
  startOf,
  ttlFor,
  keepsPrevious,
  NO_ASK,
  POINTER_HEADING,
  SETTLE_MS,
  type LastRing,
  type Layout,
  type Mood,
  type Point,
  type Shape,
} from "./draw";

/** The Tauri event Rust hands each daemon event over as. Its payload is the text of one event off the daemon's stream. */
const DAEMON_EVENT = "ora://daemon-event";

/** How long the whole drawing takes to fade away, in milliseconds. */
const FADE_MS = 320;

/** The colour of each mood. Neutral is Ora's own accent, halfway between the purple the app uses on a light theme and the one it uses on a dark one, so it reads on either. Point is the warm orange everything Ora shows you is drawn in, act is the red of a press about to happen, and done is the green of a thing finished. */
const MOODS: Record<Mood, string> = {
  neutral: "#7b68f5",
  point: "#fb7a1e",
  act: "#e5484d",
  done: "#2fb46e",
};

/** How long after the ink has finished the label appears, in milliseconds. The pointer arrives, the stroke is drawn, then the name of the thing follows, which is the order the three would happen if a person were doing the pointing. */
const LABEL_LAG = 80;

/** How a stroke draws itself on: quick at first, easing off as it reaches the end, the way a hand slows at the end of a line. */
const INK_EASING = "cubic-bezier(0.33, 0, 0.2, 1)";

/** How long the pointer stays on screen after the ink is finished, in milliseconds. The ink outlives it, because the ink is what the user is being shown and the pointer is only what drew it. */
const POINTER_IDLE_MS = 1500;

/** How long apart the two circles of a tap ripple start, in milliseconds. The circle itself runs for 380, so the whole tap is over 500 milliseconds after the pointer lands. */
const RIPPLE_GAP_MS = 120;

const layer = document.getElementById("layer") as HTMLElement;
const shapesEl = document.getElementById("shapes") as HTMLElement;
const inkEl = document.getElementById("ink") as unknown as SVGSVGElement;
const pointer = document.getElementById("pointer") as unknown as HTMLElement;

/** The desk as the last layout call described it: nothing until Rust answers, which is why the first thing start() does is ask. */
let layout: Layout = { origin_x: 0, origin_y: 0, scale: 1, monitors: [] };

/** Where the pointer is resting, in this page's CSS pixels, or null while it has never been anywhere and must fly in from a screen edge. */
let restingAt: Point | null = null;

/** The last ring drawn, so a second ring on the same rectangle can be recognised as a tap. */
let lastRing: LastRing | null = null;

/** The timers that fade the drawing when its ttl runs out and the pointer when it has sat still long enough. */
let clearTimer: number | undefined;
let pointerTimer: number | undefined;

/** The id of the ask whose drawing is on the layer now, or null when the layer is empty. Every overlay event carries the id of the ask that drew it, or "overlay" when no ask did, which is how a drawing that belongs with the one already up is told from one that replaces it. */
let drawingAsk: string | null = null;

/** One event as the daemon's /events stream sends it. Only "overlay" matters here; every other type belongs to the hover window. */
type DaemonEvent = { id: string; type: string; text?: string };

/** Whether this desk has asked for less movement. Input: none. Output: true when the pointer should jump to its target instead of flying there. */
function stillness(): boolean {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

/** Where SVG elements have to be made, because document.createElement would make an HTML element of the same name that draws nothing. */
const SVG_NS = "http://www.w3.org/2000/svg";

/** Counts the masks made so far, so each stroke's mask has an id nothing else on the page answers to. */
let maskCount = 0;

/** How many pieces a traced stroke is cut into for the pointer to follow. One every eight pixels of path is finer than the eye can tell on a curve and cheap enough on a stroke across the whole desk. */
const TRACE_STEP_PX = 8;

/** The fraction of a traced stroke over which the pointer turns onto that stroke's heading, having arrived upright. */
const TRACE_TURN_IN = 0.125;

/** Puts one set of shapes on the layer and takes off whatever was there. Input: the shapes, already in this page's CSS pixels, and how long from now the pointer touches down. Output: when the last stroke has finished drawing itself, in milliseconds from now, which is when the label may appear.
 * The order on screen is the order a person doing it would take: the pointer arrives at the start of the stroke, travels along it while the ink appears behind it, and only when it has stopped does the pill naming the thing show up.
 * When keep is set the layer is left as it is and these shapes are added to it, so several drawings can stand together; the shapes already there keep the animations they are part-way through. */
function render(shapes: Shape[], arrival = 0, keep = false): number {
  for (const el of [shapesEl, inkEl]) {
    if (!keep) {
      el.getAnimations({ subtree: true }).forEach((a) => a.cancel());
      el.replaceChildren();
    }
    el.style.opacity = "";
  }

  let inkEnd = arrival;
  let traced = false;

  for (const shape of shapes) {
    if (shape.kind !== "stroke") continue;
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", shape.d);
    path.setAttribute("class", shape.filled ? "head" : shape.dashed ? "stroke dashed" : "stroke");
    if (!shape.filled) path.style.strokeWidth = `${shape.width}px`;
    inkEl.appendChild(path);

    if (shape.filled) {
      // The head of an arrow is a solid triangle, so there is nothing to draw on: it appears once the shaft it belongs to has arrived.
      path.style.animationDelay = `${inkEnd}ms`;
      continue;
    }

    const length = path.getTotalLength();
    const ms = stillness() ? 1 : drawMs(length);
    // The stroke is revealed through a mask whose own line grows along the same path. A dashed stroke cannot use its dash pattern to reveal itself, because that pattern is already the dashes, so every stroke is revealed the same way.
    reveal(path, length, arrival, ms);
    if (!traced && !stillness()) {
      tracePointer(path, length, arrival, ms);
      traced = true;
    }
    inkEnd = Math.max(inkEnd, arrival + ms);
  }

  for (const shape of shapes) {
    if (shape.kind !== "label" && shape.kind !== "mark") continue;
    const el = document.createElement("div");
    el.style.left = `${shape.x}px`;
    el.style.top = `${shape.y}px`;
    if (shape.kind === "label") {
      el.className = `label tail-${shape.tail}`;
      el.textContent = shape.text;
      el.style.animationDelay = `${inkEnd + LABEL_LAG}ms`;
    } else {
      el.className = "mark";
      el.textContent = shape.text;
    }
    shapesEl.appendChild(el);
  }
  return inkEnd;
}

/** Makes one stroke appear from its start to its end rather than all at once. Input: the path, its length, when to start and how long to take. Output: nothing; the path is put behind a mask whose own line is dashed with the whole length and has that dash slid off. */
function reveal(path: SVGPathElement, length: number, delay: number, ms: number): void {
  const id = `ink-mask-${++maskCount}`;
  const mask = document.createElementNS(SVG_NS, "mask");
  mask.setAttribute("id", id);
  mask.setAttribute("maskUnits", "userSpaceOnUse");
  const line = document.createElementNS(SVG_NS, "path");
  line.setAttribute("d", path.getAttribute("d") ?? "");
  line.setAttribute("class", "mask-line");
  // Wide enough that the mask never clips the dashes or the round cap of the stroke it is revealing.
  line.style.strokeWidth = `${Number.parseFloat(path.style.strokeWidth || "3") + 6}px`;
  line.style.strokeDasharray = `${length}`;
  mask.appendChild(line);
  inkEl.appendChild(mask);
  path.setAttribute("mask", `url(#${id})`);
  line.animate([{ strokeDashoffset: length }, { strokeDashoffset: 0 }], {
    duration: ms,
    delay,
    easing: INK_EASING,
    fill: "both",
  });
}

/** Walks the pointer along a stroke as that stroke is drawn. Input: the path, its length, when to start and how long to take. Output: nothing; the pointer is left standing at the end of the path.
 * Both this and the reveal are given the same duration and the same easing, so the pointer's tip is always at the far end of the ink behind it. */
function tracePointer(path: SVGPathElement, length: number, delay: number, ms: number): void {
  const steps = Math.max(8, Math.min(120, Math.round(length / TRACE_STEP_PX)));
  const frames: Keyframe[] = [];
  let last = 0;
  for (let i = 0; i <= steps; i++) {
    const here = path.getPointAtLength((length * i) / steps);
    const ahead = path.getPointAtLength(Math.min(length, (length * i) / steps + 1));
    const behind = path.getPointAtLength(Math.max(0, (length * i) / steps - 1));
    const heading = headingOf(behind, ahead, last);
    last = heading;
    // The flight before this one ends upright and the pointer has to be upright again when it stops, or it would sit at the end of an arrow looking like a second arrowhead. So the turn onto the stroke's own heading is eased in over the first eighth of it and out again over the last.
    const p = i / steps;
    const banked = ease(Math.min(1, p / TRACE_TURN_IN)) * (1 - ease(Math.min(1, Math.max(0, p - 1 + TRACE_TURN_IN) / TRACE_TURN_IN)));
    const turn = Math.round(heading * banked * 100) / 100;
    frames.push({ transform: `translate(${here.x}px, ${here.y}px) rotate(${turn}deg) scale(1)`, offset: i / steps });
  }
  const end = path.getPointAtLength(length);
  restingAt = { x: end.x, y: end.y };
  pointer.style.transform = `translate(${end.x}px, ${end.y}px) rotate(0deg) scale(1)`;
  pointer.animate(frames, { duration: ms, delay, easing: INK_EASING, fill: "backwards" });
}

/** Which way the pointer faces at one place along a stroke. Input: the points just behind and just ahead of it and the heading it had a moment ago. Output: the rotation in degrees, written as the number nearest the previous one so a stroke crossing due west does not spin the pointer round. */
function headingOf(behind: DOMPoint, ahead: DOMPoint, previous: number): number {
  const raw = (Math.atan2(ahead.y - behind.y, ahead.x - behind.x) * 180) / Math.PI - POINTER_HEADING;
  return raw + Math.round((previous - raw) / 360) * 360;
}

/** Eases from 0 to 1, gently at both ends. */
function ease(t: number): number {
  return t * t * (3 - 2 * t);
}

/** Sets the colour every piece of ink, the pointer and the label pill are drawn in. Input: the mood. Output: nothing; the stylesheet tweens the change over 200 ms, so a drawing that turns from showing to about-to-press changes colour rather than blinking. */
function setMood(mood: Mood): void {
  document.documentElement.style.setProperty("--ink", MOODS[mood]);
}

/** Sends the pointer to a place on the layer. Input: the target, in this page's CSS pixels. Output: how many milliseconds the trip takes, which is when everything else about the event should show up; a desk asking for less movement gets a jump and a zero. */
function flyTo(to: Point): number {
  const from = restingAt ?? edgeStart(to, layer.clientWidth, layer.clientHeight);
  const flight = flightFor(from, to);
  restingAt = to;

  pointer.getAnimations().forEach((a) => a.cancel());
  // The resting transform lives in the element's own style, so the flight can be played without a fill and still leave the pointer standing at the target when it ends.
  pointer.style.transform = `translate(${to.x}px, ${to.y}px) rotate(0deg) scale(1)`;
  pointer.style.opacity = "1";
  if (stillness()) return 0;

  // Linear, because the easing is already baked into where the sampled keyframes sit along the curve; asking for it twice would flatten the ends into a crawl.
  pointer.animate(flightFrames(flight), { duration: flight.ms + SETTLE_MS, easing: "linear" });
  return flight.ms;
}

/** Draws the two circles of a tap ripple on the pointer's tip. Input: the point in CSS pixels, how wide the circles grow, and how long to wait before the first one starts. Output: nothing. */
function ripple(at: Point, size: number, delay: number): void {
  for (let i = 0; i < 2; i++) {
    const el = document.createElement("div");
    el.className = "ripple";
    el.style.left = `${at.x}px`;
    el.style.top = `${at.y}px`;
    el.style.width = `${size}px`;
    el.style.height = `${size}px`;
    el.style.animationDelay = `${delay + i * RIPPLE_GAP_MS}ms`;
    shapesEl.appendChild(el);
  }
}

/** Takes the whole drawing off the screen, gently. Input: none. Output: nothing. */
function fadeOut(): void {
  pointer.style.opacity = "0";
  const going = [shapesEl, inkEl].map((el) =>
    el.animate([{ opacity: 1 }, { opacity: 0 }], { duration: FADE_MS, easing: "ease", fill: "forwards" }),
  );
  going[0].onfinish = (): void => {
    render([]);
    setMood("neutral");
  };
}

/** Draws one overlay event and schedules its removal. Input: the text of the event, which is the POST /overlay body as JSON, and the id of the ask that drew it. Output: nothing; text that is not a spec leaves whatever is on screen alone, because erasing on a garbled event would take a ring away mid-look. */
function draw(text: string, askID: string): void {
  const spec = parseSpec(text);
  if (!spec) return;
  if (clearTimer !== undefined) clearTimeout(clearTimer);
  if (pointerTimer !== undefined) clearTimeout(pointerTimer);

  if (spec.kind === "clear") {
    setMood("done");
    fadeOut();
    drawingAsk = null;
    return;
  }

  const keep = keepsPrevious(askID, drawingAsk, spec);
  drawingAsk = askID;

  const shapes = shapesFor(spec, layout);
  const rect = spec.rects?.[0];
  const now = Date.now();
  const tap = spec.kind === "ring" && rect !== undefined && shouldRipple(rect.label || spec.label || "", rect, lastRing, now);
  setMood(moodFor(spec.kind, tap));
  if (rect && spec.kind === "ring") lastRing = { rect, at: now };

  // The pointer flies to where the first stroke begins, because that is where the drawing starts; an event with no ink to draw leaves it where it was.
  const first = shapes.find((s) => s.kind === "stroke" && !s.filled);
  const start = first && first.kind === "stroke" ? startOf(first.d) : null;
  const arrival = start ? flyTo(start) : 0;

  const inkEnd = render(shapes, arrival, keep);

  if (rect && tap) ripple(pointFor(rect, layout), rippleSize(rect, layout), inkEnd);
  if (start) pointerTimer = window.setTimeout(() => (pointer.style.opacity = "0"), inkEnd + POINTER_IDLE_MS);

  // The ttl is time the drawing is meant to be readable, so it starts once the ink is finished rather than once the event arrived.
  clearTimer = window.setTimeout(fadeOut, ttlFor(spec) + inkEnd);
}

/** Acts on one event off the daemon's stream. Input: the JSON text of one event. Output: nothing; anything that is not an overlay event is another window's business. */
function onEvent(payload: string): void {
  let ev: DaemonEvent;
  try {
    ev = JSON.parse(payload) as DaemonEvent;
  } catch {
    return;
  }
  if (ev.type === "overlay" && typeof ev.text === "string") draw(ev.text, ev.id);
}

/** Asks Rust where this window sits on the desk and how big its pixels are, so global screen coordinates can be turned into positions on the page. Input: none. Output: nothing; a failed call leaves the last layout in place. */
async function readLayout(): Promise<void> {
  try {
    layout = await invoke<Layout>("overlay_layout");
  } catch {
    /* Not running inside Tauri, or the app is shutting down: the last layout stands. */
  }
}

/** One ring, for a page opened with ?demo=1 and nothing else to say. */
const DEMO = '{"kind":"ring","label":"Click demo","rects":[{"x":400,"y":400,"w":300,"h":120}],"ttl_ms":15000}';

/** Starts the layer: learn the shape of the desk, then take events from Rust. A page opened with ?demo=1 draws one fixed ring instead, and ?demo= followed by the body of a POST /overlay draws that, which is how any of these drawings can be looked at in a plain browser with no daemon and no app around them. */
async function start(): Promise<void> {
  await readLayout();
  // A monitor plugged in or a resolution change makes Rust resize this window, and the resize is the page's cue that the desk it draws on has changed shape.
  window.addEventListener("resize", () => void readLayout());

  const query = new URLSearchParams(location.search);
  const demo = query.get("demo");
  if (demo !== null) {
    // Opened in a plain browser there is no Rust to ask, so the desk is taken to be this one window.
    if (layout.monitors.length === 0) {
      layout = { origin_x: 0, origin_y: 0, scale: 1, monitors: [{ x: 0, y: 0, w: innerWidth, h: innerHeight, scale: 1 }] };
    }
    draw(demo.startsWith("{") ? demo : DEMO, NO_ASK);
    // ?at=1500 winds the whole drawing to a millisecond and holds it there, so one frame of it can be looked at or photographed without racing the clock.
    const at = Number(query.get("at"));
    if (at > 0) {
      requestAnimationFrame(() => {
        for (const animation of document.getAnimations()) {
          animation.currentTime = at;
          animation.pause();
        }
      });
    }
    return;
  }
  await listen<string>(DAEMON_EVENT, (e) => onEvent(e.payload));
}

void start();
