/** Tests for parseOpenAt: what a clicked notice's target parses to, and what gets rejected instead of trusted. */

import { describe, expect, it } from "vitest";

import { parseOpenAt } from "./openAt";

describe("parseOpenAt", () => {
  it("rejects nothing stored, malformed JSON, and a place this window has no screen for", () => {
    expect(parseOpenAt(null)).toBeUndefined();
    expect(parseOpenAt("not json")).toBeUndefined();
    expect(parseOpenAt(JSON.stringify({ place: "nowhere", id: "1" }))).toBeUndefined();
    expect(parseOpenAt(JSON.stringify({ id: "1" }))).toBeUndefined();
  });
});
