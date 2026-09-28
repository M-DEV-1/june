// @vitest-environment jsdom

/** Tests for ReplyMarkdown: a link that must never navigate this window, a guard against raw HTML actually running, and the copy button on a code block. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Provider } from "react-redux";

import { ReplyMarkdown } from "./chat-markdown";
import { makeStore } from "./store";
import { mockDaemon } from "./testing";

afterEach(cleanup);

describe("ReplyMarkdown", () => {
  it("shows a raw script tag as text instead of running it", () => {
    const w = window as unknown as { __ran?: boolean };
    const { container } = render(
      <ReplyMarkdown text={"before <script>window.__ran = true</script> after"} />,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.textContent).toContain("<script>");
    expect(w.__ran).toBeUndefined();
  });

  it("shows a mailto: address as plain text, saying why, rather than as a click that does nothing", async () => {
    mockDaemon();
    const store = makeStore();
    render(
      <Provider store={store}>
        <ReplyMarkdown text="[write to her](mailto:vexil@example.com)" />
      </Provider>,
    );
    // POST /open refuses any scheme but http and https (see internal/ipc/open.go), and this window never navigates itself, so there is nothing a click could do.
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("write to her").getAttribute("title")).toContain("http");
  });

  it("opens a link by posting to the daemon's /open route, never through window.open or window.location", async () => {
    const openSpy = vi.spyOn(window, "open").mockImplementation(() => null);
    const originalHref = window.location.href;
    const calls = mockDaemon();
    const store = makeStore();
    render(
      <Provider store={store}>
        <ReplyMarkdown text="[June](https://june.example/about)" />
      </Provider>,
    );
    await userEvent.click(screen.getByRole("link", { name: "June" }));
    expect(calls).toContainEqual({ method: "POST", path: "/open", body: { url: "https://june.example/about" } });
    expect(openSpy).not.toHaveBeenCalled();
    expect(window.location.href).toBe(originalHref);
    openSpy.mockRestore();
  });
});

// A code block is something June produced to be used elsewhere — a prompt to paste into another tool, a command to run — and there was no way to get it out of the pane but to select it by hand, which in a narrow chat column with a horizontal scrollbar means dragging past the edge.
describe("ReplyMarkdown code blocks", () => {
  it("copies the block's text to the clipboard on its own button", async () => {
    const written: string[] = [];
    Object.assign(navigator, { clipboard: { writeText: (t: string) => { written.push(t); return Promise.resolve(); } } });
    render(<ReplyMarkdown text={"```\nDesign brief: Logo for June\n```"} />);

    await userEvent.click(screen.getByRole("button", { name: "Copy" }));

    expect(written).toEqual(["Design brief: Logo for June"]);
    // Saying it went is the whole confirmation: a clipboard has nothing to show for itself.
    expect(await screen.findByRole("button", { name: "Copied" })).toBeDefined();
  });

  // A WebKitGTK webview off a secure origin has no navigator.clipboard at all, and an unhandled rejection there would leave the button saying "Copy" with no word of what happened.
  it("says so when the clipboard is not there to write to", async () => {
    Object.assign(navigator, { clipboard: undefined });
    render(<ReplyMarkdown text={"```\nsome text\n```"} />);

    await userEvent.click(screen.getByRole("button", { name: "Copy" }));

    expect(await screen.findByRole("button", { name: "Could not copy" })).toBeDefined();
  });
});
