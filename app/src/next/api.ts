/** The React window's whole connection to the local daemon at http://127.0.0.1:6942: the IPC token, the RTK Query API over the daemon's routes, and the SSE stream. The token is read here and nowhere else, so every request in the window authenticates the same way and there is one place to fix when the daemon changes its secret. Deliberately independent of src/daemon.ts, which belongs to the old pages and goes when they go. */

import { createApi, fetchBaseQuery, type BaseQueryFn, type FetchArgs, type FetchBaseQueryError } from "@reduxjs/toolkit/query/react";
import { invoke } from "@tauri-apps/api/core";

/** Where the daemon listens. */
const base = "http://127.0.0.1:6942";

/** The header the daemon authenticates every call by (ipctoken.HeaderName on the Go side). */
const TOKEN_HEADER = "X-Ora-Token";

/** The port the Vite dev server runs on, which is the only origin allowed to take a token out of the page's own URL. */
const DEV_PORT = "1420";

/** The shared IPC secret, held in memory only and never logged. Undefined until refreshToken has read one. */
let token: string | undefined;

/** The IPC token a page may take from its own URL, which only the Vite dev server's origin may do. Input: the page's port and query string. Output: the value of ?token=, or undefined on any other origin or when there is none. The packaged window is served from tauri://localhost with no port, so this is never a way into the real window. */
export function devToken(loc: { port: string; search: string }): string | undefined {
  if (loc.port !== DEV_PORT) return undefined;
  return new URLSearchParams(loc.search).get("token") ?? undefined;
}

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

/** The query every endpoint below runs through: the plain one, plus a single retry with a freshly read token when the daemon answers 401. Input and output are RTK Query's own — the endpoint's arguments in, the parsed body or an error out. A 401 means the daemon restarted and wrote a new token since this window read one, which is the ordinary case after the daemon relaunches the window, so it is worth exactly one silent retry and not an error on screen. */
const baseQueryWithFreshToken: BaseQueryFn<string | FetchArgs, unknown, FetchBaseQueryError> = async (args, api, extra) => {
  let result = await rawBaseQuery(args, api, extra);
  if (result.error?.status === 401) {
    await refreshToken();
    result = await rawBaseQuery(args, api, extra);
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

/** One thing Ora read to answer with, carried both on a stored turn and on the answer event. Mirrors ipc.EvidenceItem. */
export type Evidence = { title: string; meta: string; body: string };

/** One thing said in a conversation. Mirrors ipc.TurnView. Role is "you" or "ora"; kind is "ask", "dictation", "voice" or "error"; reason is set only for an error turn. */
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

/** What POST /tasks/{id}/done may set a task to. "dropped" is only for a task Ora noticed: the daemon answers 400 for a dropped task of the user's own, because user_tasks has no third state to hold it in. */
export type TaskStatus = "open" | "done" | "dropped";

/** One thing to do on GET /tasks. Mirrors ipc.Task; source is "you" for a task the user typed in and "noticed" for an action item a meeting raised. detail is where it came from: the meeting it was raised in and the date, for example "TCFD statement pattern analysis, 2026-09-01", or "you said" for one the user typed in. GET /tasks itself takes ?owner=me|them|unclear|all (default "me"), so which of these a given fetch returns depends on how it was called, not on anything in the row itself. */
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

/** SettingsView's "first_run" field on GET /settings. Mirrors ipc.FirstRunView: which of the four ways Ora can answer text are set up on this machine, and the plain one-line steps to fix that — which the daemon fills in only while none of the four works, so an empty steps list means there is nothing left to set up and the window shows no panel. */
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
};

/** One allowance window a brain's provider reports for the user's own account: a five-hour or weekly subscription window, a daily request ceiling. Mirrors internal/agent.UsageLimit (aliased as brain.UsageLimit). used_fraction is 0 to 1; resets_at is RFC3339. */
export type UsageLimit = { window: string; used_fraction: number; resets_at: string; source: string };

/** One backend that can answer for Ora on GET /brains. Mirrors ipc.BrainView; model is the one last picked for this brain, "" when none ever was. limits and limits_at are optional so a daemon older than the field still parses; a brain with no allowance data reported sends limits as an empty list rather than leaving it out. */
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

/** What one provider has cost in tokens over a window. Mirrors ipc.ProviderTotal. */
export type UsageProvider = { provider: string; calls: number; input_tokens: number; output_tokens: number; total_tokens: number };

/** The same for one model of one provider. Mirrors ipc.ModelTotal. */
export type UsageModel = UsageProvider & { model: string };

/** One window of the ledger: a row per provider and a row per model. Mirrors ipc.UsageWindow. */
export type UsageWindow = { providers: UsageProvider[]; models: UsageModel[] };

/** One day of the week's bars. Mirrors ipc.UsageDay. */
export type UsageDay = { day: string; calls: number; total_tokens: number };

/** One finished call in the log. Mirrors ipc.UsageCall; channel says which part of Ora made it — "text", "voice", "dream", "eval" or "subtask". */
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

/** GET /usage: what each provider has cost in tokens today and over the last seven days, a figure per day of that week, and the calls themselves as a log. Every number is a count the daemon read from its own ledger; nothing here is a price. budget_used_fraction is how much of the plan's own allowance has gone, 0 to 1, and is optional because the daemon does not report it yet. */
export type Usage = { today: UsageWindow; week: UsageWindow; days: UsageDay[]; recent: UsageCall[]; budget_used_fraction?: number };

/** GET /status: whether the tracker is paused right now. POST /pause and POST /resume are what change it. */
export type TrackerStatus = { paused: boolean };

/** One tool call's step inside a computer-use job. Mirrors actjob.Step; outcome is "pass", "fail", or "" before wait_for has checked it, and expect is the change the step was written down to produce. */
export type ActStep = {
  n: number;
  tool: string;
  expect: { kind: string; value: string };
  result: string;
  outcome: string;
  why: string;
};

/** One round's tokens, filed under the model that served it. Mirrors actjob.Usage. */
export type ModelUsage = { model: string; input: number; cached: number; output: number };

/** What a whole job has cost: the rounds it took, the tokens on each side, and the same counts again per model, so two brains can be compared on the same task. Mirrors actjob.Spend. */
export type Spend = { rounds: number; input: number; cached: number; output: number; by_model?: Record<string, ModelUsage> };

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

/** Every daemon route the window reads or writes, as one RTK Query API. The daemon wraps its lists in an object named after the list ("conversations", "tasks", "days", "meetings", "brains"), so each list endpoint unwraps that here and components get a plain array. */
export const oraApi = createApi({
  reducerPath: "ora",
  baseQuery: baseQueryWithFreshToken,
  tagTypes: ["Conversation", "Task", "Day", "Meeting", "Settings", "Brain", "Usage", "Tracker", "Routine", "Job"],
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
    /** Every task regardless of owner. The Tasks screen is the only reader: Mine and "Theirs, watching" are both read off this one list, split by owner client-side, rather than fetched as three separate calls for "me", "them" and "unclear" — one round trip covers every bucket the owner field can hold. Tagged the same as tasks, so ticking a row from either list refetches both. */
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
        const patches = [dispatch(oraApi.util.updateQueryData("tasks", undefined, move)), dispatch(oraApi.util.updateQueryData("allTasks", undefined, move))];
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
    actOnNotice: build.mutation<void, { kind: string; id: string; title: string; body: string; action: "done" | "hour" | "evening" | "tomorrow" }>({
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
    /** Turns the Claude row's undocumented usage-endpoint read on or off; the daemon writes it to config and answers with the same view GET would. */
    setClaudeUsageFromLogin: build.mutation<SettingsView, boolean>({
      query: (claude_usage_from_login) => ({ url: "/settings", method: "POST", body: { claude_usage_from_login } }),
      invalidatesTags: ["Settings"],
    }),
    /** The brains Ora can call on this machine and which one is the default. */
    brains: build.query<Brain[], void>({
      query: () => "/brains",
      transformResponse: (r: { brains: Brain[] }) => r.brains ?? [],
      providesTags: ["Brain"],
    }),
    /** Makes one brain the default and remembers the model to call it with; the daemon writes both to its config and answers with the whole list again. */
    pickBrain: build.mutation<Brain[], { brain: string; model: string }>({
      query: (body) => ({ url: "/brains", method: "POST", body }),
      transformResponse: (r: { brains: Brain[] }) => r.brains ?? [],
      invalidatesTags: ["Brain", "Settings"],
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
    /** Closes the microphone and waits for the transcript. Input: the id startDictation returned. Output: the text, "" when the daemon has no such recording open any more — the silence gate or the two-minute cap already ended it and sent the text on the event stream instead, which is not an error worth showing. Any other failure is left to reject, same as every other mutation here. */
    stopDictation: build.mutation<{ text: string }, string>({
      async queryFn(id, _api, _extra, baseQuery) {
        const result = await baseQuery({ url: "/dictate/stop", method: "POST", body: { id } });
        // The daemon writes that 404 with http.Error, whose body is plain text: fetchBaseQuery cannot read it as JSON and reports it as a PARSING_ERROR carrying the real code, so the code is read off whichever of the two fields is holding it.
        const code = result.error && (result.error.status === "PARSING_ERROR" ? result.error.originalStatus : result.error.status);
        if (code === 404) return { data: { text: "" } };
        if (result.error) return { error: result.error };
        return { data: result.data as { text: string } };
      },
    }),
    /** Opens a link in the system browser. The reply markdown's own links post here (see chat-markdown.tsx) instead of calling window.open, which a Tauri WebKitGTK webview does not reliably hand off to the real browser; the daemon runs the same xdg-open/open command its open_url tool uses. The daemon answers 204, and 400 for a url that is not http or https. */
    openUrl: build.mutation<void, string>({
      query: (href) => ({ url: "/open", method: "POST", body: { url: href } }),
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
  useSetTaskOwnerMutation,
  useActOnNoticeMutation,
  useDaysQuery,
  useDayQuery,
  useMeetingsQuery,
  useSettingsQuery,
  useSetClaudeUsageFromLoginMutation,
  useBrainsQuery,
  usePickBrainMutation,
  useUsageQuery,
  useTrackerQuery,
  useSetCaptureMutation,
  useRoutinesQuery,
  useCreateRoutineMutation,
  useDeleteRoutineMutation,
  useRunRoutineMutation,
  useStartDictationMutation,
  useStopDictationMutation,
  useOpenUrlMutation,
} = oraApi;

/** One of Ora's own moments, sent by the daemon rather than asked for: the morning brief, the evening close, a meeting prep, a task or routine raised on its own. body is the routine's or task's own text for those two kinds (see internal/proactive/routine.go and proactive.go); kind is "task", "routine", "brief", "close", "meeting" or "note", and id is the row's own id. action and until are empty on a notice arriving fresh, and set once the user has pressed Done or a snooze button on the desktop notification it was also posted as: action is "snoozed" or "done", and until is the RFC 3339 moment a snoozed notice comes back. Only the hover window draws title and place (see internal/ipc/notice.go); this window reacts to action and until alone, through noticeActionMessage in format.ts. */
export type Notice = {
  title: string;
  body: string;
  place: string;
  id: string;
  kind: string;
  action?: string;
  until?: string;
};

/** One message off the daemon's SSE stream. The first five belong to an ask; "dictation" carries a finished transcript, "heard", "said", "state" and "level" belong to a live voice session, "notice" is Ora speaking first, "act" is one line of a computer-use job's progress, and "overlay" and "window" are the daemon telling the on-screen accessories and the window itself what to do. This window draws none of the last five, but they do arrive on the same stream, so they are named here rather than left to widen the type at the point of use. id is the ask's own id, or for "act" the job's id, which is how a message is tied to the thing that caused it — only the "answer" message carries a conversation_id. detail is the one-line summary a tool step reports about what it did, or for "act" the whole actjob.Event as JSON (kind, state, expect, outcome, spend), and evidence is what the answer was drawn from. notice is only carried on a "notice" event. */
export type DaemonEvent = {
  id: string;
  type: "status" | "tool" | "answer" | "done" | "error" | "dictation" | "heard" | "said" | "state" | "level" | "act" | "notice" | "overlay" | "window";
  text?: string;
  detail?: string;
  evidence?: Evidence[];
  conversation_id?: string;
  notice?: Notice;
};

/** Opens the daemon's SSE stream and forwards each parsed message to onEvent, reconnecting two seconds after a drop. Input: a callback for each event, and a callback for the stream opening again after it had dropped, which is the only signal this window gets that a daemon it had lost is answering again. Output: a stop function that closes the stream for good. The token goes in the query string because an EventSource cannot set headers, which is why the daemon accepts it there as well (see requireIPCToken in cmd/ipc.go). */
/** How long a dropped stream waits before reconnecting, in milliseconds, and how much random extra is added on top. */
const RETRY_MS = 2000;
const RETRY_JITTER_MS = 1000;

export function events(onEvent: (ev: DaemonEvent) => void, onReopen?: () => void): () => void {
  let stopped = false;
  let source: EventSource | undefined;
  // Whether the stream has failed since it was last open. Set on every error, including each failed retry, so it is still true whenever a later attempt finally succeeds.
  let dropped = false;

  async function connect(): Promise<void> {
    if (stopped) return;
    const t = await ensureToken();
    if (stopped) return;
    const url = t ? `${base}/events?token=${encodeURIComponent(t)}` : `${base}/events`;
    source = new EventSource(url);
    source.onopen = () => {
      // Only a stream that had dropped is a recovery worth telling the cache about; the first open of the window's life is not, since every query is already fetching by then. Cleared here rather than in the retry, so a second daemon restart later in the session is caught the same way this one was.
      if (!dropped) return;
      dropped = false;
      onReopen?.();
    };
    source.onmessage = (e: MessageEvent) => {
      try {
        onEvent(JSON.parse(e.data) as DaemonEvent);
      } catch {
        /* malformed message, ignore */
      }
    };
    source.onerror = () => {
      source?.close();
      dropped = true;
      // A dropped stream is also how a restarted daemon shows itself, so the token is read again on the way back in. Up to a second of jitter is added because every open window drops at the same instant when the daemon dies, and without it they all reconnect and refetch on the same tick for as long as it flaps.
      if (!stopped) setTimeout(() => void refreshToken().then(connect), RETRY_MS + Math.random() * RETRY_JITTER_MS);
    };
  }
  void connect();

  return () => {
    stopped = true;
    source?.close();
  };
}
