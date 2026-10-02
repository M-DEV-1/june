// @vitest-environment jsdom

/** Tests for ReplyMarkdown: raw HTML in a reply never runs, and a link never navigates this window. */

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

