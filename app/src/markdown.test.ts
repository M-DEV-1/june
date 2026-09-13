import { describe, expect, it } from "vitest";
import { markdown } from "./markdown";

describe("the hover's markdown", () => {
  it("turns a dashed run into a list instead of one long paragraph", () => {
    const out = markdown("Reasons for that on YouTube:\n\n- **Freshly uploaded** — it sharpens later.\n- **Source quality** — the footage is already soft.");
    expect(out).toContain("<p>Reasons for that on YouTube:</p>");
    expect(out).toContain("<ul><li><b>Freshly uploaded</b> — it sharpens later.</li>");
    expect(out).not.toContain("- **");
  });

  it("starts the list even when no blank line separates it from the paragraph above", () => {
    const out = markdown("The common reasons:\n- one thing\n- another thing");
    expect(out).toBe("<p>The common reasons:</p><ul><li>one thing</li><li>another thing</li></ul>");
  });

  it("numbers an ordered list", () => {
    expect(markdown("1. first\n2. second")).toBe("<ol><li>first</li><li>second</li></ol>");
  });

  it("marks bold, italic and code without touching the rest", () => {
    expect(markdown("a **b** and *c* and `d`")).toBe("<p>a <b>b</b> and <i>c</i> and <code>d</code></p>");
  });

  it("leaves a lone asterisk in prose alone", () => {
    expect(markdown("2 * 3 is 6")).toBe("<p>2 * 3 is 6</p>");
  });

  it("escapes markup before anything is put back, so a model cannot write a tag", () => {
    const out = markdown('<script>alert("x")</script>');
    expect(out).not.toContain("<script>");
    expect(out).toContain("&lt;script&gt;");
  });

  it("keeps a single newline inside a paragraph as a break", () => {
    expect(markdown("one line\nsecond line")).toBe("<p>one line<br>second line</p>");
  });
});
