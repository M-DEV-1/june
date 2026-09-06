import { describe, expect, it } from "vitest";
import {
  edgeStart,
  flightAt,
  flightFor,
  flightFrames,
  labelWidth,
  monitorFor,
  parseSpec,
  pointFor,
  shapesFor,
  shouldRipple,
  rippleSize,
  boxPath,
  circlePath,
  headPath,
  moodFor,
  smoothPath,
  startOf,
  ringPath,
  drawMs,
  ttlFor,
  keepsPrevious,
  nextCursor,
  NO_ASK,
  type Layout,
} from "./draw";

/** One 1920x1080 monitor at the origin, drawn at scale 1: the desk this machine has, where a desktop pixel and a CSS pixel are the same thing. */
const single: Layout = { origin_x: 0, origin_y: 0, scale: 1, monitors: [{ x: 0, y: 0, w: 1920, h: 1080, scale: 1 }] };

/** Two monitors side by side, the second one to the right, with the overlay window starting at the origin. */
const wide: Layout = {
  origin_x: 0,
  origin_y: 0,
  scale: 1,
  monitors: [
    { x: 0, y: 0, w: 1920, h: 1080, scale: 1 },
    { x: 1920, y: 0, w: 1920, h: 1080, scale: 1 },
  ],
};


/** The one stroke a ring draws, for the assertions that measure it. Input: the shapes of one event. Output: that stroke's path string. */
function strokeOf(shapes: ReturnType<typeof shapesFor>): string {
  const stroke = shapes.find((s) => s.kind === "stroke");
  if (!stroke || stroke.kind !== "stroke") throw new Error("no stroke was drawn");
  return stroke.d;
}

/** The smallest rectangle covering every coordinate named in an SVG path. Input: the path string. Output: that rectangle, which for a rounded outline is the outline's own box. */
function pathBounds(d: string): { x: number; y: number; w: number; h: number } {
  const xs: number[] = [];
  const ys: number[] = [];
  for (const m of d.matchAll(/([MLHVQCA])\s*([-\d.\s,]*)/g)) {
    const nums = (m[2].trim().match(/-?[\d.]+/g) ?? []).map(Number);
    if (m[1] === "H") xs.push(...nums);
    else if (m[1] === "V") ys.push(...nums);
    else if (m[1] === "A") { xs.push(nums[nums.length - 2]); ys.push(nums[nums.length - 1]); }
    else for (let i = 0; i + 1 < nums.length; i += 2) { xs.push(nums[i]); ys.push(nums[i + 1]); }
  }
  return { x: Math.min(...xs), y: Math.min(...ys), w: Math.max(...xs) - Math.min(...xs), h: Math.max(...ys) - Math.min(...ys) };
}


describe("a ring is one mark", () => {
  /** The ring's own rectangle: the element grown by the padding, which is what every assertion below measures against. */
  const ring = { x: 600 - 3, y: 300 - 3, w: 320 + 6, h: 40 + 6 };

  it("draws one closed outline around the element and nothing else through it", () => {
    const spec = { kind: "ring", rects: [{ x: 600, y: 300, w: 320, h: 40 }] };
    const shapes = shapesFor(spec, single);
    expect(shapes).toEqual([{ kind: "stroke", d: ringPath(600, 300, 320, 40), width: 3 }]);
    expect(shapes[0].kind === "stroke" && shapes[0].d.endsWith("Z")).toBe(true);
  });

  it("puts the outline round the element rather than under it, whatever shape the element is", () => {
    for (const rect of [
      { x: 600, y: 300, w: 320, h: 40 },
      { x: 100, y: 200, w: 32, h: 32 },
      { x: 171, y: 78, w: 1270, h: 28 },
    ]) {
      const box = pathBounds(strokeOf(shapesFor({ kind: "ring", rects: [rect] }, single)));
      expect(box.x).toBeLessThanOrEqual(rect.x);
      expect(box.y).toBeLessThanOrEqual(rect.y);
      expect(box.x + box.w).toBeGreaterThanOrEqual(rect.x + rect.w);
      expect(box.y + box.h).toBeGreaterThanOrEqual(rect.y + rect.h);
    }
  });

  // The ring and the pointer are drawn from one rectangle and the pointer starts where the ink starts, so this is what stops the two from ever drifting apart again: both are pinned to the same element.
  it("starts the ink, and so the pointer, on the ring itself", () => {
    const rect = { x: 600, y: 300, w: 320, h: 40 };
    const start = startOf(strokeOf(shapesFor({ kind: "ring", rects: [rect] }, single)));
    expect(start).not.toBeNull();
    expect(start!.x).toBeGreaterThanOrEqual(ring.x);
    expect(start!.x).toBeLessThanOrEqual(ring.x + ring.w);
    expect(start!.y).toBeGreaterThanOrEqual(ring.y);
    expect(start!.y).toBeLessThanOrEqual(ring.y + ring.h);
  });

  it("grows the ring about its own centre so a ring on a tiny icon is still something to look at", () => {
    const box = pathBounds(strokeOf(shapesFor({ kind: "ring", rects: [{ x: 100, y: 200, w: 14, h: 14 }] }, single)));
    expect(box.w).toBeGreaterThanOrEqual(30);
    expect(box.h).toBeGreaterThanOrEqual(30);
    expect(box.x + box.w / 2).toBeCloseTo(107, 5);
    expect(box.y + box.h / 2).toBeCloseTo(207, 5);
  });

  it("keeps the label clear of the ring, above it where there is room and below it where there is not", () => {
    const above = shapesFor({ kind: "ring", label: "subtitles", rects: [{ x: 600, y: 300, w: 320, h: 40 }] }, single);
    expect(above).toContainEqual({ kind: "label", x: 600 - 3, y: ring.y - 34, text: "subtitles", tail: "down" });
    const below = shapesFor({ kind: "ring", label: "Close", rects: [{ x: 40, y: 39, w: 200, h: 32 }] }, single);
    expect(below).toContainEqual({ kind: "label", x: 37, y: 39 + 32 + 3 + 12, text: "Close", tail: "up" });
  });
});

describe("parseSpec", () => {
  it("reads the body the daemon broadcasts", () => {
    const spec = parseSpec('{"kind":"ring","label":"Pause","rects":[{"x":107,"y":984,"w":73,"h":72}],"ttl_ms":3000}');
    expect(spec).toEqual({ kind: "ring", label: "Pause", rects: [{ x: 107, y: 984, w: 73, h: 72 }], ttl_ms: 3000 });
  });

  it("returns null for text that is not JSON", () => {
    expect(parseSpec("not json")).toBeNull();
  });

  it("returns null for JSON that is not an overlay spec", () => {
    expect(parseSpec('"hello"')).toBeNull();
    expect(parseSpec("null")).toBeNull();
    expect(parseSpec('{"rects":[]}')).toBeNull();
  });
});

describe("ttlFor", () => {
  it("uses the ttl the event carries", () => {
    expect(ttlFor({ kind: "ring", ttl_ms: 8000 })).toBe(8000);
  });

  it("falls back to three seconds when there is none", () => {
    expect(ttlFor({ kind: "ring" })).toBe(3000);
    expect(ttlFor({ kind: "ring", ttl_ms: 0 })).toBe(3000);
  });

  it("never schedules a drawing to vanish in the past", () => {
    expect(ttlFor({ kind: "ring", ttl_ms: -5 })).toBe(1);
  });
});

describe("monitorFor", () => {
  it("picks the monitor the rect's centre is on", () => {
    expect(monitorFor({ x: 2000, y: 100, w: 100, h: 100 }, wide.monitors)).toEqual(wide.monitors[1]);
  });

  it("returns null when the centre is off every monitor", () => {
    expect(monitorFor({ x: 5000, y: 5000, w: 10, h: 10 }, wide.monitors)).toBeNull();
  });
});

describe("shapesFor", () => {
  it("rings a wide target and puts its label above it", () => {
    const spec = { kind: "ring", label: "Graph settings", rects: [{ x: 600, y: 300, w: 320, h: 40 }] };
    expect(shapesFor(spec, single)).toEqual([
      { kind: "stroke", d: ringPath(600, 300, 320, 40), width: 3 },
      { kind: "label", x: 597, y: 263, text: "Graph settings", tail: "down" },
    ]);
  });

  it("rings a small target with the same outline, grown to the smallest a ring may be", () => {
    const spec = { kind: "ring", rects: [{ x: 100, y: 200, w: 32, h: 32 }] };
    expect(shapesFor(spec, single)).toEqual([{ kind: "stroke", d: ringPath(100, 200, 32, 32), width: 3 }]);
  });

  it("prefers the label on the rect over the one on the event", () => {
    const spec = { kind: "ring", label: "event", rects: [{ x: 10, y: 200, w: 200, h: 40, label: "rect" }] };
    expect(shapesFor(spec, single)).toContainEqual({ kind: "label", x: 8, y: 163, text: "rect", tail: "down" });
  });

  it("draws no label when neither the rect nor the event names one", () => {
    expect(shapesFor({ kind: "ring", rects: [{ x: 10, y: 200, w: 200, h: 40 }] }, single)).toHaveLength(1);
  });

  it("drops the label below a target that sits under the desktop's panel, where the panel would hide it, and turns its tail up", () => {
    const spec = { kind: "ring", label: "Close", rects: [{ x: 40, y: 39, w: 200, h: 32 }] };
    expect(shapesFor(spec, single)).toContainEqual({ kind: "label", x: 37, y: 86, text: "Close", tail: "up" });
  });

  it("numbers marks from one in the order the rects arrived", () => {
    const spec = {
      kind: "marks",
      rects: [
        { x: 10, y: 10, w: 20, h: 20 },
        { x: 60, y: 10, w: 20, h: 20 },
      ],
    };
    expect(shapesFor(spec, single)).toEqual([
      { kind: "mark", x: 10, y: 10, text: "1" },
      { kind: "mark", x: 60, y: 10, text: "2" },
    ]);
  });

  it("draws a dashed rectangle for a box", () => {
    const spec = { kind: "box", rects: [{ x: 100, y: 100, w: 200, h: 80 }] };
    expect(shapesFor(spec, single)).toEqual([{ kind: "stroke", d: boxPath(100, 100, 200, 80), width: 2, dashed: true }]);
  });

  it("inscribes a circle in the rect it is given, rather than sizing it like a spotlight", () => {
    const spec = { kind: "circle", rects: [{ x: 100, y: 100, w: 200, h: 80 }] };
    expect(shapesFor(spec, single)).toEqual([{ kind: "stroke", d: circlePath(200, 140, 40), width: 2, dashed: true }]);
  });

  it("draws a stroke along the points of a path, with no head", () => {
    const spec = { kind: "path", points: [[100, 100], [300, 200], [500, 150]] as [number, number][] };
    expect(shapesFor(spec, single)).toEqual([
      { kind: "stroke", d: smoothPath([{ x: 100, y: 100 }, { x: 300, y: 200 }, { x: 500, y: 150 }]), width: 3 },
    ]);
  });

  it("puts a solid head on the end of an arrow and a label at the point it starts from", () => {
    const spec = { kind: "arrow", label: "drag here", points: [[100, 100], [500, 400]] as [number, number][] };
    const shapes = shapesFor(spec, single);
    expect(shapes[1]).toEqual({ kind: "stroke", d: headPath([{ x: 100, y: 100 }, { x: 500, y: 400 }]), width: 0, filled: true });
    expect(shapes[2]).toEqual({ kind: "label", x: 100, y: 66, text: "drag here", tail: "down" });
  });

  it("draws nothing for a stroke kind with fewer than two points", () => {
    expect(shapesFor({ kind: "arrow", points: [[10, 10]] as [number, number][] }, single)).toEqual([]);
    expect(shapesFor({ kind: "line" }, single)).toEqual([]);
  });

  it("draws nothing for a stroke whose first point is on no monitor", () => {
    expect(shapesFor({ kind: "line", points: [[5000, 5000], [10, 10]] as [number, number][] }, single)).toEqual([]);
  });

  it("draws nothing for a clear", () => {
    expect(shapesFor({ kind: "clear", rects: [{ x: 0, y: 0, w: 10, h: 10 }] }, single)).toEqual([]);
  });

  it("draws nothing for a kind it does not know", () => {
    expect(shapesFor({ kind: "sparkle", rects: [{ x: 0, y: 0, w: 10, h: 10 }] }, single)).toEqual([]);
  });

  it("drops a rect whose centre is on no monitor", () => {
    expect(shapesFor({ kind: "ring", label: "gone", rects: [{ x: 4000, y: 4000, w: 10, h: 10 }] }, single)).toEqual([]);
  });

  it("places a target on the second monitor relative to the overlay window, not to that monitor", () => {
    const spec = { kind: "ring", rects: [{ x: 2000, y: 300, w: 100, h: 50 }] };
    expect(shapesFor(spec, wide)).toEqual([{ kind: "stroke", d: ringPath(2000, 300, 100, 50), width: 3 }]);
  });

  it("subtracts the overlay window's own corner when a monitor sits left of the origin", () => {
    const offset: Layout = {
      origin_x: -1280,
      origin_y: 0,
      scale: 1,
      monitors: [
        { x: -1280, y: 0, w: 1280, h: 1024, scale: 1 },
        { x: 0, y: 0, w: 1920, h: 1080, scale: 1 },
      ],
    };
    const spec = { kind: "ring", rects: [{ x: -1200, y: 100, w: 160, h: 60 }] };
    expect(shapesFor(spec, offset)).toEqual([{ kind: "stroke", d: ringPath(80, 100, 160, 60), width: 3 }]);
  });

  it("halves every measurement on a doubled display, so the ink lands on the element and not twice as far along", () => {
    const hidpi: Layout = { origin_x: 0, origin_y: 0, scale: 2, monitors: [{ x: 0, y: 0, w: 3840, h: 2160, scale: 2 }] };
    const spec = { kind: "ring", label: "Pause", rects: [{ x: 400, y: 800, w: 400, h: 100 }] };
    expect(shapesFor(spec, hidpi)).toEqual([
      { kind: "stroke", d: ringPath(200, 400, 200, 50), width: 3 },
      { kind: "label", x: 197, y: 363, text: "Pause", tail: "down" },
    ]);
  });

  it("maps every rect on a mixed-scale desk with the window's own scale, the same as the label and the strokes", () => {
    // Rects, labels and strokes all share one mapping, and it is the overlay window's own scale. The monitor rects the layout carries are physical pixels (monitor_rects and union_bounds in lib.rs), the window is sized to the physical union of them (arm_overlay), and the daemon gives its rects in the same physical desktop pixels — so one divide by the window's scale takes all of them into this page's CSS pixels. Dividing a rect by the scale of the monitor it happens to sit on instead put the ink somewhere the label clamp and the arrows could not follow.
    const mixed: Layout = {
      origin_x: 0,
      origin_y: 0,
      scale: 1,
      monitors: [
        { x: 0, y: 0, w: 1920, h: 1080, scale: 1 },
        { x: 1920, y: 0, w: 1920, h: 1080, scale: 2 },
      ],
    };
    const spec = { kind: "ring", label: "Send", rects: [{ x: 2000, y: 100, w: 200, h: 100 }] };
    expect(shapesFor(spec, mixed)).toEqual([
      { kind: "stroke", d: ringPath(2000, 100, 200, 100), width: 3 },
      { kind: "label", x: 1997, y: 63, text: "Send", tail: "down" },
    ]);
  });

  it("draws nothing at all when the monitor list is empty, rather than drawing in the wrong place", () => {
    // This is deliberate: an empty list is what a layout read before the overlay window was mapped looks like, and a rect placed against no monitor would land anywhere. The defect that made this look like a bug was that nothing said so out loud; see the log in draw() in main.ts.
    const blind: Layout = { origin_x: 0, origin_y: 0, scale: 1, monitors: [] };
    expect(shapesFor({ kind: "ring", rects: [{ x: 10, y: 10, w: 200, h: 20 }] }, blind)).toEqual([]);
  });

  it("treats a nonsense scale as one rather than dividing by zero", () => {
    const broken: Layout = { ...single, scale: 0 };
    expect(shapesFor({ kind: "ring", rects: [{ x: 10, y: 10, w: 200, h: 20 }] }, broken)).toEqual([
      { kind: "stroke", d: ringPath(10, 10, 200, 20), width: 3 },
    ]);
  });

  it("draws nothing when the event carries no rects", () => {
    expect(shapesFor({ kind: "ring" }, single)).toEqual([]);
  });

  it("pulls a label back from the right edge so the whole pill stays on the monitor", () => {
    const spec = { kind: "ring", label: "Graph settings", rects: [{ x: 1850, y: 400, w: 60, h: 40 }] };
    const label = shapesFor(spec, single).find((s) => s.kind === "label");
    expect(label?.x).toBe(1920 - labelWidth("Graph settings") - 8);
  });

  it("pulls a label back onto its own monitor, not onto the first one", () => {
    const spec = { kind: "ring", label: "Send", rects: [{ x: 3800, y: 400, w: 30, h: 30 }] };
    const label = shapesFor(spec, wide).find((s) => s.kind === "label");
    expect(label?.x).toBe(3840 - labelWidth("Send") - 8);
  });

  it("leaves a label alone when there is room for it where the target is", () => {
    const spec = { kind: "ring", label: "Send", rects: [{ x: 300, y: 400, w: 200, h: 30 }] };
    expect(shapesFor(spec, single)).toContainEqual({ kind: "label", x: 297, y: 363, text: "Send", tail: "down" });
  });

  it("stops drawing marks once there are more than a screen can show", () => {
    const rects = Array.from({ length: 260 }, (_, i) => ({ x: (i % 60) * 30, y: Math.floor(i / 60) * 30, w: 20, h: 20 }));
    expect(shapesFor({ kind: "marks", rects }, single)).toHaveLength(200);
  });
});

describe("the ink's own shapes", () => {
  it("draws a ring as a closed rounded outline three pixels clear of the element", () => {
    expect(ringPath(100, 200, 300, 40)).toBe(
      "M 105 197 H 395 A 8 8 0 0 1 403 205 V 235 A 8 8 0 0 1 395 243 H 105 A 8 8 0 0 1 97 235 V 205 A 8 8 0 0 1 105 197 Z",
    );
  });

  it("starts a box at its top-left corner and closes it", () => {
    const d = boxPath(10, 20, 100, 50);
    expect(d.startsWith("M 16 20")).toBe(true);
    expect(d.endsWith("Z")).toBe(true);
  });

  it("keeps a box's corners inside a target too small for the full radius", () => {
    expect(boxPath(0, 0, 8, 8).startsWith("M 4 0")).toBe(true);
  });

  it("smooths a run of points into one curve that still passes through each of them", () => {
    const d = smoothPath([{ x: 0, y: 0 }, { x: 100, y: 100 }, { x: 200, y: 0 }]);
    expect(d.startsWith("M 0 0 C")).toBe(true);
    expect(d).toContain("100 100");
    expect(d.endsWith("200 0")).toBe(true);
  });

  it("puts a closed triangle on the end of an arrow, pointing the way the arrow was going", () => {
    const d = headPath([{ x: 0, y: 0 }, { x: 100, y: 0 }]);
    expect(d.startsWith("M 100 0 L")).toBe(true);
    expect(d.endsWith("Z")).toBe(true);
    // Both corners sit back along the shaft, to the left of the tip.
    expect([...d.matchAll(/L (-?[\d.]+)/g)].map((m) => Number(m[1]))).toEqual([86.3, 86.3]);
  });

  it("draws no head when there is no direction to point in", () => {
    expect(headPath([{ x: 5, y: 5 }, { x: 5, y: 5 }])).toBe("");
  });

  it("reads back the point a path starts from, which is where the pointer flies before it draws", () => {
    expect(startOf(ringPath(100, 200, 300, 40))).toEqual({ x: 105, y: 197 });
    expect(startOf("not a path")).toBeNull();
  });

  it("draws a short stroke in 400 milliseconds and a long one in no more than 700", () => {
    expect(drawMs(100)).toBe(400);
    expect(drawMs(1400)).toBe(500);
    expect(drawMs(5000)).toBe(700);
  });
});

describe("moodFor", () => {
  it("is Ora's own colour while it is only numbering the screen", () => {
    expect(moodFor("marks", false)).toBe("neutral");
  });

  it("is the warm colour when Ora is showing you something", () => {
    expect(moodFor("ring", false)).toBe("point");
    expect(moodFor("arrow", false)).toBe("point");
  });

  it("turns to the act colour when a press is coming", () => {
    expect(moodFor("ring", true)).toBe("act");
  });

  it("is the done colour on an erase", () => {
    expect(moodFor("clear", false)).toBe("done");
  });
});

describe("pointFor", () => {
  it("gives the centre of a rect in the page's own pixels", () => {
    expect(pointFor({ x: 600, y: 300, w: 320, h: 40 }, single)).toEqual({ x: 760, y: 320 });
  });

  it("halves the centre on a doubled display and subtracts the window's corner", () => {
    const hidpi: Layout = { origin_x: 200, origin_y: 0, scale: 2, monitors: [] };
    expect(pointFor({ x: 600, y: 300, w: 320, h: 40 }, hidpi)).toEqual({ x: 280, y: 160 });
  });
});

describe("edgeStart", () => {
  it("comes in from the left when the target is nearest the left edge", () => {
    expect(edgeStart({ x: 60, y: 500 }, 1920, 1080)).toEqual({ x: -40, y: 500 });
  });

  it("comes in from the right when the target is nearest the right edge", () => {
    expect(edgeStart({ x: 1880, y: 500 }, 1920, 1080)).toEqual({ x: 1960, y: 500 });
  });

  it("comes in from the top when the target is nearest the top edge", () => {
    expect(edgeStart({ x: 900, y: 20 }, 1920, 1080)).toEqual({ x: 900, y: -40 });
  });

  it("comes in from the bottom when the target is nearest the bottom edge", () => {
    expect(edgeStart({ x: 900, y: 1060 }, 1920, 1080)).toEqual({ x: 900, y: 1120 });
  });
});

describe("flightFor", () => {
  it("lifts the curve's control point above the midpoint of the trip", () => {
    const flight = flightFor({ x: 100, y: 500 }, { x: 900, y: 500 });
    expect(flight.control).toEqual({ x: 500, y: 420 });
  });

  it("keeps a short hop's arc in proportion to the hop rather than looping it over its own target", () => {
    const flight = flightFor({ x: 400, y: 400 }, { x: 440, y: 400 });
    expect(flight.control).toEqual({ x: 420, y: 400 - 40 * 0.2 });
  });

  it("stops lifting the arc past eighty pixels, however far the trip", () => {
    expect(flightFor({ x: 0, y: 500 }, { x: 1800, y: 500 }).control.y).toBe(420);
  });

  it("takes the shortest time for a hop that goes nowhere", () => {
    expect(flightFor({ x: 400, y: 400 }, { x: 400, y: 400 }).ms).toBe(600);
  });

  it("takes longer the further it goes", () => {
    expect(flightFor({ x: 0, y: 0 }, { x: 600, y: 800 }).ms).toBe(1250);
  });

  it("never takes longer than 1400 milliseconds, however far across the desk it goes", () => {
    expect(flightFor({ x: 0, y: 0 }, { x: 3800, y: 1000 }).ms).toBe(1400);
  });
});

describe("flightAt", () => {
  it("starts at the start and ends at the end", () => {
    const flight = flightFor({ x: 100, y: 500 }, { x: 900, y: 500 });
    expect(flightAt(flight, 0)).toMatchObject({ x: 100, y: 500 });
    expect(flightAt(flight, 1)).toMatchObject({ x: 900, y: 500 });
  });

  it("passes through the lifted midpoint of the curve, halfway to the control point", () => {
    const flight = flightFor({ x: 100, y: 500 }, { x: 900, y: 500 });
    const mid = flightAt(flight, 0.5);
    expect(mid.x).toBeCloseTo(500);
    expect(mid.y).toBeCloseTo(460);
  });

  it("faces up on the way out and down on the way in, on a trip to the right", () => {
    const flight = flightFor({ x: 100, y: 500 }, { x: 900, y: 500 });
    expect(flightAt(flight, 0).deg).toBeLessThan(0);
    expect(flightAt(flight, 0.5).deg).toBeCloseTo(0);
    expect(flightAt(flight, 1).deg).toBeGreaterThan(0);
  });
});

/** Pulls the number out of one part of a transform string, for reading a keyframe back in a test. */
function part(frame: Keyframe, name: string): number {
  return Number(new RegExp(`${name}\\((-?[\\d.]+)`).exec(String(frame.transform))?.[1]);
}

describe("flightFrames", () => {
  it("gives one frame for each step, plus the four it lands on", () => {
    expect(flightFrames(flightFor({ x: 0, y: 0 }, { x: 400, y: 400 }), 8)).toHaveLength(13);
  });

  it("cuts the flight into one frame a screen frame when it is not told otherwise", () => {
    const flight = flightFor({ x: 0, y: 0 }, { x: 1200, y: 0 });
    expect(flightFrames(flight)).toHaveLength(Math.round(flight.ms / 16) + 5);
  });

  it("starts at the point it left, upright so no frame asks the pointer to jump from one angle to another, and comes to rest upright on the target", () => {
    const frames = flightFrames(flightFor({ x: 100, y: 500 }, { x: 900, y: 500 }), 8);
    expect(frames[0].transform).toBe("translate(100px, 500px) rotate(0deg) scale(1)");
    expect(frames[12].transform).toBe("translate(900px, 500px) rotate(0deg) scale(1)");
    expect(frames[12].offset).toBe(1);
  });

  it("swells a little at the top of the arc, and only a little, because the ink is what the eye is meant to follow", () => {
    expect(part(flightFrames(flightFor({ x: 0, y: 0 }, { x: 800, y: 0 }), 8)[4], "scale")).toBe(1.12);
  });

  it("gives under the landing and comes back past its own size before it stops", () => {
    const frames = flightFrames(flightFor({ x: 0, y: 0 }, { x: 800, y: 0 }), 8);
    expect(frames.slice(9).map((f) => part(f, "scale"))).toEqual([0.95, 1.03, 1, 1]);
  });

  it("hands the settle the last part of the timeline, with the offsets never going backwards", () => {
    const flight = flightFor({ x: 0, y: 0 }, { x: 800, y: 0 });
    const offsets = flightFrames(flight, 8).map((f) => f.offset as number);
    expect(offsets[8]).toBeCloseTo(flight.ms / (flight.ms + 220), 2);
    expect([...offsets].sort((a, b) => a - b)).toEqual(offsets);
  });

  it("gives every frame of a full-length flight its own instant, so the movement is not cut into steps", () => {
    const offsets = flightFrames(flightFor({ x: 0, y: 0 }, { x: 1900, y: 900 })).map((f) => f.offset as number);
    expect(new Set(offsets).size).toBe(offsets.length);
  });

  it("turns the short way round on a flight heading due west, where atan2 wraps", () => {
    const frames = flightFrames(flightFor({ x: 900, y: 500 }, { x: 100, y: 500 }), 40);
    const jumps = frames.slice(1, 41).map((f, i) => Math.abs(part(f, "rotate") - part(frames[i], "rotate")));
    expect(Math.max(...jumps)).toBeLessThan(90);
  });
});

describe("shouldRipple", () => {
  it("ripples when the label tells the user to click something", () => {
    expect(shouldRipple("Click Save", { x: 0, y: 0, w: 10, h: 10 }, null, 1000)).toBe(true);
  });

  it("does not ripple for a label that only names a thing", () => {
    expect(shouldRipple("Graph settings", { x: 0, y: 0, w: 10, h: 10 }, null, 1000)).toBe(false);
  });

  it("ripples when a second ring lands on the same rect within two seconds", () => {
    const rect = { x: 600, y: 300, w: 320, h: 40 };
    expect(shouldRipple("Graph settings", rect, { rect, at: 1000 }, 2500)).toBe(true);
  });

  it("does not ripple when the second ring comes too late", () => {
    const rect = { x: 600, y: 300, w: 320, h: 40 };
    expect(shouldRipple("Graph settings", rect, { rect, at: 1000 }, 3500)).toBe(false);
  });

  it("draws the ripple to the size of the thing being tapped, not to the size of the screen", () => {
    expect(rippleSize({ x: 0, y: 0, w: 320, h: 40 }, single)).toBeCloseTo(52);
  });

  it("keeps a ripple on a tiny icon big enough to see", () => {
    expect(rippleSize({ x: 0, y: 0, w: 12, h: 12 }, single)).toBe(24);
  });

  it("never lets a ripple on a whole panel grow into a circle across the screen", () => {
    expect(rippleSize({ x: 0, y: 0, w: 900, h: 600 }, single)).toBe(96);
  });

  it("measures a ripple in the page's own pixels on a doubled display", () => {
    const hidpi: Layout = { origin_x: 0, origin_y: 0, scale: 2, monitors: [] };
    expect(rippleSize({ x: 0, y: 0, w: 200, h: 80 }, hidpi)).toBeCloseTo(52);
  });

  it("does not ripple when the second ring lands somewhere else", () => {
    const rect = { x: 600, y: 300, w: 320, h: 40 };
    expect(shouldRipple("Graph settings", rect, { rect: { ...rect, x: 20 }, at: 1000 }, 1500)).toBe(false);
  });
});

describe("nextCursor", () => {
  it("keeps staggering strokes by the gap while the running delay is still under budget", () => {
    expect(nextCursor(500, 100, 3000, 100)).toBe(600);
  });

  it("caps a stroke that would cross the budget to land exactly on it, not one gap past", () => {
    expect(nextCursor(3450, 2950, 3000, 100)).toBe(3000);
  });

  it("freezes further strokes at the budget once it has been reached, so they land together", () => {
    expect(nextCursor(3450, 3000, 3000, 100)).toBe(3000);
  });

  it("keeps a drawing of many strokes bounded to the stagger budget plus one stroke's own draw time, however many strokes it has", () => {
    const budget = 3000;
    const gap = 100;
    const ms = drawMs(999999); // the longest a stroke may ever take to draw itself
    let cursor = 0;
    let shaftEnd = 0;
    for (let i = 0; i < 200; i++) {
      shaftEnd = cursor + ms;
      cursor = nextCursor(shaftEnd, cursor, budget, gap);
    }
    expect(shaftEnd).toBeLessThanOrEqual(budget + ms);
  });
});

describe("keepsPrevious", () => {
  const box = { kind: "box", rects: [{ x: 0, y: 0, w: 10, h: 10 }] };

  it("joins a drawing from the same ask, which is the two-boxes-in-one-answer case", () => {
    expect(keepsPrevious("ask-7", "ask-7", box)).toBe(true);
  });

  it("replaces when a different ask draws, because the old answer's ink is stale", () => {
    expect(keepsPrevious("ask-8", "ask-7", box)).toBe(false);
  });

  it("replaces when the layer is empty", () => {
    expect(keepsPrevious("ask-7", null, box)).toBe(false);
  });

  it("replaces for marks, so one screen never carries two number ones", () => {
    const marks = { kind: "marks", rects: [{ x: 0, y: 0, w: 10, h: 10 }] };
    expect(keepsPrevious("ask-7", "ask-7", marks)).toBe(false);
  });

  it("replaces for a drawing no ask made, so one program's ink never piles onto another's", () => {
    expect(keepsPrevious(NO_ASK, NO_ASK, box)).toBe(false);
  });
});
