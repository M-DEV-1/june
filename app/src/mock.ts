/** Seed data for the window: the venue matter's script, the CI-red matter already answered, and the lease stub. Also owns the ?mock=1&voice=1 switch, which fakes a live-voice session (see startMockVoice) so the voice-mode surface can be exercised and screenshotted with no daemon and no microphone. */
import type { Evidence, Matter, Notice, View } from "./state";

/** The one email both venue turns read from. Turn one leaves it collapsed; turn two is the same source, expanded. */
const venueEvidence: Evidence = {
  title: "Re: venue for the 12th",
  meta: "Vexil Zelbrak · 08:40",
  body:
    "Hi, Tuesday works for us. <mark>Could you send the invoice across first?</mark> Once it's in I'll confirm the room with the building. <mark>Parking is tight after 6</mark>, just so you know. Vexil",
};

/** The answer and evidence for each successive question asked in the venue matter. */
export const venueScript: { a: string; evidence?: Evidence[] }[] = [
  {
    a: "Nearly. Vexil said <b>yes for Tuesday</b>, but she wants the invoice before she confirms the room. I have it drafted from last month's.",
    evidence: [venueEvidence],
  },
  {
    a: 'Only that <b>parking is tight after 6</b>. Nothing to act on.',
    evidence: [venueEvidence],
  },
];

function initialMatters(): Matter[] {
  return [
    { id: "venue", title: "venue", context: "Mail · Vexil Zelbrak", turns: [] },
    {
      id: "ci",
      title: "CI red",
      context: "GitHub · 08:12",
      turns: [
        {
          q: "CI red on main",
          a: "<b>test_upload</b> fails on the new size check: it expects 10 MB and the constant now says 8. Emzor lowered it in #412 yesterday.",
          evidence: [{ title: "CI run 4471", meta: "GitHub · 08:12" }],
        },
      ],
    },
    { id: "lease", title: "lease", context: "Mail · Vandril", turns: [] },
  ];
}

/** The window as it looks the moment it opens: nothing asked yet, three matters held. */
export function initialView(): View {
  return {
    matters: initialMatters(),
    current: 0,
    evidenceOpen: false,
    input: "",
    state: "empty",
    contextChip: "",
    contextText: "",
  };
}

/** The fake voice session's id fed to main.ts's dispatch, standing in for the id a real POST /voice/start would return. */
const MOCK_VOICE_ID = "mock-voice";

/** How often a "level" event ticks, matching the daemon's real cadence (see internal/ipc/voice.go's levels) so the mock exercises the same 20-times-a-second repaint path a live session does. */
const TICK_MS = 50;

/** How long each envelope cycle runs: a second of speech, then half a second of near-quiet, then the next cycle starts. */
const BURST_MS = 1000;
const CYCLE_MS = 1500;

/** The mic/speaker reading a fake level event carries this many milliseconds into the session. Deterministic (a sine wave, not Math.random) so the same elapsed time always renders the same frame, which is what makes a headless screenshot at a given moment repeatable. Input: milliseconds since the fake session started. Output: {mic, speaker}, each 0-1 — the mic bursts between 0.3 and 0.9 for the first second of every 1.5s cycle and drops to near-silence for the half second after; the speaker rides its own 0.3-0.9 burst only on every other (odd-numbered) cycle, silent the rest of the time, so both rows get a turn on screen. */
export function fakeLevelAt(ms: number): { mic: number; speaker: number } {
  const cycle = Math.floor(ms / CYCLE_MS);
  const within = ((ms % CYCLE_MS) + CYCLE_MS) % CYCLE_MS;
  const inBurst = within < BURST_MS;
  const tick = Math.floor(within / TICK_MS);
  const burst = (phase: number) => 0.6 + 0.3 * Math.sin(tick * 0.9 + phase);
  return {
    mic: inBurst ? burst(0) : 0.03,
    speaker: inBurst && cycle % 2 === 1 ? burst(1.5) : 0,
  };
}

/** Wires ?mock=1&voice=1: starts a fake live-voice session so the hover's voice-mode surface can be driven and screenshotted without a daemon or a microphone. Input: the page's query params. Output: a function that ends the fake session, since otherwise its 50ms tick runs for the life of the page; it dispatches "voiceOn" and then, straight into main.ts's real dispatch, a "level" event every 50ms (see fakeLevelAt for the envelope) plus a fixed one-off script — a "heard" transcript line, then "thinking", then "speaking" with a "said" reply — timed against that same envelope so a capture at 1300ms (still in the first, speaker-silent cycle) shows the surface listening with a breathing grid, and one at 2000ms (the second cycle's speaker burst, cycle 1 of fakeLevelAt) shows it mid-reply with the grid driven by the real reading. main.ts imports this module (for initialView/venueScript), so importing dispatch back from "./main" at the top of this file would be a circular static import — main.ts would still be mid-load, before its own view/root/Waveform locals exist, the moment this file's top level ran. A dynamic import() instead resolves once main.ts has actually finished loading (immediately, since by then it already has), sidestepping that. */
export function startMockVoice(params: URLSearchParams): () => void {
  let timer: ReturnType<typeof setInterval> | undefined;
  let stopped = false;
  /** Ends the fake session's ticking. Input: none. Output: nothing; safe to call before the dynamic import below has resolved, and safe to call twice. */
  const stop = (): void => {
    stopped = true;
    clearInterval(timer);
    timer = undefined;
  };
  if (!params.has("mock") || params.get("voice") !== "1") return stop;
  void import("./main").then(({ dispatch }) => {
    if (stopped) return;
    dispatch({ kind: "voiceOn", id: MOCK_VOICE_ID });
    let elapsed = 0;
    let heard = false;
    let thinking = false;
    let speaking = false;
    timer = setInterval(() => {
      elapsed += TICK_MS;
      const { mic, speaker } = fakeLevelAt(elapsed);
      dispatch({
        kind: "voiceEvent",
        ev: {
          id: MOCK_VOICE_ID,
          type: "level",
          detail: JSON.stringify({ mic, speaker }),
        },
      });
      if (!heard && elapsed >= 400) {
        heard = true;
        dispatch({
          kind: "voiceEvent",
          ev: { id: MOCK_VOICE_ID, type: "heard", text: "what's the weather" },
        });
      }
      if (!thinking && elapsed >= 1600) {
        thinking = true;
        dispatch({
          kind: "voiceEvent",
          ev: { id: MOCK_VOICE_ID, type: "state", text: "thinking" },
        });
      }
      if (!speaking && elapsed >= 1800) {
        speaking = true;
        dispatch({
          kind: "voiceEvent",
          ev: { id: MOCK_VOICE_ID, type: "state", text: "speaking" },
        });
        dispatch({
          kind: "voiceEvent",
          ev: {
            id: MOCK_VOICE_ID,
            type: "said",
            text: "Sunny and 24 degrees.",
          },
        });
      }
    }, TICK_MS);
  });
  return stop;
}

// The fake session ticks for the life of the page unless it is stopped, so it is stopped with the page.
const stopMockVoice = startMockVoice(new URLSearchParams(location.search));
window.addEventListener("pagehide", stopMockVoice);

/** The stale-task question the daemon asks once a day, the one notice that names its own three answers rather than taking the Done/snooze/Open set (see maybeStaleAsk in internal/proactive). */
const askNotice: Notice = {
  title: "Still open",
  body: "Vexil — send the invoice for the venue. Open since 28 Aug. Any progress?",
  place: "tasks",
  id: "42",
  kind: "task",
  actions: [
    { key: "done", label: "Done" },
    { key: "dropped", label: "Not happening" },
    { key: "later", label: "Not urgent" },
  ],
};

/** Wires ?mock=1&notice=1 (a notice on its own, the card underneath hidden), ?mock=1&notice=stack (a notice above an open card) and ?mock=1&notice=ask (the stale-task question with its own three answers), so the notice card can be looked at and screenshotted with no daemon. Input: the page's query params. Output: nothing; one "notice" event goes into main.ts's real dispatch once it has loaded, for the same reason startMockVoice imports it dynamically. */
export function startMockNotice(params: URLSearchParams): void {
  const mode = params.get("notice");
  if (!params.has("mock") || (mode !== "1" && mode !== "stack" && mode !== "ask")) return;
  void import("./main").then(({ dispatch }) => {
    dispatch({
      kind: "notice",
      notice: mode === "ask" ? askNotice : { title: "New task from Daily AI Sprint Standup", body: "Continue the TCFD-style formatting research for the generated statements and bring a first draft on Monday.", place: "tasks", id: "42", kind: "task" },
      hoverOpen: mode === "stack",
    });
  });
}

startMockNotice(new URLSearchParams(location.search));
