/** Tests for the only part of remembering the window's place that is worth one: reading back what was stored, which has to refuse anything that would put the window somewhere a person cannot get at it. */

import { describe, expect, it } from "vitest";

import { parseGeometry } from "./window";

describe("reading a stored window geometry", () => {
  it("takes a whole rectangle back", () => {
    expect(parseGeometry(JSON.stringify({ x: 120, y: 80, width: 1200, height: 800 }))).toEqual({ x: 120, y: 80, width: 1200, height: 800 });
  });

  it("refuses nothing, rubbish, and a shape with a field missing", () => {
    expect(parseGeometry(null)).toBeUndefined();
    expect(parseGeometry("")).toBeUndefined();
    expect(parseGeometry("not json")).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: 0, y: 0, width: 900 }))).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: "0", y: 0, width: 900, height: 700 }))).toBeUndefined();
  });

  it("refuses a window too small to be worth putting back, which is what a minimised one stores", () => {
    expect(parseGeometry(JSON.stringify({ x: 0, y: 0, width: 100, height: 700 }))).toBeUndefined();
    expect(parseGeometry(JSON.stringify({ x: 0, y: 0, width: 900, height: 40 }))).toBeUndefined();
  });

  it("refuses a rectangle that is not a finite number, so no window is ever placed at NaN", () => {
    expect(parseGeometry('{"x":null,"y":0,"width":900,"height":700}')).toBeUndefined();
  });
});
