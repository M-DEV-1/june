/** The window's Redux store: what the user is looking at, what they have half-typed or half-done, what they have chosen in Settings, what the daemon is saying about the question in flight, and what it says on the stream about setup's microphone test, a local feature's download, an update's install and a restart under way. Everything the daemon holds is server state and lives in the RTK Query cache in api.ts instead; nothing is kept in two places. Component state is only for what is genuinely local, such as whether a menu is open, which the Radix components own themselves. */

import { configureStore, createAction, createListenerMiddleware, createSlice, isAnyOf, type PayloadAction } from "@reduxjs/toolkit";
import { useDispatch, useSelector } from "react-redux";

import { MIC_TEST_ID, events, juneApi, keyRefused } from "./api";
import type { DaemonEvent, Evidence, Notice, Spend } from "../shared/wire";
import { isJobLive, parseActDetail } from "../shared/job";
import { jobStateWord, noticeActionMessage } from "./format";
import type { Theme } from "../shared/theme";

export type { Theme };

/** The key a fresh, unsaved chat draft's composer text and job are kept under, before the first message has opened a real conversation and given it a real id. */
export const DRAFT_CHAT = "__draft__";

/** The screens the window can be showing. Each of Tasks, Meetings and Days is one page whose header carries a picker; none of them holds a list of its own beside the sidebar, because the sidebar is for chats and nothing else. */
export type Place = "chats" | "tasks" | "days" | "meetings" | "settings" | "routines";

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
  /** The conversation opened for a task that had none of its own, by task id. A task June noticed in a meeting is a note, not a row the daemon can hang a conversation off, and there is no route that ties the two together, so the window remembers the pairing for as long as it is open and reuses it rather than opening a second conversation on the next question. */
  taskChats: Record<string, string>;
  paletteOpen: boolean;
  /** What the sidebar's notice line says, and whether it reads as a failure (red, "text-destructive") or as plain status (the normal muted foreground). */
  notice?: { text: string; kind: "info" | "error" };
  /** The most recent notice to reach the window with no action yet — the live one the rail line offers Done/1h/Evening/Tomorrow buttons for. Cleared the moment the daemon's answer comes back as the same notice with its action set (see reactToNotice), which is what the buttons are replaced by. */
  /** at is when the notice reached this window, stamped here because the daemon sends none: the card reads it against the clock to say "now", then "5m ago" (see noticeAge). */
  liveNotice?: LiveNotice;
};

/** The notice the sidebar's card is showing. at is when it reached this window, stamped by the window because the daemon sends none: the card reads it against the clock to say "now", then "5m ago" (see noticeAge). */
export type LiveNotice = Pick<Notice, "kind" | "id" | "title" | "body" | "actions" | "expires"> & { at: number };

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
    /** Says on one line that a write did not go through, or that something succeeded, or clears what was said. */
    noticed(s, a: PayloadAction<UiState["notice"]>) {
      s.notice = a.payload;
    },
    /** Records, or clears, the live notice the rail line offers buttons for — see reactToNotice, the only dispatcher. */
    liveNoticeSet(s, a: PayloadAction<UiState["liveNotice"]>) {
      s.liveNotice = a.payload;
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

/** One tool the agent called while answering: the tool's name and the one line it reported about what it did. n is the step's number, given when it arrives and never recomputed, so a step keeps it however the list is later filtered — the same numbering a job's steps carry (JobStepRow below). */
export type Step = { n: number; name: string; detail: string; failed?: boolean };

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
  /** Whether the daemon has already said this question is over. Only ever set for a question whose "done" beat POST /ask's own reply, which is the one case the finish cannot be carried out where it arrives. */
  finished?: boolean;
  /** The "done" and "error" events that arrived before POST /ask had answered, by ask id, true for a done. Until then there is no id to tell this question's end from another client's — the hover window, a curl, a routine all finish on the same stream — so each is kept here and askAccepted settles whether one of them was this question's. */
  endings?: Record<string, boolean>;
};

/** One step of a computer-use job's live list: the action it took (or is taking), the change it was written down to produce, and, once wait_for has checked it, whether that change came and why. Input on the wire is one "act" event per step (kind "step"), closed by the next one for it (kind "verified") — see the eventArrived reducer below. */
export type JobStepRow = {
  n: number;
  text: string;
  expect: string;
  outcome?: "pass" | "fail";
  /** Whether the check already held before this step took it, which the daemon reports alongside a pass. A step like that proves nothing about what the step did, so the thread draws it as neither a pass nor a fail. */
  heldBefore?: boolean;
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
  /** What the model said it was going to do, written on its first round and sent once. The goal is the user's own words; this is the model's reading of them, which is what makes a step that wanders off it visible. */
  plan?: string;
  /** How many steps the model guessed the whole job would take, sent alongside the plan. Not a limit: the budget is twice it, and running out asks rather than fails (see actjob.outOfRoomQuestion). */
  estimate?: number;
  steps: JobStepRow[];
  question?: string;
  say?: string;
  spend?: Spend;
  startedAt: number;
};

/** What the daemon is doing right now: the one question in flight, the computer-use jobs in flight keyed by the conversation each was started from, the recording this window has open on the daemon's microphone and its words once they arrive, and whether the event stream is open. Jobs are keyed rather than held one at a time because a job runs for minutes and the user goes on to another chat while it does; a single slot would lose the first job the moment a second was started and fold the first's events into the second. */
/** What a live voice session is doing, off the daemon's "state" events; unset when no session runs. */
type VoiceState = "listening" | "thinking" | "speaking";
type ProgressState = {
  run?: Run;
  jobs: Record<string, JobRun>;
  /** The recording this window opened on the daemon's microphone, from the moment /dictate/start answered until its words have arrived; transcribing once Stop was pressed and whisper is still on it. Kept here rather than in the composer so the face can say June is listening while one is open, and so the words still land if the composer was swapped for another while whisper ran. */
  dictating?: { id: string; transcribing: boolean };
  /** The words of that recording, set only by its own stop reply or "dictation" event (another client's dictation finishing is not this window's) and held until a composer takes them into the box (dictationTaken). */
  dictation?: { id: string; text: string };
  /** A recording this window stopped waiting on without giving it up (dictationOverdue), whose words are still taken if they come. Its decode can queue behind a whole meeting's on the GPU (recorder.GPURun in internal/ipc/dictate.go), so no fixed wait bounds when they arrive. */
  dictationLate?: string;
  /** The id of a recording of this window's that the daemon said it could not transcribe, held until the composer has said so (dictationFailureSaid). */
  dictationFailed?: string;
  streaming: boolean;
  /** Whether the daemon answered this window's last request 401 or 403 even after the key was read afresh: it is running, but will not take this window's key. Set by the base query in api.ts through keyRefused; an empty pane reads it to say so rather than that nothing is answering. */
  refused?: boolean;
  voice?: VoiceState;
  /** Whether a meeting is being captured, and whether the nightly dream run is under way, off the daemon's "recording" and "dreaming" events. */
  recording?: boolean;
  dreaming?: boolean;
  /** How the last question ended and when, so the face can say done or refused for a moment after. */
  ended?: { ok: boolean; at: number };
};

/** Lets a recording of this window's go when the daemon says it could not be transcribed, and records that for the composer to say. Input: the progress state and the recording's id. Output: nothing; another client's failure changes nothing. Kept apart from takeHeard so a failure's text is never typed into the box as though it were the words. */
function failHeard(s: ProgressState, id: string): void {
  const open = s.dictating?.id === id;
  if (!open && s.dictationLate !== id) return;
  if (open) s.dictating = undefined;
  else s.dictationLate = undefined;
  s.dictationFailed = id;
}

/** Holds a recording's words for the composer when the recording is this window's: the one open now, or one it stopped waiting on (dictationLate). Input: the progress state, the recording's id and its transcript. Output: nothing; the recording is let go, and words for any other id are left alone. */
function takeHeard(s: ProgressState, id: string, text: string): void {
  const open = s.dictating?.id === id;
  if (!open && s.dictationLate !== id) return;
  s.dictation = { id, text };
  if (open) s.dictating = undefined;
  else s.dictationLate = undefined;
}

const progressSlice = createSlice({
  name: "progress",
  initialState: { streaming: false, jobs: {} } as ProgressState,
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
      // A "done" or "error" that beat this reply is only now known to be this question's own; the listener below finishes the run off the flag, and every other ending held here belonged to some other client's question and is let go.
      const ok = s.run.endings?.[a.payload.askId];
      s.run.endings = undefined;
      if (ok === undefined) return;
      s.run.finished = true;
      s.ended = { ok, at: Date.now() };
    },
    /** Gives up on a question the daemon never accepted. */
    askFailed(s) {
      s.run = undefined;
    },
    /** Gives up the question in flight once its finished turn has been read back into the thread. Input: the askId of the question being ended, which must be the one still in flight — a second question sent while the first was finishing has replaced the run by the time the first's read comes back, and ending it there would take the new question and its streaming answer off the screen. Output: nothing. Kept apart from the "done" event itself: the thread draws the question and the streamed answer out of the run alone, so clearing it on the event would blank the whole exchange until GET /conversations/{id} came back. */
    runEnded(s, a: PayloadAction<string | undefined>) {
      if (s.run && a.payload === s.run.askId) s.run = undefined;
    },
    /** Records that the run's "done" or "error" arrived while the question was still tracked under the draft key, before POST /ask had said which conversation the daemon opened for it. Input: none. Output: nothing; the middleware below repeats the finish once askAccepted supplies that id, which is the first moment there is a conversation to read back. */
    runFinished(s) {
      if (s.run) s.run.finished = true;
    },
    /** Records that a "do:" goal has been sent, before the daemon has answered, so the thread shows the goal as its own turn straight away. */
    jobSent(s, a: PayloadAction<{ conversationId: string; goal: string }>) {
      s.jobs[a.payload.conversationId] = { conversationId: a.payload.conversationId, goal: a.payload.goal, state: "planning", steps: [], startedAt: Date.now() };
    },
    /** Records what POST /act answered: the id every "act" message on the stream carries. */
    jobAccepted(s, a: PayloadAction<{ id: string; conversationId: string }>) {
      const job = s.jobs[a.payload.conversationId];
      if (job) job.id = a.payload.id;
    },
    /** Gives up on a job the daemon never accepted. Input: the conversation it was started from, which is the key it was filed under. */
    jobFailed(s, a: PayloadAction<string>) {
      delete s.jobs[a.payload];
    },
    /** Records the recording /dictate/start just opened. Input: its id, which its own "dictation" event and /dictate/stop both carry. Words from an earlier recording that no composer took are dropped, since they would otherwise land in the box ahead of these. */
    dictationStarted(s, a: PayloadAction<string>) {
      s.dictating = { id: a.payload, transcribing: false };
      s.dictation = undefined;
    },
    /** Records that Stop was pressed and the words are on their way, so the box says so instead of going quietly back to empty while whisper runs. */
    dictationStopping(s) {
      if (s.dictating) s.dictating.transcribing = true;
    },
    /** Lets the recording go with nothing more to come from it: the stop failed, or the user gave up waiting on its words. Words that arrive for it later are not this window's any more and are dropped. */
    dictationEnded(s) {
      s.dictating = undefined;
    },
    /** Stops waiting on the open recording without giving up its words: Stop found it already closed and nothing came back in time, or the stream dropped while it was open. The face and the box go back to idle, and the words still go into the box if they arrive after all. */
    dictationOverdue(s) {
      if (!s.dictating) return;
      s.dictationLate = s.dictating.id;
      s.dictating = undefined;
    },
    /** Takes the words of a recording this window opened, whether off its stop reply or its own "dictation" event: whichever lands first lets the recording go, and the other then finds nothing of its own open and changes nothing, so the words are typed once. Input: the recording's id and its transcript, "" when it heard nothing. */
    dictationHeard(s, a: PayloadAction<{ id: string; text: string }>) {
      takeHeard(s, a.payload.id, a.payload.text);
    },
    /** Records that a composer has put the words of the recording into its box, so no later composer puts them there again. */
    dictationTaken(s) {
      s.dictation = undefined;
    },
    /** Records that a composer has said a recording could not be transcribed, so it is said once. */
    dictationFailureSaid(s) {
      s.dictationFailed = undefined;
    },
    /** Folds one message from the stream into the question, the job or the dictation it belongs to. "status" sets the working line, "tool" adds a step and says what it is doing, and "answer" is the reply; "done" and "error" only say how the question ended, and ending the run is left to the middleware below, which reads the finished turn back before giving it up. "act" is a job's progress, folded into that job in state.progress.jobs rather than into run — a job is never the answer to a chat turn, only something the window shows beside one. "dictation" is the words of a recording this window opened. A message carrying another ask's or another job's id, or arriving with no question or job in flight, changes nothing. */
    eventArrived(s, a: PayloadAction<DaemonEvent>) {
      const ev = a.payload;
      // The daemon ends a dictation itself once it has heard 1.2s of silence, and the words come back on this event rather than on the stop reply, which by then answers 404 (see finish in internal/ipc/dictate.go). The composer takes them from here into whatever is half-typed. Every client's dictation is broadcast, so only the one this window opened is taken: keeping any of them left the face saying "listening" for good after the first.
      if (ev.type === "dictation") {
        if (ev.failed) failHeard(s, ev.id);
        else takeHeard(s, ev.id, ev.text ?? "");
        return;
      }
      // The three things the face is told about that belong to no question: a voice session's own state, a meeting being captured, the nightly run.
      if (ev.type === "state") {
        s.voice = ev.text === "listening" || ev.text === "thinking" || ev.text === "speaking" ? ev.text : undefined;
        return;
      }
      if (ev.type === "recording" || ev.type === "dreaming") {
        s[ev.type] = ev.text === "on";
        return;
      }
      // Every ask's end is broadcast to every client, so only this question's own says done or refused on the face; another client's failure flipped it to refused while this one was still being answered. Before POST /ask has answered there is no id to compare, so the ending is held for askAccepted to settle.
      if (ev.type === "done" || ev.type === "error") {
        const run = s.run;
        if (!run) return;
        if (!run.askId) run.endings = { ...run.endings, [ev.id]: ev.type === "done" };
        else if (ev.id === run.askId) s.ended = { ok: ev.type === "done", at: Date.now() };
        return;
      }
      if (ev.type === "act") {
        // The job this belongs to is the one whose id matches; a job whose POST /act has not answered yet has no id to match, and takes what arrives, the same tolerance an ask's own id race gets.
        const jobs = Object.values(s.jobs);
        const d = parseActDetail(ev.detail);
        let job = jobs.find((j) => j.id && j.id === ev.id) ?? jobs.find((j) => !j.id);
        // A job this window never posted: the voice session's do tool starts one in the daemon, so the first this window hears of it is its own "started" event, which carries the goal. Filed under the job's id, since there is no chat it was asked from. Without this every event of a spoken job was dropped and the running-now strip stayed empty through a chain the user could hear happening.
        if (!job && d.kind === "started" && ev.id) {
          job = { id: ev.id, conversationId: "", goal: d.text, state: d.state, steps: [], startedAt: Date.now() };
          s.jobs[ev.id] = job;
        }
        if (!job) return;
        if (d.state) job.state = d.state;
        switch (d.kind) {
          case "plan":
            // step carries the model's own estimate on a plan event, not a step number.
            job.plan = d.text;
            job.estimate = d.step;
            break;
          case "step":
            job.steps.push({ n: job.steps.length + 1, text: d.text, expect: d.expect ?? "", startedAt: Date.now() });
            break;
          case "verified": {
            const last = job.steps[job.steps.length - 1];
            if (last && last.finishedAt === undefined) {
              last.finishedAt = Date.now();
              last.outcome = d.outcome === "pass" ? "pass" : "fail";
              // A check that already held before the step took it comes back as a pass carrying this flag rather than as an outcome of its own, so nothing that reads the outcome has to learn a third word.
              last.heldBefore = d.held_before === true;
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
      // Only once POST /ask has answered is run.conversationId the daemon's own id: a question asked from a fresh draft is tracked under DRAFT_CHAT until then, and the answer event — the only one carrying a conversation_id — would otherwise be dropped whenever it beats the 202 back. Same condition as the askId guard above, and for the same race.
      if (run.askId && ev.conversation_id && run.conversationId && ev.conversation_id !== run.conversationId) return;
      switch (ev.type) {
        case "status":
          run.status = ev.text ?? "";
          break;
        case "tool":
          run.steps.push({ n: run.steps.length + 1, name: ev.text ?? "", detail: ev.detail ?? "", failed: ev.failed });
          run.status = ev.detail || ev.text || "";
          break;
        case "answer":
          run.status = "";
          run.answer += ev.text ?? "";
          if (ev.evidence?.length) run.evidence = ev.evidence;
          break;
        default:
          break;
      }
    },
  },
  extraReducers: (build) => {
    build.addCase(keyRefused, (s, a) => {
      s.refused = a.payload;
    });
    // A job started from a fresh draft is filed under the draft key and nothing else ever moves it, so the next New chat would open showing the last draft's finished job. The draft key is one slot the window reuses, unlike a conversation's own.
    build.addCase(uiSlice.actions.chatDraftOpened, (s) => {
      delete s.jobs[DRAFT_CHAT];
    });
  },
});

/** Where one local feature's download has got to, off its last "component" event: the stage word ("downloading", "verifying", "extracting", "testing"), the file and the bytes so far, and the rate. Dropped once the feature ends either way, after which GET /components is the truth again. */
export type FeatureProgress = { text: string; file: string; done: number; total: number; bps: number };

/** Where installing a newer June has got to, off its last "update" event. */
export type UpdateProgress = { text: string; done: number; total: number };

/** Why the window is waiting on the daemon to come back: a Gemini key was saved or removed, a restart was asked for, or a newer June is installing over this one — which takes longer, and is only over once the old daemon has actually gone. */
export type RestartWhy = "key" | "restart" | "update";

/** A restart the window is waiting on. slow is set once it has taken long enough that the screen should say what to do if June never comes back; failed, once the daemon has said it could not restart (or never went down at all), holds why, "" when it gave no reason, and keeps the screen up saying so until the person goes back to June. */
type RestartWait = { why: RestartWhy; since: number; slow: boolean; failed?: string };

/** What first-run setup and its descendants in Settings are waiting on that the daemon only says on the stream: the microphone test's level, each feature's download, the update's install, and a restart under way. */
type SetupState = {
  /** The microphone test's latest amplitude, 0 to 1. */
  micLevel: number;
  features: Record<string, FeatureProgress>;
  update?: UpdateProgress;
  restart?: RestartWait;
  /** A "restart_failed" that came while no restart was being waited on, and when. The event can beat the reply of the very request that set the restart off — a key saved, setup finished — so the wait that reply begins takes it up if it is recent (see restartBegan). */
  unclaimedFailure?: { error: string; at: number };
};

/** How long a "restart_failed" with no wait to land in is held for one about to begin, and then how long before it is said on the rail's notice line instead. */
const RESTART_FAILED_GRACE_MS = 3000;

/** Reads an event's JSON detail. Input: the detail string, or undefined. Output: the object, or an empty one for anything that does not parse, so a reader only ever has to default missing fields. */
function detailOf(detail?: string): Record<string, unknown> {
  try {
    const d: unknown = JSON.parse(detail ?? "");
    return d && typeof d === "object" ? (d as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** A field of a detail as a number, 0 when it is missing or not one. */
function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

const setupSlice = createSlice({
  name: "setup",
  initialState: { micLevel: 0, features: {} } as SetupState,
  reducers: {
    /** Records one "level" event of the microphone test. Input: its detail, which carries the amplitude as "mic" the way a voice session's does. */
    micLevelHeard(s, a: PayloadAction<string | undefined>) {
      const d = detailOf(a.payload);
      s.micLevel = Math.min(1, Math.max(0, num(d.mic ?? d.level)));
    },
    /** Puts the meter back at nothing, before a test starts and once it has ended. */
    micLevelReset(s) {
      s.micLevel = 0;
    },
    /** Folds one "component" event into that feature's progress. A feature that has ended — installed, failed or cancelled — leaves the map, because GET /components, which the middleware reads again on that event, then says what state it is in and why it failed. */
    featureEvent(s, a: PayloadAction<DaemonEvent>) {
      const ev = a.payload;
      if (!ev.id) return;
      const text = ev.text ?? "";
      if (ev.failed || text === "installed" || text === "failed" || text === "cancelled") {
        delete s.features[ev.id];
        return;
      }
      const d = detailOf(ev.detail);
      const was = s.features[ev.id];
      // A stage after the download carries no byte counts of its own, and the bar would otherwise drop back to nothing while the file is checked.
      s.features[ev.id] = {
        text,
        file: typeof d.file === "string" ? d.file : (was?.file ?? ""),
        done: d.done === undefined ? (was?.done ?? 0) : num(d.done),
        total: d.total === undefined ? (was?.total ?? 0) : num(d.total),
        bps: num(d.bps),
      };
    },
    /** Folds one "update" event in. A failure ends the wait on a restart that the install would have caused, since there will be none. Only the install's own steps are progress: "available" is the daily check, and holding it here would stand in front of GET /update's state, which is what the strip reads until the first step of an install arrives. */
    updateEvent(s, a: PayloadAction<DaemonEvent>) {
      const ev = a.payload;
      const text = ev.text ?? "";
      if (ev.failed || text === "failed") {
        s.update = undefined;
        if (s.restart?.why === "update") s.restart = undefined;
        return;
      }
      if (text !== "downloading" && text !== "verifying" && text !== "installing") return;
      const d = detailOf(ev.detail);
      s.update = { text, done: d.done === undefined ? (s.update?.done ?? 0) : num(d.done), total: d.total === undefined ? (s.update?.total ?? 0) : num(d.total) };
    },
    /** Starts waiting on the daemon to restart; the listener below does the waiting. A failure the daemon reported a moment ago, before this wait began, was this restart's. */
    restartBegan(s, a: PayloadAction<RestartWhy>) {
      const f = s.unclaimedFailure;
      s.unclaimedFailure = undefined;
      s.restart = { why: a.payload, since: Date.now(), slow: false, failed: f && Date.now() - f.at < RESTART_FAILED_GRACE_MS ? f.error : undefined };
    },
    restartSlow(s) {
      if (s.restart) s.restart.slow = true;
    },
    /** Records that the restart did not happen. Input: the reason, "" when there is none to give. The wait showing keeps its screen up, saying so; with no wait showing it is held for one about to begin (see unclaimedFailure). */
    restartFailed(s, a: PayloadAction<string>) {
      if (s.restart) s.restart.failed = a.payload;
      else s.unclaimedFailure = { error: a.payload, at: Date.now() };
    },
    /** Records that a "restart_failed" no wait took up has been said, by the button whose own refusal said it (POST /restart answers the failure as well as announcing it) or by the notice line below, so it is said once. */
    restartFailureSaid(s) {
      s.unclaimedFailure = undefined;
    },
    restartEnded(s) {
      s.restart = undefined;
      s.update = undefined;
    },
  },
});

export const ui = uiSlice.actions;
export const conversationsUi = conversationsSlice.actions;
export const settings = settingsSlice.actions;
export const progress = progressSlice.actions;
export const setupUi = setupSlice.actions;

/** Every tag the window reads, for the moments everything on screen may be stale at once: the stream coming back after a drop, and a restarted daemon. */
const EVERY_TAG = ["Conversation", "Task", "Day", "Meeting", "Settings", "Brain", "Usage", "Tracker", "Routine", "Job", "Setup", "Component", "Update"] as const;

/** How often a restart is checked on. */
const RESTART_POLL_MS = 600;
/** How long a restart may take before the screen says what to do if June never comes back. The old daemon keeps answering through its whole shutdown — closing a recording, flushing the last activity through a model call, bounded at 45 seconds alone (shutdownFlushBound in cmd/daemon_workers.go) — so most of a minute is still an ordinary restart after a busy hour. */
const RESTART_SLOW_MS = 45_000;
/** How long a daemon that never stops answering is waited on before the restart is taken not to be happening. Past the replacement's own wait for its predecessor (predecessorWait in cmd/lifecycle.go, 150 seconds), the daemon still answering is the old one with no restart under way. */
const RESTART_UNSEEN_MS = 180_000;

/** What a "notice" event does outside the progress slice. One arriving fresh, with no action yet, becomes the sidebar's liveNotice — the rail line's own Done/1h/Evening/Tomorrow buttons, wired through useActOnNoticeMutation in sidebar.tsx. Once its action is set — the daemon's answer to one of those buttons, or to the desktop notification's own — it says so on the rail line instead (the one surface every notice already reaches, alongside the routine run result "Could not add that routine" and the rest of ui.notice's callers), clears liveNotice so the buttons are gone, and, for a task notice pressed Done, tells the Tasks screen's cache to read the list again, since the daemon closed that task through its own task-done path (see internal/proactive/notify.go's markDone) without this window's POST /tasks/{id}/done ever running to invalidate it. */
function reactToNotice(n: Notice, api: { dispatch: AppDispatch }): void {
  // A meeting's moments ("Recording saved", "Transcribing meeting", "Meeting summary ready") are the only word this window gets that minutes were filed and their action items lifted, and the meetings list is subscribed for the life of the window, so nothing else would read it again until the window lost and regained focus.
  if (n.kind === "meeting") api.dispatch(juneApi.util.invalidateTags(["Meeting", "Task", "Day"]));
  if (!n.action) {
    api.dispatch(uiSlice.actions.liveNoticeSet({ kind: n.kind, id: n.id, title: n.title, body: n.body, actions: n.actions, expires: n.expires, at: Date.now() }));
    return;
  }
  api.dispatch(uiSlice.actions.liveNoticeSet(undefined));
  const msg = noticeActionMessage(n);
  if (msg !== undefined) api.dispatch(uiSlice.actions.noticed({ text: msg, kind: "info" }));
  if (n.kind === "task" && n.action === "done")
    api.dispatch(juneApi.util.invalidateTags(["Task"]));
}

/** Opens the daemon's SSE stream the first time progress.streamOpened is dispatched and feeds every message into the progress slice. A second streamOpened is ignored, so the window never ends up with two streams answering the same ask. Input: the function that opens a stream, which the window leaves as the real one and a test replaces. Output: the middleware. */
/** Ends the question in flight: reads the conversation it landed in back into the cache, then gives the run up. Input: anything that can dispatch and read the store. Output: nothing. The read is held and released rather than left dispatched: initiate() subscribes to that conversation's cache entry, and a subscription never let go of keeps the entry resident for the life of the window and has every later invalidation refetch it, so N conversations asked in meant N GETs on every finished ask. The conversation is not in the invalidation that follows for the same reason — it has just been read. A question asked from a fresh draft is still filed under the sentinel key here whenever its "done" beat POST /ask's reply: there is no conversation to read and nothing to give up, since the thread draws the whole exchange out of the run until the real conversation is open, so the finish is only recorded and repeated from askAccepted below. */
async function finishRun(api: { dispatch: AppDispatch; getState: () => RootState }): Promise<void> {
  const run = api.getState().progress.run;
  if (!run || run.conversationId === DRAFT_CHAT) {
    api.dispatch(progressSlice.actions.runFinished());
    return;
  }
  const read = api.dispatch(juneApi.endpoints.conversation.initiate(run.conversationId, { forceRefetch: true }));
  try {
    await read;
  } finally {
    read.unsubscribe();
  }
  api.dispatch(progressSlice.actions.runEnded(run.askId));
}

export function streamMiddleware(open: typeof events = events) {
  const listener = createListenerMiddleware();
  let stop: (() => void) | undefined;
  listener.startListening({
    actionCreator: progressSlice.actions.streamOpened,
    effect: async (_action, api) => {
      if (stop) return;
      stop = open(
        async (ev) => {
          // A live voice session sends up to twenty of these a second and nothing in this window draws them; dropping them here keeps them out of both listener matchers and the sidebar patch below. The microphone test's own are the one meter this window does draw.
          if (ev.type === "level") {
            if (ev.id === MIC_TEST_ID) api.dispatch(setupSlice.actions.micLevelHeard(ev.detail));
            return;
          }
          // A download's progress and an update's are not an ask's or a job's, and arrive several times a second, so they go to their own slice rather than through eventArrived and every listener on it. The feature or the update that has just ended is read again, since GET /components and GET /update are what say how it ended and why.
          if (ev.type === "component") {
            api.dispatch(setupSlice.actions.featureEvent(ev));
            const text = ev.text ?? "";
            if (ev.failed || text === "installed" || text === "failed" || text === "cancelled") api.dispatch(juneApi.util.invalidateTags(["Component", "Setup", "Settings"]));
            return;
          }
          if (ev.type === "update") {
            api.dispatch(setupSlice.actions.updateEvent(ev));
            // "available" is the daily check finding a release, which only GET /update describes, so the strip shows the moment it is found rather than at the next focus or poll.
            if (ev.failed || ev.text === "failed" || ev.text === "available") api.dispatch(juneApi.util.invalidateTags(["Update"]));
            // The installer closes this daemon, and in the desktop app this window with it; in a browser tab the window is still here to wait for the new one.
            else if (ev.text === "installing") api.dispatch(setupSlice.actions.restartBegan("update"));
            return;
          }
          if (ev.type === "lifecycle") {
            const typed = api as unknown as { dispatch: AppDispatch; getState: () => RootState };
            const d = detailOf(ev.detail);
            if (ev.text === "restart_failed") {
              api.dispatch(setupSlice.actions.restartFailed(typeof d.error === "string" ? d.error : ""));
              return;
            }
            if (ev.text !== "restarting") return;
            // A restart this window did not ask for — the one finishing setup put off until a download ended — would otherwise take the window away mid-sentence with nothing on it saying why.
            if (!typed.getState().setup.restart) api.dispatch(setupSlice.actions.restartBegan("restart"));
            // That restart brings its replacement up hidden, so a window the person is looking at would vanish until the hotkey. Asking for the restart again while it is under way only leaves the marker that shows the replacement's window (beginRestart in cmd/lifecycle.go), so it is asked again; a hidden window is left to come back hidden.
            if (d.reopen === false && typeof document !== "undefined" && document.visibilityState === "visible") {
              const again = typed.dispatch(juneApi.endpoints.restartJune.initiate());
              void again
                .unwrap()
                .catch(() => undefined)
                .finally(() => again.reset());
            }
            return;
          }
          api.dispatch(progressSlice.actions.eventArrived(ev));
          if (ev.type !== "done" && ev.type !== "error") return;
          // A finished ask is what changes the conversation list, the turns inside it and what has been spent, so the cache is told to read them again rather than polling on a timer. The conversation the question landed in is read back first and the run given up only then: the thread draws the question and the streamed answer out of the run alone, so giving it up on the event itself blanks the exchange until the refetch lands.
          // The list is named by its own id rather than by the bare type: the type alone matches every open conversation as well, including the one finishRun has just read, which would read it a second time for nothing.
          const tags: Parameters<typeof juneApi.util.invalidateTags>[0] = [{ type: "Conversation", id: "LIST" }, "Task", "Usage"];
          const typed = api as unknown as { dispatch: AppDispatch; getState: () => RootState };
          const run = typed.getState().progress.run;
          // Only this window's own question is finished here. Every client's done and error reach every window, and finishing on any of them gave up this window's question while it was still being answered, so its own answer never reached the open thread. One that ended before POST /ask said which it was is settled by askAccepted below.
          if (run?.askId && ev.id === run.askId) await finishRun(typed);
          // Another client's question — the hover's, a routine's, a curl — may have landed in the conversation open here, so that one is read again: by its id when the event names it, and otherwise every conversation on screen, which is the open thread and the list. Not while this window's own question is still unaccepted, since that one is read back the moment askAccepted names it.
          else if (ev.conversation_id) tags.push({ type: "Conversation", id: ev.conversation_id });
          else if (!run || run.askId) tags.push("Conversation");
          api.dispatch(juneApi.util.invalidateTags(tags));
        },
        async () => {
          // The stream opening again is this window's one signal that the daemon it had lost is answering, so everything that failed while it was gone is read once more. Without it a window left open across a daemon restart keeps showing "Nothing is answering" until something happens to focus it.
          // A question in flight across the drop is given up the way a finished one is: a restarted daemon will never send its "done", and the composer holds Send disabled until one arrives.
          const typed = api as unknown as { dispatch: AppDispatch; getState: () => RootState };
          // A recording open across the drop is let go the same way: a restarted daemon has closed its microphone and will never send its words, and the box and the face would otherwise say listening or transcribing until the window closed. Its words are still taken if the drop was only the stream's and they come after all.
          if (typed.getState().progress.dictating) {
            api.dispatch(progressSlice.actions.dictationOverdue());
            api.dispatch(uiSlice.actions.noticed({ text: "Dictation was cut off", kind: "error" }));
          }
          if (typed.getState().progress.run) await finishRun(typed);
          api.dispatch(juneApi.util.invalidateTags([...EVERY_TAG]));
        },
      );
    },
  });
  // Waits for a restarting daemon to come back: first for it to stop answering, then for it to answer again, reading GET /setup each time. A probe that got any HTTP answer at all, a refusal included, is a daemon that is up; only a request that reached nothing is one that is down. Polling rather than leaning on the stream coming back, because the stream is shared with every other reason it can drop, and a key saved during setup has nothing else open to notice by. The old daemon answers right through its shutdown, so a daemon still answering is never taken for the restarted one: only a "restart_failed", or one that has not gone down long past any shutdown, ends the wait without it, and then the screen says the restart did not happen.
  listener.startListening({
    actionCreator: setupSlice.actions.restartBegan,
    effect: async (_action, api) => {
      api.cancelActiveListeners();
      const typed = api as unknown as { dispatch: AppDispatch; getState: () => RootState };
      const began = Date.now();
      let down = false;
      for (;;) {
        await api.delay(RESTART_POLL_MS);
        const wait = typed.getState().setup.restart;
        if (!wait || wait.failed !== undefined) return;
        const waited = Date.now() - began;
        if (waited > RESTART_SLOW_MS && !wait.slow) api.dispatch(setupSlice.actions.restartSlow());
        if (!down && waited > RESTART_UNSEEN_MS) {
          api.dispatch(setupSlice.actions.restartFailed(""));
          api.dispatch(juneApi.util.invalidateTags([...EVERY_TAG]));
          return;
        }
        const probe = await typed.dispatch(juneApi.endpoints.setup.initiate(undefined, { subscribe: false, forceRefetch: true }));
        const status = probe.error && "status" in probe.error ? probe.error.status : undefined;
        const answered = !probe.error || typeof status === "number" || status === "PARSING_ERROR";
        if (!answered) {
          down = true;
          continue;
        }
        if (down) break;
      }
      api.dispatch(juneApi.util.invalidateTags([...EVERY_TAG]));
      api.dispatch(setupSlice.actions.restartEnded());
    },
  });
  // A failure that no wait took up within the grace — a restart nobody here was waiting on, such as the one setup put off — is said on the rail's notice line, since nothing else would ever say it.
  listener.startListening({
    actionCreator: setupSlice.actions.restartFailed,
    effect: async (action, api) => {
      const typed = api as unknown as { dispatch: AppDispatch; getState: () => RootState };
      if (typed.getState().setup.restart) return;
      await api.delay(RESTART_FAILED_GRACE_MS);
      if (typed.getState().setup.restart || !typed.getState().setup.unclaimedFailure) return;
      api.dispatch(setupSlice.actions.restartFailureSaid());
      const why = action.payload.trim().replace(/\.$/, "");
      api.dispatch(uiSlice.actions.noticed({ text: why ? `June couldn't restart: ${why}` : "June couldn't restart", kind: "error" }));
    },
  });
  // A live job is not a conversation the daemon knows about, so nothing tells the sidebar's own GET /conversations to say what it is doing; this patches the row's subtitle straight into the RTK Query cache instead; every other field is left as the daemon last sent it, and a job with no row to find (a fresh draft, before the first message opened one) patches nothing.
  const startTyped = listener.startListening.withTypes<RootState, AppDispatch>();
  // Reacts to eventArrived itself rather than to the open() callback above, so a "notice" event lands the same way whether the real stream carried it or a test (or the ?mock=1 fixture in mock.ts) dispatched progress.eventArrived directly.
  startTyped({
    matcher: progressSlice.actions.eventArrived.match,
    effect: (action, api) => {
      const ev = action.payload;
      if (ev.type === "notice" && ev.notice) reactToNotice(ev.notice, api);
    },
  });
  // A question whose "done" arrived before POST /ask answered was left running, because there was no conversation to read it back from yet; this is the moment there is one.
  startTyped({
    actionCreator: progressSlice.actions.askAccepted,
    effect: async (_action, api) => {
      if (api.getState().progress.run?.finished) await finishRun(api);
    },
  });
  startTyped({
    matcher: isAnyOf(progressSlice.actions.jobSent, progressSlice.actions.jobAccepted, progressSlice.actions.eventArrived),
    effect: (_action, api) => {
      const jobs = Object.values(api.getState().progress.jobs);
      if (!jobs.length) return;
      const before = api.getOriginalState().progress.jobs;
      api.dispatch(
        juneApi.util.updateQueryData("conversations", undefined, (draft) => {
          const rows = new Map(draft.map((c) => [c.id, c]));
          for (const job of jobs) {
            // A job that has ended writes its last word once, on the event that ended it, and never again: what the row says after that is the daemon's, and rewriting it on every later event of every other chat put long-finished words back over whatever GET /conversations last said.
            if (!isJobLive(job.state) && before[job.conversationId]?.state === job.state) continue;
            const row = rows.get(job.conversationId);
            if (row) row.last = jobStateWord(job.state);
          }
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
      setup: setupSlice.reducer,
      [juneApi.reducerPath]: juneApi.reducer,
    },
    preloadedState: preloaded
      ? {
          ui: { ...initialUi, ...preloaded.ui },
          settings: { theme: "system" as Theme, resolved: "light" as const, ...preloaded.settings },
        }
      : undefined,
    middleware: (getDefault) => getDefault().prepend(streamMiddleware(open).middleware).concat(juneApi.middleware),
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
