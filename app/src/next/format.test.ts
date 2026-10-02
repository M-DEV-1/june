/** Tests for the minutes reader, which turns the daemon's minutes markdown into the lines the meeting page draws. */

import { describe, expect, it } from "vitest";

import { minutesLines } from "./format";

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
