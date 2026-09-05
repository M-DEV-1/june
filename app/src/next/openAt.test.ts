/** Tests for parseOpenAt: what a clicked notice's target parses to, and what gets rejected instead of trusted. */

import { describe, expect, it } from "vitest";

import { parseOpenAt } from "./openAt";

describe("parseOpenAt", () => {
  it("reads a place and a row", () => {
    expect(parseOpenAt(JSON.stringify({ place: "routines", id: "7" }))).toEqual({ place: "routines", id: "7" });
  });

  it("reads a place with no row as no id, not an empty one", () => {
    expect(parseOpenAt(JSON.stringify({ place: "days", id: "" }))).toEqual({ place: "days", id: undefined });
    expect(parseOpenAt(JSON.stringify({ place: "days" }))).toEqual({ place: "days", id: undefined });
  });

  it("rejects nothing stored, malformed JSON, and a place this window has no screen for", () => {
    expect(parseOpenAt(null)).toBeUndefined();
    expect(parseOpenAt("not json")).toBeUndefined();
    expect(parseOpenAt(JSON.stringify({ place: "nowhere", id: "1" }))).toBeUndefined();
    expect(parseOpenAt(JSON.stringify({ id: "1" }))).toBeUndefined();
  });
});
