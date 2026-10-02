import { describe, expect, it } from "vitest";
import { markdown } from "./markdown";

describe("the hover's markdown", () => {
  it("escapes markup before anything is put back, so a model cannot write a tag", () => {
    const out = markdown('<script>alert("x")</script>');
    expect(out).not.toContain("<script>");
    expect(out).toContain("&lt;script&gt;");
  });
});
