/** The React window's whole connection to the local daemon at http://127.0.0.1:6942: the IPC token, the RTK Query API over the daemon's routes, and the SSE stream. The token is read here and nowhere else, so every request in the window authenticates the same way and there is one place to fix when the daemon changes its secret. The stream reader and its message shapes are shared with the hover's src/daemon.ts through shared/wire.ts. */

import { createAction } from "@reduxjs/toolkit";
import { createApi, fetchBaseQuery, type BaseQueryFn, type FetchArgs, type FetchBaseQueryError } from "@reduxjs/toolkit/query/react";
import { invoke } from "@tauri-apps/api/core";

import { TOKEN_HEADER, devToken } from "../shared/token";
import { openStream, type DaemonEvent, type Evidence, type Spend } from "../shared/wire";
import { DAEMON_HOST_PORT } from "./daemon-url";

export { devToken };
export type { DaemonEvent, Evidence, Notice, Spend } from "../shared/wire";

/** Where the daemon listens. */
const base = `http://${DAEMON_HOST_PORT}`;

/** The shared IPC secret, held in memory only and never logged. Undefined until refreshToken has read one. */
let token: string | undefined;

/** Re-reads the IPC token from the Tauri side, falling back to the dev server's query string. Input: none. Output: the token now in use, or undefined when there is none to be had. The daemon writes a fresh token every time it starts and it restarts this window as its child, so a token read once at startup goes stale and every later request is refused with 401; this is called again on the first 401 and whenever the window is shown. */
export async function refreshToken(): Promise<string | undefined> {
  try {
    token = await invoke<string>("ipc_token");
  } catch {
    // Not inside Tauri, or the daemon has not written its token file yet; on the dev server the token can come from the query string instead.
    token = devToken(location);
  }
  return token;
}

/** Whether the window's own global shortcut could not be registered (hotkey_status in src-tauri/src/lib.rs). Input: none. Output: true only when the window tried and another program holds the keys; false when it worked, on Linux, where the desktop's keybinding is the shortcut, and outside Tauri. */
export async function hotkeyFailed(): Promise<boolean> {
  try {
    return Boolean((await invoke<{ failed: boolean }>("hotkey_status"))?.failed);
  } catch {
    return false;
  }
}

/** The token to send, reading one if this window has never had one. Input: none. Output: the token, or undefined when none could be read. */
async function ensureToken(): Promise<string | undefined> {
  if (!token) await refreshToken();
  return token;
}

/** The plain query: the daemon's base URL with the IPC token on every request. */
const rawBaseQuery = fetchBaseQuery({
  baseUrl: base,
  prepareHeaders: async (headers) => {
    const t = await ensureToken();
    if (t) headers.set(TOKEN_HEADER, t);
    return headers;
  },
});

/** Says whether the daemon refused this window's key on its last answer, even after the key was read afresh. Input: true for a 401 or 403 that a fresh key did not cure, false for any other answer. Dispatched by the base query below only when the answer changes, and kept in the store as progress.refused, so an empty pane can tell a daemon that will not take this window's key from one that is not there at all — both used to read "Nothing is answering". */
export const keyRefused = createAction<boolean>("june/keyRefused");

/** What keyRefused last said, so it is dispatched on a change rather than after every request. */
let refusedLast = false;

/** The query every endpoint below runs through: the plain one, plus a single retry with a freshly read token when the daemon answers 401. Input and output are RTK Query's own — the endpoint's arguments in, the parsed body or an error out. A 401 means the daemon restarted and wrote a new token since this window read one, which is the ordinary case after the daemon relaunches the window, so it is worth exactly one silent retry and not an error on screen. */
const baseQueryWithFreshToken: BaseQueryFn<string | FetchArgs, unknown, FetchBaseQueryError> = async (args, api, extra) => {
  const used = await ensureToken();
  let result = await rawBaseQuery(args, api, extra);
  if (result.error?.status === 401) {
    // Only a key that actually changed is worth another try: sending the one just refused straight back doubled every request a window with a wrong key made, for the same 401.
    const fresh = await refreshToken();
    if (fresh !== used) result = await rawBaseQuery(args, api, extra);
  }
  const refused = result.error?.status === 401 || result.error?.status === 403;
  if (refused !== refusedLast) {
    refusedLast = refused;
    api.dispatch(keyRefused(refused));
  }
  return result;
};

/** One row of GET /conversations: enough to draw the sidebar without loading any turns. Mirrors ipc.ConversationSummary. */
export type ConversationSummary = {
  id: string;
  title: string;
  brain: string;
  /** The text of the newest turn, already truncated by the daemon. */
  last: string;
  /** When the conversation was last touched, RFC3339. */
  updated: string;
};

/** One thing said in a conversation. Mirrors ipc.TurnView. Role is "you" or "june"; kind is "ask", "dictation", "voice" or "error"; reason is set only for an error turn. */
export type Turn = {
  id: string;
  role: string;
  text: string;
  kind: string;
  evidence: Evidence[];
  tools: string[];
  when: string;
  reason: string;
};

/** One conversation and everything said in it, oldest first. Mirrors ipc.ConversationView. */
export type ConversationView = { id: string; title: string; brain: string; turns: Turn[] };

/** Whose task it is: "me" for the user's own, "them" for one a meeting left with someone else, "unclear" when nobody said — a task merely heard is not automatically owed, which is the whole point of the split. */
export type TaskOwner = "me" | "them" | "unclear";

/** What POST /tasks/{id}/done may set a task to. "dropped" is only for a task June noticed: the daemon answers 400 for a dropped task of the user's own, because user_tasks has no third state to hold it in. */
export type TaskStatus = "open" | "done" | "dropped";

/** One thing to do on GET /tasks. Mirrors ipc.Task; source is "you" for a task the user typed in and "noticed" for an action item a meeting raised. detail is where it came from: the meeting it was raised in and the date, for example "Meridian statement pattern analysis, 2026-09-01", or "you said" for one the user typed in. GET /tasks itself takes ?owner=me|them|unclear|all (default "me"), so which of these a given fetch returns depends on how it was called, not on anything in the row itself. */
export type Task = {
  id: string;
  title: string;
  source: string;
  owner: TaskOwner;
  when: string;
  done: boolean;
  conversation_id: string;
  detail: string;
};

/** One day that has anything in it, on GET /days. Mirrors ipc.DaySummary. */
export type DaySummary = {
  date: string;
  title: string;
  has_page: boolean;
  seen: number;
  meetings: number;
  meeting_minutes: number;
};

/** One day's page on GET /days/{date}. Mirrors ipc.DayView; heading is the daemon's own one-line summary of the day, for example "60 things seen · 1 call, 28 min". brief and close are the morning brief and evening close entries for that day, "" when the day had neither; page carries the same text as close when there is one, falling back to the day's digest when there is not. */
export type DayView = {
  date: string;
  page: string;
  brief: string;
  close: string;
  you: { when: string; text: string }[];
  /** id is the same id POST /tasks/{id}/done takes, so ticking one here and ticking it on the Tasks screen are the same action on the same row. status is the item's full state, since done alone cannot tell a dropped item from an open one. */
  tasks: { id: string; title: string; done: boolean; status: TaskStatus; owner: TaskOwner }[];
  heading: string;
};

/** One user-authored scheduled instruction on GET /routines. Mirrors ipc.RoutineView; last_run and last_answer are both "" until it has fired at least once. */
export type Routine = {
  id: string;
  text: string;
  schedule: string;
  enabled: boolean;
  last_run: string;
  last_answer: string;
};

/** One recording on GET /meetings. Mirrors ipc.Meeting; duration_s is 0 when the recording never reported a length. */
export type Meeting = {
  id: string;
  title: string;
  when: string;
  duration_s: number;
  minutes: string;
  attendees: { name: string; heard_only: boolean }[];
};

/** SettingsView's "first_run" field on GET /settings. Mirrors ipc.FirstRunView: which of the four ways June can answer text are set up on this machine, and the plain one-line steps to fix that — which the daemon fills in only while none of the four works, so an empty steps list means there is nothing left to set up and the window shows no panel. */
export type FirstRun = {
  gemini_key: boolean;
  codex_login: boolean;
  claude_cli: boolean;
  local_model: boolean;
  steps: string[];
};

/** The daemon's configuration and on-disk footprint on GET /settings. Mirrors ipc.SettingsView; keep_audio_days is -1 when the config has no retention setting, embed_model is "none" when hybrid search runs lexical-only, and hotkey is the GNOME accelerator read live off gsettings. */
export type SettingsView = {
  data_dir: string;
  store_bytes: number;
  recordings_bytes: number;
  models_bytes: number;
  voice_model: string;
  brain: string;
  embed_model: string;
  meetings_enabled: boolean;
  capture_enabled: boolean;
  keep_audio_days: number;
  daemon_started: string;
  version: string;
  hotkey: string;
  /** Optional because a daemon older than the field sends no "first_run" at all, and the window must draw nothing rather than a panel full of undefined. */
  first_run?: FirstRun;
  /** Whether GET /brains may read the Claude row's usage bars from the undocumented Anthropic endpoint using the Claude Code login's own token. POST /settings with this field writes it back. */
  claude_usage_from_login: boolean;
  /** Whether June asks GitHub once a day for a newer version. Optional, as are the two below, because a daemon older than the field sends nothing and the window then draws no control for it. */
  update_check?: boolean;
  /** Whether June's background work (note upkeep, meeting write-ups) may go to the user's other signed-in brains when the chosen one cannot do it. Questions are not covered: the router hands those on regardless. */
  allow_fallback?: boolean;
  /** Whether June offers to record when another app has held the microphone long enough to be a call. */
  meetings_offer?: "ask" | "off";
};

/** What POST /settings may change, any subset at once: a field left out is left as it is. autostart is the start-at-sign-in choice GET /setup reports. */
export type SettingsChange = Partial<{
  claude_usage_from_login: boolean;
  update_check: boolean;
  allow_fallback: boolean;
  meetings_offer: "ask" | "off";
  autostart: boolean;
}>;

/** One allowance window a brain's provider reports for the user's own account: a five-hour or weekly subscription window, a daily request ceiling. Mirrors internal/agent.UsageLimit (aliased as brain.UsageLimit). used_fraction is 0 to 1; resets_at is RFC3339. */
export type UsageLimit = { window: string; used_fraction: number; resets_at: string; source: string };

/** One backend that can answer for June on GET /brains. Mirrors ipc.BrainView; model is the one last picked for this brain, "" when none ever was. limits and limits_at are optional so a daemon older than the field still parses; a brain with no allowance data reported sends limits as an empty list rather than leaving it out. */
export type Brain = {
  id: string;
  name: string;
  signed_in: boolean;
  account: string;
  models: string[];
  model: string;
  note: string;
  default: boolean;
  limits?: UsageLimit[];
  limits_at?: string;
  /** Why this brain has no usage bars: Grok's names what its CLI has no reading for, Claude's says the usage source was turned off in Settings. Empty when the daemon has nothing to say, or the brain does report limits. */
  limits_note?: string;
};

/** GET and POST /brains' whole body: every brain, and whether no brain is pinned and the daemon's router picks one per question. automatic is false from a daemon too old to send it, which is what such a daemon means — it always pinned whichever brain was last picked. */
export type Brains = { brains: Brain[]; automatic: boolean };

/** What POST /brains takes to put the daemon back on its own routing instead of a pinned brain, and what the picker sends for its "Automatic" row. */
export const AUTOMATIC_BRAIN = "auto";

/** Reads GET or POST /brains' body into Brains. */
function brainsBody(r: Partial<Brains>): Brains {
  return { brains: r.brains ?? [], automatic: r.automatic ?? false };
}

/** What one provider has cost in tokens over a window. Mirrors ipc.ProviderTotal. */
export type UsageProvider = { provider: string; calls: number; input_tokens: number; output_tokens: number; total_tokens: number };

/** The same for one model of one provider. Mirrors ipc.ModelTotal. */
export type UsageModel = UsageProvider & { model: string };

/** One window of the ledger: a row per provider and a row per model. Mirrors ipc.UsageWindow. */
export type UsageWindow = { providers: UsageProvider[]; models: UsageModel[] };

/** One day of the week's bars. Mirrors ipc.UsageDay. */
export type UsageDay = { day: string; calls: number; total_tokens: number };

/** One finished call in the log. Mirrors ipc.UsageCall; channel says which part of June made it — "text", "voice", "dream", "eval" or "subtask". */
export type UsageCall = {
  id: number;
  when: string;
  provider: string;
  model: string;
  channel: string;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  duration_ms: number;
  question: string;
  /** How many rounds of the tool loop the question took. The daemon carries this on agent.TurnTrace but does not file it on a usage row yet, so it is optional and the window says nothing about rounds until it arrives. */
  rounds?: number;
  /** How much of input_tokens the provider answered out of its own prompt cache rather than reading afresh. Part of input_tokens, never extra to it. Optional for the same reason as rounds. */
  cached_input_tokens?: number;
};

/** One provider's own allowance windows as GET /usage reports them, keyed by provider id in Usage.limits. Mirrors ipc.ProviderLimits; limits_at is when the reading was taken, RFC3339. */
export type ProviderLimits = { limits: UsageLimit[]; limits_at: string };

/** The providers in Usage.limits that are web search rather than brains: the two engines branch() reaches the web through, named as internal/agent/websearch.go names them. A search spends a request and no tokens, so these are the only place its cost shows — the token tables draw them as rows of zeros. */
export const SEARCH_PROVIDERS = ["exa", "tavily"] as const;

/** GET /usage: what each provider has cost in tokens today and over the last seven days, a figure per day of that week, and the calls themselves as a log. Every number is a count the daemon read from its own ledger; nothing here is a price. budget_used_fraction is how much of the plan's own allowance has gone, 0 to 1, and is optional because the daemon does not report it yet. limits holds each provider's own plan allowance keyed by provider id, brains and the search engines together, and is optional so a daemon too old to send it still parses. */
export type Usage = {
  today: UsageWindow;
  week: UsageWindow;
  days: UsageDay[];
  recent: UsageCall[];
  budget_used_fraction?: number;
  limits?: Record<string, ProviderLimits>;
};

/** GET /status: whether the tracker is paused right now, and when a pause for a set time ends by itself (RFC3339; "" or absent for a pause that lasts until resumed). POST /pause and POST /resume are what change it. */
export type TrackerStatus = { paused: boolean; paused_until?: string };

/** One of Gemini Live's thirty prebuilt voices, on GET or POST /voices. Mirrors ipc.VoiceView. trait is Google's own one-word description of how it sounds ("Bright", "Gravelly"), shown beside the name because thirty star names say nothing on their own about how any of them sounds. current marks the one a live session dials with next; exactly one row carries it. */
export type Voice = { name: string; trait: string; current: boolean };

/** One of the Live models on GET or POST /voices. Mirrors ipc.LiveModelView. trait is the trade choosing it costs and buys, in one line — latency against tone, and whether the model decides for itself that audio was aimed at it. current marks the one a live session dials with next; exactly one row carries it. */
export type LiveModel = { name: string; label: string; trait: string; current: boolean };

/** GET /voices' whole body: the voice roster and the Live models beside it. */
export type Voices = { voices: Voice[]; models: LiveModel[] };

/** One tool call's step inside a computer-use job. Mirrors actjob.Step; outcome is "pass", "fail", or "" before wait_for has checked it, and expect is the change the step was written down to produce. */
export type ActStep = {
  n: number;
  tool: string;
  expect: { kind: string; value: string };
  result: string;
  outcome: string;
  why: string;
};

/** A computer-use job's whole record, from GET /act/{id}. Mirrors actjob.Job (the fields the window reads out of it); state is one of "planning", "stepping", "verifying", "paused", "stuck", "done", "stopped" or "failed". plan is written once, early, and left alone after; question is set only while state is "stuck"; say is the closing sentence, set once state is "done", "stopped" or "failed". */
export type ActJob = {
  id: string;
  goal: string;
  state: string;
  plan: string;
  steps: ActStep[];
  question: string;
  say: string;
  err: string;
  spend: Spend;
  elapsed_ms: number;
};

/** GET /setup: whether first-run setup is finished, and what its screens say. Mirrors ipc.SetupView. brain_ready is true once anything can answer (a key or a signed-in login); restart_pending says a change is waiting on a restart; hotkey is spelled the way a person reads it ("Ctrl+Alt+Space"). */
export type SetupView = {
  done: boolean;
  version: string;
  gemini_key: boolean;
  brain_ready: boolean;
  default_brain: string;
  autostart: boolean;
  hotkey: string;
  restart_pending: boolean;
  data_dir: string;
  /** Linux only: whether June's GNOME extension answers with window frames, which screen control needs. false until the first logout after installing; absent where there is no such check. */
  window_frames?: boolean;
};

/** What POST and DELETE /setup/gemini-key answer once the key is written: the daemon restarts itself straight after, because the key is read once at start and threaded into everything built then. Both refuse 409 with "recording", "processing" or "downloading" as the error word while that restart would cut a meeting or a download short, with nothing changed. */
export type KeySaved = { ok: boolean; restarting: boolean };

/** POST /setup/mic-test's verdict after three seconds of listening. consent is what Windows' privacy settings say about desktop apps using the microphone, "unknown" anywhere that has no such switch; error is why the microphone would not open, "" (or absent, from a daemon that never sends it) when it did. */
export type MicTest = { device: string; heard: boolean; peak: number; consent: "allowed" | "blocked" | "unknown"; error?: string };

/** The id the microphone test's "level" events carry, which is how they are told apart from a voice session's. */
export const MIC_TEST_ID = "mic-test";

/** What GET /components found about this machine. gpu is absent when none was read; vram_mb and ram_mb are 0 when unknown. */
export type Platform = {
  os: string;
  gpu?: { vendor: string; name: string; vram_mb: number; driver: string };
  vulkan: boolean;
  free_bytes: number;
  ram_mb: number;
  /** Windows' Smart App Control, which blocks unsigned programs outright — the ones these features download among them. Absent off Windows and from an older daemon. */
  smart_app_control?: "on" | "evaluation" | "off" | "unknown";
};

/** One optional local feature on GET /components. variant is the build that suits this machine ("cuda", "cpu", "vulkan"); requires names the features it builds on; needs_restart says June only picks it up after a restart; error is set only while state is "failed". "external" is a feature whose files were put there by hand, which June uses but will not remove. */
export type Feature = {
  id: string;
  title: string;
  variant: string;
  download_bytes: number;
  disk_bytes: number;
  state: "not_installed" | "queued" | "installing" | "installed" | "external" | "update_available" | "failed";
  error: string;
  requires: string[];
  needs_restart: boolean;
};

/** GET /components' whole body. */
export type Components = { platform: Platform; features: Feature[]; restart_pending: boolean };

/** GET /update: the running version, the newest release, and where installing it has got to. can_install is false where June cannot replace itself (Linux, or a build with no version), and the window links to release_url instead. */
export type UpdateView = {
  current: string;
  latest: string;
  available: boolean;
  release_url: string;
  can_install: boolean;
  state: "idle" | "checking" | "downloading" | "ready" | "installing" | "failed";
  error: string;
};

/** Reads the HTTP status out of whatever .unwrap() threw. Input: the caught value. Output: the status code, or undefined when there is none to read. The daemon's error routes answer plain text through http.Error, which fetchBaseQuery cannot parse as JSON, so it reports the real code as a PARSING_ERROR carrying originalStatus rather than as status itself. Also what stopDictation reads its 404 through. */
export function errorStatus(e: unknown): number | undefined {
  if (!e || typeof e !== "object" || !("status" in e)) return undefined;
  const status = (e as { status: unknown }).status;
  if (status === "PARSING_ERROR") return (e as { originalStatus?: number }).originalStatus;
  return typeof status === "number" ? status : undefined;
}

/** Reads what a refusal said. Input: the caught value. Output: the "error" field of a JSON body such as {"error":"invalid_key"}, or the first line of a plain-text one, or "" when it carried neither. The setup and component routes answer JSON and the older routes plain text through http.Error, and a route that is still a stub answers neither, so all three are read the same way here. */
export function errorWord(e: unknown): string {
  if (!e || typeof e !== "object" || !("data" in e)) return "";
  let data = (e as { data: unknown }).data;
  if (typeof data === "string") {
    const text = data;
    try {
      data = JSON.parse(text);
    } catch {
      return text.split("\n")[0].trim();
    }
  }
  if (data && typeof data === "object" && "error" in data) return String((data as { error: unknown }).error ?? "");
  return "";
}

/** Reads the sentence a refusal carried for a person. Input: the caught value. Output: the "message" field the setup, component and update routes put beside their "error" word, capitalised, or "" when there is none — in which case the caller says something of its own rather than show the bare word. */
export function errorMessage(e: unknown): string {
  if (!e || typeof e !== "object" || !("data" in e)) return "";
  const data = (e as { data: unknown }).data;
  if (!data || typeof data !== "object" || !("message" in data)) return "";
  const text = String((data as { message: unknown }).message ?? "").trim();
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/** Every daemon route the window reads or writes, as one RTK Query API. The daemon wraps its lists in an object named after the list ("conversations", "tasks", "days", "meetings", "brains"), so each list endpoint unwraps that here and components get a plain array. */
export const juneApi = createApi({
  reducerPath: "june",
  baseQuery: baseQueryWithFreshToken,
  tagTypes: ["Conversation", "Task", "Day", "Meeting", "Settings", "Brain", "Usage", "Tracker", "Routine", "Job", "Voice", "Setup", "Component", "Update"],
  endpoints: (build) => ({
    /** The 50 most recently touched conversations, newest first. The list is tagged with an id of its own so it can be read again without every open conversation being read again with it: an invalidation naming the type alone still matches this, which is what every mutation below relies on. */
    conversations: build.query<ConversationSummary[], void>({
      query: () => "/conversations",
      transformResponse: (r: { conversations: ConversationSummary[] }) => r.conversations ?? [],
      providesTags: [{ type: "Conversation" as const, id: "LIST" }],
    }),
    /** One conversation and its turns, oldest first. */
    conversation: build.query<ConversationView, string>({
      query: (id) => `/conversations/${encodeURIComponent(id)}`,
      transformResponse: (r: ConversationView) => ({ ...r, turns: r.turns ?? [] }),
      providesTags: (_result, _error, id) => [{ type: "Conversation" as const, id }],
    }),
    /** Opens a conversation and answers 201 with its id. */
    createConversation: build.mutation<{ id: string }, { title?: string; brain?: string } | void>({
      query: (body) => ({ url: "/conversations", method: "POST", body: body ?? {} }),
      invalidatesTags: ["Conversation"],
    }),
    /** Removes a conversation and every turn said in it; the daemon answers 204. */
    deleteConversation: build.mutation<void, string>({
      query: (id) => ({ url: `/conversations/${encodeURIComponent(id)}`, method: "DELETE" }),
      invalidatesTags: ["Conversation"],
    }),
    /** Renames a conversation; the daemon answers 204, and 400 for a blank title. */
    renameConversation: build.mutation<void, { id: string; title: string }>({
      query: ({ id, title }) => ({ url: `/conversations/${encodeURIComponent(id)}/title`, method: "POST", body: { title } }),
      invalidatesTags: (_result, _error, { id }) => ["Conversation", { type: "Conversation" as const, id }],
    }),
    /** Posts a question. The daemon answers 202 straight away with the id to watch on the event stream and the conversation the question landed in — the one named, or a fresh one the daemon opens for a question that named none, which is how a new chat's first message is what actually creates it. The list is told to read itself again straight away rather than waiting for the answer to finish, so a fresh conversation shows up in the sidebar the moment it exists. */
    ask: build.mutation<{ id: string; conversation_id: string }, { question: string; conversation_id?: string; brain?: string; context?: string }>({
      query: ({ question, conversation_id, brain, context }) => ({
        url: "/ask",
        method: "POST",
        body: { question, conversation_id: conversation_id ?? "", brain: brain ?? "", context: context ?? "" },
      }),
      invalidatesTags: ["Conversation"],
    }),
    /** Starts a computer-use job for a goal typed as "do: …". The daemon answers 202 with the id every "act" event on the stream carries, tagged to whichever job it belongs to; the job's own record — its plan, steps and spend — is read back with the `job` query below. Unlike ask, this opens no conversation of its own: a job is not a chat turn on the daemon's side, only something the window shows beside one. */
    startJob: build.mutation<{ id: string }, { goal: string; brain?: string }>({
      query: ({ goal, brain }) => ({ url: "/act", method: "POST", body: { goal, brain: brain ?? "" } }),
    }),
    /** One job's whole record: its plan, every step so far, what it has spent, and the question it is waiting on when stuck. Polled while the job is live, which is how the rail beside it shows the plan and the spend so far without either arriving on the event stream. */
    job: build.query<ActJob, string>({
      query: (id) => `/act/${encodeURIComponent(id)}`,
      providesTags: (_result, _error, id) => [{ type: "Job" as const, id }],
    }),
    /** Ends a running job now; the daemon answers 204. */
    stopJob: build.mutation<void, string>({
      query: (id) => ({ url: `/act/${encodeURIComponent(id)}/stop`, method: "POST" }),
    }),
    /** Pauses or resumes a job with one call, mirroring the single toggle its turn shows. */
    setJobPause: build.mutation<void, { id: string; pause: boolean }>({
      query: ({ id, pause }) => ({ url: `/act/${encodeURIComponent(id)}/${pause ? "pause" : "resume"}`, method: "POST" }),
    }),
    /** Answers the one question a stuck job asked, which is whatever the composer sends next; the daemon answers 204. */
    answerJob: build.mutation<void, { id: string; text: string }>({
      query: ({ id, text }) => ({ url: `/act/${encodeURIComponent(id)}/answer`, method: "POST", body: { text } }),
    }),
    /** The user's own open work: every task typed in, and every action item a meeting raised for the user themselves (?owner defaults to "me" when left off, which is what this sends). */
    tasks: build.query<Task[], void>({
      query: () => "/tasks",
      transformResponse: (r: { tasks: Task[] }) => r.tasks ?? [],
      providesTags: ["Task"],
    }),
    /** Every task regardless of owner. The Tasks screen is the only reader: Mine and "Watching" are both read off this one list, split by owner client-side, rather than fetched as three separate calls for "me", "them" and "unclear" — one round trip covers every bucket the owner field can hold. Tagged the same as tasks, so ticking a row from either list refetches both. */
    allTasks: build.query<Task[], void>({
      query: () => "/tasks?owner=all",
      transformResponse: (r: { tasks: Task[] }) => r.tasks ?? [],
      providesTags: ["Task"],
    }),
    /** Adds a task of the user's own and opens a conversation named after it; answers 201 with both ids. */
    createTask: build.mutation<{ id: string; conversation_id: string }, string>({
      query: (title) => ({ url: "/tasks", method: "POST", body: { title } }),
      invalidatesTags: ["Task", "Conversation"],
    }),
    /** Sets a task open, done or dropped. The daemon answers 200 with no body, and 400 for a dropped task of the user's own. The row moves the moment it is clicked and moves back if the daemon refuses, so it is never left claiming a status the store does not hold; a dropped task leaves the list altogether, which is what GET /tasks would say on the next read anyway. */
    setTaskStatus: build.mutation<void, { id: string; status: TaskStatus }>({
      query: ({ id, status }) => ({ url: `/tasks/${encodeURIComponent(id)}/done`, method: "POST", body: { status } }),
      invalidatesTags: ["Task"],
      async onQueryStarted({ id, status }, { dispatch, queryFulfilled }) {
        const move = (draft: Task[]) => {
          const at = draft.findIndex((t) => t.id === id);
          if (at < 0) return;
          if (status === "dropped") draft.splice(at, 1);
          else draft[at].done = status === "done";
        };
        // Both tasks and allTasks hold the same rows the daemon does; patching whichever of the two are actually cached (updateQueryData is a no-op on one that is not) keeps Mine and Theirs on the Tasks screen, and the Days page once it refetches, from showing three different answers to "is this done" between the click and the daemon's own reply.
        const patches = [dispatch(juneApi.util.updateQueryData("tasks", undefined, move)), dispatch(juneApi.util.updateQueryData("allTasks", undefined, move))];
        try {
          await queryFulfilled;
        } catch {
          patches.forEach((p) => p.undo());
        }
      },
    }),
    /** Takes one of the user's own tasks off the list for good. The daemon answers 204, 400 for a noticed item (which is dropped instead, since deleting it would take a line out of a meeting's minutes) and 404 for an id that names no task. The row leaves both lists the moment it is clicked and comes back if the daemon refuses. */
    deleteTask: build.mutation<void, string>({
      query: (id) => ({ url: `/tasks/${encodeURIComponent(id)}`, method: "DELETE" }),
      invalidatesTags: ["Task"],
      async onQueryStarted(id, { dispatch, queryFulfilled }) {
        const drop = (draft: Task[]) => {
          const at = draft.findIndex((t) => t.id === id);
          if (at >= 0) draft.splice(at, 1);
        };
        const patches = [dispatch(juneApi.util.updateQueryData("tasks", undefined, drop)), dispatch(juneApi.util.updateQueryData("allTasks", undefined, drop))];
        try {
          await queryFulfilled;
        } catch {
          patches.forEach((p) => p.undo());
        }
      },
    }),
    /** Corrects whose task a noticed item really is — hearing about a thing in a meeting does not make it the user's own. The daemon answers 400 for a task the user typed in, since those are always his. Invalidates Task so both the tasks and allTasks lists, and whichever day raised it, pick up the new owner on the next read. */
    setTaskOwner: build.mutation<void, { id: string; owner: TaskOwner }>({
      query: ({ id, owner }) => ({ url: `/tasks/${encodeURIComponent(id)}`, method: "PATCH", body: { owner } }),
      invalidatesTags: ["Task"],
    }),
    /** Applies one of the rail line's own Done/1h/Evening/Tomorrow buttons to a live notice, exactly as pressing it on the desktop notification would (see internal/proactive/notify.go's Act, which both call). id is "-" when the notice has none of its own (a morning brief, say) — an empty path segment is not a URL Go's own router will match, so the daemon resolves this placeholder back to "" before calling Act. title and body are the notice's own, since a snooze needs them to re-fire it later. The daemon answers 200 with no body and reports what happened back over the SSE stream as the same "notice" event a desktop press produces, which is what replaces these buttons with the line reactToNotice already draws (see store.ts) — this mutation does not touch the cache itself. 400 for an action that is none of the four; 404 when the notice names a task that no longer exists. */
    actOnNotice: build.mutation<void, { kind: string; id: string; title: string; body: string; action: string }>({
      query: ({ kind, id, title, body, action }) => ({
        url: `/notices/${encodeURIComponent(kind)}/${encodeURIComponent(id || "-")}/action`,
        method: "POST",
        body: { title, body, action },
      }),
    }),
    /** The last sixty days that have anything in them, newest first. */
    days: build.query<DaySummary[], void>({
      query: () => "/days",
      transformResponse: (r: { days: DaySummary[] }) => r.days ?? [],
      providesTags: ["Day"],
    }),
    /** One day's page, its questions and the work it raised. Also tagged "Task", the same tag setTaskStatus invalidates, so ticking a task on the Tasks screen refetches whichever day is open and its raised list moves with it — the two screens read the same rows rather than two copies that can drift apart. */
    day: build.query<DayView, string>({
      query: (date) => `/days/${encodeURIComponent(date)}`,
      providesTags: (_result, _error, date) => [{ type: "Day" as const, id: date }, "Task"],
    }),
    /** Every recording the daemon kept, newest first. */
    /** Removes one meeting's write-up; the daemon answers 204, and 404 for an id that names no meeting. The recording itself is left on disk. */
    deleteMeeting: build.mutation<void, string>({
      query: (id) => ({ url: `/meetings/${encodeURIComponent(id)}`, method: "DELETE" }),
      invalidatesTags: ["Meeting"],
    }),
    meetings: build.query<Meeting[], void>({
      query: () => "/meetings",
      transformResponse: (r: { meetings: Meeting[] }) => r.meetings ?? [],
      providesTags: ["Meeting"],
    }),
    /** The daemon's configuration and how much disk it is using. */
    settings: build.query<SettingsView, void>({
      query: () => "/settings",
      providesTags: ["Settings"],
    }),
    /** Writes one or more settings; the daemon persists them and answers with the same view GET would. Setup is read again too, since start-at-sign-in is reported there. */
    saveSettings: build.mutation<SettingsView, SettingsChange>({
      query: (body) => ({ url: "/settings", method: "POST", body }),
      invalidatesTags: ["Settings", "Setup"],
    }),
    /** The brains June can call on this machine, which one is the default, and whether none is and the daemon picks per question. */
    brains: build.query<Brains, void>({
      query: () => "/brains",
      transformResponse: brainsBody,
      providesTags: ["Brain"],
    }),
    /** Makes one brain the default and remembers the model to call it with; the daemon writes both to its config and answers with the whole list again. model "" is the brain's own default model, not a pin. default false stores the model for that brain without making it the default, which is what a model chip in Settings sends. brain AUTOMATIC_BRAIN with no model unpins whichever brain was picked and hands the choice back to the daemon's router. */
    pickBrain: build.mutation<Brains, { brain: string; model?: string; default?: boolean }>({
      query: (body) => ({ url: "/brains", method: "POST", body }),
      transformResponse: brainsBody,
      invalidatesTags: ["Brain", "Settings"],
    }),
    /** The whole voice roster and the Live models beside it: all thirty of Gemini Live's prebuilt voices, one carrying current true, and the models June can speak through, one of which also carries current true. */
    voices: build.query<Voices, void>({
      query: () => "/voices",
      transformResponse: (r: Partial<Voices>) => ({ voices: r.voices ?? [], models: r.models ?? [] }),
      providesTags: [{ type: "Voice" as const, id: "LIST" }],
    }),
    /** Sets which voice June speaks in and persists it; the daemon answers the same body GET /voices would, with the new one marked current. A session already under way keeps the voice it dialled with, so this is heard on the next one, not this one. 400 for a name that is not one of the thirty. */
    setVoice: build.mutation<Voices, string>({
      query: (name) => ({ url: "/voices", method: "POST", body: { name } }),
      transformResponse: (r: Partial<Voices>) => ({ voices: r.voices ?? [], models: r.models ?? [] }),
      invalidatesTags: [{ type: "Voice" as const, id: "LIST" }],
    }),
    /** Sets which Live model a voice session dials and persists it; the daemon answers the same body GET /voices would, with the new one marked current. Heard on the next session, same as setVoice. 400 for a name that is not one of the two. */
    setLiveModel: build.mutation<Voices, string>({
      query: (model) => ({ url: "/voices", method: "POST", body: { model } }),
      transformResponse: (r: Partial<Voices>) => ({ voices: r.voices ?? [], models: r.models ?? [] }),
      invalidatesTags: [{ type: "Voice" as const, id: "LIST" }],
    }),
    /** Speaks one fixed line out of this machine's speaker in the named voice, without changing which voice is configured — hearing a voice first is the whole point, so nothing here is invalidated. Answers {played: name}. 400 for a name that is not one of the thirty, 500 when synthesis or playback failed, 503 when this daemon has no speaker to play through. */
    previewVoice: build.mutation<{ played: string }, string>({
      query: (name) => ({ url: "/voices/preview", method: "POST", body: { name } }),
    }),
    /** What every provider has cost in tokens, which is the ledger at the foot of Settings. */
    usage: build.query<Usage, void>({
      query: () => "/usage",
      providesTags: ["Usage"],
    }),
    /** Whether the tracker is watching the screen right now. */
    tracker: build.query<TrackerStatus, void>({
      query: () => "/status",
      providesTags: ["Tracker"],
    }),
    /** Starts or stops watching the screen. Both routes answer a word of plain text rather than JSON, so the response is read as text and thrown away. */
    setCapture: build.mutation<string, boolean>({
      query: (on) => ({ url: on ? "/resume" : "/pause", method: "POST", responseHandler: "text" }),
      invalidatesTags: ["Tracker", "Settings"],
    }),
    /** Stops watching the screen for a number of minutes, after which the daemon starts again by itself; 0 pauses until resumed, the same as setCapture(false). */
    pauseFor: build.mutation<string, number>({
      query: (minutes) => ({ url: "/pause", method: "POST", body: minutes > 0 ? { minutes } : undefined, responseHandler: "text" }),
      invalidatesTags: ["Tracker", "Settings"],
    }),
    /** Every routine the user has written. */
    routines: build.query<Routine[], void>({
      query: () => "/routines",
      transformResponse: (r: { routines: Routine[] }) => r.routines ?? [],
      providesTags: ["Routine"],
    }),
    /** Adds a routine from its instruction and schedule text; answers 201 with its id, and 400 when either is blank. */
    createRoutine: build.mutation<{ id: string }, { text: string; schedule: string }>({
      query: (body) => ({ url: "/routines", method: "POST", body }),
      invalidatesTags: ["Routine"],
    }),
    /** Removes a routine; the daemon answers 204. */
    deleteRoutine: build.mutation<void, string>({
      query: (id) => ({ url: `/routines/${encodeURIComponent(id)}`, method: "DELETE" }),
      invalidatesTags: ["Routine"],
    }),
    /** Starts a routine right now instead of waiting for its schedule. The daemon answers 202 with the routine's id as soon as the run is under way and the ask itself can take minutes, so the answer is not in this response: it arrives as a notice, exactly as it does when the routine fires on its own schedule. */
    runRoutine: build.mutation<{ id: string }, string>({
      query: (id) => ({ url: `/routines/${encodeURIComponent(id)}/run`, method: "POST" }),
      invalidatesTags: ["Routine"],
    }),
    /** Opens the daemon's own microphone (see internal/ipc/dictate.go), answering the id the "dictation" event and stopDictation both carry. The daemon records from PulseAudio itself, so this never touches the browser's microphone. */
    startDictation: build.mutation<{ id: string }, void>({
      query: () => ({ url: "/dictate/start", method: "POST" }),
    }),
    /** Closes the microphone and waits for the transcript. Input: the id startDictation returned. Output: the text; or, with gone true and no text, word that the daemon has no such recording open any more — the silence gate or the two-minute cap already ended it and its words come on the event stream instead, possibly not yet, since it is still transcribing them. Neither is an error worth showing. Any other failure is left to reject, same as every other mutation here. */
    stopDictation: build.mutation<{ text: string; gone?: boolean }, string>({
      async queryFn(id, _api, _extra, baseQuery) {
        const result = await baseQuery({ url: "/dictate/stop", method: "POST", body: { id } });
        if (errorStatus(result.error) === 404) return { data: { text: "", gone: true } };
        if (result.error) return { error: result.error };
        return { data: result.data as { text: string } };
      },
    }),
    /** Opens a link in the system browser. The reply markdown's own links post here (see chat-markdown.tsx) instead of calling window.open, which a Tauri WebKitGTK webview does not reliably hand off to the real browser; the daemon runs the same xdg-open/open command its open_url tool uses. The daemon answers 204, and 400 for a url that is not http or https. */
    openUrl: build.mutation<void, string>({
      query: (href) => ({ url: "/open", method: "POST", body: { url: href } }),
    }),
    /** Whether first-run setup is finished, which is what decides whether the window opens on its setup screens or on Chats. */
    setup: build.query<SetupView, void>({
      query: () => "/setup",
      providesTags: ["Setup"],
    }),
    /** The brains with every login checked again now rather than read off the ten-minute cache, for the setup screen a person has just signed in somewhere before opening. What it reads is written into the plain brains entry too, so every brain picker in the window agrees with it at once. */
    freshBrains: build.query<Brains, void>({
      query: () => "/brains?refresh=1",
      transformResponse: brainsBody,
      providesTags: ["Brain"],
      async onQueryStarted(_arg, { dispatch, queryFulfilled }) {
        try {
          const { data } = await queryFulfilled;
          dispatch(juneApi.util.upsertQueryData("brains", undefined, data));
        } catch {
          // The plain list keeps whatever it last read.
        }
      },
    }),
    /** Checks a Gemini key with Google, writes it into June's env file and restarts the daemon. Refused 422 {"error":"invalid_key"} for a key Google turned down, 502 {"error":"unreachable"} when Google could not be asked (force true saves it unchecked), and 409 {"error":"env_var_set"} when the key comes from the computer's own environment, which a file cannot override. Every setup and component route below reads its body by content type, since a route the daemon has not grown yet answers 404 in plain text. */
    saveGeminiKey: build.mutation<KeySaved, { key: string; force?: boolean }>({
      query: (body) => ({ url: "/setup/gemini-key", method: "POST", body, responseHandler: "content-type" }),
    }),
    /** Takes the Gemini key back out of June's env file, which restarts the daemon the same way saving one does. */
    removeGeminiKey: build.mutation<KeySaved, void>({
      query: () => ({ url: "/setup/gemini-key", method: "DELETE", responseHandler: "content-type" }),
    }),
    /** Listens on the daemon's microphone for three seconds. The reply is the verdict; the meter while it listens comes off the "level" events carrying MIC_TEST_ID. */
    micTest: build.mutation<MicTest, void>({
      query: () => ({ url: "/setup/mic-test", method: "POST", responseHandler: "content-type" }),
    }),
    /** Opens the system's own microphone privacy page, the only place a blocked microphone can be allowed again. */
    openMicSettings: build.mutation<unknown, void>({
      query: () => ({ url: "/setup/mic-settings", method: "POST", responseHandler: "content-type" }),
    }),
    /** Marks setup finished, which also starts watching the screen. restarting is true when an earlier change was waiting on a restart and the daemon is now doing it; false also when one is owed but a download or a meeting would be cut short, which the daemon then carries out by itself once they are over. Nothing is invalidated here: whoever calls it decides whether the window waits for a restart first or goes straight to Chats. */
    completeSetup: build.mutation<{ restarting: boolean }, void>({
      query: () => ({ url: "/setup/complete", method: "POST", responseHandler: "content-type" }),
    }),
    /** The optional local features, what each would cost to download on this machine, and how far each has got. */
    components: build.query<Components, void>({
      query: () => "/components",
      transformResponse: (r: Partial<Components>) => ({
        platform: r.platform ?? { os: "", vulkan: false, free_bytes: 0, ram_mb: 0 },
        features: r.features ?? [],
        restart_pending: r.restart_pending ?? false,
      }),
      providesTags: ["Component"],
    }),
    /** Queues one feature's download; the daemon answers 202 and reports progress as "component" events. 507 when the disk is too full for it. */
    installFeature: build.mutation<unknown, string>({
      query: (id) => ({ url: `/components/${encodeURIComponent(id)}/install`, method: "POST", body: { variant: "auto" }, responseHandler: "content-type" }),
      invalidatesTags: ["Component"],
    }),
    /** Stops a queued or running download; the part already fetched is kept so a later Set up resumes it. */
    cancelFeature: build.mutation<unknown, string>({
      query: (id) => ({ url: `/components/${encodeURIComponent(id)}/cancel`, method: "POST", responseHandler: "content-type" }),
      invalidatesTags: ["Component"],
    }),
    /** Deletes a feature June installed. 409 while a meeting or a dictation is using its files. */
    removeFeature: build.mutation<{ restart_pending: boolean }, string>({
      query: (id) => ({ url: `/components/${encodeURIComponent(id)}`, method: "DELETE", responseHandler: "content-type" }),
      invalidatesTags: ["Component", "Settings", "Setup"],
    }),
    /** Whether a newer June is out, read off the daemon's daily check. */
    update: build.query<UpdateView, void>({
      query: () => "/update",
      providesTags: ["Update"],
    }),
    /** Asks GitHub now rather than waiting for the daily check, for the Check now button; what it finds becomes what GET /update says everywhere in the window. A check that could not reach GitHub comes back as state "failed" with the reason in error. */
    checkUpdate: build.mutation<UpdateView, void>({
      query: () => "/update?refresh=1",
      async onQueryStarted(_arg, { dispatch, queryFulfilled }) {
        try {
          const { data } = await queryFulfilled;
          dispatch(juneApi.util.upsertQueryData("update", undefined, data));
        } catch {
          // GET /update keeps what it last read.
        }
      },
    }),
    /** Downloads the newest installer, checks it against the release's SHA256SUMS and runs it, which closes June and opens the new one. Answers 202 with what GET /update now says, state "downloading"; the steps come as "update" events. 409 {"error":"recording"|"processing"|"downloading","message":…} while quitting June would cut a recording, a write-up or a download short. The 202's body goes straight into GET /update's cache, since the first event only comes once the installer's download has begun, and until then the strip would read "is out" again. */
    installUpdate: build.mutation<unknown, void>({
      query: () => ({ url: "/update", method: "POST", responseHandler: "content-type" }),
      async onQueryStarted(_arg, { dispatch, queryFulfilled }) {
        try {
          const { data } = await queryFulfilled;
          if (data && typeof data === "object" && "state" in data) dispatch(juneApi.util.upsertQueryData("update", undefined, data as UpdateView));
          else dispatch(juneApi.util.invalidateTags(["Update"]));
        } catch {
          // Refused: the strip says why, off the refusal itself.
        }
      },
    }),
    /** Restarts the daemon, and with it this window, so a change that only takes effect at start does. Refused 409 {"error":"recording"|"processing"|"downloading","message":…} while a restart would cut one of those short, and 500 {"error":"restart_failed","message":…} when the replacement could not be started, June still running. Asked again while a restart is under way it answers 202 and only makes the replacement show its window. */
    restartJune: build.mutation<unknown, void>({
      query: () => ({ url: "/restart", method: "POST", responseHandler: "content-type" }),
    }),
  }),
});

export const {
  useConversationsQuery,
  useConversationQuery,
  useCreateConversationMutation,
  useDeleteConversationMutation,
  useRenameConversationMutation,
  useAskMutation,
  useStartJobMutation,
  useJobQuery,
  useStopJobMutation,
  useSetJobPauseMutation,
  useAnswerJobMutation,
  useTasksQuery,
  useAllTasksQuery,
  useCreateTaskMutation,
  useSetTaskStatusMutation,
  useDeleteTaskMutation,
  useSetTaskOwnerMutation,
  useActOnNoticeMutation,
  useDaysQuery,
  useDayQuery,
  useDeleteMeetingMutation,
  useMeetingsQuery,
  useSettingsQuery,
  useSaveSettingsMutation,
  useBrainsQuery,
  usePickBrainMutation,
  useVoicesQuery,
  useSetVoiceMutation,
  useSetLiveModelMutation,
  usePreviewVoiceMutation,
  useUsageQuery,
  useTrackerQuery,
  useSetCaptureMutation,
  usePauseForMutation,
  useRoutinesQuery,
  useCreateRoutineMutation,
  useDeleteRoutineMutation,
  useRunRoutineMutation,
  useStartDictationMutation,
  useStopDictationMutation,
  useOpenUrlMutation,
  useSetupQuery,
  useFreshBrainsQuery,
  useSaveGeminiKeyMutation,
  useRemoveGeminiKeyMutation,
  useMicTestMutation,
  useOpenMicSettingsMutation,
  useComponentsQuery,
  useInstallFeatureMutation,
  useCancelFeatureMutation,
  useRemoveFeatureMutation,
  useUpdateQuery,
  useCheckUpdateMutation,
  useInstallUpdateMutation,
  useRestartJuneMutation,
} = juneApi;

/** Opens the daemon's SSE stream for this window (see openStream in shared/wire.ts). Input: a callback for each event, and a callback for the stream opening again after it had dropped. Output: a stop function. The first connect reads the token this window already holds; a reconnect reads it again, because a dropped stream is also how a restarted daemon shows itself. */
export function events(onEvent: (ev: DaemonEvent) => void, onReopen?: () => void): () => void {
  return openStream(base, (retry) => (retry ? refreshToken() : ensureToken()), onEvent, onReopen);
}
