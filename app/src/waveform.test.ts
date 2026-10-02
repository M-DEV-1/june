/** Tests for the hover's braille waveform: the smoothing and the render shape. */

import { describe, expect, it } from "vitest";

import { Waveform, workingRow, WORKING_PERIOD_MS } from "./waveform";

describe("Waveform", () => {
  it("rises fast toward a loud sample", () => {
    const w = new Waveform(10);
    w.update(1.0);
    expect(w.smoothed).toBeGreaterThanOrEqual(0.5);
  });

  it("smooths the falling edge instead of dropping straight to zero", () => {
    const w = new Waveform(10);
    for (let i = 0; i < 5; i++) w.update(1.0);
    const high = w.smoothed;
    expect(high).toBeGreaterThanOrEqual(0.5);

    w.update(0.0);
    expect(w.smoothed).not.toBe(0);
    expect(w.smoothed).toBeLessThan(high);
  });

  it("renders four rows — blank far rows, dim centreline near rows — while silent", () => {
    const w = new Waveform(8);
    const [farTop, nearTop, nearBottom, farBottom] = w.render();
    expect(farTop).toBe("⠀".repeat(8));
    expect(nearTop).toBe("⣀".repeat(8));
    expect(nearBottom).toBe("⠉".repeat(8));
    expect(farBottom).toBe("⠀".repeat(8));
  });

  it("saturates the near rows to a full block before the far rows fill at all", () => {
    const w = new Waveform(20);
    // A moderate, non-maxed level should fill the near rows solid without spilling into the far rows yet.
    for (let i = 0; i < 10; i++) w.update(0.25);
    const [farTop, nearTop, nearBottom, farBottom] = w.render();
    expect(nearTop).toContain("⣿");
    expect(nearBottom).toContain("⣿");
    expect(farTop).toBe("⠀".repeat(20));
    expect(farBottom).toBe("⠀".repeat(20));
  });

  it("fills the far rows once a column's amplitude saturates past the near rows", () => {
    const w = new Waveform(20);
    for (let i = 0; i < 10; i++) w.update(1.0);
    const [farTop, nearTop, nearBottom, farBottom] = w.render();
    // width 20 spans enough columns that buildVariation's sine mix puts at least one near its 1.0 peak, which at smoothed≈1 pushes that column's level past 4 and fills its far rows too.
    expect(nearTop).toContain("⣿");
    expect(nearBottom).toContain("⣿");
    expect(farTop).toContain("⣿");
    expect(farBottom).toContain("⣿");
  });
});

describe("workingRow", () => {
  it("is a wave that travels: the same moment draws the same row, a later moment a different one, and it is never flat", () => {
    expect(workingRow(12, 700)).toBe(workingRow(12, 700));
    expect(workingRow(12, 700)).not.toBe(workingRow(12, 1400));
    expect(new Set([...workingRow(12, 0)]).size).toBeGreaterThan(1);
  });

  it("stays a whole period long, so the loop point is invisible", () => {
    expect(workingRow(12, 0)).toBe(workingRow(12, WORKING_PERIOD_MS));
  });
});
