/** The window's Redux store: what the user is looking at, what they have half-typed or half-done, what they have chosen in Settings, and what the daemon is saying about the question in flight. Everything the daemon holds is server state and lives in the RTK Query cache in api.ts instead; nothing is kept in two places. Component state is only for what is genuinely local, such as whether a menu is open, which the Radix components own themselves. */

import { configureStore, createAction, createListenerMiddleware, createSlice, isAnyOf, type PayloadAction } from "@reduxjs/toolkit";
import { useDispatch, useSelector } from "react-redux";

import { events, oraApi, type DaemonEvent, type Evidence, type Spend } from "./api";
import { jobStateWord } from "./format";

/** The key a fresh, unsaved chat draft's composer text and job are kept under, before the first message has opened a real conversation and given it a real id. */
export const DRAFT_CHAT = "__draft__";

/** The screens the window can be showing. Each of Tasks, Meetings and Days is one page whose header carries a picker; none of them holds a list of its own beside the sidebar, because the sidebar is for chats and nothing else. */
export type Place = "chats" | "tasks" | "days" | "meetings" | "settings" | "routines";

/** Light and dark. "system" means follow the desktop, which still resolves to a light or dark stamp on the root element. */
export type Theme = "light" | "dark" | "system";

/** What each searchable list is being filtered by. Empty strings mean the whole list shows. */
export type Queries = { chats: string; tasks: string; days: string; meetings: string };

/** What is on screen: the place showing, what is selected in each list, what each search field and each composer holds, which replies have their sources unfolded, whether the jump-to palette is up, the one line saying a write did not go through, and the layout flag. */
type UiState = {
  place: Place;
  /** Where Escape goes back to when Settings is showing. */
  back: Place;
  conversationId?: string;
  /** An unsaved chat draft is open: the composer is ready but nothing is picked, and the conversation last open stays selected underneath rather than being cleared, since App.tsx's own "keep some chat picked whenever any exist" effect would otherwise just reselect it the instant this is set. Every screen that shows this reads chatDraft first and shows the fresh, empty page instead of the conversation still sitting in conversationId. Cleared the moment any real conversation is opened — the draft's own first message included, since that is answered by opening one. */
  chatDraft?: boolean;
  taskId?: string;
  meetingId?: string;
  routineId?: string;
  date?: string;
  query: Queries;
  /** What the composer holds, kept per conversation so switching chats does not lose a half-typed question. */
  ask: Record<string, string>;
  /** What the box under the task list holds. */
  newTask: string;
  /** The ids of the turns whose sources are unfolded; every other reply shows the one closed line. */
  openRails: string[];
  /** The conversation opened for a task that had none of its own, by task id. A task Ora noticed in a meeting is a note, not a row the daemon can hang a conversation off, and there is no route that ties the two together, so the window remembers the pairing for as long as it is open and reuses it rather than opening a second conversation on the next question. */
  taskChats: Record<string, string>;
  paletteOpen: boolean;
  notice?: string;
};

/** What Escape does, dispatched once and answered by whichever slices have something to give up. */
export const escaped = createAction("escaped");

const initialUi: UiState = {
  place: "chats",
  back: "chats",
  query: { chats: "", tasks: "", days: "", meetings: "" },
  ask: {},
  newTask: "",
  taskChats: {},
  openRails: [],
  paletteOpen: false,
};

const uiSlice = createSlice({
  name: "ui",
  initialState: initialUi,
  reducers: {
    /** Shows one of the places, remembering where to come back to. Leaving a place that is not Settings records it as the one Escape returns to, so Settings never becomes its own back destination. */
    placeShown(s, a: PayloadAction<Place>) {
      if (a.payload === s.place) return;
      if (s.place !== "settings") s.back = s.place;
      s.place = a.payload;
      s.notice = undefined;
    },
    /** What one of the rows at the foot of the sidebar does: shows its place when it is not showing, and Chats when it is, so the row that is lit is also the way back to the conversation that was open. */
    footToggled(s, a: PayloadAction<Place>) {
      uiSlice.caseReducers.placeShown(s, { ...a, payload: s.place === a.payload ? "chats" : a.payload });
    },
    /** Opens a conversation, from whichever place the sidebar was clicked in, and shows Chats, so a chat clicked from Tasks, Days or Settings is a way back. Ends a chat draft, if one was open — a real conversation, the draft's own first message included, is always what a draft was for. */
    conversationOpened(s, a: PayloadAction<string | undefined>) {
      uiSlice.caseReducers.placeShown(s, { ...a, payload: "chats" });
      s.place = "chats";
      s.conversationId = a.payload;
      s.chatDraft = false;
      s.paletteOpen = false;
    },
    /** Opens an unsaved chat draft: nothing is posted and nothing is selected, so nothing appears in the sidebar's own list until the draft's first message opens a real conversation (see conversationOpened, which is what ends the draft). */
    chatDraftOpened(s) {
      if (s.place !== "chats") {
        if (s.place !== "settings") s.back = s.place;
        s.place = "chats";
      }
      s.chatDraft = true;
      s.paletteOpen = false;
    },
    /** Selects one task, which is the one the composer at the foot of the page talks to. */
    taskOpened(s, a: PayloadAction<string>) {
      s.taskId = a.payload;
    },

    /** Records the conversation opened for a task that had none, so the next question about that task goes into the same one. */
    taskChatOpened(s, a: PayloadAction<{ taskId: string; conversationId: string }>) {
      s.taskChats[a.payload.taskId] = a.payload.conversationId;
    },
    /** Opens one day's page. */
    dayOpened(s, a: PayloadAction<string>) {
      s.date = a.payload;
      s.place = "days";
    },
    /** Opens one meeting's minutes. */
    meetingOpened(s, a: PayloadAction<string>) {
      s.meetingId = a.payload;
      s.place = "meetings";
    },
    /** Opens one routine's row. */
    routineOpened(s, a: PayloadAction<string>) {
      s.routineId = a.payload;
      s.place = "routines";
    },
    /** Records what one list's search field holds. */
    searched(s, a: PayloadAction<{ list: keyof Queries; text: string }>) {
      s.query[a.payload.list] = a.payload.text;
    },
    /** Records what the composer holds for one conversation. */
    asked(s, a: PayloadAction<{ conversationId: string; text: string }>) {
      s.ask[a.payload.conversationId] = a.payload.text;
    },
    /** Records what the new-task box holds. */
    taskTyped(s, a: PayloadAction<string>) {
      s.newTask = a.payload;
    },
    /** Unfolds or folds one reply's sources. */
    railToggled(s, a: PayloadAction<string>) {
      s.openRails = s.openRails.includes(a.payload) ? s.openRails.filter((id) => id !== a.payload) : [...s.openRails, a.payload];
    },
    /** Opens or closes the jump-to-a-chat palette. */
    paletteToggled(s, a: PayloadAction<boolean | undefined>) {
      s.paletteOpen = a.payload ?? !s.paletteOpen;
    },
    /** Says on one line that a write did not go through, or clears what was said. */
    noticed(s, a: PayloadAction<string | undefined>) {
      s.notice = a.payload;
    },
    /** Puts the layout flag on another of its options, so the alternative can be tried without a rebuild. */
  },
  extraReducers: (build) => {
    // Escape clears the notice first, and otherwise comes back from Settings to the place it was opened from.
    build.addCase(escaped, (s) => {
      if (s.notice) {
        s.notice = undefined;
        return;
      }
      if (s.place === "settings") s.place = s.back;
    });
  },
});

/** What the user has half-done to a conversation: the one being renamed and the title typed so far, and the one a delete is being confirmed for. The conversations themselves are the daemon's, and are read through the RTK Query cache. */
type ConversationsState = {
  renamingId?: string;
  draftTitle: string;
  confirmingDeleteId?: string;
};

const conversationsSlice = createSlice({
  name: "conversations",
  initialState: { draftTitle: "" } as ConversationsState,
  reducers: {
    /** Starts renaming one conversation, with its current title as the draft. */
    renameStarted(s, a: PayloadAction<{ id: string; title: string }>) {
      s.renamingId = a.payload.id;
      s.draftTitle = a.payload.title;
    },
    /** Records what the rename box holds. */
    draftTitleTyped(s, a: PayloadAction<string>) {
      s.draftTitle = a.payload;
    },
    /** Abandons or finishes a rename. */
    renameEnded(s) {
      s.renamingId = undefined;
      s.draftTitle = "";
    },
    /** Asks for, or dismisses, the confirmation before a conversation is deleted. */
    deleteConfirmed(s, a: PayloadAction<string | undefined>) {
      s.confirmingDeleteId = a.payload;
    },
  },
  extraReducers: (build) => {
    // Escape gives up a rename and a pending delete, whichever is open.
    build.addCase(escaped, (s) => {
      s.renamingId = undefined;
      s.draftTitle = "";
      s.confirmingDeleteId = undefined;
    });
  },
});

/** The choices that belong to this window rather than to the daemon: which theme was picked and which of light and dark it resolved to. */
type SettingsState = { theme: Theme; resolved: "light" | "dark" };

const settingsSlice = createSlice({
  name: "settings",
  initialState: { theme: "system", resolved: "light" } as SettingsState,
  reducers: {
    /** Records the theme the user picked, before it has been resolved and stamped. */
    themePicked(s, a: PayloadAction<Theme>) {
      s.theme = a.payload;
    },
    /** Records the light or dark actually stamped on the root element. */
    themeResolved(s, a: PayloadAction<"light" | "dark">) {
      s.resolved = a.payload;
    },
  },
});

/** One tool the agent called while answering: the tool's name and the one line it reported about what it did. */
export type Step = { name: string; detail: string };

/** The question in flight: which conversation it was asked in, what was asked, the last working line, the answer as far as it has arrived, the tools called so far, and what the answer was drawn from. The window waits on one question at a time, exactly as the current window does, so a message on the stream that belongs to something else — a question asked in the hover window, a voice session — changes nothing here. */
export type Run = {
  /** The daemon's id for this ask, known once POST /ask has answered; until then any message on the stream is taken as this run's. */
  askId?: string;
  conversationId: string;
  question: string;
  status: string;
  answer: string;
  steps: Step[];
  evidence: Evidence[];
};

/** One step of a computer-use job's live list: the action it took (or is taking), the change it was written down to produce, and, once wait_for has checked it, whether that change came and why. Input on the wire is one "act" event per step (kind "step"), closed by the next one for it (kind "verified") — see the eventArrived reducer below. */
export type JobStepRow = {
  n: number;
  text: string;
  expect: string;
  outcome?: "pass" | "fail";
  why?: string;
  startedAt: number;
  finishedAt?: number;
};

/** A computer-use job in flight, attached to the chat it was asked from. id is the daemon's id for it, known once POST /act answers; until then any "act" event on the stream is taken as this job's, the same tolerance an ask's own id race gets. state is the job's own state word off the wire (see actjob.State). question is the one thing a stuck job is waiting on, cleared once the composer's next message answers it. say and spend are only set once, by the "done" event: the closing sentence and what the whole job cost. */
export type JobRun = {
  id?: string;
  conversationId: string;
  goal: string;
  state: string;
  steps: JobStepRow[];
  question?: string;
  say?: string;
  spend?: Spend;
  startedAt: number;
};

/** The shape of an "act" event's JSON detail (see internal/actjob.Event on the Go side). kind is "started", "step", "verified", "question", "answered", "paused", "resumed" or "done". */
type ActDetail = { kind: string; state: string; text: string; expect?: string; outcome?: string; spend?: Spend };

/** Decodes one "act" event's detail. Input: the detail text off the wire. Output: the parts, or every field empty when the text will not parse — which never happens against a daemon that sent it, but leaves nothing to throw on a malformed one. */
function parseActDetail(detail: string | undefined): ActDetail {
  try {
    return JSON.parse(detail ?? "{}") as ActDetail;
  } catch {
    return { kind: "", state: "", text: "" };
  }
}

type ProgressState = { run?: Run; job?: JobRun; streaming: boolean };

const progressSlice = createSlice({
  name: "progress",
  initialState: { streaming: false } as ProgressState,
  reducers: {
    /** Opens the daemon's event stream. The middleware below acts on this; the reducer only records that it happened, so a second dispatch is a no-op. */
    streamOpened(s) {
      s.streaming = true;
    },
    /** Records that a question has been sent, before the daemon has answered, so the thread shows it straight away. */
    askSent(s, a: PayloadAction<{ conversationId: string; question: string }>) {
      s.run = { conversationId: a.payload.conversationId, question: a.payload.question, status: "", answer: "", steps: [], evidence: [] };
    },
    /** Records what POST /ask answered: the id every message on the stream carries, and the conversation the question actually landed in, which the daemon opens itself when the window named none. */
    askAccepted(s, a: PayloadAction<{ askId: string; conversationId: string }>) {
      if (!s.run) return;
      s.run.askId = a.payload.askId;
      if (a.payload.conversationId) s.run.conversationId = a.payload.conversationId;
    },
    /** Gives up on a question the daemon never accepted. */
    askFailed(s) {
      s.run = undefined;
    },
    /** Records that a "do:" goal has been sent, before the daemon has answered, so the thread shows the goal as its own turn straight away. */
    jobSent(s, a: PayloadAction<{ conversationId: string; goal: string }>) {
      s.job = { conversationId: a.payload.conversationId, goal: a.payload.goal, state: "planning", steps: [], startedAt: Date.now() };
    },
    /** Records what POST /act answered: the id every "act" message on the stream carries. */
    jobAccepted(s, a: PayloadAction<{ id: string; conversationId: string }>) {
      if (s.job) s.job.id = a.payload.id;
    },
    /** Gives up on a job the daemon never accepted. */
    jobFailed(s) {
      s.job = undefined;
    },
    /** Folds one message from the stream into the question or the job in flight. An ask's own five event types are unchanged from before; "act" is a job's progress instead, folded into the job in state.progress.job rather than into run — a job is never the answer to a chat turn, only something the window shows beside one. "status" sets the working line, "tool" adds a step and says what it is doing, "answer" is the reply, and "done" and "error" end the run — the finished turn, failed or not, is then read back from the conversation itself. A message carrying another ask's or another job's id, or arriving with no question or job in flight, changes nothing. */
    eventArrived(s, a: PayloadAction<DaemonEvent>) {
      const ev = a.payload;
      if (ev.type === "act") {
        const job = s.job;
        if (!job) return;
        if (job.id && ev.id && ev.id !== job.id) return;
        const d = parseActDetail(ev.detail);
        if (d.state) job.state = d.state;
        switch (d.kind) {
          case "step":
            job.steps.push({ n: job.steps.length + 1, text: d.text, expect: d.expect ?? "", startedAt: Date.now() });
            break;
          case "verified": {
            const last = job.steps[job.steps.length - 1];
            if (last && last.finishedAt === undefined) {
              last.finishedAt = Date.now();
              last.outcome = d.outcome === "pass" ? "pass" : "fail";
              last.why = d.text;
            }
            break;
          }
          case "question":
            job.question = d.text;
            break;
          case "answered":
            job.question = undefined;
            break;
          case "done":
            job.say = d.text;
            job.spend = d.spend;
            break;
          default:
            // "started", "paused" and "resumed" only touch state, already applied above.
            break;
        }
        return;
      }
      const run = s.run;
      if (!run) return;
      if (run.askId && ev.id && ev.id !== run.askId) return;
      if (ev.conversation_id && run.conversationId && ev.conversation_id !== run.conversationId) return;
      switch (ev.type) {
        case "status":
          run.status = ev.text ?? "";
          break;
        case "tool":
          run.steps.push({ name: ev.text ?? "", detail: ev.detail ?? "" });
          run.status = ev.detail || ev.text || "";
          break;
        case "answer":
          run.status = "";
          run.answer += ev.text ?? "";
          if (ev.evidence?.length) run.evidence = ev.evidence;
          break;
        case "done":
        case "error":
          s.run = undefined;
          break;
        default:
          break;
      }
    },
  },
});

export const ui = uiSlice.actions;
export const conversationsUi = conversationsSlice.actions;
export const settings = settingsSlice.actions;
export const progress = progressSlice.actions;

/** Opens the daemon's SSE stream the first time progress.streamOpened is dispatched and feeds every message into the progress slice. A second streamOpened is ignored, so the window never ends up with two streams answering the same ask. Input: the function that opens a stream, which the window leaves as the real one and a test replaces. Output: the middleware. */
export function streamMiddleware(open: typeof events = events) {
  const listener = createListenerMiddleware();
  let stop: (() => void) | undefined;
  listener.startListening({
    actionCreator: progressSlice.actions.streamOpened,
    effect: async (_action, api) => {
      if (stop) return;
      stop = open((ev) => {
        api.dispatch(progressSlice.actions.eventArrived(ev));
        // A finished ask is what changes the conversation list, the turns inside it and what has been spent, so the cache is told to read them again rather than polling on a timer.
        if (ev.type === "done" || ev.type === "error") api.dispatch(oraApi.util.invalidateTags(["Conversation", "Task", "Usage"]));
      });
    },
  });
  // A live job is not a conversation the daemon knows about, so nothing tells the sidebar's own GET /conversations to say what it is doing; this patches the row's subtitle straight into the RTK Query cache instead; every other field is left as the daemon last sent it, and a job with no row to find (a fresh draft, before the first message opened one) patches nothing.
  const startTyped = listener.startListening.withTypes<RootState, AppDispatch>();
  startTyped({
    matcher: isAnyOf(progressSlice.actions.jobSent, progressSlice.actions.jobAccepted, progressSlice.actions.eventArrived),
    effect: (_action, api) => {
      const job = api.getState().progress.job;
      if (!job) return;
      api.dispatch(
        oraApi.util.updateQueryData("conversations", undefined, (draft) => {
          const row = draft.find((c) => c.id === job.conversationId);
          if (row) row.last = jobStateWord(job.state);
        }),
      );
    },
  });
  return listener;
}

/** Builds a store. Input: the state to start from, which a test uses to open the window on a given screen, and the function that opens the event stream. Output: the store. The window itself uses the one built below; every test builds its own, so no state leaks from one test to the next. */
export function makeStore(preloaded?: { ui?: Partial<UiState>; settings?: Partial<SettingsState> }, open?: typeof events) {
  return configureStore({
    reducer: {
      ui: uiSlice.reducer,
      conversations: conversationsSlice.reducer,
      settings: settingsSlice.reducer,
      progress: progressSlice.reducer,
      [oraApi.reducerPath]: oraApi.reducer,
    },
    preloadedState: preloaded
      ? {
          ui: { ...initialUi, ...preloaded.ui },
          settings: { theme: "system" as Theme, resolved: "light" as const, ...preloaded.settings },
        }
      : undefined,
    middleware: (getDefault) => getDefault().prepend(streamMiddleware(open).middleware).concat(oraApi.middleware),
  });
}

export const store = makeStore();

export type AppStore = ReturnType<typeof makeStore>;
export type RootState = ReturnType<AppStore["getState"]>;
export type AppDispatch = AppStore["dispatch"];

/** The typed dispatch, so a component never has to name the store's type itself. */
export const useAppDispatch = useDispatch.withTypes<AppDispatch>();

/** The typed selector, same reason. */
export const useAppSelector = useSelector.withTypes<RootState>();
