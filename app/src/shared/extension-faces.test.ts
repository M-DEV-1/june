// The GNOME Shell extension (overlay/extension.js) cannot import faces.ts — it's plain GJS, not bundled — so it keeps its own hand-copied FACES table for the word beside the clock. This test reads both files off disk and checks the copy has not drifted from the original.

import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { FACES } from "./faces";

const here = dirname(fileURLToPath(import.meta.url));
const extensionPath = join(here, "..", "..", "..", "overlay", "extension.js");

// Pulls the FACES table's entries out of extension.js's source text. Input: the file's source. Output: a map from state name to the face string, read straight off the object literal's "state: 'face'," lines.
function readExtensionFaces(source: string): Record<string, string> {
  const block = source.match(/const FACES = \{([\s\S]*?)\};/);
  if (!block) throw new Error("extension.js has no FACES table");
  const entries: Record<string, string> = {};
  for (const m of block[1].matchAll(/(\w+):\s*'([^']*)'/g)) {
    entries[m[1]] = m[2];
  }
  return entries;
}

describe("extension.js FACES table", () => {
  it("matches the first face for each state in faces.ts", () => {
    const source = readFileSync(extensionPath, "utf8");
    const extensionFaces = readExtensionFaces(source);
    expect(Object.keys(extensionFaces).length).toBeGreaterThan(0);
    for (const [state, faceStr] of Object.entries(extensionFaces)) {
      expect(FACES[state as keyof typeof FACES][0]).toBe(faceStr);
    }
  });
});
