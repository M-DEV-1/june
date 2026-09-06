import {
  availableMonitors,
  currentMonitor,
  cursorPosition,
  getCurrentWindow,
} from "@tauri-apps/api/window";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { PhysicalPosition } from "@tauri-apps/api/dpi";
import {
  dotClass,
  dotLabel,
  voiceStateWord,
  isJobLive,
  noticeActionLine,
  NOTICE_MS,
  placeholder,
  placeholderMatter,
  RESTING_PLACEHOLDER,
  sourceMeta,
  step,
  stepIconKind,
  stepLabel,
  stepSeconds,
  stepsCollapsed,
  stepsSummaryLine,
  THEME_KEY,
  themeChoice,
  themeFromStorage,
  type JobMeta,
  type Matter,
  type Notice,
  type Theme,
  type ToolStep,
  type View,
} from "./state";
// The key a clicked notice's target is left under is defined beside the code in the app window that reads it, so there is one spelling of it rather than two.
import { OPEN_AT_KEY } from "./app/state";
import { initialView, venueScript } from "./mock";
import {
  actAnswer,
  actPauseResume,
  actStart,
  actStop,
  ask,
  context,
  endpoint,
  events,
  matters,
  probe,
  setPort,
  setToken,
  TOKEN_HEADER,
  voiceStart,
  voiceStatus,
  voiceStop,
} from "./daemon";
import { dictationKey, startDictation, stopDictation } from "./dictate";
import { Waveform, workingRow } from "./waveform";
import {
  fitWindow as winplaceFitWindow,
  noticePlacement,
  resolveContext,
  threadMaxHeight,
  storedHoverPosition,
  toggleWindow,
  type Desktop,
  type Dock,
  type PlaceContext,
  type WinLike,
} from "./winplace";

const root = document.getElementById("w") as HTMLElement;
/** The notice bubble, a sibling above the card rather than part of it: a notice that arrives while a question is on screen has to stack over that question, and the card is rebuilt from scratch on every render. */
const noticeEl = document.getElementById("n") as HTMLElement;
const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;

const params = new URLSearchParams(location.search);

// Fake data only when the page is opened with ?mock=1 for UI work; otherwise the window starts empty and everything on it comes from the daemon.
const mockMode = params.has("mock");

// Switches that only exist while the page is served from the Vite dev server, so a packaged build ignores them entirely. They exist so every state of this window can be driven from a URL and photographed without a keyboard: ?token= stands in for the secret Tauri hands over through invoke("ipc_token"), ?port= points the client at another port (a closed one is how the offline state is reached without stopping the user's real daemon), each ?q= asks one question — a second one is asked as a follow-up once the first is answered — ?evidence=open unfolds the evidence as soon as an answer arrives, and ?theme= forces light or dark.
const devMode = location.hostname === "localhost";
const devPort = devMode ? params.get("port") : null;
const devToken = devMode ? params.get("token") : null;
const devQuestions = devMode ? params.getAll("q") : [];
const devTheme = devMode ? params.get("theme") : null;
const devEvidenceOpen = devMode && params.get("evidence") === "open";
// ?dictate=1 opens the microphone on load and ?voice=1 starts a live session, so both states can be reached without a hand on the keyboard.
const devDictate = devMode && params.get("dictate") === "1";
const devVoice = devMode && params.get("voice") === "1";
if (devPort) setPort(devPort);

let view: View = mockMode ? initialView() : emptyView();

/** The view before anything has been asked: one placeholder matter, no context chip. Input: none. Output: the view. */
function emptyView(): View {
  return {
    matters: [placeholderMatter()],
    current: 0,
    evidenceOpen: false,
    input: "",
    state: "empty",
    contextChip: "",
    contextText: "",
    dictating: false,
    hint: "",
    voice: "",
    voiceState: "idle",
  };
}

/** Whether the local daemon answered the last health probe; decides if a question goes to it. Refreshed by connect() at start and every time the window is shown. */
let daemonUp = false;
let eventsStarted = false;

/** The daemon's id for the question this window asked. The /events stream carries every client's ask — the CLI and the main app window included — and each event names the ask it belongs to, so this is what tells one apart from another. Undefined until the POST /ask reply lands. */
let askId: string | undefined;

/** The daemon's id for the computer-use job this window started with "do:", the same way askId tracks an ask's. Undefined until the POST /act reply lands, and again once the job is done — a fresh "do:" always starts a fresh job, never answers a stale one. */
let jobId: string | undefined;

/** How many columns wide the live-voice waveform is: the centrepiece grid of the voice-mode surface that takes over the whole card while a session runs (see cardHtml's v.voice branch), not a sliver beside the input any more. Roughly the terminal client's own width (see internal/ui/waveform.go) at the larger 16px type the surface reads at. */
const VOICE_WAVE_WIDTH = 40;

/** The speaker's smoothed amplitude for the live-voice waveform (Ora's voice, not the user's mic — see voiceWaveInnerHtml), owned here rather than in the view: it updates up to 20 times a second off the daemon's "level" events (see voiceEvent in state.ts), and running that through the full reducer-and-render path on every tick would rebuild the whole card that often for nothing. Reset to a fresh (silent) instance every time a session starts, in dispatch's "voiceOn" case, so a new session never shows the tail end of the last one's bar. */
let speakerWave = new Waveform(VOICE_WAVE_WIDTH);

/** How many probes in a row have gone unanswered. The probe gives the daemon 500ms (see probe in daemon.ts), and a daemon mid-whisper-decode or mid-AT-SPI-read can miss that and still be there, so one miss is not enough to call it gone: the last known-good answer stands until a second miss in a row. */
let probeMisses = 0;

/** Whether the daemon counts as up after this probe. Input: what the probe answered. Output: true while it is up, false once it has missed twice in a row; a single miss leaves the last answer standing, so a busy daemon does not turn the card to "Not connected" and send the next question to the offline sentence instead. */
function daemonUpAfter(probed: boolean): boolean {
  if (probed) {
    probeMisses = 0;
    return true;
  }
  probeMisses++;
  return probeMisses >= 2 ? false : daemonUp;
}

/** Re-reads the daemon's token (it changes on every daemon restart), probes it and opens the event stream once. Input: none. Output: nothing; leaves daemonUp set and the view rendered either way. This is everything the hotkey waits on before the window is shown, and the probe is the only call in it with a network round trip — what fills the card in (the context chip, the matters, a live session) runs after the window is up, see hydrate. */
async function connect(): Promise<void> {
  try {
    setToken(await invoke<string>("ipc_token"));
  } catch {
    // Not inside Tauri, or the daemon has not written its token file yet; in the dev server the token can come from the query string instead.
    if (devToken) setToken(devToken);
  }
  daemonUp = daemonUpAfter(await probe());
  if (daemonUp) {
    if (!eventsStarted) {
      eventsStarted = true;
      events((ev) => {
        // Ora speaking first, which is nobody's answer and belongs to no session: taken before every filter below, all of which are about matching an event to something this window asked for.
        if (ev.type === "notice" && ev.notice) {
          void showNotice(ev.notice).catch((e) =>
            console.error("ora: showing the notice failed", e),
          );
          return;
        }
        // Everything a live voice session hears, says and calls arrives on this same stream under the session's id.
        if (view.voice && ev.id === view.voice) {
          dispatch({ kind: "voiceEvent", ev });
          return;
        }
        // The daemon's silence gate ends a dictation without being asked, and the words come back here rather than on a stop reply.
        if (ev.type === "dictation") {
          if (ev.id === dictateId) finishDictation(ev.text ?? "");
          return;
        }
        // A computer-use job's progress, tagged with its own id rather than an ask's — matched against jobId the same way an ask is matched against askId, and dropped rather than guessed at when it belongs to some other job (or beats the POST /act reply that would have told this window the id to match).
        if (ev.type === "act") {
          if (jobId && ev.id === jobId) dispatch({ kind: "daemonEvent", ev });
          return;
        }
        // Only the answer to the question this window is waiting on is applied: anything that arrives while nothing is pending, or that names a different ask, belongs to another client.
        // ponytail: an event that beats the POST /ask reply is accepted because askId is not known yet; that only picks up the wrong answer if another client asks in the same instant. Have the daemon accept a client-supplied id if that ever matters.
        if (view.state !== "asking") return;
        if (ev.id && askId && ev.id !== askId) return;
        dispatch({ kind: "daemonEvent", ev });
      });
    }
  }
  // The breath runs on this window's own clock and is stopped when the window is hidden (see hideWindow), so a live session that outlived a hide gets its grid moving again here, on the way back to being shown.
  syncBreath(Boolean(view.voice));
  render(view);
}

/** Fills the card in from the daemon after the window is already up: the context chip, the matters behind it, and whatever live voice session the daemon is running. Input: none. Output: nothing, and nothing is awaited by the caller — /context reads the focused window through AT-SPI and can take seconds, and the hover must not wait on it (each of the three reads gives up after its own deadline, see readJson in daemon.ts). */
function hydrate(): void {
  if (!daemonUp) return;
  void loadFromDaemon().catch((e) => console.error("ora: context read failed", e));
  void syncVoice().catch((e) => console.error("ora: voice status read failed", e));
}

/** Matches the window to whatever live session the daemon is actually running. Hiding this window does not end a session, and neither does reloading it, so every time the window comes back it joins a session it was not part of, picks up the state that session moved to while nothing was on screen, and drops one that has already ended — by the spoken "stop", or from another window. Input: none. Output: nothing. */
async function syncVoice(): Promise<void> {
  const live = await voiceStatus();
  if (live?.active) {
    if (live.id !== view.voice) {
      dispatch({ kind: "voiceOn", id: live.id });
      clearHintSoon();
    }
    dispatch({
      kind: "voiceEvent",
      ev: { id: live.id, type: "state", text: live.state },
    });
  } else if (live && view.voice) {
    dispatch({ kind: "voiceOff" });
  }
}

/** What the hotkey runs before the window is shown: the probe and the event stream, then the slower reads fired off behind them. Input: none. Output: a promise that settles as soon as the window can be put on screen. */
async function beforeShow(): Promise<void> {
  await connect();
  hydrate();
}

void connect()
  .then(() => {
    hydrate();
    runDevSwitches();
  })
  .catch((e) => console.error("ora: first connect failed", e));

/** Runs the dev-only URL switches once the daemon is connected: the queued ?q= questions, a four-second dictation, a live voice session. Input: none. Output: nothing. */
function runDevSwitches(): void {
  askNextDevQuestion();
  if (devDictate) beginDictation();
  if (devVoice) void toggleVoice();
}

/** Asks the next question left in the ?q= list, if any. Input: none. Output: nothing; dispatch calls this again each time an answer lands, so two ?q= values become a question and its follow-up. */
function askNextDevQuestion(): void {
  const q = devQuestions.shift();
  if (!q) return;
  dispatch({ kind: "type", value: q });
  dispatch({ kind: "enter" });
}

/** Refreshes the context chip from the daemon, and the matters list behind it. Nothing on the window draws the matters yet — the user is still deciding what belongs in the empty state — but the reducer keeps them so a later screen can read them without another round trip. Input: none. Output: nothing; leaves the current state alone when a call fails. */
async function loadFromDaemon(): Promise<void> {
  const [ctx, rows] = await Promise.all([context(), matters()]);
  if (ctx) dispatch({ kind: "contextLoaded", ctx });
  if (rows) dispatch({ kind: "mattersLoaded", rows });
}

/** Escapes text pulled into a template as plain text (questions, titles) so it can never be read as markup. */
function esc(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

/** Renders text that is allowed the two emphasis tags an answer or an evidence body may carry. Everything is escaped first and then only <b> and <mark> are put back, so nothing else in model output or in captured screen text can turn into markup. Input: the raw text. Output: HTML safe to assign. */
function rich(s: string): string {
  return esc(s).replace(/&lt;(\/?)(b|mark)&gt;/g, "<$1$2>");
}

/** Whether the input has text or a turn is on screen; the footer, the divider above it, and the roomier input padding all wait for one of those, so the empty state is one calm row: dot, input, chip. Input: the view and its current matter. Output: true once there is more than that one row to show. */
function hasContent(v: View, m: Matter): boolean {
  return v.input.trim() !== "" || m.turns.length > 0;
}

/** Rebuilds the window's whole innerHTML from the current state, resizes the OS window to fit it, and puts the caret back in the input. Input: the view. Output: nothing, the DOM is the output. */
function render(v: View): void {
  const m = v.matters[v.current] ?? placeholderMatter();
  // Every deferred write into the card is dropped here: the elements they were measured against are about to be thrown away, and what they were going to write is in this render already.
  clearDeferredWrites();
  root.innerHTML = cardHtml(v, m);
  root.classList.toggle("bare", !hasContent(v, m));
  // Past its ceiling the thread scrolls inside itself (see threadMaxHeight and --thread-max), and the newest turn is at the bottom of it — but this rebuild has just thrown the old element and its scroll position away, so the freshly built one starts at the top. Pinning it to the bottom is what keeps the answer being written in view.
  const thread = root.querySelector<HTMLElement>(".thread");
  if (thread) thread.scrollTop = thread.scrollHeight;
  renderNotice(v);
  fit();

  const input = root.querySelector<HTMLInputElement>(".q");
  if (input) {
    // preventScroll: focusing on every render must not jerk a long answer out of view.
    input.focus({ preventScroll: true });
    input.setSelectionRange(input.value.length, input.value.length);
    input.addEventListener("input", () => {
      // A full render on every keystroke would move the caret and resize the window each time (see dispatch below), but the footer appearing the moment text shows up needs no re-render, just this class flip; fitWindow no-ops unless the height actually changed.
      root.classList.toggle(
        "bare",
        input.value.trim() === "" && m.turns.length === 0,
      );
      fit();
      dispatch({ kind: "type", value: input.value });
    });
    input.addEventListener("keydown", onInputKeydown);
  }
  bindFold(root.querySelector(".evd .h"), v.evidenceOpen, () =>
    dispatch({ kind: "toggleEvidence" }),
  );
  bindFold(root.querySelector(".stepsum"), v.stepsOpen === true, () =>
    dispatch({ kind: "toggleSteps" }),
  );
  root.querySelector(".job-stop")?.addEventListener("click", () => {
    const job = currentJobView();
    if (job) void actStop(job.id);
  });
  root.querySelector(".job-pauseresume")?.addEventListener("click", () => {
    const job = currentJobView();
    if (job) void actPauseResume(job.id, job.state !== "paused");
  });
  root
    .querySelector(".vs-stop")
    ?.addEventListener("click", () => void toggleVoice());
}

/** Resizes the OS window to the card that was just drawn, without anyone waiting on it. Input: none. Output: nothing; a window call that fails is logged rather than left as an unhandled rejection, which is all a page with no window to resize can do about it. */
function fit(): void {
  void fitWindow().catch((e) => console.error("ora: resizing the window failed", e));
}

/** Makes one of the card's two fold-out headers — the evidence line and the collapsed step summary — a control rather than a div with a click handler: reachable by Tab, operable with Enter and Space, and announced with whether it is open. Input: the header element (or null when the card has none), whether its fold is open, and what to run when it is pressed. Output: nothing. */
function bindFold(el: Element | null, open: boolean, toggle: () => void): void {
  if (!(el instanceof HTMLElement)) return;
  el.setAttribute("role", "button");
  el.tabIndex = 0;
  el.setAttribute("aria-expanded", String(open));
  el.addEventListener("click", toggle);
  el.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    e.preventDefault();
    toggle();
  });
}

/** The two writes this window defers into a card it may no longer be looking at: the 150ms half of a label's cross-fade (see crossFadeText) and the render that lands after a finished turn's step list has shrunk (see collapseStepsThenRender). Cleared whenever the card is rebuilt or the window is hidden, so neither can write into markup that has since been thrown away. */
let fadeTimer: ReturnType<typeof setTimeout> | undefined;
let collapseTimer: ReturnType<typeof setTimeout> | undefined;
function clearDeferredWrites(): void {
  clearTimeout(fadeTimer);
  fadeTimer = undefined;
  clearTimeout(collapseTimer);
  collapseTimer = undefined;
}

/** The notice drawn into the bubble right now, so an unrelated render — a tool event landing while the notice is up — leaves it alone instead of rewriting its markup and replaying its entrance. The reducer builds a new view object for every event but only replaces the notice itself when the notice changes, so identity is what says whether anything has to be redrawn. */
let drawnNotice: Notice | undefined;

/** Draws the notice bubble from the current state: the title in bold, up to three lines of body under it, and the card underneath hidden when the notice is the only thing this window is up to say. Input: the view. Output: nothing, the DOM is the output. */
function renderNotice(v: View): void {
  const n = v.notice;
  noticeEl.hidden = n === undefined;
  document.body.classList.toggle(
    "alone",
    n !== undefined && v.noticeAlone === true,
  );
  if (n === drawnNotice) return;
  drawnNotice = n;
  if (!n) {
    noticeEl.innerHTML = "";
    return;
  }
  // The two classes are margins, one above and one below (see styles.css): a card on its own is placed under the top bar by noticePlacement whatever position the hover itself is set to, so it takes its air above; one stacked over an open card takes its air below, between itself and that card. Which of the two it is comes from noticeOnly, not from the stored hover position, which says nothing about where a notice-only window went.
  noticeEl.className = noticeOnly ? "N up" : "N down";
  // A notice whose action is set is the desktop notification's own follow-up, not a fresh card: it shows what happened, one line, instead of the title and body drawn the first time.
  const actionLine = noticeActionLine(n, new Date());
  noticeEl.innerHTML =
    actionLine !== undefined
      ? `<div class="nt">${esc(actionLine)}</div>`
      : `<div class="nh">Ora</div><div class="nt">${esc(n.title)}</div><div class="nb">${esc(n.body)}</div>${noticeButtonsHtml()}`;
}

/** The buttons every fresh notice carries, in the order the desktop banner offered them, so the card is dealt with where it appears. Input: none. Output: the row's HTML. */
function noticeButtonsHtml(): string {
  const acts: [string, string][] = [["done", "Done"], ["hour", "In an hour"], ["evening", "This evening"], ["tomorrow", "Tomorrow"], ["open", "Open"]];
  return `<div class="nr">${acts.map(([act, label]) => `<button class="na" data-act="${act}" type="button">${label}</button>`).join("")}</div>`;
}

/** Sends one of the card's buttons to the daemon's notice route, the same one the desktop banner's buttons and the app window's rail line use. The card is still up while this runs (see the noticeAct case in state.ts), so a refusal has somewhere to be said: the daemon answers 500 when it could not write the snooze or the done (see internal/ipc/notices.go), and a press that goes nowhere must not look like it took. Input: the notice and the button pressed ("done", "hour", "evening" or "tomorrow"). Output: nothing; the daemon's own follow-up "notice" event is what replaces the card with its one-line confirmation when the press did take. */
function actOnNotice(n: Notice, act: string): void {
  const { base, token } = endpoint();
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers[TOKEN_HEADER] = token;
  void fetch(`${base}/notices/${encodeURIComponent(n.kind)}/${encodeURIComponent(n.id || "-")}/action`, {
    method: "POST",
    headers,
    body: JSON.stringify({ title: n.title, body: n.body, action: act }),
  })
    .then((r) => {
      if (!r.ok) noticeFailed();
    })
    .catch(noticeFailed);
}

/** Puts one muted line at the bottom of the notice card saying the button did not take, for a daemon that answered with an error or could not be reached at all. Input: none. Output: nothing; the line is appended to the card, once however many presses fail, and goes when the card is next redrawn for a different notice (see renderNotice). */
function noticeFailed(): void {
  if (!noticeEl.querySelector(".nf"))
    noticeEl.insertAdjacentHTML("beforeend", `<div class="nf">Couldn't do that</div>`);
}

/** The six seconds a notice stays up. Input: the view. Output: nothing; the timer is cleared and started again from the top whenever the notice itself changes, and left cleared while the pointer is over the card, which is what pauses it — the card then gets its full six seconds again when the pointer leaves. The card is asked directly whether the pointer is on it rather than only reading noticeHeld, because a notice landing under a pointer that is already there fires no pointerenter of its own, so the flag would say the card is free while the user is reading it (a tick can post five notices in a row; see maybeTaskNotices in internal/proactive/notify.go). */
let noticeTimer: ReturnType<typeof setTimeout> | undefined;
function armNotice(v: View): void {
  clearTimeout(noticeTimer);
  noticeTimer = undefined;
  if (!v.notice || v.noticeHeld || noticeEl.matches(":hover")) return;
  noticeTimer = setTimeout(() => dispatch({ kind: "noticeGone" }), NOTICE_MS);
}

// Bound once to the bubble itself, which survives every render, rather than to the markup inside it, which does not.
noticeEl.addEventListener("pointerenter", () =>
  dispatch({ kind: "noticeHold" }),
);
noticeEl.addEventListener("pointerleave", () =>
  dispatch({ kind: "noticeRelease" }),
);
noticeEl.addEventListener("click", (e) => {
  // A press on one of the card's own buttons is that button; a press anywhere else on the card is still the click that opens the app window.
  const act = (e.target as HTMLElement).closest<HTMLElement>("button.na")?.dataset.act;
  dispatch(act ? { kind: "noticeAct", act } : { kind: "noticeClick" });
});

/** Opens the main app window at what the clicked notice was about. The window is opened through the daemon, which broadcasts the instruction the Rust side acts on (see internal/ipc/window.go), and the screen and row are left in the localStorage both windows share, under a key beside the theme and the hover position, for the app window to read when it opens. Input: the notice's place ("tasks", "days") and row id, either of which may be empty when the notice pointed at nothing in particular. Output: nothing; a daemon that cannot be reached simply leaves the window shut. */
function openNotice(place: string, id: string): void {
  try {
    localStorage.setItem(OPEN_AT_KEY, JSON.stringify({ place, id }));
  } catch {
    /* storage blocked */
  }
  const { base, token } = endpoint();
  const headers: Record<string, string> = {};
  if (token) headers[TOKEN_HEADER] = token;
  void fetch(`${base}/window?action=open`, { method: "POST", headers }).catch(
    () => {
      /* the daemon is gone, so there is nothing to open the window */
    },
  );
}

/** Ticks the live step list's elapsed-seconds numbers up and advances its braille working grid while an ask is running; nothing else re-renders on its own between daemon events, so a slow tool call would otherwise sit on a stale number and a frozen grid until the next one arrives. It runs ten times a second because that is the rate the grid needs to read as motion; the seconds numbers, which only ever changed once a second, cost nothing extra for being written more often. Input: none. Output: nothing. */
const STEP_TICK_MS = 100;
let stepTicker: ReturnType<typeof setInterval> | undefined;
function startStepTicker(): void {
  stopStepTicker();
  stepTicker = setInterval(tickSteps, STEP_TICK_MS);
}
function stopStepTicker(): void {
  clearInterval(stepTicker);
  stepTicker = undefined;
}

/** The four-row braille bar for Ora's voice, read straight off the speaker Waveform. Input: the Waveform to render. Output: the row markup, one ".vw-row" span per line. */
function voiceWaveRowsHtml(w: Waveform): string {
  return w
    .render()
    .map((row) => `<span class="vw-row">${esc(row)}</span>`)
    .join("");
}

/** The live-voice waveform's inner content: Ora's own four rows, always — the silent centreline while she listens, animated once she speaks — split out from voiceWaveHtml so a level event can patch just this in place (see scheduleVoiceWaveRepaint) instead of tearing down and rebuilding the ".vwave" span, title attribute included, on every one of up to 20 ticks a second. There is no user-microphone grid: this is a hands-free session, so only Ora's voice is drawn, and it stays on screen for the session's whole length rather than appearing only once she starts speaking (gating it on speakerWave.smoothed > 0 made the row jump in and out). Input: none, reads the module's own speakerWave. Output: the inner HTML. */
function voiceWaveInnerHtml(): string {
  return `<span class="vw-spk">${voiceWaveRowsHtml(speakerWave)}</span>`;
}

/** The live transcript under the voice-mode grid: the last thing the user said and the last thing Ora said, one line each. Both come straight off the current matter's last turn — a live session's "heard" and "said" events already fold into a turn's q and a the same way a typed question and its answer do (see the voiceEvent case in state.ts) — so there is nothing new to store here, only to read. Input: the matter the session's turns are landing in. Output: the two lines' HTML, either one left out while it has nothing yet, or "" while neither does. */
function voiceTranscriptHtml(m: Matter): string {
  const last = m.turns[m.turns.length - 1];
  const you = last?.q ? `<div class="vs-you">${esc(last.q)}</div>` : "";
  const ora = last?.a ? `<div class="vs-ora">${esc(last.a)}</div>` : "";
  return you || ora ? `<div class="vs-transcript">${you}${ora}</div>` : "";
}

/** The stop control's icon: a filled square, the universal "stop" glyph a recorder uses in place of its round record dot. Input: none. Output: the svg's HTML. */
function voiceStopIcon(): string {
  return `<svg viewBox="0 0 16 16" width="12" height="12"><rect x="3" y="3" width="10" height="10" rx="1.5"/></svg>`;
}

/** The whole card while a live voice session runs, replacing the input and the thread entirely — the way Gemini Live and ChatGPT's own voice mode take over the screen instead of sharing it with the composer. Ora's braille rows (see voiceWaveInnerHtml) sit centred as the surface's centrepiece; the state word and transcript above and below it read off the same view and matter the resting card would; the stop control ends the session the same way Shift+Space does (see the click handler in render()). Input: the view and the matter the session's turns are landing in. Output: the surface's HTML. */
function voiceSurfaceHtml(v: View, m: Matter): string {
  return `
    <div class="voicebox">
      <div class="vs-state">${esc(voiceStateWord(v.voiceState))}</div>
      <span class="vwave">${voiceWaveInnerHtml()}</span>
      ${voiceTranscriptHtml(m)}
      <div class="vs-ctl">
        <button type="button" class="vs-stop" aria-label="Stop voice">${voiceStopIcon()}</button>
        <span class="vs-hint">Shift+Space</span>
      </div>
    </div>
  `;
}

/** The words shown after the shortcuts in the input's placeholder used to be the only way a user with the input empty and nothing running learned that Space and Shift+Space did anything at all. They now live permanently beside the input instead, in the same slot the window's context chip uses once the daemon reports one, at DESIGN.md's text-micro (11px/1.3/500/+0.02em) rather than the chip's own size, so they read as a quiet legend rather than a second copy of what the chip says. */
const HINT_TEXT = "⎵ dictate · ⇧⎵ voice";

/** What the input's placeholder actually renders. Same as state.ts's placeholder(v) except at rest, where the full RESTING_PLACEHOLDER sentence (which used to carry the two shortcut hints inline) shortens to "Ask Ora" now that the hints live beside the input instead (see HINT_TEXT and cardHtml). Input: the view. Output: the placeholder text to render. */
function displayPlaceholder(v: View): string {
  const p = placeholder(v);
  return p === RESTING_PLACEHOLDER ? "Ask Ora" : p;
}

/** The whole card: the input line, the thread of what has been asked so far, and the footer — or, for the whole length of a live voice session, the voice-mode surface instead (see voiceSurfaceHtml). The input carries a fixed aria-label rather than leaning on its placeholder for a name: the placeholder is also this window's status line ("Listening…", "Live voice on"), so it changes under a user who is part way through typing, and a control whose name moves has no name. Input: the view and the matter the turns belong to. Output: the card's HTML. */
function cardHtml(v: View, m: Matter): string {
  if (v.voice) return voiceSurfaceHtml(v, m);
  const tag = daemonUp ? "daemon" : mockMode ? "mock" : "daemon offline";
  // A 9px dot telling "not connected" apart from "idle" by colour alone is invisible to begin with, so the first thing a user sees when the daemon is down is not a red dot but these two words, in the same place the dot sat and in the "something wrong" colour rather than a shape nobody can name.
  const status = daemonUp
    ? `<span class="dot ${dotClass(v, daemonUp)}" role="img" title="${esc(dotLabel(v, daemonUp))}" aria-label="${esc(dotLabel(v, daemonUp))}"></span>`
    : `<span role="status" style="flex:none;color:var(--bad);font:400 12px/1.4 var(--body);">${esc(dotLabel(v, daemonUp))}</span>`;
  // Permanent, not just while resting: the chip shows the window's own context once the daemon reports one, and the shortcut hints otherwise — never both, since there is only room for one aside next to the input.
  const ctx = v.contextChip
    ? `<span class="ctx">${esc(v.contextChip)}</span>`
    : `<span class="ctx" style="font:500 11px/1.3 var(--body);letter-spacing:.02em;color:var(--mute);">${HINT_TEXT}</span>`;

  return `
    <div class="in${v.dictating ? " holding" : ""}">
      ${status}
      <input class="q" value="${esc(v.input)}" aria-label="Ask Ora" placeholder="${esc(displayPlaceholder(v))}" />
      <span class="wave"><i></i><i></i><i></i></span>
      ${ctx}
    </div>
    ${threadHtml(v, m)}
    <div class="foot"><span>↵ ask</span><span>ctrl↵ new thread</span><span>esc close</span><span class="tag">${tag}</span></div>
  `;
}

/** Repaints just the live-voice waveform's rows in place, patching the ".vwave" span's own inner HTML rather than going through render()'s full card rebuild. Scheduled at most once per animation frame (see scheduleVoiceWaveRepaint), so however many "level" events arrive between two frames, the DOM only gets touched once. Input: none. Output: nothing; a no-op once the session has ended and render() has already removed the span. */
function repaintVoiceWave(): void {
  const el = root.querySelector<HTMLElement>(".vwave");
  if (el) el.innerHTML = voiceWaveInnerHtml();
}

/** Coalesces a burst of "level" events (up to 20 a second) into at most one DOM write per animation frame. Input: none. Output: nothing; a repaint already pending for this frame is left to run rather than queuing a second one. */
let voiceWaveFrame: number | undefined;
function scheduleVoiceWaveRepaint(): void {
  if (voiceWaveFrame !== undefined) return;
  voiceWaveFrame = requestAnimationFrame(() => {
    voiceWaveFrame = undefined;
    repaintVoiceWave();
  });
}

/** When the last real "level" event arrived, in milliseconds since the epoch, so the breath below knows whether the daemon is still driving the grid. */
let lastLevelAt = 0;

/** How long the grid waits after the last level event before breathing on its own, in milliseconds: three of the daemon's 50 ms ticks, so a real reading always wins. */
const BREATH_AFTER_MS = 150;

/** The timer that keeps the voice grid breathing while the daemon is quiet. The daemon skips a level tick when nothing changed (see levelChangeThreshold in internal/ipc/voice.go), which is exactly the silent stretch the breath exists for, so the breath cannot be driven off the daemon's events and runs on this window's own clock instead. */
let breathTimer: ReturnType<typeof setInterval> | undefined;

/** Starts or stops the breath with the session. Input: whether a live voice session is on. Output: nothing. Under reduced motion the grid holds still, so nothing is started. */
function syncBreath(voiceOn: boolean): void {
  if (voiceOn && breathTimer === undefined && !reduce) {
    breathTimer = setInterval(() => {
      // A session outlives a hide of this window, and nothing this draws is on screen while it is hidden, so the ten wakeups a second do no work until it comes back — the same guard tickSteps has. hideWindow stops the timer outright as well, because a hidden GTK window under X11 does not reliably set document.hidden.
      if (document.hidden) return;
      if (Date.now() - lastLevelAt < BREATH_AFTER_MS) return;
      speakerWave.update(voiceGridAmplitude(0));
      scheduleVoiceWaveRepaint();
    }, 100);
  }
  if (!voiceOn && breathTimer !== undefined) {
    clearInterval(breathTimer);
    breathTimer = undefined;
  }
}

/** The amplitude fed to the voice-mode grid's Waveform for one "level" tick. A real reading above the noise floor drives it as-is; below that — Ora listening rather than speaking, which is most of a session — a small deterministic sine (0.05-0.12 at 0.4Hz, so one full breath takes about 2.5s) takes over instead, the same idle "breathing" a voice orb does, so the grid still reads as alive instead of flatlining the moment she stops talking. Input: the level event's speaker reading (0-1). Output: the amplitude to update the Waveform with. */
function voiceGridAmplitude(speaker: number): number {
  const BREATH_FLOOR = 0.02;
  if (speaker > BREATH_FLOOR) return speaker;
  const t = Date.now() / 1000;
  return 0.085 + 0.035 * Math.sin(2 * Math.PI * 0.4 * t);
}

/** The check or cross a finished step's mark draws itself with (see the "draw" keyframe in styles.css) — an SVG stroke rather than a character so it can animate stroke-dashoffset instead of just popping in. Input: which one. Output: the svg's HTML; its colour comes from .step-mark's CSS via currentColor, the same way the old plain "✓"/"✕" text did. */
function markSvg(kind: "ok" | "err"): string {
  const d = kind === "ok" ? "M3 8.5L6.5 12L13 4" : "M4 4L12 12M12 4L4 12";
  return `<svg class="mark-svg" viewBox="0 0 16 16" width="12" height="12"><path d="${d}"/></svg>`;
}

/** One row of the live step list: an icon by tool kind (breathing while the step runs), the plain-English label (shimmering while it runs), a check or cross that draws itself once it finishes, and how many seconds it has run — ticked live for a step still running (see startStepTicker), fixed once it finishes. A failed step also gets the daemon's error text under it, since that is the one row an "Ora stopped" answer alone does not explain. Input: the step and the current time. Output: the row's HTML (plus its error line, if any). */
function stepRowHtml(s: ToolStep, now: number): string {
  const seconds = stepSeconds(s, now);
  const cls = s.error ? "err" : s.finishedAt !== undefined ? "ok" : "run";
  const mark = cls === "run" ? "" : markSvg(cls);
  return `<div class="step ${cls}">
      <span class="step-ic ${stepIconKind(s.name)}"></span>
      <span class="step-label">${esc(stepLabel(s.name, s.detail))}</span>
      <span class="step-mark">${mark}</span>
      <span class="step-time">${seconds > 0 ? seconds.toFixed(1) + "s" : ""}</span>
    </div>${s.error ? `<div class="step-err">${esc(s.error)}</div>` : ""}`;
}

/** How many braille cells wide the live row's working grid is drawn. */
const WORKING_CELLS = 24;

/** What the live row shows before the first tool call starts, and what it falls back to between one tool call finishing and the next starting: one row of the same braille dot grid live voice draws, which is Ora's single signature for "listening or working". It replaces the words that used to sit here. Input: none, reads the clock. Output: the row's characters — a fixed first frame under reduced motion, since nothing repaints it there. */
function workingGridText(): string {
  return workingRow(WORKING_CELLS, reduce ? 0 : Date.now());
}

/** The placeholder row shown before any tool call has started. Same four children a real step's row has — icon, mark, time, all empty — as stepRowHtml, not the bare icon-and-label pair this used to be: that structural parity is what lets the live row survive the change from this placeholder to the first real step in place (see patchLiveSteps) instead of being swapped for a differently-shaped element, which was one more place the shimmer used to restart. The label carries the "work" class while it holds the grid, which is what turns the shimmer off for it in styles.css. Input: none. Output: the row's HTML. */
function gridRowHtml(): string {
  return `<div class="step run"><span class="step-ic"></span><span class="step-label work">${workingGridText()}</span><span class="step-mark"></span><span class="step-time"></span></div>`;
}

/** The live step list shown in place of an answer while an ask is still running. A row of the braille dot grid stands in until the first tool call arrives, so the daemon's own "Checking." status text is never what the user sees — that hardcoded, unchanging line was the entire complaint this replaces. Only ever builds the list from scratch, for the first render of a turn; every event after that patches this same markup in place instead (see patchLiveSteps), which is what keeps the running row's animations from restarting on every tool call. Input: the turn's steps so far. Output: the list's HTML. */
function stepsHtml(steps: ToolStep[]): string {
  if (steps.length === 0)
    return `<div class="steps">${gridRowHtml()}</div>`;
  const now = Date.now();
  return `<div class="steps">${steps.map((s) => stepRowHtml(s, now)).join("")}</div>`;
}

/** Fades one animated element's contents out and back in instead of swapping the element itself, so a CSS animation running on it (the live row's shimmer, its icon's breathe) keeps running through the change instead of restarting the way replacing the element with a fresh one would. Input: the element and its next inner HTML. Output: nothing; skipped under reduced motion, where every other animation on the card is already cut. */
function crossFadeText(el: HTMLElement, html: string): void {
  if (reduce) {
    el.innerHTML = html;
    return;
  }
  el.style.opacity = "0";
  clearTimeout(fadeTimer);
  fadeTimer = setTimeout(() => {
    fadeTimer = undefined;
    el.innerHTML = html;
    el.style.opacity = "1";
  }, 150);
}

/** Updates the one live step row in place: the icon's kind and the label's text (cross-faded, see crossFadeText), and only when either actually changed — a "status" event that leaves the running step exactly as it was must touch nothing, or it would blank the ticking elapsed-time number for no reason (see tickSteps) and cross-fade text that never changed. With no step running the label holds the braille grid instead, which repaints from the clock ten times a second (see tickSteps) and so is rewritten rather than cross-faded: a fade on every frame would be a flicker, not a transition. Input: the row element, kept alive across the whole turn by patchLiveSteps, and the step it should now show, or undefined for the grid before the first tool call or between one finishing and the next starting. Output: nothing. */
function updateLiveRow(row: HTMLElement, s: ToolStep | undefined): void {
  const iconEl = row.querySelector<HTMLElement>(".step-ic");
  const labelEl = row.querySelector<HTMLElement>(".step-label");
  if (!iconEl || !labelEl) return;
  const timeEl = row.querySelector<HTMLElement>(".step-time");
  if (!s) {
    if (!labelEl.classList.contains("work")) {
      iconEl.className = "step-ic";
      labelEl.classList.add("work");
      if (timeEl) timeEl.textContent = "";
    }
    labelEl.textContent = workingGridText();
    return;
  }
  const html = esc(stepLabel(s.name, s.detail));
  if (!labelEl.classList.contains("work") && labelEl.innerHTML === html) return;
  labelEl.classList.remove("work");
  iconEl.className = `step-ic ${stepIconKind(s.name)}`;
  if (timeEl) timeEl.textContent = "";
  crossFadeText(labelEl, html);
}

/** Applies a "tool" or "status" daemon event to the live step list without the full-card rebuild render() does: every step that has now finished is appended as its own row, once, and the one row still running is updated in place (see updateLiveRow) instead of recreated. Recreating it on every tool call — and once a second besides, from the elapsed-time ticker — was what actually restarted the shimmer and breathe animations; the reported "stuttering" was that restart, not anything about the animations themselves. Input: the view, already patched by the reducer. Output: whether the patch applied; false when there is no live row on screen yet to patch (the very first render of a turn), which tells the caller to fall back to the ordinary render(). */
function patchLiveSteps(v: View): boolean {
  const wrap = root.querySelector<HTMLElement>(".steps");
  const row = wrap?.lastElementChild as HTMLElement | null;
  if (!wrap || !row || !row.classList.contains("run")) return false;
  const turns = v.matters[v.current]?.turns ?? [];
  const steps = turns[turns.length - 1]?.steps ?? [];
  const now = Date.now();
  const last = steps[steps.length - 1];
  // The index of the step still running, or steps.length when none is (the gap between one finishing and the next starting).
  const openIdx =
    last && last.finishedAt === undefined ? steps.length - 1 : steps.length;
  // Every already-finished step not yet its own row gets appended now, in the order they ran, just before the live row.
  for (let i = wrap.children.length - 1; i < openIdx; i++) {
    const tmp = document.createElement("template");
    tmp.innerHTML = stepRowHtml(steps[i], now);
    while (tmp.content.firstChild)
      wrap.insertBefore(tmp.content.firstChild, row);
  }
  updateLiveRow(row, openIdx < steps.length ? steps[openIdx] : undefined);
  fit();
  return true;
}

/** Ticks the running step's own elapsed-seconds number without touching anything else — what startStepTicker calls once a second instead of the full render() it used to, which tore the whole steps list down and rebuilt it every second an ask ran, restarting its shimmer and breathe animations on a one-second loop regardless of whether any tool call had actually happened. Input: none, reads the live DOM and the view. Output: nothing; does nothing once the running step has finished (the gap before the next one starts, or the turn's last step already done) rather than show a stale number. */
function tickSteps(): void {
  // An ask, and a computer-use job even more so, runs for minutes; none of what this draws is on screen while the window is hidden, so the ten wakeups a second do no work until it comes back.
  if (document.hidden) return;
  const turns = view.matters[view.current]?.turns ?? [];
  const last = turns[turns.length - 1];

  // The braille row standing in while no tool call is running is drawn from the clock, so this is the only thing that moves it; under reduced motion it stays on the frame it was built with.
  if (!reduce) {
    const gridEl = root.querySelector<HTMLElement>(
      ".steps .step.run .step-label.work",
    );
    if (gridEl) gridEl.textContent = workingGridText();
  }

  // A live job's title line carries its own running seconds, next to the goal — ticked here in place for the same reason a step's own time is: rebuilding the row on every tick would restart its animations.
  if (last?.job && isJobLive(last.job.state)) {
    const elapsedEl = root.querySelector<HTMLElement>(".qq .elapsed");
    if (elapsedEl)
      elapsedEl.textContent = ` · ${Math.floor((Date.now() - last.job.startedAt) / 1000)}s`;
  }

  const timeEl = root.querySelector<HTMLElement>(".steps .step.run .step-time");
  if (!timeEl) return;
  const steps = last?.steps ?? [];
  const s = steps[steps.length - 1];
  if (!s || s.finishedAt !== undefined) return;
  const seconds = stepSeconds(s, Date.now());
  timeEl.textContent = seconds > 0 ? seconds.toFixed(1) + "s" : "";
}

/** The one-line summary a finished turn's step list collapses to ("4 steps · 6.2 s"), click to expand back into the full list (see toggleSteps). Input: the view (for stepsOpen's arrow) and the turn's steps. Output: the line's HTML. Only called once stepsSummaryLine has something to say. */
function stepsSummaryHtml(v: View, steps: ToolStep[]): string {
  return `<div class="stepsum">${esc(stepsSummaryLine(steps))}<span class="tw">${v.stepsOpen ? "⇧ hide" : "⇩ show"}</span></div>`;
}

/** The job on the turn on screen, if there is one. Input: none, reads the module's own view. Output: the job, or undefined. Used by the control buttons' click handlers, which are bound once per render and so cannot close over the turn a later render replaces. */
function currentJobView(): JobMeta | undefined {
  const turns = view.matters[view.current]?.turns ?? [];
  return turns[turns.length - 1]?.job;
}

/** The title line above a turn's steps: the question for an ask, or, for a job, the goal plus its running seconds (ticked in place by tickSteps, same as a step's own time) and, for one Escape away from being stopped, the confirmation prompt. Input: the view (for confirmStopJob) and the turn. Output: the line's HTML. */
function qqHtml(v: View, last: Matter["turns"][number]): string {
  if (!last.job) return `<div class="qq">${esc(last.q)}</div>`;
  const elapsed = Math.floor((Date.now() - last.job.startedAt) / 1000);
  const confirm = v.confirmStopJob
    ? `<span class="confirm"> — esc again to stop</span>`
    : "";
  return `<div class="qq">${esc(last.q)}<span class="elapsed"> · ${elapsed}s</span>${confirm}</div>`;
}

/** The Stop and Pause/Resume controls on a live job's card, the second one label alone deciding which of the two it means. Input: the job (for its state, which says whether the second button reads Pause or Resume). Output: the controls' HTML. The click handlers below read the job fresh off the view rather than close over this one, since a render between the click and here would leave them holding a stale copy. */
function jobControlsHtml(job: JobMeta): string {
  const paused = job.state === "paused";
  return `<div class="jobctl"><button type="button" class="job-stop">Stop</button><button type="button" class="job-pauseresume">${paused ? "Resume" : "Pause"}</button></div>`;
}

/** The one question a stuck job is waiting on, shown as the current row under its steps — in the same big type an answer reads at, since it is the one thing on the card asking for a reply right now. The composer underneath it is already focused (render() focuses the input on every render), and Enter there posts the reply to /act/{id}/answer instead of asking something new (see the submit case in state.ts). Input: the job. Output: the row's HTML, or "" when nothing is asked right now. */
function jobQuestionHtml(job: JobMeta): string {
  if (!job.question) return "";
  return `<div class="jobq">${esc(job.question)}</div>`;
}

/** The cost line a finished job's card ends on: how many rounds it took and what they cost in tokens. Input: the job's spend. Output: the line's HTML. */
function spendLineHtml(spend: NonNullable<JobMeta["spend"]>): string {
  return `<div class="spend">${spend.rounds} round${spend.rounds === 1 ? "" : "s"} · ${spend.input} in · ${spend.cached} cached · ${spend.output} out</div>`;
}

/** The thread under the input line: every finished turn folded to a grey question-and-answer pair, then the turn on screen with its question in grey; the live step list or its collapsed summary; the answer in big type; and the evidence fold-out. Input: the view and the matter. Output: the thread's HTML, or "" when nothing has been asked. */
function threadHtml(v: View, m: Matter): string {
  const last = m.turns[m.turns.length - 1];
  if (!last) return "";

  const folded = m.turns
    .slice(0, -1)
    .map(
      (t) =>
        `<div class="prev"><b>${esc(t.q)}</b>${esc(t.a.replace(/<\/?b>/g, ""))}</div>`,
    )
    .join("");
  const evidence = last.evidence ?? [];
  const steps = last.steps ?? [];
  // The "answer" event lands a beat before "done" flips the state, so a turn can be mid-transition with an answer already in hand but state still "asking" — last.a, not v.state alone, is what decides whether the step list is still the live one or something to collapse.
  const stillAsking = v.state === "asking" && !last.a;

  const stepsBlock = stillAsking
    ? stepsHtml(steps)
    : steps.length === 0
      ? ""
      : stepsCollapsed(steps, v.stepsOpen)
        ? stepsSummaryHtml(v, steps)
        : stepsHtml(steps);

  return `<div class="thread">
      ${folded}
      ${qqHtml(v, last)}
      ${last.job && isJobLive(last.job.state) ? jobControlsHtml(last.job) : ""}
      ${stepsBlock}
      ${last.job ? jobQuestionHtml(last.job) : ""}
      ${stillAsking ? "" : `<div class="a">${last.a ? rich(last.a) : ""}</div>`}
      ${last.job?.spend ? spendLineHtml(last.job.spend) : ""}
      ${
        evidence.length > 0
          ? `<div class="evd">
              <div class="h">read <b>${esc(evidence[0].title)}</b>${metaSuffix(evidence[0])}${evidence.length > 1 ? ` +${evidence.length - 1} more` : ""}<span class="tw">${v.evidenceOpen ? "⇧ hide" : "⇩ show"}</span></div>
              ${v.evidenceOpen ? `<div class="items">${evidence.map(evidenceBlock).join("")}</div>` : ""}
            </div>`
          : detailHtml(v, last)
      }
    </div>`;
}

/** The fold under a failed ask, holding the whole of a message the answer slot only took one line of (see state.errorLine). It is the same fold-out an answer's sources use, which means the same look and, more to the point, the same 300px scrolling body: a provider's error has no size limit, and nothing it sends can grow the window past the screen from in there. Input: the view and the turn on screen. Output: the fold's HTML, or "" when the turn kept nothing back. */
function detailHtml(v: View, last: Matter["turns"][number]): string {
  if (!last.detail) return "";
  return `<div class="evd">
      <div class="h">the whole message<span class="tw">${v.evidenceOpen ? "⇧ hide" : "⇩ show"}</span></div>
      ${v.evidenceOpen ? `<div class="items"><div class="body">${esc(last.detail)}</div></div>` : ""}
    </div>`;
}

/** The part of an evidence line that follows its title, already escaped and with its leading separator, or "" when the meta says nothing the title has not. Input: the item. Output: the HTML fragment. */
function metaSuffix(e: { title: string; meta: string }): string {
  const parts = sourceMeta(e.title, e.meta);
  return parts.length > 0 ? ` · ${esc(parts.join(" · "))}` : "";
}

/** Renders one evidence item as its own block: title, meta and body. Input: an Evidence entry. Output: the block's HTML. */
function evidenceBlock(e: {
  title: string;
  meta: string;
  body?: string;
}): string {
  return `<div class="body"><div class="im"><b>${esc(e.title)}</b>${metaSuffix(e)}</div>${rich(e.body ?? "")}</div>`;
}

/** Applies an event to the pure state, re-renders, and carries out any effect it returns. Typing is the one event that skips the re-render, because the input element already holds the new value and rebuilding it on every keystroke would move the caret and resize the OS window each time. A "tool" or "status" event that leaves the ask still running patches the live step list in place instead (see patchLiveSteps), which is what keeps its shimmer and breathe animations from restarting on every single tool call. Input: the event. Output: nothing. */
export function dispatch(event: Parameters<typeof step>[1]): void {
  // A fresh session starts its waveform silent: reusing the smoothed values a just-ended session left behind would show a leftover bar for a moment before the first real level arrives.
  if (event.kind === "voiceOn") {
    speakerWave = new Waveform(VOICE_WAVE_WIDTH);
  }
  const before = view.state;
  const wasAsking = before === "asking";
  // A window that was only up to show a notice goes again with it, whether the six seconds ran out or the card was clicked through.
  const wasNoticeAlone = view.notice !== undefined && view.noticeAlone === true;
  const result = step(view, event);
  view = result.view;
  syncBreath(Boolean(view.voice));
  // The moment an ask finishes gets its own render below (see collapseStepsThenRender), timed to land after the 200ms shrink instead of snapping the step list down immediately.
  const justFinished = wasAsking && view.state !== "asking";
  const stillRunning =
    event.kind === "daemonEvent" &&
    (event.ev.type === "tool" || event.ev.type === "status") &&
    wasAsking &&
    view.state === "asking";
  const patched = stillRunning && patchLiveSteps(view);
  // A "level" tick can arrive up to 20 times a second; it only ever moves the waveform, so it feeds the Waveform instance directly and patches its rows in place (see scheduleVoiceWaveRepaint) instead of taking the full render() path below. The event still carries a mic reading (see renderLevelEvent in waveform.ts) — nothing draws it any more, so it is read into view.voiceLevel and left there unused rather than drawn, which is harmless.
  const isVoiceLevel = event.kind === "voiceEvent" && event.ev.type === "level";
  if (isVoiceLevel) {
    lastLevelAt = Date.now();
    speakerWave.update(voiceGridAmplitude(view.voiceLevel?.speaker ?? 0));
  }
  // "asked" only writes down which conversation the daemon put the question in, which nothing on the card draws, so it is not worth a rebuild and a window resize mid-question.
  if (
    event.kind !== "type" &&
    event.kind !== "asked" &&
    !justFinished &&
    !patched &&
    !isVoiceLevel
  )
    render(view);
  if (isVoiceLevel) scheduleVoiceWaveRepaint();
  // Only the notice's own events restart its six seconds: a brief that arrived while a question was being answered must not be held up by every tool event that follows.
  if (event.kind.startsWith("notice")) armNotice(view);
  if (wasNoticeAlone && !view.notice) hideWindow();
  if (result.effect?.kind === "close") hideWindow();
  if (result.effect?.kind === "openNotice")
    openNotice(result.effect.place, result.effect.id);
  if (result.effect?.kind === "noticeAct")
    actOnNotice(result.effect.notice, result.effect.act);
  if (result.effect?.kind === "ask") {
    startAsk(result.effect.question, result.effect.conversation);
    startStepTicker();
  }
  if (result.effect?.kind === "startJob") {
    startJob(result.effect.goal);
    startStepTicker();
  }
  if (result.effect?.kind === "stopJob") void actStop(result.effect.id);
  if (result.effect?.kind === "answerJob")
    void actAnswer(result.effect.id, result.effect.text);
  if (justFinished) {
    stopStepTicker();
    collapseStepsThenRender();
  }
  if (before !== "answered" && view.state === "answered") {
    if (devEvidenceOpen && !view.evidenceOpen)
      dispatch({ kind: "toggleEvidence" });
    askNextDevQuestion();
  }
}

/** Plays the 200ms shrink when a finished turn's step list is about to fold down to its one-line summary, instead of the next render just snapping straight to the short form. Runs the instant a turn finishes, while the full list from the render before this one is still on screen: measures its current height, then transitions it to nothing before the deferred render() below settles the DOM into its normal (unanimated) collapsed shape. Plain immediate render instead — no animation — when there is nothing to collapse to (stepsCollapsed says no, e.g. a step failed and stays expanded) or the viewer asked for reduced motion. Input: none, reads the view and the DOM the last render left behind. Output: nothing. */
function collapseStepsThenRender(): void {
  const turns = view.matters[view.current]?.turns ?? [];
  const last = turns[turns.length - 1];
  const wrap = root.querySelector<HTMLElement>(".steps");
  if (
    !last ||
    !wrap ||
    reduce ||
    !stepsCollapsed(last.steps ?? [], view.stepsOpen)
  ) {
    render(view);
    return;
  }
  wrap.style.height = `${wrap.getBoundingClientRect().height}px`;
  wrap.style.overflow = "hidden";
  void wrap.offsetHeight; // commits the explicit height above so the transition below has something to animate away from, instead of starting from "auto"
  wrap.style.transition = "height 200ms ease, opacity 200ms ease";
  requestAnimationFrame(() => {
    wrap.style.height = "0px";
    wrap.style.opacity = "0";
  });
  clearTimeout(collapseTimer);
  collapseTimer = setTimeout(() => {
    collapseTimer = undefined;
    render(view);
  }, 200);
}

/** Sends the question that has just been put on the pending turn: to the daemon when it answered the last probe, to the mock script under ?mock=1, and otherwise straight to the offline sentence in the answer slot. Input: the question, and the conversation to append it to, or undefined to have the daemon open one (see askConversation). Output: nothing; the answer arrives later as daemon events. */
function startAsk(q: string, conversation: string | undefined): void {
  const m = view.matters[view.current];
  if (!m) return;
  if (daemonUp) {
    askId = undefined;
    void ask(q, view.contextText || m.context, conversation)
      .then((res) => {
        askId = res.id;
        // The daemon says which conversation it stored the question in. Writing it down here is the whole of the follow-up: the next question names it, and the daemon then puts the turns already in it in front of the model.
        if (res.conversationId)
          dispatch({ kind: "asked", conversationId: res.conversationId });
      })
      .catch(() =>
        offline("Ora's daemon stopped answering, so I could not look this up."),
      );
  } else if (mockMode) {
    scheduleAnswer(m.id);
  } else {
    offline("Ora's daemon is not running, so I cannot look anything up.");
  }
}

/** Starts a computer-use job for a goal, the "do:" counterpart to startAsk. Input: the goal in the user's own words. Output: nothing; the job's progress arrives later as "act" daemon events, matched against jobId once the POST /act reply names it. */
function startJob(goal: string): void {
  if (!daemonUp) {
    offline("Ora's daemon is not running, so I cannot start that.");
    return;
  }
  jobId = undefined;
  void actStart(goal).then((id) => {
    if (!id) {
      offline("Ora's daemon would not start that job.");
      return;
    }
    jobId = id;
    dispatch({ kind: "jobStarted", id });
  });
}

/** The daemon's id for the dictation on screen, undefined while the microphone is closed. The window matches the daemon's own "dictation" event against it, because that event is how a dictation the silence gate ended gets its words back. */
let dictateId: string | undefined;

/** The open dictation's start request, awaited by endDictation so a key pressed before the daemon answered still stops the right recording. */
let dictation: Promise<string> | undefined;

/** Whether the dictation on screen has already had its words applied. The transcript can arrive twice — once on the daemon's event and once on a stop reply — and only the first of them is used. */
let dictateTaken = true;

/** Opens the daemon's microphone and puts the window into its listening look. Input: none. Output: nothing; the transcript arrives later through finishDictation, from whichever of the two sources gets there first. */
function beginDictation(): void {
  // Dictation and a live session would fight over the same microphone, and there is nothing to dictate into when the daemon is down.
  if (!daemonUp || view.voice || dictation) return;
  const { base, token } = endpoint();
  dictateId = undefined;
  dictateTaken = false;
  dispatch({ kind: "dictating" });
  dictation = startDictation(base, token);
  dictation
    .then((id) => {
      dictateId = id;
    })
    .catch(() => failDictation());
}

/** Whether a stop has gone to the daemon and not come back. The transcript can take minutes to arrive (whisper queues behind a meeting decode), and until it does the view still says the window is dictating — so without this a second Space would call endDictation again, and the daemon, having already closed that recording, would answer that second stop with a 404 and an empty transcript, which is then taken as the words and drops the real ones as a duplicate. */
let dictateStopping = false;

/** Closes the microphone, if the daemon has not already closed it itself. Input: none. Output: a promise for when the transcript has been applied. */
async function endDictation(): Promise<void> {
  const started = dictation;
  if (!started || dictateStopping) return;
  dictateStopping = true;
  const { base, token } = endpoint();
  try {
    finishDictation(await stopDictation(base, token, await started));
  } catch {
    failDictation();
  } finally {
    dictateStopping = false;
  }
}

/** Says the dictation could not be done, once, whether the microphone would not open or whisper would not transcribe. Input: none. Output: nothing. */
function failDictation(): void {
  if (dictateTaken) return;
  dictateTaken = true;
  dictation = undefined;
  dispatch({ kind: "dictationFailed" });
  clearHintSoon();
}

/** Puts the words of a finished dictation into the input, once, whichever source they came from. Input: the transcript, empty when nothing was said. Output: nothing; an empty transcript leaves the input alone and shows a one-second hint instead. */
function finishDictation(text: string): void {
  if (dictateTaken) return;
  dictateTaken = true;
  dictation = undefined;
  dispatch({ kind: "dictated", text });
  clearHintSoon();
}

/** Takes the hint back down a second after it went up, if one did. Input: none. Output: nothing. */
function clearHintSoon(): void {
  if (view.hint) setTimeout(() => dispatch({ kind: "hint", text: "" }), 1000);
}

/** Starts a live voice session, or ends the one already running. Input: none. Output: a promise for when the daemon has answered. */
async function toggleVoice(): Promise<void> {
  if (!daemonUp) return;
  if (view.voice) return stopVoice();
  const id = await voiceStart();
  if (id) {
    dispatch({ kind: "voiceOn", id });
    clearHintSoon();
  }
}

/** Ends the live voice session and returns the window to its resting look. Input: none. Output: a promise for when the daemon has released the microphone. */
async function stopVoice(): Promise<void> {
  await voiceStop();
  dispatch({ kind: "voiceOff" });
}

/** Puts one sentence in the answer slot and moves the view to answered, for the cases where nothing was asked of the daemon at all. Input: the sentence. Output: nothing. */
function offline(text: string): void {
  dispatch({
    kind: "daemonEvent",
    ev: { id: "", type: "error", text, evidence: [] },
  });
}

/** After the ~900ms "asking" delay, fills the pending turn in from the mock script. Input: the matter id asked of. Output: nothing. */
function scheduleAnswer(matterId: string): void {
  setTimeout(
    () => {
      const m = view.matters.find((x) => x.id === matterId);
      if (!m || m.turns.length === 0) return;
      const idx = Math.min(m.turns.length - 1, venueScript.length - 1);
      const script =
        matterId === "venue"
          ? venueScript[idx]
          : { a: "Nothing else on this yet.", evidence: undefined };
      const turns = m.turns
        .slice(0, -1)
        .concat([
          {
            ...m.turns[m.turns.length - 1],
            a: script.a,
            evidence: script.evidence,
          },
        ]);
      dispatch({ kind: "answered", patch: { turns } });
    },
    reduce ? 50 : 900,
  );
}

// Both microphone keys are bound to the document in the capture phase rather than to the input: the desktop hotkey shows this window and the next key press can land before the input has taken focus, and every voice turn rebuilds the card, which would take a listener on the input with it. Capturing also means the input's own Enter and Escape never see a key that stopped a dictation.
document.addEventListener(
  "keydown",
  (e) => {
    if (e.code === "Space" && e.shiftKey) {
      e.preventDefault();
      void toggleVoice();
      return;
    }
    // The card's two fold-out headers are keyboard controls (see bindFold), and Space is how a button is pressed; a Space that belongs to one of them is not a Space that opens the microphone.
    if ((e.target as HTMLElement | null)?.closest?.('[role="button"]')) return;
    const value = root.querySelector<HTMLInputElement>(".q")?.value ?? "";
    const action = dictationKey(
      e,
      value.trim() === "",
      view.dictating === true,
      dictateStopping,
    );
    if (!action) return;
    e.preventDefault();
    e.stopPropagation();
    if (action === "start") beginDictation();
    else void endDictation();
  },
  true,
);

function onInputKeydown(e: KeyboardEvent): void {
  if (e.key === "Enter") {
    e.preventDefault();
    // Ctrl+Enter is the way to ask something unrelated without waiting the few minutes out: it drops the thread in hand so the daemon opens a new conversation for this question. Plain Enter carries on in the thread (see askConversation), which is what a follow-up needs.
    dispatch({ kind: "enter", fresh: e.ctrlKey });
  } else if (e.key === "Escape") {
    e.preventDefault();
    dispatch({ kind: "escape" });
  }
}

// Tauri wiring; harmless in a plain browser.
/** Hides the OS window. Replaced by the real one below when this page is running inside Tauri. */
let hide: () => Promise<void> = async () => {};
/** The last hide's own promise. A notice arriving milliseconds behind a hide has to wait for it before asking whether the window is visible, or isVisible() answers true for a window on its way out and the notice takes the "already open" branch and is never shown (see showNotice). */
let hidden: Promise<void> = Promise.resolve();
/** Hides the hover and stops the step ticker with it, whichever way the hide was asked for — Escape, a notice's six seconds running out, or the card being clicked through. Input: none. Output: nothing; the hide's promise is kept in `hidden` for showNotice to wait on. */
function hideWindow(): void {
  stopStepTicker();
  // A live voice session outlives a hide, so its grid's breath is stopped here rather than left running ten times a second against a window nobody can see; connect() starts it again on the way back to being shown.
  syncBreath(false);
  clearDeferredWrites();
  // showNotice waits on this before asking whether the window is visible, so a hide that failed has to settle rather than reject there.
  hidden = hide().catch((e) => console.error("ora: hiding the window failed", e));
}
/** Shows the hover so it can say one of Ora's own moments, and takes no focus doing it: the notice arrives while the user is working in another window, so the window is placed and shown exactly as the hotkey path places and shows it but with no raise() and nothing else that asks GNOME for focus. Input: the notice off the daemon's stream. Output: a promise for when the window is up. */
let showNotice: (n: Notice) => Promise<void> = async () => {};
/** How wide the hover is, in logical pixels: the width of the ask card. */
const HOVER_WIDTH = 720;
/** How wide a window showing nothing but a notice is, in logical pixels: the notice card's own 420 maximum (see .N in styles.css) plus the 18 of body padding on each side. The window is transparent and paints nothing outside the card, but it still takes the pointer, so a window any wider than this would sit as an invisible band over the top right of the screen swallowing clicks meant for whatever is under it. */
const NOTICE_WIDTH = 456;

/** Sizes the OS window to the rendered content so the empty state is a short strip and an answer grows the window, keeping it anchored to the dock whenever that height changes while the window is visible. Input: none. Output: the logical size the window was set to. */
let fitWindow: () => Promise<{ width: number; height: number }> = async () => ({
  width: HOVER_WIDTH,
  height: 0,
});

/** Whether this window is up only to show a notice, which is a different window from the hover: it is the width of the notice card rather than of the ask card, it sits under the top bar at the right rather than where the hover opens, and it goes again when the notice does. Set when a notice arrives at a shut window and cleared when the hotkey opens the hover proper, so a second notice landing on top of the first is still treated as a notice-only window rather than as a hover the user opened. Read by renderNotice as well as by the sizing, which is why it lives out here rather than inside wireWindow. */
let noticeOnly = false;

/** What the Tauri shell hands this module: the window itself, the desktop the placement reads, the one focus request, and a way to hear the toggle hotkey. Passed in rather than reached for, so the show, the notice-only window and the sizing can all be driven from a fake — under jsdom getCurrentWindow() throws, and everything wired up below it was unreachable from any test. */
export interface Shell {
  win: WinLike;
  desktop: Desktop;
  raise: () => Promise<void>;
  onToggle: (run: () => void) => void;
}

/** Wires the window calls this module cannot make on its own: hide, fitWindow, showNotice and the toggle hotkey. Input: the shell. Output: nothing; the four module-level bindings above are what it leaves behind. */
export function wireWindow(shell: Shell): void {
  const { win, desktop } = shell;
  hide = () => win.hide();

  // The monitor, dock and chosen position this open used, held for as long as the window stays up so that an answer growing the window keeps sitting where it opened instead of following the pointer onto another screen.
  let placeCtx: PlaceContext | null = null;

  // The card does not fill the window: the body keeps a gutter around it so the card's shadow has somewhere to fall, so the window has to be as tall as the card's bottom edge plus that gutter. Growing it downwards alone would walk it off the position it opened at, so every height change is followed by a move (see winplace.fitWindow).
  let lastHeight = 0;
  let lastWidth = 0;
  fitWindow = async () => {
    const gutter =
      parseFloat(getComputedStyle(document.body).paddingBottom) || 0;
    // Whichever of the two reaches lowest: the card, or the notice bubble above it when a notice is the only thing on screen and the card is hidden.
    const bottom = Math.max(
      root.getBoundingClientRect().bottom,
      noticeEl.getBoundingClientRect().bottom,
    );
    // The 96px floor is the card's own minimum height; a notice showing on its own has no card under it, so it is only as tall as the bubble and would otherwise float half a card's height off the dock.
    const floor = noticeOnly ? 0 : 96;
    const height = Math.max(floor, Math.ceil(bottom + gutter));
    const width = noticeOnly ? NOTICE_WIDTH : HOVER_WIDTH;
    const changed = height !== lastHeight || width !== lastWidth;
    lastHeight = height;
    lastWidth = width;
    const size = { width, height };
    // Both kinds of window are sized here and each is then moved exactly once: the hover by winplace.fitWindow, which puts it back where it opened, and a notice-only window by placeNotice, which puts it under the top bar at the right. winplace.fitWindow is told not to move a notice-only window, or it would fly off to the hover's own position (placementFor, which is bottom-centre by default) and be moved back a moment later.
    await winplaceFitWindow(win, size, changed, placeCtx, !noticeOnly);
    if (noticeOnly && changed) await placeNotice(size);
    return size;
  };
  /** Puts the window where a notice-only window belongs. Input: the window's logical size. Output: nothing; does nothing when no monitor was resolved for this open. */
  const placeNotice = async (size: { width: number; height: number }): Promise<void> => {
    if (!placeCtx) return;
    const at = noticePlacement(placeCtx, size);
    await win.setPosition(new PhysicalPosition(at.x, at.y));
  };
  /** Sets the thread's ceiling for the screen this open landed on; the stylesheet reads it as --thread-max and scrolls the thread inside itself past it. The cap is written as CSS pixels, so it is worked out with this window's own scale factor rather than with the pointer monitor's, which is a different number on a mixed-DPI desk. Input: the placement context, or null when no monitor was resolved. Output: nothing. */
  const capThread = async (ctx: PlaceContext | null): Promise<void> => {
    if (!ctx) return;
    const cap = threadMaxHeight(ctx.work, await win.scaleFactor());
    document.body.style.setProperty("--thread-max", `${cap}px`);
  };
  showNotice = async (n) => {
    // A notice pressed on a notice-only window hides that window and the daemon's confirmation follows milliseconds later; without waiting for the hide to land, isVisible() answers true for a window on its way out and the confirmation takes the "already open" branch below and is never seen.
    await hidden;
    const open = await win.isVisible();
    // A hover already on screen keeps the position it opened at; one that is shut is placed for this notice the same way the hotkey path places it, before anything is shown.
    if (!open) {
      placeCtx = await resolveContext(desktop, storedHoverPosition());
      // A shut window is about to be placed for this notice against a context that may name a different monitor from the last one's, so the fit below has to run even if the card comes out exactly the height the last one was.
      lastHeight = 0;
    }
    await capThread(placeCtx);
    // A window that is up only for the notice before this one is still a notice-only window: the card stays on its own and goes with the notice, rather than un-hiding an empty ask card that nothing would then take down.
    if (!open) noticeOnly = true;
    dispatch({ kind: "notice", notice: n, hoverOpen: open && !noticeOnly });
    if (open) return;
    // One call: fitWindow sizes the window and, for a notice-only window, puts it under the top bar at the right — where notices go, not where the hover opens for a question.
    await fitWindow();
    // show() and nothing else. No raise(), no setFocus(): the user is typing in another window and a notice must not take the keyboard off them.
    await win.show();
  };

  // The desktop hotkey signals the Rust side, which emits an event the shell passes on here; showing and hiding from here keeps every window call on the main loop.
  shell.onToggle(() => {
    void toggleWindow(win, {
      // Probe the daemon and open its stream before this window takes focus; what fills the card in follows behind the show (see beforeShow and hydrate).
      beforeShow,
      openContext: async () => {
        // The hotkey opens the hover proper, so whatever notice-only window came before it is over: full width, the chosen position, and the card underneath on show again.
        noticeOnly = false;
        // The position is re-read from storage on every open, so choosing a different one on the Settings screen takes effect on the next hotkey press without a restart.
        placeCtx = await resolveContext(desktop, storedHoverPosition());
        await capThread(placeCtx);
        return placeCtx;
      },
      sizeToContent: fitWindow,
      raise: shell.raise,
      focusInput: () => root.querySelector<HTMLInputElement>(".q")?.focus(),
    }).catch((e) => console.error("ora: the toggle failed", e));
  });
}

try {
  const win = getCurrentWindow();
  // Where the dock is, re-read from the desktop on every open so moving the dock takes effect on the next hotkey press instead of on the next restart. The last answer is kept as the fallback, and the first one is a bottom dock that reserves no space, which is also what the Rust side returns when it can read nothing.
  let dock: Dock = { edge: "bottom", clearance: 0 };
  wireWindow({
    win,
    desktop: {
      monitors: () => availableMonitors(),
      pointer: () => cursorPosition(),
      focused: () => currentMonitor(),
      dock: async () => {
        dock = await invoke<Dock>("dock_anchor").catch(() => dock);
        return dock;
      },
    },
    raise: () => invoke("raise"),
    onToggle: (run) => {
      void listen("ora://toggle", run).catch((e) =>
        console.error("ora: the toggle listener would not attach", e),
      );
    },
  });
} catch {
  /* not inside Tauri */
}

// Which theme was asked for last, so an earlier "system" whose answer is still on its way from Rust cannot land on top of a later choice.
let themeAsk = 0;

/** Puts a theme choice on the page. Input: the choice. Output: nothing; "light" and "dark" are stamped on the root element straight away, and "system" is resolved by asking the desktop through the Rust system_theme command, which reads GNOME's own setting. It has to be asked, because WebKitGTK's prefers-color-scheme media query does not follow that setting; the media query is only the fallback, for a plain browser tab with no Tauri behind it. The app window resolves "system" the same way (see stampSystemTheme in src/app/main.ts), so the two windows never disagree about what it means. */
function applyThemeChoice(choice: Theme): void {
  const ask = ++themeAsk;
  if (choice !== "system") {
    document.documentElement.dataset.theme = choice;
    return;
  }
  const stamp = (t: string) => {
    if (ask === themeAsk) document.documentElement.dataset.theme = t;
  };
  void invoke<string>("system_theme")
    .then(stamp)
    .catch(() =>
      stamp(
        matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light",
      ),
    );
}

// Theme choice: the dev switch wins, then the setting the app window's Settings screen stored, and nothing stored leaves it to the desktop.
let storedTheme: string | null = null;
try {
  storedTheme = localStorage.getItem(THEME_KEY);
} catch {
  /* storage blocked */
}
applyThemeChoice(themeChoice(devTheme ?? storedTheme));

// The hover is shown and hidden rather than reloaded, so the choice read above would otherwise be the only one this page ever saw, and a theme picked in the app window afterwards would never reach it. The storage event fires here whenever the app window writes the key, which is what makes that change land while the hover is still up. A page opened with ?theme= is being held at one theme on purpose, so it does not follow.
if (!devTheme) {
  window.addEventListener("storage", (e) => {
    const choice = themeFromStorage(e);
    if (choice) applyThemeChoice(choice);
  });
}

render(view);
