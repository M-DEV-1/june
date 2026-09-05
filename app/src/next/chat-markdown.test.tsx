// @vitest-environment jsdom

/** Tests for ReplyMarkdown: the pieces the daemon's replies actually use — a fenced code block, inline maths, a gfm table, a link that must never navigate this window, and a guard against raw HTML actually running. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ReplyMarkdown } from "./chat-markdown";

afterEach(cleanup);

describe("ReplyMarkdown", () => {
  it("renders a fenced code block as pre>code", () => {
    const { container } = render(<ReplyMarkdown text={"```js\nconst x = 1;\n```"} />);
    const code = container.querySelector("pre > code");
    expect(code).not.toBeNull();
    expect(code?.textContent).toContain("const x = 1;");
  });

  it("renders inline math as a katex element", () => {
    const { container } = render(<ReplyMarkdown text="the answer is $x^2$ always" />);
    expect(container.querySelector(".katex")).not.toBeNull();
  });

  it("renders a gfm table as a table", () => {
    const text = ["| a | b |", "| - | - |", "| 1 | 2 |"].join("\n");
    render(<ReplyMarkdown text={text} />);
    expect(screen.getByRole("table")).toBeTruthy();
  });

  it("shows a raw script tag as text instead of running it", () => {
    const w = window as unknown as { __ran?: boolean };
    const { container } = render(
      <ReplyMarkdown text={"before <script>window.__ran = true</script> after"} />,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.textContent).toContain("<script>");
    expect(w.__ran).toBeUndefined();
  });

  it("opens a link through the system browser and never through window.location", async () => {
    const openSpy = vi.spyOn(window, "open").mockImplementation(() => null);
    const originalHref = window.location.href;
    render(<ReplyMarkdown text="[Ora](https://ora.example/about)" />);
    await userEvent.click(screen.getByRole("link", { name: "Ora" }));
    expect(openSpy).toHaveBeenCalledWith("https://ora.example/about", "_blank", "noopener,noreferrer");
    expect(window.location.href).toBe(originalHref);
    openSpy.mockRestore();
  });
});
