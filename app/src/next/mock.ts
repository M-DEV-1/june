/** The fake daemon: one fetch function that answers every route api.ts calls, out of a plain object of canned answers. It is imported by the tests in this folder through testing.tsx, and by the ?mock=1 browser mode below, so a screen and its test and a screenshot all read the same fixtures. Nothing here imports vitest or React.
 */

import type {
  ActJob,
  Brain,
  Components,
  ConversationSummary,
  ConversationView,
  DaySummary,
  DayView,
  Feature,
  LiveModel,
  Meeting,
  MicTest,
  Notice,
  Routine,
  SettingsView,
  SetupView,
  Task,
  UpdateView,
  Usage,
  Voice,
} from "./api";
import type { DaemonEvent } from "../shared/wire";
import { isoDay } from "./format";

/** What the fake daemon holds. Anything left out answers as an empty list or an empty object, which is what a daemon with nothing recorded would say. */
export type Canned = {
  conversations?: ConversationSummary[];
  /** One entry per conversation id, the answer to GET /conversations/{id}. */
  turns?: Record<string, ConversationView>;
  tasks?: Task[];
  days?: DaySummary[];
  /** One entry per date, the answer to GET /days/{date}. */
  pages?: Record<string, DayView>;
  meetings?: Meeting[];
  brains?: Brain[];
  /** Whether GET /brains starts out saying no brain is pinned and the router picks per question. */
  automatic?: boolean;
  voices?: Voice[];
  /** The Live models GET /voices reports beside the voice roster, one carrying current true. */
  models?: LiveModel[];
  settings?: Partial<SettingsView>;
  usage?: Usage;
  paused?: boolean;
  routines?: Routine[];
  /** One entry per job id, the answer to GET /act/{id}. */
  jobs?: Record<string, ActJob>;
  /** A job already under way when the page opens, for ?mock=1 to show a running job without a click: the conversation it is shown beside, its goal, and the steps it has taken so far. Read by installMock alone — daemonFetch answers GET /act/{id} from `jobs` regardless of this, and a test that wants a live job in the store dispatches progress.jobSent/jobAccepted/eventArrived itself, the same way it does for a live ask. */
  runningJob?: { id: string; conversationId: string; goal: string; steps: { text: string; expect?: string; outcome?: "pass" | "fail"; why?: string }[] };
  /** A notice already dealt with from its own desktop notification when the page opens, for ?mock=1 to show the rail line it turns into (see noticeActionMessage in format.ts and reactToNotice in store.ts) without a click. title and place are the hover window's own concern and left out here since this window never reads them. */
  notice?: Pick<Notice, "body" | "id" | "kind" | "action" | "until">;
  /** What POST /dictate/stop answers with once a recording is stopped. Defaults to a fixed sentence, same as a daemon that transcribed something. */
  dictateText?: string;
  /** What GET /setup starts out saying. Left out, setup reads as long finished, so every other fixture opens on Chats the way it always has. */
  setup?: Partial<SetupView>;
  /** What GET /components starts out saying; left out, the route answers 404, as a daemon without local features does. */
  components?: Components;
  /** What GET /update starts out saying; left out, the route answers 404. */
  update?: UpdateView;
  /** Whether POST /update fails partway, the way a download that does not match SHA256SUMS does. */
  updateFails?: boolean;
  /** What POST /setup/mic-test answers, and after how many milliseconds. */
  mic?: MicTest;
  micMs?: number;
  /** How long the fake daemon stays unreachable after anything that restarts it, in milliseconds. */
  restartMs?: number;
  /** How long the old daemon keeps answering once a restart has begun, as the real one does through its shutdown (closing a recording, flushing the last activity), in milliseconds. */
  stopMs?: number;
  /** Whether every restart fails to start its replacement, leaving the old daemon running, as a program held by antivirus does. */
  restartFails?: boolean;
  /** Whether POST /setup/complete fails to save, as a locked or unwritable june-config.json does. */
  completeFails?: boolean;
  /** Whether no newer release is known when the page opens, and the daily check finds one a few seconds later. */
  updateLater?: boolean;
  /** Whether a meeting is being recorded, which the routes that restart June refuse for, and which the browser mode's stream says the moment it opens. */
  recording?: boolean;
  /** The routes that should fail, each written "METHOD /path", so a test can see what the window says when a write does not go through. */
  fails?: string[];
};

/** One call the window made, as the fake daemon saw it. */
export type Call = { method: string; path: string; body?: unknown };

/** The settings a screen can read without the test having to fill in every field. */
const emptySettings: SettingsView = {
  data_dir: "/home/you/.june",
  store_bytes: 0,
  recordings_bytes: 0,
  models_bytes: 0,
  voice_model: "",
  brain: "",
  embed_model: "none",
  meetings_enabled: false,
  capture_enabled: true,
  keep_audio_days: -1,
  daemon_started: "",
  version: "dev",
  hotkey: "",
  claude_usage_from_login: true,
  update_check: true,
  allow_fallback: true,
  meetings_offer: "ask",
};

/** What GET /setup says when a fixture names nothing: setup long finished. */
const finishedSetup: SetupView = {
  done: true,
  version: "dev",
  gemini_key: false,
  brain_ready: true,
  default_brain: "",
  autostart: true,
  hotkey: "Ctrl+Alt+Space",
  restart_pending: false,
  data_dir: "/home/you/.june",
};

/** The file each feature's progress names while it downloads, the largest one it fetches, as the real downloader reports it. */
const FEATURE_FILE: Record<string, string> = {
  transcribe: "ggml-medium.bin",
  speakers: "wespeaker_en_voxceleb_CAM++.onnx",
  memory: "embeddinggemma-300M-Q8_0.gguf",
  summaries: "gemma-4-E2B-it-Q4_0.gguf",
};

/** How often the fake daemon sends a download's or an update's progress, and in how many steps a download completes. */
const MOCK_TICK_MS = 250;
const MOCK_TICKS = 28;

/** A fake daemon as one fetch function. Input: what it should answer with, the list to record every call in — the tests read that list to check a click wrote what it should have, and the browser mode passes one it ignores — and, for the browser mode, where to send the events a real daemon would put on its stream (a download's progress, the microphone test's level, an update's steps). Output: a function with fetch's own signature, to be put in fetch's place.
 *
 * Nothing here imports vitest, so the same fake serves the test suite and the ?mock=1 page a plain browser tab opens. Anything that restarts the real daemon — a Gemini key, finishing setup with a restart pending, POST /restart, an update — has this one announce it on the stream, keep answering for stopMs as the real one does through its shutdown, and then go unreachable for restartMs, so the window's own wait for a restart runs against it exactly as it would against the real one.
 */
export function daemonFetch(canned: Canned = {}, calls: Call[] = [], emit?: (ev: DaemonEvent) => void): typeof fetch {
  const fails = new Set(canned.fails ?? []);
  let setupState: SetupView = { ...finishedSetup, ...(canned.setup ?? {}) };
  let brains = [...(canned.brains ?? [])];
  const components: Components | undefined = canned.components ? { ...canned.components, features: canned.components.features.map((f) => ({ ...f })) } : undefined;
  // With updateLater the daily check has not found the release yet; it does a few seconds in (see the timer below).
  let update: UpdateView | undefined = canned.update ? (canned.updateLater ? { ...canned.update, latest: "", available: false, release_url: "", can_install: false } : { ...canned.update }) : undefined;
  // A restart runs in two parts, as the real one does: the old daemon still answering through its shutdown until stoppingUntil, then nothing answering until downUntil.
  let stoppingUntil = 0;
  let downUntil = 0;
  const stopping = () => Date.now() < stoppingUntil;
  const recording = canned.recording ?? false;
  // One download at a time, the way the real downloader runs them.
  const queue: string[] = [];
  let running: { id: string; timer: ReturnType<typeof setInterval> } | undefined;
  // How far each download had got when a restart cut it off, in ticks, so it carries on from there the way the real downloader resumes a partial file.
  const partial: Record<string, number> = {};
  // A restart POST /setup/complete owed but put off while something downloads, carried out once the queue is empty.
  let restartWhenIdle = false;
  const busy = () => Boolean(running) || queue.length > 0;

  /** Restarts the fake daemon, announcing it on the stream the way announce in cmd/lifecycle.go does. Input: whether the replacement shows its window — true when a request the person made set the restart off, false for the one setup put off until a download ended — and how long nothing answers. Output: false when the restart failed (restartFails), with the daemon still running and "restart_failed" announced instead.
   * The old daemon answers for stopMs more and then goes for restartMs, and comes back as a restarted one: nothing waiting on a restart any more, and the download queue taken up again, since the real downloader keeps it in state.json and resumes it from the partial files (resume in internal/components). */
  const goDown = (reopen: boolean, ms = canned.restartMs ?? 2500): boolean => {
    if (canned.restartFails) {
      emit?.({ id: "june", type: "lifecycle", text: "restart_failed", detail: JSON.stringify({ error: "the replacement daemon could not be started: Access is denied." }), failed: true });
      return false;
    }
    emit?.({ id: "june", type: "lifecycle", text: "restarting", detail: JSON.stringify({ reopen }), failed: false });
    comeBackAfter(ms);
    return true;
  };

  /** Stops the fake daemon and brings it back as a new one, with nothing announced: what a restart does once under way, and what the installer's quit does on its own. Input: how long nothing answers. */
  const comeBackAfter = (ms: number) => {
    stoppingUntil = Date.now() + (canned.stopMs ?? 600);
    downUntil = stoppingUntil + ms;
    setupState = { ...setupState, restart_pending: false };
    restartWhenIdle = false;
    if (running) {
      clearInterval(running.timer);
      queue.unshift(running.id);
      running = undefined;
    }
    if (!components) return;
    components.restart_pending = false;
    for (const f of components.features) if (f.state === "installing") f.state = "queued";
    if (queue.length) setTimeout(runQueue, downUntil - Date.now() + 50);
  };

  /** The refusal the routes that restart June give while a restart would cut something short, as internal/ipc/setup.go words it, or undefined when nothing would. Input: what the route does, as the start of the sentence. */
  const restartRefusal = (doing: string) => {
    if (recording) return { status: 409, body: { error: "recording", message: `${doing} restarts June, which would end the meeting recording, and the rest of the call would not be recorded. Try again once the meeting is over.` } };
    if (busy()) return { status: 409, body: { error: "downloading", message: `${doing} restarts June, which would stop the local features downloading now. Try again once they finish, or cancel the download first.` } };
    return undefined;
  };

  // The daily check finding the release, which the real updater announces as an "update" event with text "available" and the version as its id (internal/update/check.go).
  if (canned.update && canned.updateLater && emit) {
    const found = canned.update;
    setTimeout(() => {
      update = { ...found };
      emit({ id: found.latest, type: "update", text: "available", detail: "", failed: false });
    }, 4000);
  }

  /** Starts the next queued download, playing its progress onto the stream as the real downloader would. Without a stream to play onto — the test suite — a queued feature simply stays queued. */
  const runQueue = () => {
    if (!components || running || !emit) return;
    const id = queue.shift();
    if (!id) {
      // Nobody asked for this restart just now, so its replacement comes up hidden, as restartWhenFree's does.
      if (restartWhenIdle) goDown(false);
      return;
    }
    const f = components.features.find((x) => x.id === id);
    if (!f) return runQueue();
    f.state = "installing";
    const send = (text: string, detail = {}) => emit({ id, type: "component", text, detail: JSON.stringify(detail), failed: false });
    let i = partial[id] ?? 0;
    const timer = setInterval(() => {
      i++;
      if (i <= MOCK_TICKS) partial[id] = i;
      const total = f.download_bytes;
      if (i <= MOCK_TICKS) send("downloading", { file: FEATURE_FILE[id] ?? id, done: Math.round((total * i) / MOCK_TICKS), total, bps: Math.round(((total / MOCK_TICKS) * 1000) / MOCK_TICK_MS) });
      else if (i === MOCK_TICKS + 3) send("verifying");
      else if (i === MOCK_TICKS + 6) send("extracting");
      else if (i === MOCK_TICKS + 9) send("testing");
      else if (i === MOCK_TICKS + 12) {
        clearInterval(timer);
        running = undefined;
        delete partial[id];
        f.state = "installed";
        if (f.needs_restart) {
          components.restart_pending = true;
          setupState = { ...setupState, restart_pending: true };
        }
        send("installed");
        runQueue();
      }
    }, MOCK_TICK_MS);
    running = { id, timer };
  };

  /** Plays an update's download, check and install onto the stream, ending in the fake daemon going away and coming back as the new version — or, with updateFails, in the failure the real updater reports for a download that does not match SHA256SUMS. */
  const runUpdate = () => {
    if (!update || !emit) return;
    const u = update;
    const total = 26_214_400;
    let i = 0;
    update = { ...u, state: "downloading", error: "" };
    const timer = setInterval(() => {
      i++;
      if (i <= 16) emit({ id: "update", type: "update", text: "downloading", detail: JSON.stringify({ done: Math.round((total * i) / 16), total }) });
      else if (i === 18) {
        if (canned.updateFails) {
          clearInterval(timer);
          update = { ...u, state: "failed", error: "the download did not match its checksum" };
          emit({ id: "update", type: "update", text: "failed", detail: "{}", failed: true });
          return;
        }
        update = { ...u, state: "ready" };
        emit({ id: "update", type: "update", text: "verifying", detail: "{}" });
      } else if (i === 21) {
        clearInterval(timer);
        emit({ id: "update", type: "update", text: "installing", detail: "{}" });
        update = { ...u, current: u.latest, available: false, state: "idle" };
        setupState = { ...setupState, version: u.latest };
        comeBackAfter(4000);
      }
    }, MOCK_TICK_MS);
  };

  /** Plays the microphone test's levels onto the stream for as long as it listens: speech-shaped when the fixture says the test heard something, a near-silent floor when it did not. */
  const playLevels = (heard: boolean, ms: number) => {
    if (!emit) return;
    let i = 0;
    const timer = setInterval(() => {
      i++;
      const mic = heard ? 0.04 + 0.4 * Math.abs(Math.sin(i / 2.3)) * Math.abs(Math.sin(i / 7.1)) : 0.003;
      emit({ id: "mic-test", type: "level", detail: JSON.stringify({ mic }) });
    }, 60);
    setTimeout(() => clearInterval(timer), ms);
  };
  // Both lists are the daemon's own copies: a delete takes a row out, a new chat puts one in, and a status change moves a task, so the next read answers what the write left behind rather than what the test first handed over.
  let conversations = [...(canned.conversations ?? [])];
  let tasks = [...(canned.tasks ?? [])];
  let routines = [...(canned.routines ?? [])];
  let nextRoutineID = routines.length + 1;
  // What a tick on either screen last set a task to, kept apart from `tasks` itself because a dropped item falls out of that list — GET /tasks would not return it either — but a day that raised it still needs to say so.
  const statusById = new Map<string, { done: boolean; status: string }>();
  // Settings the mock daemon holds in place: a POST /settings changes this, and the next GET /settings sees it, the same way the task lists above hold what a write left behind.
  let settingsState: Partial<SettingsView> = { ...(canned.settings ?? {}) };
  // Pausing and resuming change what GET /status says, the way they do on the daemon, so the rail's pause control and the strip across the top can be seen to work.
  let paused = canned.paused ?? false;
  let pausedUntil = "";
  // Whether no brain is pinned, which POST /brains changes and GET /brains reads back.
  let automatic = canned.automatic ?? false;

  const answer = (method: string, path: string, body: unknown, params: URLSearchParams): { status: number; body: unknown } => {
    const conversation = /^\/conversations\/([^/]+)$/.exec(path);
    const title = /^\/conversations\/([^/]+)\/title$/.exec(path);
    const day = /^\/days\/(.+)$/.exec(path);
    const done = /^\/tasks\/([^/]+)\/done$/.exec(path);
    const ownerPatch = /^\/tasks\/([^/]+)$/.exec(path);
    const routineRun = /^\/routines\/([^/]+)\/run$/.exec(path);
    const noticeAction = /^\/notices\/[^/]+\/[^/]+\/action$/.exec(path);
    const routineID = /^\/routines\/([^/]+)$/.exec(path);
    const jobID = /^\/act\/([^/]+)$/.exec(path);
    const jobStop = /^\/act\/([^/]+)\/stop$/.exec(path);
    const jobPause = /^\/act\/([^/]+)\/pause$/.exec(path);
    const jobResume = /^\/act\/([^/]+)\/resume$/.exec(path);
    const jobAnswer = /^\/act\/([^/]+)\/answer$/.exec(path);
    if (method === "GET" && path === "/conversations") return { status: 200, body: { conversations } };
    if (method === "GET" && conversation) return { status: 200, body: canned.turns?.[decodeURIComponent(conversation[1])] ?? { id: conversation[1], title: "", brain: "", turns: [] } };
    if (method === "POST" && path === "/conversations") {
      conversations = [{ id: "new", title: "New conversation", brain: "", last: "", updated: new Date().toISOString() }, ...conversations];
      return { status: 201, body: { id: "new" } };
    }
    if (method === "DELETE" && conversation) {
      conversations = conversations.filter((c) => c.id !== decodeURIComponent(conversation[1]));
      return { status: 204, body: null };
    }
    if (method === "POST" && title) return { status: 204, body: null };
    if (method === "POST" && path === "/ask") {
      // A question naming no conversation is what a fresh draft's first message sends; the daemon opens one of its own, exactly as POST /conversations does, and every later question in it names the id back.
      const cid = typeof body === "object" && body !== null && "conversation_id" in body ? String((body as { conversation_id: unknown }).conversation_id ?? "") : "";
      if (cid) return { status: 202, body: { id: "ask-1", conversation_id: cid } };
      const made: ConversationSummary = { id: "new-ask", title: "New conversation", brain: "", last: "", updated: new Date().toISOString() };
      conversations = [made, ...conversations];
      return { status: 202, body: { id: "ask-1", conversation_id: made.id } };
    }
    if (method === "POST" && path === "/act") return { status: 202, body: { id: "act-1" } };
    if (method === "GET" && jobID) {
      const fallback: ActJob = { id: jobID[1], goal: "", state: "done", plan: "", steps: [], question: "", say: "", err: "", spend: { rounds: 0, input: 0, cached: 0, output: 0 }, elapsed_ms: 0 };
      return { status: 200, body: canned.jobs?.[decodeURIComponent(jobID[1])] ?? fallback };
    }
    if (method === "POST" && (jobStop || jobPause || jobResume || jobAnswer)) return { status: 204, body: null };
    if (method === "GET" && path === "/tasks") {
      // Mirrors internal/ipc/tasks.go's listTasks: ?owner defaults to "me", "all" filters nothing out, anything else matches the row's own owner exactly.
      const owner = params.get("owner") || "me";
      const filtered = owner === "all" ? tasks : tasks.filter((t) => t.owner === owner);
      return { status: 200, body: { tasks: filtered } };
    }
    if (method === "POST" && path === "/tasks") return { status: 201, body: { id: "task-9", conversation_id: "c9" } };
    if (method === "POST" && done) {
      const status = typeof body === "object" && body !== null && "status" in body ? String((body as { status: unknown }).status) : "";
      const id = decodeURIComponent(done[1]);
      statusById.set(id, { done: status === "done", status });
      if (status === "dropped") tasks = tasks.filter((t) => t.id !== id);
      else tasks = tasks.map((t) => (t.id === id ? { ...t, done: status === "done" } : t));
      return { status: 200, body: null };
    }
    if (method === "DELETE" && ownerPatch) {
      // Mirrors internal/ipc.taskDelete: only a task the user typed in goes, and a noticed item is refused.
      const id = decodeURIComponent(ownerPatch[1]);
      if (!id.startsWith("task-")) return { status: 400, body: null };
      tasks = tasks.filter((t) => t.id !== id);
      return { status: 204, body: null };
    }
    if (method === "PATCH" && ownerPatch) {
      // Mirrors internal/ipc.TaskOwner: writes the owner the user just picked onto that row, so the next GET /tasks reads it back the same way the daemon would.
      const owner = typeof body === "object" && body !== null && "owner" in body ? String((body as { owner: unknown }).owner) : "";
      const id = decodeURIComponent(ownerPatch[1]);
      tasks = tasks.map((t) => (t.id === id ? { ...t, owner: owner as Task["owner"] } : t));
      return { status: 200, body: null };
    }
    if (method === "GET" && path === "/days") return { status: 200, body: { days: canned.days ?? [] } };
    if (method === "GET" && day) {
      const date = decodeURIComponent(day[1]);
      const page = canned.pages?.[date] ?? { date, page: "", you: [], tasks: [], heading: "" };
      // The day's raised list and GET /tasks are two views of the same rows: a tick made on either screen is read here off the same statusById a done toggle just wrote, so a fixture only has to say what a day raised, not repeat whatever it was last ticked to.
      const raised = page.tasks.map((t) => {
        const live = statusById.get(t.id);
        return live ? { ...t, done: live.done, status: live.status } : t;
      });
      return { status: 200, body: { ...page, tasks: raised } };
    }
    if (method === "GET" && path === "/meetings") return { status: 200, body: { meetings: canned.meetings ?? [] } };
    if (method === "GET" && path === "/brains") return { status: 200, body: { brains, automatic } };
    // Picking "auto" hands the choice back to the router and any brain pins it again, the same way round the real route has it, so the picker's Automatic row lights and unlights against the mock too.
    if (method === "POST" && path === "/brains") {
      const { brain, default: makeDefault } = (body ?? {}) as { brain?: string; default?: boolean };
      if (brain === "auto") automatic = true;
      else if (makeDefault !== false) automatic = false;
      return { status: 200, body: { brains, automatic } };
    }
    if (method === "GET" && path === "/setup") return { status: 200, body: setupState };
    // The real route asks Google; this one reads the key itself, so every answer the window has to explain can be had by typing it: "offline" is Google out of reach, "env" a key the environment already sets, "noapi" a well-formed key Google refuses for a reason of its own, and anything else not shaped like a Gemini key one Google calls not valid. force saves whatever was typed, as the real route does. The messages are the real route's and Google's own.
    if (method === "POST" && path === "/setup/gemini-key") {
      const { key = "", force = false } = (body ?? {}) as { key?: string; force?: boolean };
      if (!force && key === "offline") return { status: 502, body: { error: "unreachable", message: "June could not reach Google to check the key: dial tcp: lookup generativelanguage.googleapis.com: no such host" } };
      if (!force && key === "env") return { status: 409, body: { error: "env_var_set", message: "GEMINI_API_KEY is set in Windows' environment variables for your account, and that copy always wins. Change or remove it there, then restart June." } };
      if (!force && key === "noapi")
        return { status: 422, body: { error: "invalid_key", message: "Generative Language API has not been used in project 381294411 before or it is disabled. Enable it by visiting https://console.developers.google.com/apis/api/generativelanguage.googleapis.com/overview?project=381294411 then retry." } };
      if (!force && key.length < 20) return { status: 422, body: { error: "invalid_key", message: "That is not a Gemini API key. Copy the whole key from Google AI Studio: one line of letters, digits, dashes and underscores." } };
      if (!force && !/^AIza[0-9A-Za-z_-]{35}$/.test(key)) return { status: 422, body: { error: "invalid_key", message: "API key not valid. Please pass a valid API key." } };
      const refused = restartRefusal("Saving the key");
      if (refused) return refused;
      brains = brains.map((b) => (b.id === "gemini" ? { ...b, signed_in: true, note: "" } : b));
      setupState = { ...setupState, gemini_key: true, brain_ready: true };
      // The answer goes out whether or not the restart then starts, as answerRestarting's does: the key is written either way.
      goDown(true);
      return { status: 200, body: { ok: true, restarting: true } };
    }
    if (method === "DELETE" && path === "/setup/gemini-key") {
      const refused = restartRefusal("Removing the key");
      if (refused) return refused;
      brains = brains.map((b) => (b.id === "gemini" ? { ...b, signed_in: false } : b));
      setupState = { ...setupState, gemini_key: false, brain_ready: brains.some((b) => b.signed_in) };
      goDown(true);
      return { status: 200, body: { ok: true, restarting: true } };
    }
    if (method === "POST" && path === "/setup/mic-settings") return { status: 204, body: null };
    // The restart setup owes is put off while anything downloads, and carried out once the queue is empty, as the real route does.
    if (method === "POST" && path === "/setup/complete") {
      if (canned.completeFails) return { status: 500, body: { error: "save_failed", message: "open C:\\Users\\you\\AppData\\Local\\june\\june-config.json: The process cannot access the file because it is being used by another process." } };
      const pending = setupState.restart_pending;
      const restarting = pending && !recording && !busy();
      setupState = { ...setupState, done: true };
      if (restarting) goDown(true);
      else if (pending) restartWhenIdle = true;
      return { status: 200, body: { restarting } };
    }
    // As lifecycleRoutes answers it: refused for what a restart would cut short, except while a restart is already under way, when asking again only has the replacement show its window.
    if (method === "POST" && path === "/restart") {
      if (stopping()) return { status: 202, body: { restarting: true, pid: 4242 } };
      if (recording) return { status: 409, body: { error: "recording", message: "Restarting June now would end the meeting recording, and the rest of the call would not be recorded. Restart once the meeting is over." } };
      if (busy()) return { status: 409, body: { error: "downloading", message: "Restarting June now would stop the local features it is downloading. Restart once they finish, or cancel the download first." } };
      if (!goDown(true)) return { status: 500, body: { error: "restart_failed", message: "June could not restart: the replacement daemon could not be started: Access is denied." } };
      return { status: 202, body: { restarting: true, pid: 4242 } };
    }
    if (method === "GET" && path === "/components" && components) return { status: 200, body: components };
    const feature = /^\/components\/([^/]+)(\/install|\/cancel)?$/.exec(path);
    const f: Feature | undefined = feature && components ? components.features.find((x) => x.id === decodeURIComponent(feature[1])) : undefined;
    if (feature && components && !f) return { status: 404, body: { error: "no such feature" } };
    if (f && components && method === "POST" && feature?.[2] === "/install") {
      if (f.state === "queued" || f.state === "installing") return { status: 202, body: {} };
      f.state = "queued";
      f.error = "";
      queue.push(f.id);
      runQueue();
      return { status: 202, body: {} };
    }
    if (f && components && method === "POST" && feature?.[2] === "/cancel") {
      if (running?.id === f.id) {
        clearInterval(running.timer);
        running = undefined;
      }
      queue.splice(0, queue.length, ...queue.filter((id) => id !== f.id));
      f.state = "not_installed";
      emit?.({ id: f.id, type: "component", text: "cancelled", detail: "{}", failed: false });
      runQueue();
      return { status: 204, body: null };
    }
    if (f && components && method === "DELETE" && !feature?.[2]) {
      f.state = "not_installed";
      if (f.needs_restart) {
        components.restart_pending = true;
        setupState = { ...setupState, restart_pending: true };
      }
      return { status: 200, body: { restart_pending: components.restart_pending } };
    }
    if (method === "GET" && path === "/update" && update) return { status: 200, body: update };
    if (method === "POST" && path === "/update" && update) {
      if (recording) return { status: 409, body: { error: "recording", message: "Updating June now would end the meeting recording, and the rest of the call would not be recorded. Update once the meeting is over." } };
      if (busy()) return { status: 409, body: { error: "downloading", message: "Updating June now would stop the local features it is downloading. Update once they finish, or cancel the download first." } };
      runUpdate();
      return { status: 202, body: update };
    }
    if (method === "GET" && path === "/voices") return { status: 200, body: { voices: canned.voices ?? [], models: canned.models ?? [] } };
    // Picking a voice or a Live model in the mock answers the roster with that one marked, so the section behaves the way it does against a real daemon rather than freezing on its first answer. A body names one or the other, never both, same as the real route.
    if (method === "POST" && path === "/voices") {
      const { name, model } = body as { name?: string; model?: string };
      if (model) return { status: 200, body: { voices: canned.voices ?? [], models: (canned.models ?? []).map((m) => ({ ...m, current: m.name === model })) } };
      return { status: 200, body: { voices: (canned.voices ?? []).map((v) => ({ ...v, current: v.name === (name ?? "") })), models: canned.models ?? [] } };
    }
    // Nothing can be played in a browser, so the mock reports a machine with no speaker, which is a real answer the section already knows how to say.
    if (method === "POST" && path === "/voices/preview") return { status: 503, body: {} };
    if (method === "GET" && path === "/settings") return { status: 200, body: { ...emptySettings, ...settingsState } };
    if (method === "POST" && path === "/settings") {
      const { autostart, ...rest } = (body ?? {}) as Partial<SettingsView> & { autostart?: boolean };
      // Start-at-sign-in is written through /settings and read back through /setup, as on the daemon.
      if (autostart !== undefined) setupState = { ...setupState, autostart };
      settingsState = { ...settingsState, ...rest };
      return { status: 200, body: { ...emptySettings, ...settingsState } };
    }
    if (method === "GET" && path === "/usage") return { status: 200, body: canned.usage ?? { today: { providers: [], models: [] }, week: { providers: [], models: [] }, days: [], recent: [] } };
    if (method === "GET" && path === "/routines") return { status: 200, body: { routines } };
    if (method === "POST" && path === "/routines") {
      const made: Routine = { id: `r-${nextRoutineID++}`, text: "", schedule: "", enabled: true, last_run: "", last_answer: "" };
      routines = [made, ...routines];
      return { status: 201, body: { id: made.id } };
    }
    if (method === "DELETE" && routineID) {
      routines = routines.filter((r) => r.id !== decodeURIComponent(routineID[1]));
      return { status: 204, body: null };
    }
    if (method === "POST" && routineRun) {
      // The daemon answers 202 the moment the run has started and knows neither when it will end nor what it will say; both come back minutes later as a "notice" event (see internal/ipc/routines.go), so nothing about the routine changes here.
      return { status: 202, body: { id: decodeURIComponent(routineRun[1]) } };
    }
    // The rail line's own Done/1h/Evening/Tomorrow buttons; nothing here fires the "notice" event the real Act does, since a fixture that wants to show one arriving already dispatches progress.eventArrived directly (see store.test.ts).
    if (method === "POST" && noticeAction) return { status: 200, body: null };
    if (method === "GET" && path === "/status") return { status: 200, body: { paused, paused_until: pausedUntil } };
    if (method === "POST" && path === "/pause") {
      const minutes = (body as { minutes?: number } | undefined)?.minutes ?? 0;
      paused = true;
      pausedUntil = minutes > 0 ? new Date(Date.now() + minutes * 60_000).toISOString() : "";
      return { status: 200, body: "paused" };
    }
    if (method === "POST" && path === "/resume") {
      paused = false;
      pausedUntil = "";
      return { status: 200, body: "resumed" };
    }
    if (method === "POST" && path === "/dictate/start") return { status: 202, body: { id: "dictate-1" } };
    if (method === "POST" && path === "/dictate/stop") return { status: 200, body: { text: canned.dictateText ?? "send this thought" } };
    return { status: 404, body: null };
  };

  // RTK Query hands its fetch a Request object rather than a string, so the call is read off whichever of the two arrived.
  return async (input: RequestInfo | URL, init?: RequestInit) => {
    const asRequest = typeof input === "object" && input !== null && "url" in input ? (input as Request) : undefined;
    const url = new URL(asRequest ? asRequest.url : String(input));
    const method = ((asRequest ? asRequest.method : init?.method) ?? "GET").toUpperCase();
    let sent = "";
    if (asRequest) sent = await asRequest.clone().text();
    else if (typeof init?.body === "string") sent = init.body;
    const body = sent ? JSON.parse(sent) : undefined;
    // A restarting daemon is not there at all once its shutdown is over, which fetch says by throwing rather than by any status.
    if (Date.now() >= stoppingUntil && Date.now() < downUntil) throw new TypeError("Failed to fetch");
    calls.push({ method, path: url.pathname, body });
    if (fails.has(`${method} ${url.pathname}`)) return new Response("no", { status: 500 });
    // The one route that takes its time: the real one listens for three seconds before it answers.
    if (method === "POST" && url.pathname === "/setup/mic-test") {
      const mic = canned.mic ?? { device: "Microphone Array (Realtek Audio)", heard: true, peak: 0.42, consent: "allowed" };
      const ms = canned.micMs ?? 3000;
      playLevels(mic.heard, ms);
      await new Promise((done) => setTimeout(done, ms));
      return new Response(JSON.stringify(mic), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    const { status, body: out } = answer(method, url.pathname, body, url.searchParams);
    if (out === null) return new Response(null, { status });
    if (typeof out === "string") return new Response(out, { status });
    return new Response(JSON.stringify(out), { status, headers: { "Content-Type": "application/json" } });
  };
}

/** A day counted back from now, so the fixtures below always read as "today", "yesterday" and "last week" whenever they are opened rather than drifting into a fixed date in the past. Input: how many days back, and the hour and minute of that day. Output: the RFC3339 string the daemon would have sent. */
function ago(days: number, hour = 9, minute = 0): string {
  const d = new Date();
  d.setDate(d.getDate() - days);
  d.setHours(hour, minute, 0, 0);
  return d.toISOString();
}

/** The date part of a day counted back from now, which is what /days and /days/{date} are keyed by. */
function day(days: number): string {
  const d = new Date();
  d.setDate(d.getDate() - days);
  return isoDay(d);
}

/** The local features as GET /components reports them, on a Windows laptop with a small NVIDIA card, or with gpu false on one without, which gets the CPU build and the small whisper model. Sizes are the manifest's own. Input: whether there is an NVIDIA card, and the state to give each feature by id (not_installed for any left out). Output: the body. */
function featuresFixture(gpu: boolean, states: Record<string, Feature["state"]> = {}): Components {
  const feature = (id: string, title: string, variant: string, download: number, disk: number, more: Partial<Feature> = {}): Feature => ({
    id,
    title,
    variant,
    download_bytes: download,
    disk_bytes: disk,
    state: states[id] ?? "not_installed",
    error: "",
    requires: [],
    needs_restart: false,
    ...more,
  });
  return {
    platform: {
      os: "windows",
      gpu: gpu ? { vendor: "nvidia", name: "NVIDIA GeForce RTX 3050 Laptop GPU", vram_mb: 4096, driver: "581.57" } : { vendor: "intel", name: "Intel Iris Xe Graphics", vram_mb: 128, driver: "31.0.101.4502" },
      vulkan: true,
      free_bytes: 182_000_000_000,
      ram_mb: 16084,
    },
    features: [
      gpu
        ? feature("transcribe", "Voice typing & meeting transcripts", "cuda", 2_209_187_442, 2_730_000_000)
        : feature("transcribe", "Voice typing & meeting transcripts", "cpu", 509_847_299, 560_000_000),
      feature("speakers", "Who said what", "cpu", 61_056_987, 70_000_000, { requires: ["transcribe"] }),
      feature("memory", "Smarter memory search", "vulkan", 366_802_356, 400_000_000, { needs_restart: true }),
      feature("summaries", "On-device summaries", "vulkan", 2_874_692_596, 2_900_000_000, { needs_restart: true }),
    ],
    restart_pending: false,
  };
}

/** What ?mock=1 shows: enough of every screen to see the design working — a conversation with an answer, its sources and a failed ask; work of both kinds; a recording with real minutes; a day's page; brains signed in and not; and a week of token use. This is the only fixture the browser mode has, because its whole job is to make one page look at. */
export const demo: Canned = {
  conversations: [
    { id: "c1", title: "Flights to Zurich", brain: "claude", last: "Both are refundable until the 12th.", updated: ago(0, 15, 12) },
    { id: "c2", title: "Meridian statement pattern analysis", brain: "claude", last: "She wants the file before Friday.", updated: ago(0, 11, 40) },
    { id: "c3", title: "What Vexil asked for on the call", brain: "claude", last: "The legal review, and the numbers behind table 4.", updated: ago(1, 17, 5) },
    { id: "c4", title: "Rewriting the onboarding note", brain: "codex", last: "Shorter, and without the second heading.", updated: ago(3, 10, 20) },
    { id: "c5", title: "Why the nightly loop stopped", brain: "claude", last: "It ran out of disk at 02:14.", updated: ago(9, 22, 30) },
    { id: "c6", title: "Reordering the slide deck", brain: "claude", last: "", updated: ago(0, 15, 30) },
  ],
  jobs: {
    "act-1": {
      id: "act-1",
      goal: "Move the Risk slide before Mitigation in the review deck",
      state: "stepping",
      plan: "Open the deck, drag the Risk slide above Mitigation, then check the new order stuck.",
      steps: [
        { n: 1, tool: "click", expect: { kind: "item_present", value: "Risk" }, result: "clicked Slide 4", outcome: "pass", why: "the change came: found \"Risk\"" },
        { n: 2, tool: "click", expect: { kind: "title_contains", value: "Risk, Mitigation" }, result: "dragging", outcome: "", why: "" },
      ],
      question: "",
      say: "",
      err: "",
      spend: { rounds: 3, input: 15400, cached: 8200, output: 640, by_model: { opus: { model: "opus", input: 15400, cached: 8200, output: 640 } } },
      elapsed_ms: 42000,
    },
  },
  runningJob: {
    id: "act-1",
    conversationId: "c6",
    goal: "Move the Risk slide before Mitigation in the review deck",
    steps: [
      { text: "Clicking Slide 4", expect: 'an item labelled "Risk" is showing', outcome: "pass", why: 'the change came: found "Risk"' },
      { text: "Dragging Slide 4 above Slide 3", expect: 'the title contains "Risk, Mitigation"' },
    ],
  },
  turns: {
    c1: {
      id: "c1",
      title: "Flights to Zurich",
      brain: "claude",
      turns: [
        { id: "t1", role: "you", text: "what did she say about the deadline for the Zurich trip?", kind: "ask", evidence: [], tools: [], when: ago(0, 15, 10), reason: "" },
        {
          id: "t2",
          role: "june",
          text: "Friday, and she meant end of the working day rather than midnight.\n\nShe also said the flights can wait until the statement is signed off, so there is no need to book before Wednesday.",
          kind: "ask",
          evidence: [
            { title: "Meridian statement pattern analysis", meta: "meeting · yesterday", body: "the deadline is Friday, close of business — not midnight, please" },
            { title: "A note you wrote", meta: "note · 2 days ago", body: "flights can wait until sign-off" },
          ],
          tools: ["search_memory", "read_meeting"],
          when: ago(0, 15, 11),
          reason: "",
        },
        { id: "t3", role: "you", text: "and are the two I looked at still refundable?", kind: "ask", evidence: [], tools: [], when: ago(0, 15, 12), reason: "" },
        {
          id: "t4",
          role: "june",
          text: "Both are refundable until the 12th. After that the LX flight keeps half the fare and the BA one keeps all of it.",
          kind: "ask",
          evidence: [{ title: "Browser · Skyscanner", meta: "seen · 3 days ago", body: "Refundable until 12 Sep · after that 50% retained" }],
          tools: ["search_memory"],
          when: ago(0, 15, 13),
          reason: "",
        },
        {
          id: "t5",
          role: "june",
          text: '{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_011CS4"}',
          kind: "error",
          evidence: [],
          tools: [],
          when: ago(0, 15, 20),
          reason: "The model was too busy to answer. Ask again in a moment.",
        },
        { id: "t6", role: "you", text: "put the two fares in a table, and show me the retry you used against the overload", kind: "ask", evidence: [], tools: [], when: ago(0, 15, 21), reason: "" },
        {
          id: "t7",
          role: "june",
          text: [
            "| Flight | Refundable until | Kept after |",
            "| --- | --- | --- |",
            "| LX | 12 Sep | 50% |",
            "| BA | 12 Sep | 100% |",
            "",
            "The kept fraction after the 12th is $f = 1 - r$, where $r$ is the refund rate — so LX keeps $f = 0.5$ and BA keeps $f = 1$.",
            "",
            "This is the retry that waited out the overload:",
            "```js",
            "for (let i = 0; i < 3; i++) {",
            "  const res = await ask(question);",
            "  if (res.ok) break;",
            "  await sleep(2 ** i * 1000);",
            "}",
            "```",
          ].join("\n"),
          kind: "ask",
          evidence: [],
          tools: [],
          when: ago(0, 15, 22),
          reason: "",
        },
      ],
    },
  },
  tasks: [
    { id: "11", title: "Send the Meridian file to legal", source: "noticed", when: ago(0, 11, 40), done: false, conversation_id: "", detail: `Meridian statement pattern analysis, ${day(0)}`, owner: "me" },
    { id: "12", title: "Book the Zurich flight before the 12th", source: "you", when: ago(0, 9, 0), done: false, conversation_id: "c1", detail: "you said", owner: "me" },
    { id: "13", title: "Ask Vexil for the numbers behind table 4", source: "noticed", when: ago(1, 17, 5), done: false, conversation_id: "", detail: `Meridian statement pattern analysis, ${day(1)}`, owner: "me" },
    { id: "14", title: "Rewrite the onboarding note without the second heading", source: "you", when: ago(3, 10, 20), done: false, conversation_id: "c4", detail: "you said", owner: "me" },
    { id: "15", title: "Clear the disk the nightly loop filled", source: "you", when: ago(9, 22, 30), done: true, conversation_id: "c5", detail: "you said", owner: "me" },
    // Raised by a meeting but not the user's own — this is the "Watching" section: one clearly someone else's, one nobody named.
    { id: "16", title: "Re-run the source data once the directory is updated", source: "noticed", when: ago(0, 11, 40), done: false, conversation_id: "", detail: `Meridian statement pattern analysis, ${day(0)}`, owner: "them" },
    { id: "17", title: "Write up what the cap was costing", source: "noticed", when: ago(1, 9, 45), done: false, conversation_id: "", detail: `Daily standup, ${day(1)}`, owner: "unclear" },
  ],
  meetings: [
    {
      id: "m1",
      title: "Meridian statement pattern analysis",
      when: ago(0, 11, 0),
      duration_s: 2820,
      attendees: [
        { name: "Zemna Braxen", heard_only: false },
        { name: "Vexil Quorin", heard_only: false },
        { name: "Turnek", heard_only: true },
      ],
      minutes: [
        "# Meridian statement pattern analysis",
        "The call was about which of the 240 statements actually follow the agreed template, and what to do about the ones that do not. Vexil opened by saying legal will not sign anything before Friday, so the shape of the argument matters more this week than the wording of it.",
        "## What was decided",
        "- The template stays as it is, and the statements that do not fit it are listed separately rather than forced into a section.",
        "- Table 4 keeps its current numbers until the source data is re-run.",
        "- Legal sees the whole file at once, not section by section.",
        "## What was said",
        "Vexil walked through the eleven statements that fail the template. Nine of them fail because they name a team that is not in the project directory; the other two are written as future plans, which belong in a different part of the file entirely.",
        "Turnek asked whether the eleven should be dropped. The answer was no: they are still statements, and dropping them would change the denominator every other number in the paper is worked out against.",
        "## Action items",
        "- send the file to legal before Friday, close of business",
        "- ask Vexil for the numbers behind table 4",
        "- re-run the source data once the directory is updated",
        "## What is still open",
        "Whether the two future plans belong in this paper at all. Nobody had the template to hand and it was left for the next call.",
      ].join("\n"),
    },
    {
      id: "m2",
      title: "Daily standup",
      when: ago(1, 9, 30),
      duration_s: 900,
      attendees: [
        { name: "Zemna Braxen", heard_only: false },
        { name: "Vexil", heard_only: false },
      ],
      minutes: [
        "## What was said",
        "- The retrieval work landed and the excerpt cap is gone.",
        "- The window rebuild is the week's main thread.",
        "## Action items",
        "- write up what the cap was costing",
      ].join("\n"),
    },
  ],
  days: [
    { date: day(0), title: "", has_page: false, seen: 212, meetings: 2, meeting_minutes: 62 },
    { date: day(1), title: "A long day of Meridian work.", has_page: true, seen: 366, meetings: 5, meeting_minutes: 140 },
    { date: day(2), title: "", has_page: true, seen: 288, meetings: 3, meeting_minutes: 95 },
    { date: day(8), title: "", has_page: true, seen: 120, meetings: 1, meeting_minutes: 28 },
  ],
  pages: {
    [day(1)]: {
      date: day(1),
      heading: "366 things seen · 5 calls, 140 min",
      brief: "Two calls before lunch, both about the Meridian statements, and the file is owed to legal by Friday. Nothing else is due today.",
      close: "",
      page: [
        "You spent the morning on the Meridian statements, and most of it on the eleven that do not fit the agreed template.",
        "The call with Vexil settled the shape of the argument rather than the wording: the eleven stay in, listed separately, because dropping them would move the denominator under every other number in the paper. Turnek wanted them dropped and was talked out of it.",
        "The afternoon went to the flights. You looked at two, both refundable until the 12th, and booked neither — the trip depends on legal signing off, and legal will not look at anything before Friday.",
        "The evening was the window rebuild. The retrieval work from last week is holding: nothing you searched for came back empty.",
      ].join("\n"),
      you: [],
      tasks: [
        { id: "11", title: "Send the Meridian file to legal", done: false, status: "open", owner: "me" },
        { id: "13", title: "Ask Vexil for the numbers behind table 4", done: false, status: "open", owner: "me" },
        { id: "15", title: "Clear the disk the nightly loop filled", done: true, status: "done", owner: "me" },
      ],
    },
  },
  routines: [
    {
      id: "1",
      text: "tell me the one thing I must do today",
      schedule: "weekdays at 8",
      enabled: true,
      last_run: ago(0, 8, 0),
      last_answer: "Send the Meridian file to legal — Vexil said Friday, close of business.",
    },
    { id: "2", text: "tell me if anything is on fire", schedule: "every 3 hours", enabled: true, last_run: ago(0, 6, 0), last_answer: "NOTHING" },
    { id: "3", text: "when Vexil replies about the venue, tell me", schedule: "when Vexil replies about the venue", enabled: true, last_run: "", last_answer: "" },
  ],
  voices: [
    { name: "Iapetus", trait: "Clear", current: true },
    { name: "Sulafat", trait: "Warm", current: false },
    { name: "Algenib", trait: "Gravelly", current: false },
    { name: "Puck", trait: "Upbeat", current: false },
    { name: "Sadaltager", trait: "Knowledgeable", current: false },
  ],
  models: [
    { name: "gemini-3.1-flash-live-preview", label: "Gemini 3.1 Flash Live", trait: "Fast — about two seconds to first word, one tone, hears everything", current: true },
    { name: "gemini-3.8-live", label: "Gemini 3.8 Live", trait: "Always decides whether the audio was meant for it, and cannot be told not to", current: false },
    { name: "gemini-2.5-flash-native-audio-preview-12-2025", label: "Gemini 2.5 Native Audio", trait: "Warm — five to eight seconds, but it has moods and can ignore the room", current: false },
  ],
  brains: [
    {
      id: "claude",
      name: "Claude",
      signed_in: true,
      account: "max",
      models: ["opus", "sonnet", "haiku"],
      model: "opus",
      note: "",
      default: true,
      limits: [
        { window: "5h", used_fraction: 0.42, resets_at: new Date(Date.now() + 3 * 3600000 + 56 * 60000).toISOString(), source: "claude-cli" },
        { window: "weekly", used_fraction: 0.93, resets_at: new Date(Date.now() + 3 * 86400000).toISOString(), source: "claude-cli" },
      ],
      limits_at: new Date().toISOString(),
    },
    { id: "codex", name: "Codex", signed_in: true, account: "plus", models: ["gpt-5.5", "gpt-5.5-mini"], model: "gpt-5.5", note: "", default: false },
    { id: "gemini", name: "Gemini", signed_in: false, account: "", models: [], model: "", note: "set GEMINI_API_KEY in ~/.config/june/env", default: false },
  ],
  setup: { ...finishedSetup, version: "0.2.0", default_brain: "claude" },
  components: featuresFixture(true, { transcribe: "installed", memory: "external" }),
  settings: {
    data_dir: "/home/you/.june",
    store_bytes: 22020096,
    recordings_bytes: 481247232,
    models_bytes: 1288490188,
    voice_model: "whisper-small",
    brain: "Claude, opus",
    embed_model: "embeddinggemma-300m",
    meetings_enabled: true,
    capture_enabled: true,
    keep_audio_days: -1,
    daemon_started: ago(0, 7, 42),
    version: "dev",
    hotkey: "<Control><Alt>space",
  },
  usage: {
    today: {
      providers: [
        { provider: "claude", calls: 14, input_tokens: 184000, output_tokens: 9200, total_tokens: 193200 },
        { provider: "codex", calls: 3, input_tokens: 41000, output_tokens: 2600, total_tokens: 43600 },
      ],
      models: [
        { provider: "claude", model: "opus", calls: 11, input_tokens: 160000, output_tokens: 8100, total_tokens: 168100 },
        { provider: "claude", model: "sonnet", calls: 3, input_tokens: 24000, output_tokens: 1100, total_tokens: 25100 },
        { provider: "codex", model: "gpt-5.5", calls: 3, input_tokens: 41000, output_tokens: 2600, total_tokens: 43600 },
      ],
    },
    week: {
      providers: [
        { provider: "claude", calls: 96, input_tokens: 1240000, output_tokens: 61000, total_tokens: 1301000 },
        { provider: "codex", calls: 22, input_tokens: 288000, output_tokens: 14000, total_tokens: 302000 },
      ],
      models: [],
    },
    days: [
      { day: day(6), calls: 12, total_tokens: 141000 },
      { day: day(5), calls: 21, total_tokens: 264000 },
      { day: day(4), calls: 9, total_tokens: 98000 },
      { day: day(3), calls: 18, total_tokens: 233000 },
      { day: day(2), calls: 24, total_tokens: 311000 },
      { day: day(1), calls: 17, total_tokens: 219000 },
      { day: day(0), calls: 17, total_tokens: 236800 },
    ],
    recent: [
      {
        id: 1,
        when: ago(0, 15, 13),
        provider: "claude",
        model: "opus",
        channel: "text",
        input_tokens: 41200,
        output_tokens: 640,
        total_tokens: 41840,
        duration_ms: 8400,
        question: "and are the two I looked at still refundable?",
        cached_input_tokens: 33000,
      },
      {
        id: 2,
        when: ago(0, 15, 11),
        provider: "claude",
        model: "opus",
        channel: "text",
        input_tokens: 38600,
        output_tokens: 1120,
        total_tokens: 39720,
        duration_ms: 11200,
        question: "what did she say about the deadline for the Zurich trip?",
        cached_input_tokens: 28400,
      },
      {
        id: 3,
        when: ago(0, 11, 58),
        provider: "claude",
        model: "sonnet",
        channel: "dream",
        input_tokens: 9800,
        output_tokens: 320,
        total_tokens: 10120,
        duration_ms: 2400,
        question: "summarise the Meridian call",
        cached_input_tokens: 0,
      },
      {
        id: 4,
        when: ago(0, 10, 12),
        provider: "codex",
        model: "gpt-5.5",
        channel: "text",
        input_tokens: 14200,
        output_tokens: 900,
        total_tokens: 15100,
        duration_ms: 90000,
        question: "rewrite the onboarding note without the second heading",
        cached_input_tokens: 4100,
      },
      {
        id: 5,
        when: ago(0, 9, 4),
        provider: "claude",
        model: "opus",
        channel: "voice",
        input_tokens: 2100,
        output_tokens: 180,
        total_tokens: 2280,
        duration_ms: 1900,
        question: "what time is the standup",
        cached_input_tokens: 0,
      },
    ],
  },
};

/** Whether this page was asked to run against the fake daemon. Input: the page's query string. Output: true only for ?mock=1, so nothing but that one spelling ever takes the real window off the real daemon. */
export function wantsMock(search: string): boolean {
  return new URLSearchParams(search).get("mock") === "1";
}

/** The steps a daemon with none of the four ways to answer set up sends, for ?firstrun=1 to show the panel Settings draws while June cannot answer yet. */
const firstRunSteps = {
  gemini_key: false,
  codex_login: false,
  claude_cli: false,
  local_model: false,
  steps: ["Set GEMINI_API_KEY in ~/.config/june/env.", "Or sign in with the Claude CLI: run claude login.", "Or sign in with the Codex CLI: run codex login.", "Or point JUNE_LOCAL_MODEL at a model on this machine."],
};

/** The release the ?update=1 page offers. */
const newerRelease: UpdateView = {
  current: "0.2.0",
  latest: "0.2.1",
  available: true,
  release_url: "https://github.com/M-DEV-1/june/releases/tag/v0.2.1",
  can_install: true,
  state: "idle",
  error: "",
};

/** What the page's query string asks the fake daemon to be, on top of the demo. Input: the query string. Output: the fixture.
 *
 * ?firstrun=1 answers /settings and /brains as a daemon that cannot answer yet, so the panels Settings and the chat page only draw in that state can be screenshotted.
 * ?onboarding=1 is a fresh install on Windows: setup not done, Claude signed in, an expired ChatGPT login, no Gemini key, an NVIDIA laptop with no local features yet. On top of it, nobrain=1 signs everything out, chatgpt=1 keeps ChatGPT signed in (and so answering, ahead of Claude), grok=1 adds a signed-in Grok, mic=blocked or mic=silent fails the microphone test the two ways it can (mic=denied is blocked with the microphone also refusing to open), gpu=none is a machine without an NVIDIA card, and done=fail has finishing setup fail to save.
 * ?update=1 offers a newer June (update=fail has its download fail the checksum, update=linux is a machine June cannot replace itself on, update=later has the daily check find it a few seconds after the page opens).
 * ?restart=fail has every restart fail to start its replacement; restart=slow has the old daemon keep answering through twenty seconds of shutdown first, as one flushing a busy hour does.
 * ?recording=1, with any of the above, has a meeting being recorded, which every button that restarts June waits for.
 */
function fixtureFor(search: string): Canned {
  const q = new URLSearchParams(search);
  let canned: Canned = demo;
  if (q.get("firstrun") === "1") canned = { ...demo, brains: demo.brains?.map((b) => ({ ...b, signed_in: false })), settings: { ...demo.settings, first_run: firstRunSteps } };
  if (q.get("onboarding") === "1") {
    const nobrain = q.get("nobrain") === "1";
    const chatgpt = q.get("chatgpt") === "1";
    const rows: Brain[] = (canned.brains ?? []).map((b) =>
      nobrain || b.id === "gemini"
        ? { ...b, signed_in: false, limits: [], limits_note: "" }
        : b.id === "codex" && !chatgpt
          ? { ...b, signed_in: false, limits: [], limits_note: "the Codex login was refused: run codex login to sign in again" }
          : b,
    );
    // Grok signed in is the case where it must not be offered: the router sends it no questions.
    if (q.get("grok") === "1") rows.push({ id: "grok", name: "Grok", signed_in: true, account: "", models: [], model: "", note: "", default: false, limits: [], limits_note: "" });
    // Marked default the way GET /brains marks it with nothing picked: the first signed in of automaticRank (internal/ipc/brains.go), which is the one the router answers on.
    const answering = ["codex", "antigravity", "claude"].find((id) => rows.some((b) => b.id === id && b.signed_in)) ?? "";
    canned = {
      ...canned,
      setup: { ...finishedSetup, done: false, version: "0.2.0", brain_ready: answering !== "", default_brain: answering, data_dir: "C:\\Users\\you\\AppData\\Local\\june" },
      brains: rows.map((b) => ({ ...b, default: b.id === answering })),
      components: featuresFixture(q.get("gpu") !== "none"),
      mic:
        q.get("mic") === "blocked"
          ? { device: "", heard: false, peak: 0, consent: "blocked" }
          : q.get("mic") === "denied"
            ? { device: "Microphone Array (Realtek Audio)", heard: false, peak: 0, consent: "blocked", error: "IAudioClient::Initialize failed: E_ACCESSDENIED" }
            : q.get("mic") === "silent"
              ? { device: "Microphone Array (Realtek Audio)", heard: false, peak: 0.002, consent: "allowed" }
              : undefined,
      completeFails: q.get("done") === "fail",
    };
  }
  const upd = q.get("update");
  if (upd) canned = { ...canned, update: { ...newerRelease, can_install: upd !== "linux" }, updateFails: upd === "fail", updateLater: upd === "later" };
  if (q.get("restart") === "fail") canned = { ...canned, restartFails: true };
  if (q.get("restart") === "slow") canned = { ...canned, stopMs: 20_000 };
  if (q.get("recording") === "1") canned = { ...canned, recording: true };
  return canned;
}

/** Puts the fake daemon in fetch's place for a page opened with ?mock=1, so the window can be looked at in a plain browser tab with no daemon running. Input: the page's location. Output: true when the fake was installed. Called once from main.tsx before anything is rendered; on any other URL it does nothing and the window talks to the real daemon as always. What else the query string can ask for is in fixtureFor. */
export function installMock(loc: { search: string } = location): boolean {
  if (!wantsMock(loc.search)) return false;
  const canned = fixtureFor(loc.search);
  // The event stream is a fake too: whatever the fake daemon would broadcast is handed to every stream the window has open, through the same onmessage the real EventSource calls, so a download's progress or the microphone's level reaches the store by the window's own path.
  const streams = new Set<{ onmessage: ((e: MessageEvent) => void) | null }>();
  const emit = (ev: DaemonEvent) => {
    for (const s of streams) s.onmessage?.({ data: JSON.stringify(ev) } as MessageEvent);
  };
  window.fetch = daemonFetch(canned, [], emit);
  // A job in flight belongs in the store, not in fetch's fake answers, so this reaches the store directly rather than growing a second daemon fake that only ever plays back one fixed script. Loaded lazily and only here: daemonFetch above is also what the vitest suite imports for its own fake daemon, and it never triggers this path, so the test suite never pulls Redux in by way of a fixture.
  if (canned.runningJob) {
    const job = canned.runningJob;
    void import("./store").then(({ progress, store }) => {
      // Waits for the window's own start-up fetch of GET /conversations to land, so the sidebar row the job patches (see the listener in store.ts) already exists to be patched — this file loads before main.tsx even renders the window.
      setTimeout(() => {
        store.dispatch(progress.jobSent({ conversationId: job.conversationId, goal: job.goal }));
        store.dispatch(progress.jobAccepted({ id: job.id, conversationId: job.conversationId }));
        for (const step of job.steps) {
          store.dispatch(progress.eventArrived({ id: job.id, type: "act", detail: JSON.stringify({ kind: "step", state: "stepping", text: step.text, expect: step.expect ?? "" }) }));
          if (step.outcome) {
            store.dispatch(progress.eventArrived({ id: job.id, type: "act", detail: JSON.stringify({ kind: "verified", state: "stepping", text: step.why ?? "", outcome: step.outcome }) }));
          }
        }
      }, 300);
    });
  }
  // A notice's action is likewise store state rather than a fetch answer, and is fed in as the same "notice" event the real stream would carry, so a capture shows the exact rail line reactToNotice in store.ts produces.
  if (canned.notice) {
    const n = canned.notice;
    void import("./store").then(({ progress, store }) => {
      setTimeout(() => {
        store.dispatch(
          progress.eventArrived({
            id: "",
            type: "notice",
            notice: { title: "", body: n.body, place: "", id: n.id, kind: n.kind, action: n.action, until: n.until },
          }),
        );
      }, 300);
    });
  }
  // A stream that connects to nothing over the network keeps the page from retrying against a closed port; it only ever carries what emit above hands it.
  window.EventSource = class {
    onmessage: ((e: MessageEvent) => void) | null = null;
    onopen: (() => void) | null = null;
    onerror: (() => void) | null = null;
    constructor() {
      streams.add(this);
      // The real daemon only says "recording" when one starts, so this stands in for a window that was open then.
      if (canned.recording) setTimeout(() => this.onmessage?.({ data: JSON.stringify({ type: "recording", text: "on" }) } as MessageEvent), 0);
    }
    close() {
      streams.delete(this);
    }
    addEventListener() {}
    removeEventListener() {}
  } as unknown as typeof EventSource;
  return true;
}
