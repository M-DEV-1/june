import { describe, expect, it } from "vitest";
import { parseSpec, shapesFor, ringPath, keepsPrevious, type Layout } from "./draw";

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

describe("parseSpec", () => {
  it("returns null for JSON that is not an overlay spec", () => {
    expect(parseSpec('"hello"')).toBeNull();
    expect(parseSpec("null")).toBeNull();
    expect(parseSpec('{"rects":[]}')).toBeNull();
  });
});

describe("shapesFor", () => {
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
});

describe("keepsPrevious", () => {
  const box = { kind: "box", rects: [{ x: 0, y: 0, w: 10, h: 10 }] };
  const drawn = (ask: string, group = ""): { ask: string; group: string } => ({ ask, group });

  it("joins a drawing from the same ask, which is the two-boxes-in-one-answer case", () => {
    expect(keepsPrevious("ask-7", drawn("ask-7"), box)).toBe(true);
  });

  it("replaces when a different ask draws, because the old answer's ink is stale", () => {
    expect(keepsPrevious("ask-8", drawn("ask-7"), box)).toBe(false);
  });
});
