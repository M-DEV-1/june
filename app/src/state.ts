/** Pure state model for the window. No DOM here — main.ts renders View, this file only computes it. */

import { truncateAtWord } from "./shared/errorline";
import { noticeActionSuffix } from "./shared/notice";
import {type OraState} from "./shared/faces";
import { THEME_KEY, themeChoice, type Theme } from "./shared/theme";
import type { DaemonEvent, Notice as WireNotice } from "./daemon";
import { renderLevelEvent, type LevelDetail } from "./waveform";

export { THEME_KEY, themeChoice, type Theme };

export type Evidence = { title: string; meta: string; body?: string };

/** One tool call's row in the live step list. Opened by the "tool" event that starts it (name is the daemon's tool name, detail is that event's Detail — the argument summary, e.g. a quoted query) and closed by the next "tool" event, which sets finishedAt and nothing else: the finish event's own Detail is just a generic result word ("done", "3 hits") that the row's icon already conveys by turning into a check, so there is nothing worth overwriting the label with. error is set instead of finishedAt completing normally when the daemon's whole ask fails while this step was still open, so its spinner has something to turn into besides a check. */
export type ToolStep = {
  name: string;
  detail: string;
  startedAt: number;
  finishedAt?: number;
  error?: string;
};

/** Turn.progress is the latest live "tool" event's Detail, the short summary of what the daemon is doing right now ("clicking Reload", "3 hits"). It is shown under the question while the answer is still empty, so a long multi-step screen task has something on screen instead of a bare "…", and it stops being read once the answer lands, since the answer then takes over the same line. Turn.detail is the whole of a failed ask's message when only one line of it went into the answer (see errorLine); it is what the fold under the answer opens on, and it is absent on every turn that did not fail. Turn.steps is the same tool activity as progress/tools, kept instead as a timeline for the live step-list card (see applyToolEvent) — progress and tools stay as they were for whatever still reads them; steps is what the card actually draws now. */
/** A long computer-use job in flight, attached to the turn its goal opened (see the "do:" prefix in the submit case below). id is the daemon's job id ("act-1"), the one every "act" event on the stream carries as its own id. state is the job's own state word straight off the wire (see internal/actjob.State: "planning", "stepping", "verifying", "paused", "stuck", "done", "stopped" or "failed") — isJobLive below is what turns that into a yes/no. question is the one thing a stuck job is waiting on, set by a "question" event and cleared by "answered" or "done"; while it is set, the composer's Enter answers it instead of asking something new (see the submit case). spend is only set once, by "done": what the whole job cost, for the closing cost line. */
export type JobMeta = {
  id: string;
  state: string;
  startedAt: number;
  question?: string;
  spend?: { rounds: number; input: number; cached: number; output: number };
};

type Turn = {
  q: string;
  a: string;
  evidence?: Evidence[];
  status?: string;
  tools?: string[];
  progress?: string;
  detail?: string;
  steps?: ToolStep[];
  job?: JobMeta;
  /** When this turn was asked, set only by a typed submit (see the "submit" case below); a voice turn or one already sitting in a matter before this window ever read it carries none. This is what conversationSeparator compares against the turn before it to say whether a thread break belongs above this one. */
  at?: number;
};
/** One answer button a notice names for itself: key is what goes to the daemon's notice route as the action, label is what the button reads. The daily stale-task question ("Still open — any progress?") is the first notice to name its own, because Done / Not happening / Not urgent are not the Done / snooze / Open set every other notice takes. */
export type NoticeAction = { key: string; label: string };

// Notice and DaemonEvent are the daemon's wire shapes, not this window's own state, so daemon.ts (the module that actually talks to the wire) declares them once and this file only widens them — the point being that main.ts's events() callback and this reducer read the very same type instead of two same-named declarations that happened to agree by hand.
/** The daemon's notice, plus the answers it may carry. A notice with actions gets exactly those buttons, in that order, in place of the default set; one without is drawn exactly as it always has been (see noticeButtonsHtml in main.ts). */
export type Notice = WireNotice & { actions?: NoticeAction[] };
export type { DaemonEvent };

export type Matter = {
  id: string;
  title: string;
  context: string;
  turns: Turn[];
  /** Carried straight from the daemon's /matters row, drives the pill's ✓ (done) or status word (watching). */
  status?: "open" | "done" | "watching";
  kind?: "action" | "thread" | "meeting";
};

/** One row from the daemon's GET /matters. */
export type MatterRow = {
  id: string;
  title: string;
  kind: "action" | "thread" | "meeting";
  status: "open" | "done" | "watching";
  when: string;
  detail: string;
};

/** The daemon's GET /context, shape fixed by the Go side. */
export type ContextInfo = { app: string; title: string; text: string };

type ViewState = "empty" | "asking" | "answered";

/** What a live voice session is doing right now, straight from the daemon's "state" events. */
export type VoiceState = "idle" | "listening" | "speaking" | "thinking";

/** What the input says when nothing is being dictated, no hint is up and no voice session is running. */
export const ASK_PLACEHOLDER = "Ask about this window, or anything.";

/** What the input says when it is empty and nothing at all is running. It carries the two keys because nothing else on the hover does: there is no microphone button and no menu, so a user who has not been told about Space and Shift+Space has no way to find out that dictation and live voice exist. */
export const RESTING_PLACEHOLDER = `${ASK_PLACEHOLDER} Space to dictate · Shift+Space for voice`;

export type View = {
  matters: Matter[];
  current: number;
  evidenceOpen: boolean;
  /** Whether the collapsed step-list summary ("4 steps · 6.2 s") is expanded back into the full list. Only meaningful once a turn has answered — see stepsHtml. */
  stepsOpen?: boolean;
  input: string;
  state: ViewState;
  /** Context chip text ("app · title") from the last contextLoaded event; empty until the daemon answers. */
  contextChip: string;
  /** Raw context text from the same event, sent as the `context` field of the next ask(). */
  contextText: string;
  /* The four fields below are optional so a view can be written out without naming the microphone at all (the mock does); every one of them reads as "nothing is happening" when it is missing, and the reducer always sets them explicitly. */
  /** True while the space bar is held and the daemon's microphone is open for dictation. */
  dictating?: boolean;
  /** A short message shown in place of the placeholder for one second after a dictation that gave nothing back or failed; empty the rest of the time. */
  hint?: string;
  /** The daemon's id for the live voice session, empty when none is running. */
  voice?: string;
  /** What that session is doing; "idle" whenever `voice` is empty. */
  voiceState?: VoiceState;
  /** The daemon's last "level" reading for this session, {mic, speaker} each 0-1 (see renderLevelEvent in waveform.ts), undefined whenever `voice` is empty. main.ts owns the Waveform instances that smooth these into the braille bar; this is just the latest raw reading, kept here only so it is visible to the reducer's own tests. */
  voiceLevel?: LevelDetail;
  /** The notice card showing right now, undefined when none is. It is nothing to do with the ask on the rest of the card: a notice arriving mid-question leaves that question exactly as it was. */
  notice?: Notice;
  /** Whether the notice is the only thing on screen. True when the hover was not open when the notice arrived, so the card underneath has nothing on it worth showing; false when the user already had the hover open and the notice stacks above what is there. */
  noticeAlone?: boolean;
  /** Whether the pointer is over the notice, which pauses its six-second timer: a card being read must not be taken away mid-sentence. */
  noticeHeld?: boolean;
  /** Whether the one-time "Live voice started" hint has already been shown this window session. Space alone starts dictation and Shift+Space starts a live voice session, so a slipped modifier can open one without meaning to; the hint only fires the first time, so it teaches the state without repeating on every toggle. */
  voiceHintShown?: boolean;
  /** The daemon's conversation the hover's questions are being appended to, undefined until the first question has opened one. Sent with every later ask, which is what gives the model the earlier turns to refer to; see askConversation. */
  conversationId?: string;
  /** When that conversation was last used, as a millisecond clock reading. The thread lapses CONVERSATION_MS after this, counted from the last question or answer rather than from when the window was dismissed. */
  conversationAt?: number;
  /** True after Escape has been pressed once while a job is live: the next Escape stops it instead of just asking. Set by the first escape, cleared by any other event (including a second escape, which fires the stop instead) — see the escape case below. */
  confirmStopJob?: boolean;
};

export type Event =
  | { kind: "type"; value: string }
  /** fresh asks the question in a conversation of its own, dropping whatever thread was in hand. */
  | { kind: "submit"; fresh?: boolean }
  | { kind: "enter"; fresh?: boolean }
  /** The conversation the daemon answered the ask with, either the one the ask named or the one it opened for a question that named none. */
  | { kind: "asked"; conversationId: string }
  /** The daemon's reply to POST /act: the job id, attached to the turn "do:" opened so later "act" events can be matched to it. */
  | { kind: "jobStarted"; id: string }
  | { kind: "escape" }
  | { kind: "tab" }
  | { kind: "toggleEvidence" }
  | { kind: "toggleSteps" }
  | { kind: "answered"; patch: Partial<Matter> }
  | { kind: "daemonEvent"; ev: DaemonEvent }
  | { kind: "mattersLoaded"; rows: MatterRow[] }
  | { kind: "contextLoaded"; ctx: ContextInfo }
  | { kind: "dictating" }
  | { kind: "dictated"; text: string }
  | { kind: "dictationFailed" }
  | { kind: "hint"; text: string }
  | { kind: "voiceOn"; id: string }
  | { kind: "voiceOff" }
  | { kind: "voiceEvent"; ev: DaemonEvent }
  /** hoverOpen says whether the window was already on screen when the notice arrived, which is the one thing about the notice the reducer cannot work out for itself; main.ts reads it off the OS window. */
  | { kind: "notice"; notice: Notice; hoverOpen: boolean }
  /** One of the card's own buttons pressed: "done", "hour", "evening", "tomorrow" or "open". */
  | { kind: "noticeAct"; act: string }
  | { kind: "noticeHold" }
  | { kind: "noticeRelease" }
  | { kind: "noticeGone" }
  | { kind: "noticeClick" };

/** How long a notice stays on screen before it goes by itself, in milliseconds. Long enough to read three lines, short enough that a card the user is not interested in is gone before it becomes something to dismiss. The timer itself runs in main.ts; the pointer being over the card pauses it (see noticeHeld). */
export const NOTICE_MS = 6000;

/** The clock reading on a moment, 24-hour and to the minute ("18:00"), with no date on it. Shared by a snoozed notice's "until" line and the thread's own separator between two conversations, so the hover has one way of writing a time rather than one per place that needed one. Input: the moment. Output: the label. */
function clockLabel(d: Date): string {
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

/** The one-line text a notice whose action is set shows instead of its usual title and body: "Snoozed until 18:00" for one snoozed from its own desktop notification's buttons, "Snoozed until tomorrow 09:00" once the snooze crosses midnight, or "Done" for one dismissed outright. Input: the notice, and the current moment, used only to tell whether until falls on today. Output: the line, or undefined for a notice with no action, which draws exactly as it always has (see renderNotice in main.ts). */
export function noticeActionLine(n: Notice, now: Date): string | undefined {
  return noticeActionSuffix(n, now, clockLabel);
}

/** close hides the hover. openNotice opens the main app window at the screen and row the clicked notice named, which are empty when it named none and the window should just open. ask sends the question that has just gone on the card to the daemon, in the conversation named, or in a new one the daemon opens when none is named. */
type Effect =
  | { kind: "close" }
  | { kind: "openNotice"; place: string; id: string }
  /** Sends Done or a snooze for the notice to the daemon's notice route. */
  | { kind: "noticeAct"; notice: Notice; act: string }
  | { kind: "ask"; question: string; conversation?: string }
  /** Starts a computer-use job for a goal (POST /act), opened by a "do:" question. */
  | { kind: "startJob"; goal: string }
  /** Ends a live job now (POST /act/{id}/stop), from the second Escape's confirmation. */
  | { kind: "stopJob"; id: string }
  /** Answers the one question a stuck job asked (POST /act/{id}/answer). */
  | { kind: "answerJob"; id: string; text: string };

export type StepResult = { view: View; effect?: Effect };

/** How long the hover keeps asking into the same conversation, in milliseconds, counted from the last question or answer in it. Five minutes. The hover is opened by a hotkey over whatever the user is working in, so the shape of its use is a question, the window dismissed, a look at the thing the answer was about, and a follow-up ("show me", "again") typed a minute or two later; all of that has to land in one thread or the model is handed an empty one and answers "Show you what?". Five minutes covers that and stops well short of the next unrelated hotkey press an hour later, which should start clean. */
export const CONVERSATION_MS = 5 * 60 * 1000;

/** The conversation a question about to be sent belongs to. Input: the view and the current time. Output: the id to send with the ask, or undefined when no thread has been opened yet or the one in hand has been quiet for longer than CONVERSATION_MS — either of which asks the daemon to open a conversation and hand its id back. */
export function askConversation(v: View, now: number): string | undefined {
  if (v.conversationId === undefined || v.conversationAt === undefined)
    return undefined;
  return now - v.conversationAt <= CONVERSATION_MS
    ? v.conversationId
    : undefined;
}

/** Whether the thread should draw a separator above the turn at index i of a matter's turns — the point where one conversation lapsed and the next began. Input: the matter's turns and an index into them. Output: undefined for the first turn (nothing came before it to separate it from) and for a turn on either side with no timestamp (a voice exchange, or one already sitting in a matter before this window read it, carries none — see Turn.at); otherwise, once the gap between the two turns' timestamps runs longer than CONVERSATION_MS, the clock time the later one opened, for the separator to show. This reads the gap between the two turns' own moments rather than the view's conversationAt, so it still works once those turns are long past and nothing about the conversation they were in is left in the live view — the two agree in the ordinary case (a turn opens a conversation, is answered, and the next one either follows soon after or waits out the same five minutes) and can disagree only when a single turn takes longer than CONVERSATION_MS to answer and the very next question follows within a heartbeat of that answer, which the daemon still counts as the same conversation but this draws as a break anyway. */
export function conversationSeparator(
  turns: Matter["turns"],
  i: number,
): string | undefined {
  if (i <= 0) return undefined;
  const prev = turns[i - 1].at;
  const at = turns[i].at;
  if (prev === undefined || at === undefined) return undefined;
  return at - prev > CONVERSATION_MS ? clockLabel(new Date(at)) : undefined;
}

/** What a storage event means for the theme. Input: the event's key and new value; a key of null is the whole store being cleared. Output: the choice to apply now, or undefined when the event was about some other key and the theme has not changed. */
export function themeFromStorage(
  e: Pick<StorageEvent, "key" | "newValue">,
): Theme | undefined {
  if (e.key !== null && e.key !== THEME_KEY) return undefined;
  return themeChoice(e.newValue);
}

function currentMatter(view: View): Matter | undefined {
  return view.matters[view.current];
}

/** The one matter the window falls back to before the daemon has listed any, and whenever it lists none: it is what the turns of a question hang off. Input: none. Output: a fresh empty matter named "now". */
export function placeholderMatter(): Matter {
  return { id: "now", title: "now", context: "", turns: [] };
}

function patchCurrent(view: View, patch: Partial<Matter>): Matter[] {
  return view.matters.map((m, i) =>
    i === view.current ? { ...m, ...patch } : m,
  );
}

/** Patches the last (pending) turn of the current matter. Input: the view and a partial turn. Output: the matters array with that turn updated, or unchanged if there is no turn. */
function patchPendingTurn(view: View, patch: Partial<Turn>): Matter[] {
  const m = currentMatter(view);
  if (!m || m.turns.length === 0) return view.matters;
  const turns = m.turns.map((t, i) =>
    i === m.turns.length - 1 ? { ...t, ...patch } : t,
  );
  return patchCurrent(view, { turns });
}

/** The one line a failed ask reads as. Input: the message the daemon sent, which may be a provider's whole error. Output: line, its first line cut on a word boundary with an ellipsis (see truncateAtWord's own default cap), and detail, the whole message when anything was left out of the line and undefined when nothing was. */
export function errorLine(text: string): { line: string; detail?: string } {
  const { line, more } = truncateAtWord(text);
  return more ? { line, detail: text } : { line };
}

/** Turns an RFC3339 timestamp into a short readable date and leaves anything else untouched. Input: one part of an evidence meta line. Output: "31 Aug 2026" for a timestamp, the part unchanged otherwise. */
export function shortDate(part: string): string {
  const t = Date.parse(part);
  if (!/^\d{4}-\d{2}-\d{2}T/.test(part) || Number.isNaN(t)) return part;
  return new Date(t).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

/** Application names that turn up as one segment of a window title. A segment ending in one of these names the program rather than what the window is about, so it is dropped from the label and gives the chip its short app name instead. Only last words are listed, because that is all the match and the short name ever use. */
const APP_WORDS = new Set([
  "teams",
  "chrome",
  "chromium",
  "brave",
  "firefox",
  "safari",
  "edge",
  "docs",
  "sheets",
  "slides",
  "gmail",
  "mail",
  "outlook",
  "thunderbird",
  "code",
  "slack",
  "discord",
  "zoom",
  "notion",
  "obsidian",
  "figma",
  "excel",
  "powerpoint",
  "terminal",
  "spotify",
]);

/** The last whitespace-separated word of a name, which is the part worth showing ("Teams" out of "Microsoft Teams"). Input: any string. Output: its last word, or "" when there is none. */
function lastWord(s: string): string {
  return s.trim().split(/\s+/).pop() ?? "";
}

/** Whether a title segment names nothing a person would recognise: a size, a percentage or a clock time the app appended ("920 MB", "37%", "14:05"), or a machine-generated code such as a meeting id ("abc-defg-hij"). Input: one segment. Output: true when the segment should not be shown on the chip. */
function isNoise(s: string): boolean {
  // ponytail: the code test is the hyphen-joined lowercase shape meeting ids and slugs take; widen it only if a real title gets eaten by it.
  return (
    /^[\d.,:\s]+(%|[kmgt]b|[ap]\.?m\.?)?$/i.test(s) ||
    /^[a-z0-9]+(-[a-z0-9]+){2,}$/.test(s)
  );
}

/** Whether a title segment reads as a person's name (two or more capitalised words) or as a document (a name with a file extension). Those are the two things the chip is most useful naming, so one of them wins over the position rule. Input: one segment. Output: true for "Vexil Quorin" and "upload.go", false for "High memory usage". */
function isSpecific(s: string): boolean {
  const words = s.split(/\s+/);
  return (
    (words.length >= 2 && words.every((w) => /^[A-Z]/.test(w))) ||
    /^[\w-]+\.[a-z]{1,5}$/.test(s)
  );
}

/** Turns the foreground app and its window title into one label for the context chip. The title is split on the separators window titles use (" | ", " - ", " — " and " – "), a trailing unread count like "(3)" is dropped from each segment, and then two kinds of segment are thrown away: the ones that name the program itself, and the ones that carry no name at all (a memory figure, a meeting code). The label is the most specific segment left — a person or a filename if there is one, otherwise the second-from-last when three or more survive and the first when fewer do — and the chip is that label, a middle dot, and the program's short name. Input: the app name and the window title. Output: the chip text, capped at 34 characters with a trailing ellipsis.
 * "Chat | Vexil Quorin | Microsoft Teams - High memory usage - 920 MB" splits into five segments; "Microsoft Teams" gives the short name "Teams" and leaves, "920 MB" is dropped as a figure, and of "Chat", "Vexil Quorin" and "High memory usage" the person wins, so the chip reads "Vexil Quorin · Teams". */
export function chipLabel(app: string, title: string): string {
  const appTail = lastWord(app).toLowerCase();
  const parts = title
    .split(/\s+[|\u2013\u2014-]\s+/)
    .map((p) => p.replace(/\s*\(\d+\)\s*$/, "").trim())
    .filter((p) => p !== "");

  const namesApp = (p: string): boolean => {
    const w = lastWord(p).toLowerCase();
    return APP_WORDS.has(w) || (appTail !== "" && w === appTail);
  };
  const appParts = parts.filter(namesApp);
  const short = lastWord(appParts[appParts.length - 1] ?? app);

  const kept = parts.filter((p) => !namesApp(p) && !isNoise(p));
  if (kept.length === 0) return capChip(short);

  const label =
    kept.find(isSpecific) ??
    (kept.length >= 3 ? kept[kept.length - 2] : kept[0]);
  return capChip(
    label === short || short === "" ? label : `${label} \u00b7 ${short}`,
  );
}

/** Cuts a chip label to 34 characters on a character boundary, the last of them an ellipsis. Input: the label. Output: the label unchanged if it already fits. */
function capChip(s: string): string {
  const runes = Array.from(s);
  return runes.length > 34 ? runes.slice(0, 33).join("") + "…" : s;
}

/** The one line that names an evidence item. The daemon sends meta as "kind · when" and uses the kind as the title when the row had none, so a part that just repeats the title is dropped and a timestamp is shortened. Input: the item's title and meta. Output: the parts to show after the title, which may be empty. */
export function sourceMeta(title: string, meta: string): string[] {
  return meta
    .split(" · ")
    .map((p) => p.trim())
    .filter((p) => p !== "" && p !== title)
    .map(shortDate);
}

/** What the input's placeholder should say. Input: the view. Output: the listening line while a dictation is open, the one-second hint after a dictation gave nothing back (or after a live voice session's first start), the way out of a live voice session while one runs, the resting line naming both voice keys when the input is empty and nothing is happening, and otherwise the standing invitation to ask. Space alone starts dictation and Shift+Space starts a live voice session — a slipped modifier opens the wrong one silently — so the live line is worded to say plainly that voice is on and how to stop it, not just that something is on. */
export function placeholder(v: View): string {
  if (v.dictating) return "Listening… Space or Enter to stop";
  if (v.hint) return v.hint;
  if (v.voice) return "Live voice on · Shift+Space to stop";
  // Only at rest: with half a question typed Space is a space rather than a dictation, and mid-ask the keys are not what the line should be spending its width on.
  if (v.input === "" && v.state === "empty") return RESTING_PLACEHOLDER;
  return ASK_PLACEHOLDER;
}

/** Which of Ora's faces the hover window shows beside the input, from the same signals as dotLabel. Input: the view and whether the daemon is reachable. Output: the state, see src/shared/faces.ts. */
export function faceState(v: View, daemonUp: boolean): OraState {
  if (v.dictating) return "listening";
  if (v.voice) return v.voiceState === "speaking" ? "speaking" : v.voiceState === "thinking" ? "thinking" : "listening";
  if (v.state === "asking") return "thinking";
  if (v.state === "answered") return "done";
  return daemonUp ? "watching" : "asleep";
}

/** The same status the dot shows, in words, used as its title and aria-label. The dot is a 9px circle whose five states differ only by colour, which tells a screen reader nothing and tells a sighted user only what they happened to learn elsewhere. Input: the view and whether the daemon is reachable. Output: a short phrase naming the current state. */
export function dotLabel(v: View, daemonUp: boolean): string {
  if (v.dictating) return "Listening";
  if (v.voice) return voiceStateWord(v.voiceState);
  if (v.state === "asking") return "Working";
  if (v.state === "answered") return "Answered";
  return daemonUp ? "Ready" : "Not connected";
}

/** The word for what a live voice session is doing right now, straight off the daemon's "state" events (see internal/ipc/voice.go's setState: "listening", "thinking" or "speaking" while a session runs, "idle" only once it has ended, at which point voiceOff has already taken the whole surface down). Shown above the voice-mode grid and returned by dotLabel for the same session, so the surface and the dot never name the same state two different ways. Input: the view's voiceState. Output: the word, defaulting to "Listening" for "idle" or anything unrecognised — a session with nothing to show yet is still just waiting to hear something. */
export function voiceStateWord(state: VoiceState | undefined): string {
  if (state === "thinking") return "Thinking";
  if (state === "speaking") return "Speaking";
  return "Listening";
}

/** Folds one "tool" daemon event into a turn's step list. The daemon reports each tool call as two events carrying the same Text (its name): the first as the call starts, its Detail the argument summary; the second as it finishes, its Detail just a generic result word ("done", "3 hits"). Nothing on the event says which is which, so this closes the most recently opened step if one is still running (leaving its label alone — only the finish time is worth keeping from that second event) and opens a new one otherwise. Input: the steps so far, the event, and the current time. Output: the updated steps. */
export function applyToolEvent(
  steps: ToolStep[],
  ev: DaemonEvent,
  now: number,
): ToolStep[] {
  const running = steps[steps.length - 1];
  if (running && running.finishedAt === undefined) {
    return [
      ...steps.slice(0, -1),
      {
        ...running,
        finishedAt: now,
        ...(ev.failed ? { error: ev.detail || "failed" } : {}),
      },
    ];
  }
  const name = ev.text ?? "";
  const detail = ev.detail ?? "";
  // A screen task calls observe_screen and look over and over, and both of them say "Looking at the screen": a row each meant the card filled with the same sentence repeated. A call that would say what the row above it already says reopens that row instead of adding another, so the list has one line per thing Ora is doing rather than one per tool call.
  const done = steps[steps.length - 1];
  if (done && stepLabel(done.name, done.detail) === stepLabel(name, detail)) {
    return [...steps.slice(0, -1), { ...done, finishedAt: undefined }];
  }
  return [...steps, { name, detail, startedAt: now }];
}

/** Marks the step still running, if any, as failed instead of finished — for when the daemon's whole ask fails partway through a tool call, which never sends that call its own finishing "tool" event. Input: the steps so far, the daemon's error text, and the current time. Output: the updated steps, unchanged when none is running. */
export function failRunningStep(
  steps: ToolStep[],
  errText: string,
  now: number,
): ToolStep[] {
  const running = steps[steps.length - 1];
  if (!running || running.finishedAt !== undefined) return steps;
  return [
    ...steps.slice(0, -1),
    { ...running, finishedAt: now, error: errText },
  ];
}

/** How long a step has been (or was) running. Input: the step and the current time. Output: elapsed seconds, one decimal place — against finishedAt once it has one, otherwise against now, so a still-running row's number can be re-read on every render without the reducer doing anything. */
export function stepSeconds(s: ToolStep, now: number): number {
  return Math.round(((s.finishedAt ?? now) - s.startedAt) / 100) / 10;
}

/** The small icon kind drawn beside a step's label — one of the eight the card knows: look, point, click, type, scroll, note, search, open. Input: a tool's name. Output: its kind, or "step" for a tool with no icon of its own (shell_exec, read_file, list_files, action_items — plain enough that a generic dot reads fine). */
export function stepIconKind(name: string): string {
  return TOOL_STEP.get(name)?.icon ?? "step";
}

/** Strips the double quotes the daemon wraps an argument summary in ("venue" -> venue), for splicing a Detail into a label sentence instead of printing it as a quoted literal. Input: a tool event's Detail. Output: the same text with every double quote removed. */
function unquote(s: string): string {
  return s.replace(/"/g, "").trim();
}

/** Per-tool-name icon kind and label template. template gets the step's Detail already unquoted, and returns "" to mean "print the bare verb" for a tool whose Detail carries nothing worth splicing in (click, scroll_to, type_text and point_at currently send no argument summary on the daemon side — see toolActivitySummary in internal/agent/tools.go — so those always fall back to the bare verb today; the template is still written to use the detail if a future daemon change starts sending one). Unlisted tool names fall back to stepLabel's own generic wording. */
const TOOL_STEP = new Map<
  string,
  { icon: string; verb: string; template?: (detail: string) => string }
>([
  ["observe_screen", { icon: "look", verb: "Looking at the screen" }],
  // look takes the picture that observe_screen reads, so it says the same thing rather than printing its own name; two of them in a row fold into one row (see applyToolEvent).
  ["look", { icon: "look", verb: "Looking at the screen" }],
  ["show_marks", { icon: "look", verb: "Marking the screen" }],
  [
    "point_at",
    {
      icon: "point",
      verb: "Pointing at the screen",
      template: (d) => (d ? `Pointing at ${d}` : ""),
    },
  ],
  [
    "click",
    {
      icon: "click",
      verb: "Clicking",
      template: (d) => (d ? `Clicking ${d}` : ""),
    },
  ],
  [
    "scroll_to",
    {
      icon: "scroll",
      verb: "Scrolling",
      template: (d) => (d ? `Scrolling to ${d}` : ""),
    },
  ],
  [
    "type_text",
    { icon: "type", verb: "Typing", template: (d) => (d ? `Typing ${d}` : "") },
  ],
  ["save_note", { icon: "note", verb: "Saving a note" }],
  ["revise", { icon: "note", verb: "Fixing a note" }],
  ["action_items", { icon: "note", verb: "Checking action items" }],
  [
    "personal_context",
    { icon: "note", verb: "Checking what I know about you" },
  ],
  [
    "query_memory",
    {
      icon: "search",
      verb: "Searching memory",
      template: (d) => (d ? `Searching memory for ${d}` : ""),
    },
  ],
  [
    "recall",
    {
      icon: "search",
      verb: "Recalling",
      template: (d) => (d ? `Recalling ${d}` : ""),
    },
  ],
  [
    "query_store",
    {
      icon: "search",
      verb: "Searching",
      template: (d) => (d ? `Searching for ${d}` : ""),
    },
  ],
  [
    "branch",
    {
      icon: "search",
      verb: "Looking that up",
      template: (d) => (d ? `Looking up ${d}` : ""),
    },
  ],
  [
    "open_url",
    {
      icon: "open",
      verb: "Opening a page",
      template: (d) => (d ? `Opening ${d}` : ""),
    },
  ],
  [
    "shell_exec",
    {
      icon: "step",
      verb: "Running a command",
      template: (d) => (d ? `Running ${d}` : ""),
    },
  ],
  [
    "read_file",
    {
      icon: "step",
      verb: "Reading a file",
      template: (d) => (d ? `Reading ${d}` : ""),
    },
  ],
  [
    "list_files",
    {
      icon: "step",
      verb: "Listing files",
      template: (d) => (d ? `Listing ${d}` : ""),
    },
  ],
]);

/** The plain-English label for one step's row: "Looking at the screen", "Clicking Reload", "Searching memory for venue". Input: the tool's name and the step's Detail (its argument summary, quoted by the daemon). Output: the label, falling back to the tool's own name, spaces for underscores, for one this card has no wording for. */
export function stepLabel(name: string, detail: string): string {
  const meta = TOOL_STEP.get(name);
  if (!meta) return name.replace(/_/g, " ");
  return meta.template?.(unquote(detail)) || meta.verb;
}

/** The collapsed line a finished turn's step list folds down to: "4 steps · 6.2 s". Input: the turn's steps. Output: that line, or "" for a turn with no steps at all (the model answered with no tool calls), so the card has nothing to collapse to. */
export function stepsSummaryLine(steps: ToolStep[]): string {
  if (steps.length === 0) return "";
  const last = steps[steps.length - 1];
  const end = last.finishedAt ?? last.startedAt;
  const seconds = Math.round((end - steps[0].startedAt) / 100) / 10;
  return `${steps.length} step${steps.length === 1 ? "" : "s"} · ${seconds.toFixed(1)} s`;
}

/** Whether a finished turn's step list should show as the collapsed one-line summary rather than the full list. Input: the turn's steps and whether the user has clicked to expand them back open. Output: true once there is a summary worth showing (at least one step ran), nothing failed, and the user has not expanded it. A step that failed stays expanded instead: its cross and error text are the answer to "what happened", and folding that away into "4 steps · 6.2 s" would hide the one row that matters. Called only once a turn has answered, by which point every step this turns up is already finished (see applyToolEvent) - it does not need to check that itself. */
export function stepsCollapsed(
  steps: ToolStep[],
  stepsOpen: boolean | undefined,
): boolean {
  if (steps.length === 0 || stepsOpen) return false;
  return !steps.some((s) => s.error);
}

/** The goal of a "do:" question, which is what starts a computer-use job instead of an ask. Input: the text typed into the composer. Output: the goal with the prefix and any leading space stripped, or undefined for a question that does not start with it (including "do:" with nothing after it, which has no goal to run). Case-insensitive, so "Do: reload the page" works the same as "do:". */
export function jobGoal(text: string): string | undefined {
  const m = /^do:\s*(.+)/is.exec(text.trim());
  return m ? m[1].trim() || undefined : undefined;
}

/** Whether a job's own state word (see JobMeta) is one it may still take a step from. Input: the state. Output: false for "done", "stopped", "failed" and the empty string (no job attached yet), true for every other word this daemon sends. */
export function isJobLive(state: string): boolean {
  return (
    state !== "" &&
    state !== "done" &&
    state !== "stopped" &&
    state !== "failed"
  );
}

/** The live job on the turn on screen, if there is one. Input: the view. Output: the job, or undefined when the current matter has no turns or its last turn opened no job. */
function currentJob(view: View): JobMeta | undefined {
  const m = currentMatter(view);
  return m?.turns[m.turns.length - 1]?.job;
}

/** The shape of an "act" event's JSON detail (see internal/actjob.Event). kind is "started", "step", "verified", "question", "answered", "paused", "resumed" or "done". */
type ActDetail = {
  kind: string;
  state: string;
  text: string;
  expect?: string;
  outcome?: string;
  spend?: JobMeta["spend"];
};

/** Decodes one "act" event's detail. Input: the detail text off the wire. Output: the parts, or every field empty when the text will not parse — which never happens against a daemon that sent it, but leaves nothing to throw on a malformed one. */
function parseActDetail(detail: string | undefined): ActDetail {
  try {
    return JSON.parse(detail ?? "{}") as ActDetail;
  } catch {
    return { kind: "", state: "", text: "" };
  }
}

// now defaults to the real clock so every existing call site (main.ts's dispatch) needs no change; tests pass it explicitly so a step's elapsed time is deterministic instead of racing the test's own wall clock.
export function step(
  view: View,
  event: Event,
  now: number = Date.now(),
): StepResult {
  // A one-shot confirmation: the first Escape while a job is live sets it, and it takes effect on the very next Escape or lapses on anything else — typing, a daemon event, another key — rather than staying armed indefinitely.
  if (event.kind !== "escape" && view.confirmStopJob)
    view = { ...view, confirmStopJob: false };

  switch (event.kind) {
    case "type":
      return { view: { ...view, input: event.value } };

    case "submit": {
      const text = view.input.trim();
      if (!text) return { view };
      const m = currentMatter(view);
      const last = m?.turns[m.turns.length - 1];

      // A stuck job's one question is answered by whatever the composer sends next, not asked as a fresh question of the daemon's own model.
      if (last?.job && isJobLive(last.job.state) && last.job.question) {
        const job: JobMeta = { ...last.job, question: undefined };
        return {
          view: {
            ...view,
            input: "",
            matters: patchPendingTurn(view, { job }),
          },
          effect: { kind: "answerJob", id: last.job.id, text },
        };
      }

      const goal = jobGoal(text);
      if (goal) {
        const turns = [...(m?.turns ?? []), { q: goal, a: "", at: now }];
        return {
          view: {
            ...view,
            input: "",
            state: "asking",
            matters: patchCurrent(view, { turns }),
          },
          effect: { kind: "startJob", goal },
        };
      }

      const turns = [...(m?.turns ?? []), { q: text, a: "", at: now }];
      // A question asked as a fresh thread names no conversation, so the daemon opens one and the model starts with nothing to refer to; so does the first question of all, and one asked after the thread in hand went quiet.
      const conversation = event.fresh ? undefined : askConversation(view, now);
      return {
        view: {
          ...view,
          input: "",
          state: "asking",
          matters: patchCurrent(view, { turns }),
          conversationId: conversation,
          conversationAt: now,
        },
        effect: { kind: "ask", question: text, conversation },
      };
    }

    case "enter": {
      // Text in the input asks it, whether that is the first question or a follow-up; an empty input means there is nothing to ask, so Enter closes the window like Escape does.
      if (view.input.trim())
        return step(view, { kind: "submit", fresh: event.fresh }, now);
      return { view, effect: { kind: "close" } };
    }

    case "asked": {
      // The daemon answers every ask with the conversation it stored the question in, whether that is the one the ask named or one it opened. Keeping it is the whole of the follow-up: the next question names it, and the daemon then hands the model the turns already in it.
      return {
        view: {
          ...view,
          conversationId: event.conversationId || undefined,
          conversationAt: now,
        },
      };
    }

    case "jobStarted":
      return {
        view: {
          ...view,
          matters: patchPendingTurn(view, {
            job: { id: event.id, state: "planning", startedAt: now },
          }),
        },
      };

    case "escape": {
      // Escape only hides the window. A live session is not the window's to end — the user hides this and goes back to what they were doing while Ora keeps listening — so only Shift+Space, the spoken "stop", or a stop from somewhere else finishes it.
      if (view.evidenceOpen) return { view: { ...view, evidenceOpen: false } };
      // A live job is not dropped by one stray Escape: the first asks for confirmation, and only the next one actually stops it.
      const job = currentJob(view);
      if (job && isJobLive(job.state)) {
        if (!view.confirmStopJob)
          return { view: { ...view, confirmStopJob: true } };
        return {
          view: { ...view, confirmStopJob: false },
          effect: { kind: "stopJob", id: job.id },
        };
      }
      return { view, effect: { kind: "close" } };
    }

    case "tab": {
      if (view.matters.length === 0) return { view };
      const current = (view.current + 1) % view.matters.length;
      const m = view.matters[current];
      const state: ViewState = m.turns.length > 0 ? "answered" : "empty";
      // Another matter is another subject, so the thread in hand goes with the card it belonged to and the next question there opens one of its own.
      return {
        view: {
          ...view,
          current,
          evidenceOpen: false,
          stepsOpen: false,
          input: "",
          state,
          conversationId: undefined,
          conversationAt: undefined,
        },
      };
    }

    case "toggleEvidence":
      return { view: { ...view, evidenceOpen: !view.evidenceOpen } };

    case "toggleSteps":
      return { view: { ...view, stepsOpen: !view.stepsOpen } };

    case "answered":
      return {
        view: {
          ...view,
          matters: patchCurrent(view, event.patch),
          state: "answered",
        },
      };

    case "daemonEvent": {
      const ev = event.ev;
      switch (ev.type) {
        case "status":
          return {
            view: {
              ...view,
              matters: patchPendingTurn(view, { status: ev.text ?? "" }),
            },
          };

        case "tool": {
          const m = currentMatter(view);
          const last = m?.turns[m.turns.length - 1];
          const tools = [...(last?.tools ?? []), ev.text ?? ""];
          // An empty Detail (a tool with no argument summary, e.g. click) must not blank out the line the previous event set; keep the last one shown until a real one replaces it or the answer arrives.
          const progress = ev.detail || last?.progress || "";
          const steps = applyToolEvent(last?.steps ?? [], ev, now);
          return {
            view: {
              ...view,
              matters: patchPendingTurn(view, { tools, progress, steps }),
            },
          };
        }

        case "answer":
          return {
            view: {
              ...view,
              matters: patchPendingTurn(view, {
                a: ev.text ?? "",
                evidence: ev.evidence,
              }),
            },
          };

        case "done":
          // The answer landing is the last thing that happened in this thread, so the few minutes before it lapses are counted from here rather than from the question that started it.
          return { view: { ...view, state: "answered", conversationAt: now } };

        case "error": {
          // A failure can be a provider's whole error — a thousand characters of JSON and map literals in the quota case — so the answer slot takes one line of it and the fold under it holds the rest, rather than the window growing until the input and the footer are off screen.
          const { line, detail } = errorLine(ev.text ?? "");
          const m = currentMatter(view);
          const last = m?.turns[m.turns.length - 1];
          // The step still running, if any, never gets a finishing "tool" event of its own when the whole ask fails instead — its spinner has to turn into a cross here or it spins forever.
          const steps = failRunningStep(last?.steps ?? [], ev.text ?? "", now);
          return {
            view: {
              ...view,
              matters: patchPendingTurn(view, { a: line, detail, steps }),
              state: "answered",
              conversationAt: now,
            },
          };
        }

        case "act": {
          const m = currentMatter(view);
          const last = m?.turns[m.turns.length - 1];
          // Only an event for the job this turn actually opened is applied; one that beats the POST /act reply (jobStarted has not attached the id yet) is dropped rather than guessed at, the same tolerance the ask path already gives its own id race.
          if (!last?.job || last.job.id !== ev.id) return { view };
          const d = parseActDetail(ev.detail);
          const job: JobMeta = {
            ...last.job,
            state: d.state || last.job.state,
          };
          let steps = last.steps ?? [];

          switch (d.kind) {
            case "step":
              steps = [
                ...steps,
                {
                  name: d.expect ? `${d.text} — expecting ${d.expect}` : d.text,
                  detail: "",
                  startedAt: now,
                },
              ];
              break;
            case "verified": {
              const i = steps.length - 1;
              if (i >= 0) {
                const s = steps[i];
                steps = [
                  ...steps.slice(0, i),
                  {
                    ...s,
                    finishedAt: now,
                    ...(d.outcome === "fail" ? { error: d.text } : {}),
                  },
                ];
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
              job.question = undefined;
              job.spend = d.spend;
              // The closing text and the finished look are one event, same as an ask's own "done": justFinished in main.ts's dispatch is what stops the elapsed-time ticker and collapses the step list, keyed off this same state transition.
              return {
                view: {
                  ...view,
                  matters: patchPendingTurn(view, { job, steps, a: d.text }),
                  state: "answered",
                },
              };
          }
          return {
            view: { ...view, matters: patchPendingTurn(view, { job, steps }) },
          };
        }

        default:
          // A dictation transcript or a voice event that reached the wrong door; the dictation and voiceEvent cases below own those.
          return { view };
      }
    }

    case "mattersLoaded": {
      // Anything with something on screen survives the reload, answered or not, until the thread lapses. The list is read again every time the hover is shown, and while it kept only questions still waiting for an answer, asking something, switching to another window to check on it and coming back threw the whole exchange away.
      // The lapse is the same CONVERSATION_MS the daemon side already used to decide which thread a question joins (see askConversation): once the thread has been quiet that long the card opens clean, so a hotkey press an hour later is a fresh start with nothing to dismiss. A question still waiting for its answer is never dropped, however long it has taken. Ctrl+Enter clears it before then.
      const lapsed =
        view.conversationAt !== undefined && now - view.conversationAt > CONVERSATION_MS;
      const isMidQuestion = (m: Matter) =>
        m.turns.length > 0 && (!lapsed || m.turns[m.turns.length - 1].a === "");
      const openId = view.matters[view.current]?.id;
      const kept = view.matters.filter(isMidQuestion);
      const fresh = event.rows
        .filter((r) => !kept.some((m) => m.id === r.id))
        .map((r): Matter => ({
          id: r.id,
          title: r.title,
          context: "",
          turns: [],
          status: r.status,
          kind: r.kind,
        }));
      // A daemon with nothing open still leaves the placeholder, because submit appends the next question to the current matter and an empty list would swallow it.
      const listed = [...kept, ...fresh];
      const matters = listed.length ? listed : [placeholderMatter()];
      // The matter on screen is found again by id, not by carrying its old numeric index across the merge: kept and fresh can come back in a different order than view.matters was in, and a raw index would then point at whichever matter now sits at that position instead of the one the user had open.
      const byId = matters.findIndex((m) => m.id === openId);
      const current =
        byId >= 0
          ? byId
          : matters.length === 0
            ? 0
            : Math.min(view.current, matters.length - 1);
      // Reloading the list drops any matter that is not mid-question, so a matter with no turns left has nothing on screen to be answering: the view goes back to empty instead of keeping the last answer's state.
      const hasTurns = (matters[current]?.turns.length ?? 0) > 0;
      return {
        view: {
          ...view,
          matters,
          current,
          state: hasTurns ? view.state : "empty",
        },
      };
    }

    case "contextLoaded":
      return {
        view: {
          ...view,
          contextChip: chipLabel(event.ctx.app, event.ctx.title),
          contextText: event.ctx.text,
        },
      };

    case "dictating":
      return { view: { ...view, dictating: true, hint: "" } };

    case "dictated": {
      const text = event.text.trim();
      // An empty transcript must not wipe what is already in the input, so only real words are written there.
      if (!text)
        return { view: { ...view, dictating: false, hint: "Heard nothing." } };
      return { view: { ...view, dictating: false, hint: "", input: text } };
    }

    case "dictationFailed":
      return { view: { ...view, dictating: false, hint: "Dictation failed." } };

    case "hint":
      return { view: { ...view, hint: event.text } };

    case "voiceOn": {
      // The hint only fires the first time a live session starts in this window: a slipped Shift+Space needs to be caught once, not announced every single time voice turns on.
      const hint = view.voiceHintShown ? "" : "Live voice started";
      return {
        view: {
          ...view,
          voice: event.id,
          voiceState: "listening",
          voiceLevel: undefined,
          dictating: false,
          hint,
          voiceHintShown: true,
        },
      };
    }

    case "voiceOff":
      // Clears the "Live voice started" hint too, if it is still up: the resting look shows no hint, only ever the standing placeholder.
      return {
        view: {
          ...view,
          voice: "",
          voiceState: "idle",
          voiceLevel: undefined,
          hint: "",
        },
      };

    // A notice is Ora speaking first, so every case below touches the notice fields and nothing else: a brief landing while a question is being answered must leave that question, its steps and the input exactly as they were.
    case "notice":
      return {
        view: {
          ...view,
          notice: event.notice,
          noticeAlone: !event.hoverOpen,
          noticeHeld: false,
        },
      };

    case "noticeHold":
      return { view: { ...view, noticeHeld: true } };

    case "noticeRelease":
      return { view: { ...view, noticeHeld: false } };

    case "noticeGone":
      // The pointer being over the card is what pauses its timer, and a timer armed before the pointer arrived can still fire; a held card stays up until it is let go.
      // A card that asked a question is the exception: its answer window closes on the daemon's clock whatever the pointer is doing, so holding it open would leave a button that answers "Could not do that".
      if (view.noticeHeld && !view.notice?.expires) return { view };
      return {
        view: {
          ...view,
          notice: undefined,
          noticeAlone: false,
          noticeHeld: false,
        },
      };

    case "noticeClick": {
      const notice = view.notice;
      if (!notice) return { view };
      return {
        view: {
          ...view,
          notice: undefined,
          noticeAlone: false,
          noticeHeld: false,
        },
        effect: { kind: "openNotice", place: notice.place, id: notice.id },
      };
    }

    case "noticeAct": {
      const notice = view.notice;
      if (!notice) return { view };
      // Open is the click the card has always answered, and it takes the card down because it leaves for the app window. "default" is the same button under the name the desktop gives a press on the notification body, which is the key the daemon's own Open carries (actionOpen in internal/proactive/notify.go); posting it back would be refused, since opening a window is not something the daemon can do for the card.
      if (event.act === "open" || event.act === "default")
        return {
          view: { ...view, notice: undefined, noticeAlone: false, noticeHeld: false },
          effect: { kind: "openNotice", place: notice.place, id: notice.id },
        };
      // Done and the snoozes go to the daemon and leave the card standing, because the daemon can refuse them (a store it cannot write answers 500) and a card dropped at the press would show a refusal as done. The daemon's follow-up event, the same notice with its action filled in, is what replaces it with the one-line confirmation, and the six-second timer takes it away if nothing comes back.
      return { view, effect: { kind: "noticeAct", notice, act: event.act } };
    }

    case "voiceEvent": {
      const ev = event.ev;
      if (ev.type === "state") {
        // The daemon ends the session itself when it hears "stop", and this is how a window that pressed nothing finds out.
        if (ev.text === "idle") return step(view, { kind: "voiceOff" });
        return {
          view: { ...view, voiceState: (ev.text as VoiceState) || "listening" },
        };
      }

      // Ticks in every 50ms while a session runs; main.ts reads this straight back out to smooth into the braille bar (see its Waveform instances) without going through a full re-render.
      if (ev.type === "level")
        return { view: { ...view, voiceLevel: renderLevelEvent(ev.detail ?? "") } };

      const m = currentMatter(view);
      if (!m) return { view };
      const turns = [...m.turns];
      const last = turns[turns.length - 1];
      const text = ev.text ?? "";

      switch (ev.type) {
        case "heard":
          // Speech is transcribed in pieces, so a run of them is one thing the user said; anything Ora has already answered ends the turn and the next piece starts a new one.
          if (last && last.a === "")
            turns[turns.length - 1] = {
              ...last,
              q: `${last.q} ${text}`.trim(),
            };
          else turns.push({ q: text, a: "" });
          break;

        case "said":
          // Ora's reply arrives word by word and joins onto whatever turn is on screen, without a space: the words already carry their own.
          if (last)
            turns[turns.length - 1] = { ...last, a: `${last.a}${text}` };
          else turns.push({ q: "", a: text });
          break;

        case "tool":
          if (!last) return { view };
          turns[turns.length - 1] = {
            ...last,
            tools: [...(last.tools ?? []), text],
          };
          break;

        default:
          return { view };
      }
      return { view: { ...view, matters: patchCurrent(view, { turns }) } };
    }
  }
}
