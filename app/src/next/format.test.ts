/** Tests for the minutes reader, which turns the daemon's minutes markdown into the lines the meeting page draws. */

import { describe, expect, it } from "vitest";

import { keyOnClipboard, minutesLines } from "./format";

describe("reading the daemon's minutes", () => {
  it("reads the minutes into headings, bullets and paragraphs and drops the heading that repeats the title", () => {
    const lines = minutesLines(
      "# Standup\n\n## What was said\n- **Vexil** will send the file\nA plain sentence.\n",
      "Standup",
    );
    expect(lines).toMatchObject([
      { kind: "h", text: "What was said" },
      { kind: "bullet", text: "Vexil will send the file" },
      { kind: "text", text: "A plain sentence." },
    ]);
  });

  it('reads the real shape of "You now owe": the label names the list under it rather than being an item of it', () => {
    const owed = [
      "## Your part",
      "- **You now owe**",
      "  - Build a single Excel workbook of the Meridian statement patterns.",
      "  - Check the remaining rows against the template.",
    ].join("\n");
    expect(minutesLines(owed)).toMatchObject([
      { kind: "h", text: "Your part" },
      { kind: "label", text: "You now owe" },
      { kind: "bullet", text: "Build a single Excel workbook of the Meridian statement patterns." },
      { kind: "bullet", text: "Check the remaining rows against the template." },
    ]);
  });
});

describe("picking a Gemini key off the clipboard", () => {
  // The rule as the daemon sends it in GET /setup (plausibleKey in internal/ipc/setup.go).
  const rule = "^[A-Za-z0-9._\\-]{20,256}$";
  const key = "AIzaSy" + "x".repeat(33);

  it("takes a key by the daemon's rule, with the space around it trimmed", () => {
    expect(keyOnClipboard(`  ${key}\n`, rule)).toBe(key);
  });

  it("leaves other text alone", () => {
    for (const text of ["", "short", `${key}\nsecond line`, "two words that add up to a long line", `export GEMINI_API_KEY=${key}`]) {
      expect(keyOnClipboard(text, rule)).toBe("");
    }
  });

  it("takes nothing without a rule", () => {
    expect(keyOnClipboard(key, undefined)).toBe("");
    expect(keyOnClipboard(key, "[")).toBe("");
  });
});
