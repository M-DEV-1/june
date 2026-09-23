// @vitest-environment jsdom

/** Tests for the Meetings screen: one page with a picker in its header rather than a list beside the sidebar, which is one recording's minutes with what it left the user to do pinned above them. The Days screen these tests used to share this file with has its own days.test.tsx now, which is also where its raised list's tie to GET /tasks is tested. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { Meeting, Task } from "./api";
import { openPicker, renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const meetings: Meeting[] = [
  {
    id: "m1",
    title: "Meridian statement pattern analysis",
    when: new Date().toISOString(),
    duration_s: 1680,
    minutes: "# Meridian statement pattern analysis\n## What was said\n- **Vexil** will send the file\n## Action items\n- send the file to legal\n",
    attendees: [
      { name: "Zemna Braxen", heard_only: false },
      { name: "Vexil Quorin", heard_only: true },
    ],
  },
];

/** One action item that meeting raised for the user and one raised somewhere else, as GET /tasks would answer them. */
const meetingWork: Task[] = [
  { id: "12", title: "send the file to legal", source: "noticed", when: new Date().toISOString(), done: false, conversation_id: "", detail: "Meridian statement pattern analysis", owner: "me" },
  { id: "13", title: "chase the standup notes", source: "noticed", when: new Date().toISOString(), done: false, conversation_id: "", detail: "Daily AI Sprint Standup", owner: "me" },
];

describe("Meetings", () => {
  it("names the recording in the header and says when it ran and who was there", async () => {
    renderApp({ meetings }, { place: "meetings" });
    expect(await screen.findByRole("heading", { name: "Meridian statement pattern analysis" })).toBeDefined();
    expect(screen.getByText(/28 min · Zemna Braxen, Vexil Quorin \(heard\)/)).toBeDefined();
    const picker = await openPicker("Choose a meeting");
    expect(picker.getByText("Today")).toBeDefined();
    expect(picker.getByRole("option", { name: /Meridian statement pattern analysis/ })).toBeDefined();
  });

  it("deletes a meeting whose minutes are not worth keeping, once the question is answered", async () => {
    const { calls } = renderApp({ meetings }, { place: "meetings" });
    await screen.findByRole("heading", { name: "Meridian statement pattern analysis" });

    await userEvent.click(screen.getByRole("button", { name: /^Delete/ }));
    // Nothing is removed on the press alone: a write-up is not recoverable, so the question is asked first.
    expect(calls.find((c) => c.method === "DELETE")).toBeUndefined();

    await userEvent.click(await screen.findByRole("button", { name: "Delete it" }));
    await waitFor(() => expect(calls.find((c) => c.method === "DELETE" && c.path === "/meetings/m1")).toBeDefined());
  });

  it("typesets the minutes as a document: real headings and one real list, not a paragraph per bullet with a dot typed in front of it", async () => {
    renderApp({ meetings }, { place: "meetings" });
    const heading = await screen.findByRole("heading", { level: 3, name: "What was said" });
    expect(heading).toBeDefined();
    // Two headings in these minutes, and a run of bullets under each becomes one list rather than a paragraph each.
    const lists = screen.getAllByRole("list").filter((l) => l.closest(".document"));
    expect(lists).toHaveLength(2);
    expect(within(lists[0]).getAllByRole("listitem")).toHaveLength(1);
    expect(screen.getByText("Vexil will send the file").tagName).toBe("LI");
    // Nothing draws its own bullet character: the marker is the list's, in the margin.
    expect(screen.getByText("Vexil will send the file").textContent).not.toContain("•");
  });

  it("pins what this meeting left the user to do above the minutes, and nothing raised elsewhere", async () => {
    renderApp({ meetings, tasks: meetingWork }, { place: "meetings" });
    expect(await screen.findByText("What you owe from this")).toBeDefined();
    // The item appears twice: once as a task with a tick on it, and once in the minutes text where the model wrote it.
    expect(screen.getAllByText("send the file to legal")).toHaveLength(2);
    expect(screen.getByRole("checkbox", { name: "Mark send the file to legal done" })).toBeDefined();
    expect(screen.queryByText("chase the standup notes")).toBeNull();
  });

  it("ticks an item off from the meeting it was raised in", async () => {
    const { calls } = renderApp({ meetings, tasks: meetingWork }, { place: "meetings" });
    await userEvent.click(await screen.findByRole("checkbox", { name: "Mark send the file to legal done" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/tasks/12/done")?.body).toEqual({ status: "done" }));
  });

  it("leaves only what the picker's search matches, and says when nothing does", async () => {
    renderApp({ meetings }, { place: "meetings" });
    await screen.findByText("Vexil will send the file");
    const picker = await openPicker("Choose a meeting");
    await userEvent.type(screen.getByLabelText("Search meetings"), "vexil");
    expect(picker.getByRole("option", { name: /Meridian/ })).toBeDefined();
    await userEvent.clear(screen.getByLabelText("Search meetings"));
    await userEvent.type(screen.getByLabelText("Search meetings"), "nothing like this");
    expect(await picker.findByText(/Nothing matches/)).toBeDefined();
  });

  it("forgets the search when the picker is shut, so no filter outlives the field that set it", async () => {
    const { store } = renderApp({ meetings }, { place: "meetings" });
    await screen.findByText("Vexil will send the file");
    await openPicker("Choose a meeting");
    await userEvent.type(screen.getByLabelText("Search meetings"), "vexil");
    expect(store.getState().ui.query.meetings).toBe("vexil");
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(store.getState().ui.query.meetings).toBe(""));
  });

  it("puts what was around the meeting in a rail beside the document once the pane is wide, and leaves it inside the page when it is not", async () => {
    renderApp({ meetings, tasks: meetingWork }, { place: "meetings", wide: true });
    const rail = await screen.findByRole("complementary", { name: "About this meeting" });
    // Who was there, when it ran, and what it left the user to do all move out of the document and sit beside it.
    expect(within(rail).getByText(/Vexil Quorin/)).toBeDefined();
    expect(within(rail).getByText("28 min")).toBeDefined();
    expect(within(rail).getByText("What you owe from this")).toBeDefined();
    // The document's own headings become the outline, which is a way into the page rather than a second copy of it.
    const outline = within(rail).getByRole("navigation", { name: "Sections" });
    const first = within(outline).getByRole("button", { name: "What was said" });
    expect(first).toBeDefined();
    expect(within(outline).getByRole("button", { name: "Action items" })).toBeDefined();
  });

  it("draws no rail at all in a narrow pane, and nothing is lost from the page", async () => {
    renderApp({ meetings, tasks: meetingWork }, { place: "meetings" });
    await screen.findByText("Vexil will send the file");
    expect(screen.queryByRole("complementary", { name: "About this meeting" })).toBeNull();
    expect(screen.getByText("What you owe from this")).toBeDefined();
    expect(screen.getByText(/Vexil Quorin/)).toBeDefined();
  });

  it("scrolls the document to a section when its outline row is clicked", async () => {
    renderApp({ meetings }, { place: "meetings", wide: true });
    const rail = await screen.findByRole("complementary", { name: "About this meeting" });
    // The spy goes on after the render, because stubBrowser puts its own do-nothing scrollIntoView on the prototype.
    const scrolled: string[] = [];
    Element.prototype.scrollIntoView = function (this: Element) {
      scrolled.push(this.textContent ?? "");
    };
    await userEvent.click(within(rail).getByRole("button", { name: "Action items" }));
    expect(scrolled).toContain("Action items");
  });
});
