// @vitest-environment jsdom

/** Tests for the Routines screen: the empty state, adding one from the two fields, running one right now and reading back what it said, and dropping one. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { Routine } from "./api";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** The list on the page, so a row is looked for among them rather than in the header. */
function list() {
  return within(screen.getByRole("list", { name: "Routines" }));
}

const routines: Routine[] = [
  { id: "1", text: "tell me the one thing I must do today", schedule: "weekdays at 8", enabled: true, last_run: "", last_answer: "" },
  {
    id: "2",
    text: "tell me if anything is on fire",
    schedule: "every 3 hours",
    enabled: true,
    last_run: new Date().toISOString(),
    last_answer: "Ship the report — it's due today.",
  },
];

describe("the list", () => {
  it("shows every routine with its schedule and what it last said", async () => {
    renderApp({ routines }, { place: "routines" });
    await screen.findByText("tell me the one thing I must do today");
    expect(list().getByText("weekdays at 8")).toBeDefined();
    expect(list().getByText(/Never run yet/)).toBeDefined();
    expect(list().getByText(/Ship the report/)).toBeDefined();
  });

  it("says there are none yet rather than showing an empty list", async () => {
    renderApp({}, { place: "routines" });
    expect(await screen.findByText("No routines yet. Write one above.")).toBeDefined();
  });
});

describe("adding one", () => {
  it("posts the instruction and schedule and clears the fields", async () => {
    const { calls } = renderApp({}, { place: "routines" });
    await screen.findByText("No routines yet. Write one above.");
    await userEvent.type(screen.getByLabelText("Instruction"), "tell me the one thing I must do today");
    await userEvent.type(screen.getByLabelText("When"), "weekdays at 8");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.path === "/routines")?.body).toEqual({
        text: "tell me the one thing I must do today",
        schedule: "weekdays at 8",
      }),
    );
    expect((screen.getByLabelText("Instruction") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("When") as HTMLInputElement).value).toBe("");
  });

  it("keeps the button off until both fields are filled in", async () => {
    renderApp({}, { place: "routines" });
    await screen.findByText("No routines yet. Write one above.");
    expect(screen.getByRole("button", { name: "Add" }).hasAttribute("disabled")).toBe(true);
    await userEvent.type(screen.getByLabelText("Instruction"), "tell me something");
    expect(screen.getByRole("button", { name: "Add" }).hasAttribute("disabled")).toBe(true);
    await userEvent.type(screen.getByLabelText("When"), "every 3 hours");
    expect(screen.getByRole("button", { name: "Add" }).hasAttribute("disabled")).toBe(false);
  });
});

describe("running one now", () => {
  it("asks the daemon and shows what it said", async () => {
    const { calls } = renderApp({ routines, routineAnswer: "Ship the report — it's due today." }, { place: "routines" });
    await screen.findByText("tell me the one thing I must do today");
    await userEvent.click(screen.getByRole("button", { name: /Run "tell me the one thing I must do today" now/ }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/routines/1/run")).toBe(true));
    expect(await screen.findByText("Ship the report — it's due today.")).toBeDefined();
  });

  it("says nothing was worth saying for a NOTHING answer", async () => {
    renderApp({ routines, routineAnswer: "NOTHING" }, { place: "routines" });
    await screen.findByText("tell me the one thing I must do today");
    await userEvent.click(screen.getByRole("button", { name: /Run "tell me the one thing I must do today" now/ }));
    expect(await screen.findByText("Nothing worth saying right now")).toBeDefined();
  });
});

describe("dropping one", () => {
  it("removes it from the list", async () => {
    const { calls } = renderApp({ routines }, { place: "routines" });
    await screen.findByText("tell me the one thing I must do today");
    // Removing is behind the row's overflow menu now, so it takes two presses rather than one adjacent to Run.
    await userEvent.click(screen.getByRole("button", { name: /More for "tell me the one thing I must do today"/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Remove" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.path === "/routines/1")).toBe(true));
    await waitFor(() => expect(list().queryByText("tell me the one thing I must do today")).toBeNull());
  });
});
