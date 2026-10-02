/** Tests for the only part of remembering the window's place that is worth one: reading back what was stored, which has to refuse anything that would put the window somewhere a person cannot get at it. */

import { describe, expect, it } from "vitest";

import { parseGeometry } from "./window";

describe("reading a stored window geometry", () => {
  it("refuses nothing, rubbish, a shape with a field missing, and a window too small to be worth putting back, which is what a minimised one stores", () => {
    expect(parseGeometry(null)).toBeUndefined();
    expect(parseGeometry("")).toBeUndefined();
    expect(parseGeometry("not json")).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: 0, y: 0, width: 900 }))).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: "0", y: 0, width: 900, height: 700 }))).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: 0, y: 0, width: 100, height: 700 }))).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: 0, y: 0, width: 900, height: 40 }))).toBeUndefined();
  });
});
