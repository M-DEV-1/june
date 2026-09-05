/** shared/tokens.css carries every raw colour token, read by both windows; index.css must no longer define :root or [data-theme="dark"] colours itself, since a second copy is exactly what let the two files drift apart. This just reads the two files as text and checks that split held. */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const tokens = readFileSync(resolve(__dirname, "../shared/tokens.css"), "utf-8");
const indexCss = readFileSync(resolve(__dirname, "./index.css"), "utf-8");

describe("shared/tokens.css", () => {
  it("carries the widened dark elevation ramp", () => {
    const dark = tokens.slice(tokens.indexOf('[data-theme="dark"]'));
    expect(dark).toMatch(/--card:\s*#26282d;/);
    expect(dark).toMatch(/--popover:\s*#2d3036;/);
    expect(dark).toMatch(/--background:\s*#1b1c20;/);
  });

  it("is imported by index.css, which no longer defines these tokens itself", () => {
    expect(indexCss).toMatch(/@import\s+["']\.\.\/shared\/tokens\.css["'];/);
    expect(indexCss).not.toMatch(/--card:\s*#/);
    expect(indexCss).not.toMatch(/--popover:\s*#/);
    expect(indexCss).not.toMatch(/--background:\s*#/);
  });
});
