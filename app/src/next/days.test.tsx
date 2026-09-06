// @vitest-environment jsdom

/** Tests for the Days screen: one day's page at a time, chosen from the header picker, with no list beside the sidebar — and the work it raised, which reads and ticks off the same rows GET /tasks does. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { DaySummary, DayView, Task } from "./api";
import { openPicker, renderApp, type Call } from "./testing";

/** Holds GET /days/2026-09-04 open. setTaskStatus patches the tasks and allTasks caches on its way out, but the Days page reads its ticks off day.tasks, which changes only when the day is read again: holding that read is what makes the gap between the daemon answering a status change and the day reporting it wide enough to look at. Input: none. Output: the function that lets the held read through. */
function holdTheDay(): () => void {
  const daemon = globalThis.fetch;
  let release = () => {};
  const held = new Promise<void>((r) => {
    release = r;
  });
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(typeof input === "object" && "url" in input ? input.url : String(input)).pathname;
    if (path !== "/days/2026-09-04") return daemon(input, init);
    return held.then(() => daemon(input, init));
  });
  return release;
}

/** What was sent to the daemon for task 11, in the order it went. Input: every call the window made. Output: one body per status change. */
function sent(calls: Call[]): unknown[] {
  return calls.filter((c) => c.path === "/tasks/11/done").map((c) => c.body);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const days: DaySummary[] = [
  { date: "2026-09-04", title: "A long day of TCFD work.", has_page: true, seen: 366, meetings: 5, meeting_minutes: 140 },
  { date: "2026-09-03", title: "", has_page: false, seen: 60, meetings: 1, meeting_minutes: 28 },
  { date: "2026-08-30", title: "", has_page: false, seen: 0, meetings: 0, meeting_minutes: 0 },
];

const page: DayView = {
  date: "2026-09-04",
  brief: "",
  close: "",
  page: "You spent the morning on the TCFD statements.\nThe afternoon went to the flights.",
  you: [],
  tasks: [
    { id: "11", title: "Send the TCFD file", done: false, status: "open", owner: "me" },
    { id: "12", title: "Book the flight", done: true, status: "done", owner: "me" },
  ],
  heading: "366 things seen · 5 calls, 140 min",
};

/** The same two rows GET /tasks would answer, so a tick made on the Days page has something in the Tasks list to keep in step with. */
const tasks: Task[] = [
  { id: "11", title: "Send the TCFD file", source: "noticed", when: new Date().toISOString(), done: false, conversation_id: "", detail: "TCFD call, 2026-09-04", owner: "me" },
  { id: "12", title: "Book the flight", source: "you", when: new Date().toISOString(), done: true, conversation_id: "c1", detail: "you said", owner: "me" },
];

describe("the day's own page", () => {
  it("offers the days that hold something in the header picker, grouped by month and with their counts", async () => {
    renderApp({ days, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText(/You spent the morning/);
    const picker = await openPicker("Choose a day");
    expect(picker.getByRole("option", { name: /Friday 4/ })).toBeDefined();
    expect(picker.getByText("60 seen · 1 call, 28 min")).toBeDefined();
    // The day with nothing recorded at all is left out, so two of the three are offered, both under the one month they fell in.
    expect(picker.queryByText(/Sunday 30/)).toBeNull();
    expect(picker.getAllByRole("option")).toHaveLength(2);
  });

  it("draws the day's own date, the daemon's line about it, and what it raised, with no list beside it", async () => {
    renderApp({ days, pages: { "2026-09-04": page } }, { place: "days" });
    expect(await screen.findByText(/You spent the morning/)).toBeDefined();
    expect(screen.getByText("366 things seen · 5 calls, 140 min")).toBeDefined();
    expect(screen.getByText("Raised that day")).toBeDefined();
    expect(screen.getByRole("heading", { level: 2, name: /September/ })).toBeDefined();
    // The sidebar is for chats: the day's own list lives in the header's picker and nowhere else.
    expect(screen.queryByRole("listbox", { name: "Days" })).toBeNull();
  });

  it("opens another day from the picker", async () => {
    const { store, calls } = renderApp({ days, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText(/You spent the morning/);
    const picker = await openPicker("Choose a day");
    await userEvent.click(picker.getByRole("option", { name: /Thursday 3/ }));
    expect(store.getState().ui.date).toBe("2026-09-03");
    await waitFor(() => expect(calls.some((c) => c.path === "/days/2026-09-03")).toBe(true));
  });

  it("gives a bare day no rail at all, so its column is not pushed off centre by an empty one", async () => {
    // Nothing the rail could carry: no line about the day, no brief, no close, and one part, which is too few for an outline.
    const bare: DayView = { date: "2026-09-04", brief: "", close: "", page: "Quiet.", you: [], tasks: [], heading: "" };
    renderApp({ days, pages: { "2026-09-04": bare } }, { place: "days", wide: true });
    await screen.findByText("Quiet.");
    expect(screen.queryByRole("complementary", { name: "About this day" })).toBeNull();
  });

  it("says no days have been written rather than showing an empty page", async () => {
    renderApp({}, { place: "days" });
    expect(await screen.findByText("No days written yet.")).toBeDefined();
  });
});

describe("ticking a raised task", () => {
  it("gives the raised list's circles real buttons, named the same way the Tasks screen names its own", async () => {
    renderApp({ days, tasks, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText("Raised that day");
    expect(await screen.findByRole("checkbox", { name: "Mark Send the TCFD file done" })).toBeDefined();
    expect(screen.getByRole("checkbox", { name: "Reopen Book the flight" })).toBeDefined();
  });

  it("ticks a raised item done through the same route the Tasks screen uses, and the row updates", async () => {
    const { calls } = renderApp({ days, tasks, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText("Raised that day");
    await userEvent.click(screen.getByRole("checkbox", { name: "Mark Send the TCFD file done" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/11/done")?.body).toEqual({ status: "done" }));
    // The tag setTaskStatus invalidates ("Task") is the same tag the day query carries, so the day refetches and the tick it now sees reflects the same row the Tasks screen would.
    expect(await screen.findByRole("checkbox", { name: "Reopen Send the TCFD file" })).toBeDefined();
  });

  it("holds the circle filled from the click until the day itself agrees, rather than emptying it while the day is being read again", async () => {
    const { calls } = renderApp({ days, tasks, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText("Raised that day");
    const release = holdTheDay();

    await userEvent.click(screen.getByRole("checkbox", { name: "Mark Send the TCFD file done" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/11/done")?.body).toEqual({ status: "done" }));
    // The status change has been answered and the day has not been read again yet: the circle must still be filled.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByRole("checkbox", { name: "Reopen Send the TCFD file" })).toBeDefined();

    release();
    expect(await screen.findByRole("checkbox", { name: "Reopen Send the TCFD file" })).toBeDefined();
  });

  it("takes a click after the change has gone out as a fresh tick rather than swallowing it", async () => {
    const { calls } = renderApp({ days, tasks, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText("Raised that day");
    holdTheDay();

    await userEvent.click(screen.getByRole("checkbox", { name: "Mark Send the TCFD file done" }));
    await waitFor(() => expect(sent(calls)).toHaveLength(1));
    // The day has still not caught up, so the circle is showing a change that has already gone out: clicking it again asks for the opposite, rather than undoing something that was never sent.
    await userEvent.click(screen.getByRole("checkbox", { name: "Reopen Send the TCFD file" }));
    await waitFor(() => expect(sent(calls)).toEqual([{ status: "done" }, { status: "open" }]));
    expect(screen.getByRole("checkbox", { name: "Mark Send the TCFD file done" })).toBeDefined();
  });

  it("gives the circle up after a while when the day never comes round to agreeing", async () => {
    const { calls } = renderApp({ days, tasks, pages: { "2026-09-04": page } }, { place: "days" });
    await screen.findByText("Raised that day");
    // Held and never released: a day that reports its tasks from somewhere the status change did not reach would otherwise leave the circle showing a thing the row never comes to say.
    holdTheDay();
    vi.useFakeTimers();

    fireEvent.click(screen.getByRole("checkbox", { name: "Mark Send the TCFD file done" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(sent(calls)).toHaveLength(1);
    expect(screen.getByRole("checkbox", { name: "Reopen Send the TCFD file" })).toBeDefined();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(screen.getByRole("checkbox", { name: "Mark Send the TCFD file done" })).toBeDefined();
    vi.useRealTimers();
  });
});
