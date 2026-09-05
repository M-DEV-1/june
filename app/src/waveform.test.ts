/** Tests for the hover's braille waveform, ported alongside waveform.ts from internal/ui/waveform_test.go so the smoothing and the render shape stay the ones the terminal client already proved out. */

import { describe, expect, it } from "vitest";

import { buildVariation, renderLevelEvent, Waveform } from "./waveform";

describe("buildVariation", () => {
  it("keeps every column's multiplier in (0, 1]", () => {
    const v = buildVariation(20);
    expect(v).toHaveLength(20);
    for (const x of v) {
      expect(x).toBeGreaterThan(0);
      expect(x).toBeLessThanOrEqual(1.0);
    }
  });
});

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

  it("builds one variation multiplier per column at construction", () => {
    const w = new Waveform(20);
    expect(w.width).toBe(20);
    expect(w.variation).toHaveLength(20);
  });

  it("falls back to width 40 for a non-positive width", () => {
    const w = new Waveform(0);
    expect(w.width).toBe(40);
    expect(w.variation).toHaveLength(40);
  });

  it("renders the dim centreline in both rows while silent", () => {
    const w = new Waveform(8);
    const [top, bottom] = w.render();
    expect(top).toBe("⣀".repeat(8));
    expect(bottom).toBe("⠉".repeat(8));
  });

  it("renders two rows of the same width as the bar, both changed once loud", () => {
    const w = new Waveform(12);
    for (let i = 0; i < 10; i++) w.update(1.0);
    const [top, bottom] = w.render();
    expect(top).toHaveLength(12);
    expect(bottom).toHaveLength(12);
    expect(top).not.toBe("⣀".repeat(12));
    expect(bottom).not.toBe("⠉".repeat(12));
  });

  it("fills a full block once a column's amplitude saturates the gamma curve", () => {
    const w = new Waveform(20);
    for (let i = 0; i < 10; i++) w.update(1.0);
    const [top, bottom] = w.render();
    // width 20 spans enough columns that buildVariation's sine mix puts at least one near its 1.0 peak, which at smoothed≈1 saturates that column's near level to 4 (the full-block braille cell) on both rows.
    expect(top).toContain("⣿");
    expect(bottom).toContain("⣿");
  });
});

describe("renderLevelEvent", () => {
  it("parses the mic/speaker reading out of a level event's Detail", () => {
    expect(renderLevelEvent('{"mic":0.42,"speaker":0.13}')).toEqual({ mic: 0.42, speaker: 0.13 });
  });

  it("returns zeros for malformed JSON instead of throwing", () => {
    expect(renderLevelEvent("not json")).toEqual({ mic: 0, speaker: 0 });
    expect(renderLevelEvent("")).toEqual({ mic: 0, speaker: 0 });
  });

  it("defaults a missing field to zero rather than undefined", () => {
    expect(renderLevelEvent('{"mic":0.5}')).toEqual({ mic: 0.5, speaker: 0 });
  });
});
