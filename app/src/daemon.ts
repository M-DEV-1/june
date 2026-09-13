/** HTTP/SSE client for the local daemon at http://127.0.0.1:6942. No DOM or state here, just I/O. */

import { TOKEN_HEADER, devToken } from "./shared/token";
import { DAEMON_HOST_PORT } from "./next/daemon-url";

export { TOKEN_HEADER, devToken };

/** Where the daemon listens. Only setPort moves it, which is how a test or a throwaway build can point the client somewhere else without stopping the user's real daemon. */
let base = `http://${DAEMON_HOST_PORT}`;

/** Points every call in this module at a different port on 127.0.0.1. Input: the port as digits. Output: nothing; a non-numeric value is ignored so a stray query string cannot redirect the client. */
export function setPort(port: string): void {
  if (/^\d{1,5}$/.test(port)) base = `http://127.0.0.1:${port}`;
}

/** The shared IPC secret read from the daemon's token file (see main.ts). Unset in a plain browser, where the mock path runs instead. */
let token: string | undefined;
export function setToken(t: string | undefined): void {
  token = t;
}

/** Where the daemon is and the secret to reach it, for the dictation calls, which take both as arguments. Input: none. Output: the current base URL and token. */
export function endpoint(): { base: string; token: string | undefined } {
  return { base, token };
}

/** The headers every authenticated call sends. Input: none. Output: the JSON content type and, when there is one, the IPC token. */
function authHeaders(): Record<string, string> {
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (token) h[TOKEN_HEADER] = token;
  return h;
}

/** Opens a live voice session on the daemon. Input: none. Output: the session id, or null when the daemon refused (409 because one is already running, 500 because the audio devices would not open) or could not be reached. */
export async function voiceStart(): Promise<string | null> {
  try {
    const res = await fetch(`${base}/voice/start`, {
      method: "POST",
      headers: authHeaders(),
    });
    if (!res.ok) return null;
    const body = (await res.json()) as { id: string };
    return body.id;
  } catch {
    return null;
  }
}

/** Ends the live voice session and releases the microphone and speaker. Input: none. Output: nothing; stopping when nothing is running is not an error, and a failure is swallowed because there is nothing the window could do about it. */
export async function voiceStop(): Promise<void> {
  try {
    await fetch(`${base}/voice/stop`, {
      method: "POST",
      headers: authHeaders(),
    });
  } catch {
    /* the daemon is gone; the session went with it */
  }
}

/** How long one of the three reads that fill the card in may take before it is given up on, in milliseconds. /context reads the focused window through AT-SPI and can block for seconds; the window is already on screen by the time these run (see connect in main.ts), so giving up costs a stale context chip rather than a hover that never appears. */
const READ_TIMEOUT_MS = 3000;

/** Reads a JSON body from the daemon under READ_TIMEOUT_MS. Input: the path to GET. Output: the parsed body, or null when the daemon answered with an error, could not be reached, or did not answer in time. */
async function readJson<T>(path: string): Promise<T | null> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), READ_TIMEOUT_MS);
  try {
    const headers: Record<string, string> = {};
    if (token) headers[TOKEN_HEADER] = token;
    const res = await fetch(`${base}${path}`, {
      headers,
      signal: controller.signal,
    });
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

/** Asks whether a voice session is already running, which is how the window picks one up again after a reload. Input: none. Output: the daemon's status, or null when it cannot be reached or did not answer in time. */
export async function voiceStatus(): Promise<{
  active: boolean;
  id: string;
  state: string;
} | null> {
  return readJson<{ active: boolean; id: string; state: string }>(
    "/voice/status",
  );
}

/** One of Ora's own moments, sent by the daemon rather than asked for: the morning brief, the evening close, a meeting prep. title is the card's bold first line and body the few lines under it; place names the app window's screen a click opens ("tasks", "days") and id the row to select there, both empty when the moment points at nothing in particular; kind names the moment ("brief", "close", "meeting", "note"). action and until are empty on a notice arriving fresh, and set when the user has since pressed a button on the desktop notification it was also posted as: action is "snoozed" or "done", and until is the RFC 3339 moment a snoozed notice comes back. The shape is fixed by the Go side, see internal/ipc/notice.go. */
export type Notice = {
  title: string;
  body: string;
  place: string;
  id: string;
  kind: string;
  action?: string;
  until?: string;
  /** The RFC 3339 moment this notice's buttons stop working, set only on a notice that asked a question. The daemon's goroutine waiting on the answer gives up then, so the card counts down to it and takes itself off screen rather than leaving a button that answers "Could not do that". */
  expires?: string;
};

/** The shape of every event on the daemon's SSE stream, whichever client reads it. The one place this is declared: a client-side copy that drifts from this adds a field the compiler cannot check against what the wire actually sends. */
export type DaemonEvent = {
  id: string;
  /** The first five belong to an ask; "dictation" carries a finished transcript, "heard", "said", "state" and "level" belong to a live voice session, "notice" is Ora speaking first, "recording" and "dreaming" say ("on" or "off") that a meeting is being captured or the nightly run is under way, and "act" carries one line of a computer-use job's progress, its parts as JSON in detail (see internal/ipc/actjob.go). */
  type:
    | "status"
    | "tool"
    | "answer"
    | "done"
    | "error"
    | "dictation"
    | "heard"
    | "said"
    | "state"
    | "level"
    | "notice"
    | "act"
    | "recording"
    | "dreaming";
  text?: string;
  /** Carried on a "tool" event (a short summary of what that call is doing or found), on an "act" event (the job progress, as JSON text — see internal/ipc/actjob.go's ActEmitter) and on a "level" event (the session's mic/speaker amplitude as JSON text, {"mic":0-1,"speaker":0-1} — see internal/ipc/voice.go's levels and waveform.ts's renderLevelEvent). */
  detail?: string;
  /** Only meaningful on the after-call "tool" event: true when that tool call's result was an error, so a step closes as failed instead of done. */
  failed?: boolean;
  evidence?: { title: string; meta: string; body: string }[];
  /** Only carried on a "notice" event: the whole card. */
  notice?: Notice;
};

/** Swappable for tests: what to construct an EventSource with. Defaults to the real browser class, read lazily so importing this module doesn't require EventSource to exist (it doesn't in the test/node environment). */
let eventSourceCtor: (new (url: string) => EventSource) | undefined;
export function setEventSourceCtor(
  ctor: new (url: string) => EventSource,
): void {
  eventSourceCtor = ctor;
}

/** Posts a question to the daemon. Input: the question text, the current context chip text, and the conversation to append the question to, or undefined to have the daemon open one. Output: the daemon's id for the resulting turn, and the conversation the question was stored in — the one that was named, or the one the daemon opened for a question that named none. Naming a conversation is what puts the turns already in it in front of the model, which is how a follow-up like "again" has anything to refer to. Throws on anything but a 2xx, same as a daemon that could not be reached — the caller's own catch already has to handle that case, so a 4xx/5xx body (which is not this shape) lands there too instead of being parsed as if it were. */
export async function ask(
  question: string,
  context: string,
  conversation?: string,
): Promise<{ id: string; conversationId: string }> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
  };
  if (token) headers[TOKEN_HEADER] = token;
  const res = await fetch(`${base}/ask`, {
    method: "POST",
    headers,
    body: JSON.stringify({
      question,
      context,
      conversation_id: conversation ?? "",
    }),
  });
  if (!res.ok) throw new Error(`ask failed: ${res.status}`);
  const body = (await res.json()) as { id: string; conversation_id?: string };
  return { id: body.id, conversationId: body.conversation_id ?? "" };
}

/** Opens the daemon's SSE stream and forwards each parsed event to onEvent, reconnecting on drop. Input: a callback for each event. Output: a stop function that closes the stream for good. */
export function events(onEvent: (ev: DaemonEvent) => void): () => void {
  let stopped = false;
  let source: EventSource | undefined;

  function connect(): void {
    if (stopped) return;
    const Ctor = eventSourceCtor ?? EventSource;
    const url = token
      ? `${base}/events?token=${encodeURIComponent(token)}`
      : `${base}/events`;
    source = new Ctor(url);
    source.onmessage = (e: MessageEvent) => {
      try {
        onEvent(JSON.parse(e.data));
      } catch {
        /* malformed message, ignore */
      }
    };
    source.onerror = () => {
      source?.close();
      if (!stopped) setTimeout(connect, 2000);
    };
  }
  connect();

  return () => {
    stopped = true;
    source?.close();
  };
}

export type ContextInfo = { app: string; title: string; text: string };
export type MatterRow = {
  id: string;
  title: string;
  kind: "action" | "thread" | "meeting";
  status: "open" | "done" | "watching";
  when: string;
  detail: string;
};

/** Fetches what the user is currently looking at. Input: none. Output: the app, window title and visible text, or null on any failure (no daemon, bad response, network error, or no answer inside READ_TIMEOUT_MS). */
export async function context(): Promise<ContextInfo | null> {
  return readJson<ContextInfo>("/context");
}

/** Fetches the daemon's list of open matters. Input: none. Output: the matter rows, or null on any failure. */
export async function matters(): Promise<MatterRow[] | null> {
  const body = await readJson<{ matters: MatterRow[] }>("/matters");
  return body ? body.matters : null;
}

/** Starts a long computer-use job on the daemon (POST /act). Input: the goal in the user's own words. Output: the job's id, or null when the daemon refused (an empty goal or an unknown brain) or could not be reached; its progress then arrives on the /events stream tagged with that id. */
export async function actStart(goal: string): Promise<string | null> {
  try {
    const res = await fetch(`${base}/act`, {
      method: "POST",
      headers: authHeaders(),
      body: JSON.stringify({ goal }),
    });
    if (!res.ok) return null;
    const body = (await res.json()) as { id: string };
    return body.id;
  } catch {
    return null;
  }
}

/** Ends a running job now (POST /act/{id}/stop). Input: the job id. Output: nothing; a daemon that cannot be reached, or a job that has already ended, leaves nothing more for the window to do. */
export async function actStop(id: string): Promise<void> {
  try {
    await fetch(`${base}/act/${id}/stop`, {
      method: "POST",
      headers: authHeaders(),
    });
  } catch {
    /* the daemon is gone */
  }
}

/** Pauses or resumes a running job — one call for the single toggle the card shows (POST /act/{id}/pause or /act/{id}/resume). Input: the job id, and true to pause or false to resume. Output: nothing. */
export async function actPauseResume(
  id: string,
  pause: boolean,
): Promise<void> {
  try {
    await fetch(`${base}/act/${id}/${pause ? "pause" : "resume"}`, {
      method: "POST",
      headers: authHeaders(),
    });
  } catch {
    /* the daemon is gone */
  }
}

/** Answers the one question a stuck job asked (POST /act/{id}/answer). Input: the job id and the answer text. Output: nothing. */
export async function actAnswer(id: string, text: string): Promise<void> {
  try {
    await fetch(`${base}/act/${id}/answer`, {
      method: "POST",
      headers: authHeaders(),
      body: JSON.stringify({ text }),
    });
  } catch {
    /* the daemon is gone */
  }
}

/** Checks whether the daemon is up. Input: none. Output: true if /status answers 200 with the token within 500ms, false otherwise. */
export async function probe(): Promise<boolean> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 500);
  try {
    const headers: Record<string, string> = {};
    if (token) headers[TOKEN_HEADER] = token;
    const res = await fetch(`${base}/status`, {
      signal: controller.signal,
      headers,
    });
    return res.ok;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}
