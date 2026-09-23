// @vitest-environment jsdom

/** Tests for the rail every screen shares and the three things that sit above every screen: the conversations under their date headings, the menu on a row, the rename box, the confirmation in front of a delete, and the jump-to-a-chat palette. */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ConversationSummary } from "./api";
import { progress } from "./store";
import { renderApp } from "./testing";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** Two conversations, one touched today and one a month ago, so the rail has two headings to draw. */
function conversations(): ConversationSummary[] {
  const today = new Date();
  const then = new Date(today.getTime() - 40 * 86400000);
  return [
    { id: "c1", title: "Flights to Zurich", brain: "claude", last: "booked", updated: today.toISOString() },
    { id: "c2", title: "The old one", brain: "", last: "", updated: then.toISOString() },
  ];
}

/** The row one conversation has in the rail, which is a button whose name opens with the conversation's title — the "…" beside it names the same conversation, so the match is anchored to the start. Input: the title. Output: the row, once the daemon's answer has arrived. */
function row(title: string) {
  return screen.findByRole("button", { name: new RegExp(`^${title}`) });
}

describe("the rail", () => {
  it("heads the conversations by when they were last touched", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    expect(screen.getByText("Today")).toBeDefined();
    // The second heading is whatever month that day fell in, so the assertion is that both groups are drawn rather than what the month is called.
    expect(screen.getAllByRole("group")).toHaveLength(2);
    expect(await row("The old one")).toBeDefined();
  });

  it("leaves only what the search matches, and says so when nothing does", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    const field = screen.getByLabelText("Search chats");
    await userEvent.type(field, "zurich");
    expect(screen.queryByRole("button", { name: /^The old one/ })).toBeNull();
    await userEvent.clear(field);
    await userEvent.type(field, "nothing like this");
    expect(await screen.findByText(/Nothing matches/)).toBeDefined();
  });

  it("says the daemon is not connected rather than that there are no chats", async () => {
    renderApp({ fails: ["GET /conversations"] });
    expect(await screen.findByText("Not connected.")).toBeDefined();
  });

  it("moves the keyboard's own focus onto the chat the arrow just selected", async () => {
    renderApp({ conversations: conversations() }, { conversationId: "c1" });
    const first = await row("Flights to Zurich");
    first.focus();
    await userEvent.keyboard("{ArrowDown}");
    await waitFor(async () => expect(document.activeElement).toBe(await row("The old one")));
  });

  it("opens a chat and comes back to Chats from another screen", async () => {
    const { store } = renderApp({ conversations: conversations() }, { place: "days" });
    await userEvent.click(await row("The old one"));
    expect(store.getState().ui).toMatchObject({ place: "chats", conversationId: "c2" });
  });

  it("shows each place from its foot row and comes back to Chats from the row that is lit", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await userEvent.click(await screen.findByRole("button", { name: "Tasks" }));
    expect(store.getState().ui.place).toBe("tasks");
    await userEvent.click(screen.getByRole("button", { name: "Tasks" }));
    expect(store.getState().ui.place).toBe("chats");
  });

  it("shows Ora's face at the head of the rail, watching while nothing is in flight and thinking during an ask", async () => {
    const { store } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    expect(await screen.findByRole("img", { name: "ora is watching" })).toBeDefined();
    store.dispatch(progress.askSent({ conversationId: "c1", question: "and the flights?" }));
    // Two thinking faces: the rail's and the one beside the run in the thread.
    await waitFor(() => expect(screen.getAllByRole("img", { name: "ora is thinking" })).toHaveLength(2));
  });

  it("opens an unsaved draft when New chat is clicked, posting nothing and adding nothing to the list", async () => {
    const { calls, store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "New chat" }));
    expect(store.getState().ui.chatDraft).toBe(true);
    expect(screen.getByRole("heading", { name: "Ora" })).toBeDefined();
    expect(calls.some((c) => c.method === "POST" && c.path === "/conversations")).toBe(false);
  });
});

describe("the menu on a row", () => {
  it("renames a chat through the box the menu opens", async () => {
    const { calls } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
    const box = await screen.findByLabelText("New title");
    await userEvent.clear(box);
    await userEvent.type(box, "Zurich, September");
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/conversations/c1/title")?.body).toEqual({ title: "Zurich, September" }));
  });

  it("will not take a blank title", async () => {
    renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
    await userEvent.clear(await screen.findByLabelText("New title"));
    expect(screen.getByRole("button", { name: "Rename" }).hasAttribute("disabled")).toBe(true);
  });

  it("asks before a delete, and falls to the next chat when the one that was open is removed", async () => {
    const { store, calls } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.path === "/conversations/c1")).toBe(true));
    await waitFor(() => expect(store.getState().ui.conversationId).toBe("c2"));
  });

  it("opens an empty draft when the only chat there was is deleted, rather than staying on the deleted one", async () => {
    const one = [conversations()[0]];
    const { store } = renderApp({ conversations: one }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Delete" }));
    // Clearing conversationId is not enough: App's own "keep some chat picked" effect puts the deleted id straight back off the list RTK Query has not refetched yet. The draft is what makes the composer post no conversation_id at all.
    await waitFor(() => expect(store.getState().ui.chatDraft).toBe(true));
    expect(await screen.findByRole("heading", { name: "Ora" })).toBeDefined();
  });

  it("leaves the row where it is and says so when a delete does not go through", async () => {
    renderApp({ conversations: conversations(), fails: ["DELETE /conversations/c1"] }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Delete" }));
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Could not delete");
    expect(await row("Flights to Zurich")).toBeDefined();
  });

  it("keeps the chat when the confirmation is dismissed", async () => {
    const { calls } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.click(screen.getByRole("button", { name: "More for Flights to Zurich" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const asked = await screen.findByRole("alertdialog");
    await userEvent.click(within(asked).getByRole("button", { name: "Keep it" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
  });
});

describe("the jump-to-a-chat palette", () => {
  it("opens on Ctrl+K and opens the chat that is chosen", async () => {
    const { store } = renderApp({ conversations: conversations() }, { conversationId: "c1" });
    await row("Flights to Zurich");
    await userEvent.keyboard("{Control>}k{/Control}");
    const palette = await screen.findByRole("dialog");
    await userEvent.click(within(palette).getByRole("option", { name: /The old one/ }));
    expect(store.getState().ui.conversationId).toBe("c2");
    expect(store.getState().ui.paletteOpen).toBe(false);
  });
});

// What the daemon puts on a task notice: the same five buttons its desktop banner offers (noticeActions in internal/proactive/notify.go). A card draws the actions its notice names, so a fixture that presses a button has to carry them.
const TASK_ACTIONS = [
  { key: "default", label: "Open in Ora" },
  { key: "done", label: "Done" },
  { key: "hour", label: "In an hour" },
  { key: "evening", label: "This evening" },
  { key: "tomorrow", label: "Tomorrow" },
];

// A live notice — one that reached the window with no action yet — offers its own Done/1h/Evening/Tomorrow row, wired through POST /notices/{kind}/{id}/action (see internal/proactive/notify.go's Act, the same code a desktop notification's own buttons call).
describe("a live notice's own buttons", () => {
  it("posts the pressed button's action for that notice", async () => {
    const { store, calls } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );

    await userEvent.click(await screen.findByRole("button", { name: /^1 h/ }));

    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.path === "/notices/task/task-42/action")?.body).toEqual({
        title: "Still open",
        body: "Send the invoice",
        action: "hour",
      }),
    );
  });

  // The design sheets of 2026-09-12 draw a notice as a face tile, then "Ora" with how long ago it landed, then the line, then the detail under it in muted grey. The card said only the title and the body, so it read as a loose paragraph with buttons rather than as something Ora said.
  it("reads as a notification from Ora: the face, the name, how long ago, then the words", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Your daily brief is ready.", body: "3 key things, 2 decisions, 1 follow-up.", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );

    const card = await screen.findByRole("group", { name: "Notice from Ora" });
    expect(within(card).getByText("Ora")).toBeDefined();
    expect(within(card).getByText("now")).toBeDefined();
    expect(within(card).getByText("Your daily brief is ready.")).toBeDefined();
    expect(within(card).getByText("3 key things, 2 decisions, 1 follow-up.")).toBeDefined();
    expect(within(card).getByRole("img", { name: /^ora is/ })).toBeDefined();
  });

  it("closes on its own cross, telling the daemon nothing", async () => {
    const { store, calls } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );
    const before = calls.length;

    await userEvent.click(await screen.findByRole("button", { name: /^Close/ }));

    await waitFor(() => expect(store.getState().ui.liveNotice).toBeUndefined());
    expect(screen.queryByRole("button", { name: /^1 h/ })).toBeNull();
    // Dismissing is the user saying they have seen it: it answers nothing, so nothing is posted.
    expect(calls.length).toBe(before);
  });

  it("names the notice on each button, so the four words are not four unattached labels", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );
    expect(await screen.findByRole("button", { name: "Done — Still open" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Tomorrow — Still open" })).toBeDefined();
  });

  it("keeps the card up and says so on it when the daemon will not act on the notice", async () => {
    const { store } = renderApp({ conversations: conversations(), fails: ["POST /notices/task/task-42/action"] });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );

    // A press that did not take is one quiet line in the same card (DESIGN.md rule 18), the way the hover window says it: the card stays up and the buttons can be pressed again, since the press is the thing that failed, not the notice.
    await userEvent.click(await screen.findByRole("button", { name: /^Done/ }));
    expect(await screen.findByText("Could not do that")).toBeDefined();
    expect(screen.getByRole("button", { name: /^1 h/ })).toBeDefined();
    expect(store.getState().ui.liveNotice).toBeDefined();
  });

  // The daemon says what each notice can answer and the window draws that, rather than each window deciding for itself. A task can be completed or pushed to later; a routine's report is Ora saying what it found, with nothing to complete and nowhere to push it to, and the four buttons it used to get all came back "Could not do that".
  it("draws the buttons the notice names, and none for a routine that names only Open", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );
    expect(await screen.findByRole("button", { name: /^Done/ })).toBeDefined();
    expect(screen.getByRole("button", { name: /^Evening/ })).toBeDefined();

    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Routine", body: "Vexil replied about the venue.", place: "", id: "7", kind: "routine", actions: [{ key: "default", label: "Open in Ora" }] },
      }),
    );
    // Open is the one button the app window drops: it is already the thing Open would open.
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Done/ })).toBeNull());
    expect(screen.queryByRole("button", { name: /^Evening/ })).toBeNull();
    expect(screen.getByText("Vexil replied about the venue.")).toBeDefined();
  });

  // A question's answer window closes on the daemon's clock, after which its buttons answer "Could not do that". The hover card took itself down at expires; the window's card had no expires at all and stayed up with dead buttons.
  it("takes a question off the card once its answer window has passed", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: {
          title: "Still open",
          body: "Send the invoice",
          place: "tasks",
          id: "7",
          kind: "stale",
          actions: [{ key: "dropped", label: "Not happening" }],
          expires: new Date(Date.now() + 100).toISOString(),
        },
      }),
    );
    expect(await screen.findByRole("button", { name: /^Not happening/ })).toBeDefined();

    await waitFor(() => expect(store.getState().ui.liveNotice).toBeUndefined());
    expect(screen.queryByRole("button", { name: /^Not happening/ })).toBeNull();
  });

  it("replaces the buttons with the rail line's own text once the daemon answers", async () => {
    const { store } = renderApp({ conversations: conversations() });
    await row("Flights to Zurich");
    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", actions: TASK_ACTIONS },
      }),
    );
    await screen.findByRole("button", { name: /^1 h/ });

    store.dispatch(
      progress.eventArrived({
        id: "",
        type: "notice",
        notice: { title: "Still open", body: "Send the invoice", place: "tasks", id: "task-42", kind: "task", action: "done", until: "" },
      }),
    );

    expect(await screen.findByText("Send the invoice: Done")).toBeDefined();
    expect(screen.queryByRole("button", { name: /^1 h/ })).toBeNull();
  });
});

// A job runs for minutes and the user goes on to another screen while it does, which left it with nowhere to be seen: the step list lives inside the turn that opened it, so walking to Tasks or Days meant losing sight of a task still moving things on the real desktop. The sidebar is on every screen, and the notice card already sits in it, so a running job belongs in the same place.
describe("a job running while you are elsewhere", () => {
  it("shows what is running from any screen, with the step it is on and a way to stop it", async () => {
    const { store, calls } = renderApp({ conversations: conversations() }, { place: "days" });
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    store.dispatch(
      progress.eventArrived({
        id: "act-1",
        type: "act",
        detail: JSON.stringify({ kind: "plan", state: "stepping", step: 6, text: "open the deck, move Slide 4 up" }),
      }),
    );
    store.dispatch(
      progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: "Clicking Slide 4" }) }),
    );

    const strip = await screen.findByRole("status", { name: "Running now" });
    expect(within(strip).getByText("reorder the slides")).toBeDefined();
    // The count reads against the model's own estimate rather than against a limit nobody set.
    expect(within(strip).getByText(/step 1 of about 6/i)).toBeDefined();

    await userEvent.click(within(strip).getByRole("button", { name: "Stop reorder the slides" }));
    await waitFor(() => expect(calls.find((c) => c.path === "/act/act-1/stop")).toBeDefined());
  });

  // A job the user starts by speaking is opened by the daemon, not by this window, so no jobSent ever ran for it and s.jobs has no entry to match its id against. Before this the reducer dropped every one of its events and the strip stayed empty through a four-minute chain the user could hear happening.
  it("shows a job the daemon started on its own, from the voice session", async () => {
    const { store } = renderApp({ conversations: conversations() }, { place: "days" });
    store.dispatch(
      progress.eventArrived({
        id: "act-9",
        type: "act",
        detail: JSON.stringify({ kind: "started", state: "planning", text: "open spotify and play Teenage Dream, then message Vexil" }),
      }),
    );
    store.dispatch(
      progress.eventArrived({ id: "act-9", type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: "Opening Spotify" }) }),
    );

    const strip = await screen.findByRole("status", { name: "Running now" });
    expect(within(strip).getByText("open spotify and play Teenage Dream, then message Vexil")).toBeDefined();
    expect(within(strip).getByText(/step 1/i)).toBeDefined();
  });

  it("says nothing at all once the job has ended", async () => {
    const { store } = renderApp({ conversations: conversations() }, { place: "days" });
    store.dispatch(progress.jobSent({ conversationId: "c1", goal: "reorder the slides" }));
    store.dispatch(progress.jobAccepted({ id: "act-1", conversationId: "c1" }));
    expect(await screen.findByRole("status", { name: "Running now" })).toBeDefined();

    store.dispatch(
      progress.eventArrived({ id: "act-1", type: "act", detail: JSON.stringify({ kind: "done", state: "done", text: "Reordered." }) }),
    );
    await waitFor(() => expect(screen.queryByRole("status", { name: "Running now" })).toBeNull());
  });
});
