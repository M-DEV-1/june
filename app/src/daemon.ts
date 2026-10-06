/** HTTP/SSE client for the local daemon at http://127.0.0.1:6942. No DOM or state here, just I/O. */

import { TOKEN_HEADER, devToken } from "./shared/token";
import { openStream, type DaemonEvent } from "./shared/wire";
import { DAEMON_HOST_PORT } from "./next/daemon-url";

export { devToken };

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

/** The headers every call to the daemon sends. Input: none. Output: the JSON content type and, when there is one, the IPC token. */
function authHeaders(): Record<string, string> {
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (token) h[TOKEN_HEADER] = token;
  return h;
}

/** Posts to the daemon with the token. Input: the path, and a body to send as JSON when there is one. Output: the response, or null when the daemon could not be reached. */
export async function post(path: string, body?: unknown): Promise<Response | null> {
  try {
    return await fetch(`${base}${path}`, {
      method: "POST",
      headers: authHeaders(),
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    return null;
  }
}

/** Opens a live voice session on the daemon. Input: none. Output: the session id, or null when the daemon could not be reached. Throws an Error carrying the daemon's own sentence when it refused (409 because one is already running, 503 because there is no Gemini API key, 500 because the audio devices would not open), so the caller can say why instead of staying silent. */
export async function voiceStart(): Promise<string | null> {
  const res = await post("/voice/start");
  if (!res) return null;
  if (!res.ok) throw new Error((await res.text()).trim() || "I couldn't start talking. Try again.");
  return ((await res.json()) as { id: string }).id;
}

/** Ends the live voice session and releases the microphone and speaker. Input: none. Output: nothing; stopping when nothing is running is not an error, and a failure is swallowed because there is nothing the window could do about it. */
export async function voiceStop(): Promise<void> {
  await post("/voice/stop");
}

/** A dictation the daemon refused, carrying the sentence it gave for a person, "" when it gave none. A plain-text refusal is whisper's own error — a path and a file name — so it is not carried; only the "message" a JSON refusal puts beside its error word is. */
export class DictationRefused extends Error {}

/** Reads a refused dictation's sentence. Input: the response. Output: the error to throw. */
async function dictationRefused(res: Response): Promise<DictationRefused> {
  try {
    const body = (await res.json()) as { message?: unknown };
    return new DictationRefused(typeof body.message === "string" ? body.message.trim() : "");
  } catch {
    return new DictationRefused("");
  }
}

/** Opens the microphone on the daemon. Input: none. Output: the id of the recording, which stopDictation and the "dictation" event both carry. Throws DictationRefused if the daemon would not start one, and whatever fetch threw if it could not be reached. */
export async function startDictation(): Promise<string> {
  const res = await fetch(`${base}/dictate/start`, {
    method: "POST",
    headers: authHeaders(),
  });
  if (!res.ok) throw await dictationRefused(res);
  const body = (await res.json()) as { id: string };
  return body.id;
}

/** Closes the microphone and waits for the transcript. Input: the id startDictation returned. Output: what was said, trimmed; an empty string when the daemon has no such recording open any more (the silence gate or the two-minute limit already ended it and sent the text on the event stream instead, or a later dictation replaced it). Throws if whisper failed. */
export async function stopDictation(id: string): Promise<string> {
  const res = await fetch(`${base}/dictate/stop`, {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify({ id }),
  });
  if (res.status === 404) return "";
  if (!res.ok) throw await dictationRefused(res);
  const body = (await res.json()) as { text: string };
  return (body.text ?? "").trim();
}

/** How long one of the three reads that fill the card in may take before it is given up on, in milliseconds. /context reads the focused window through AT-SPI and can block for seconds; the window is already on screen by the time these run (see connect in main.ts), so giving up costs a stale context chip rather than a hover that never appears. */
const READ_TIMEOUT_MS = 3000;

/** Reads a JSON body from the daemon under READ_TIMEOUT_MS. Input: the path to GET. Output: the parsed body, or null when the daemon answered with an error, could not be reached, or did not answer in time. */
async function readJson<T>(path: string): Promise<T | null> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), READ_TIMEOUT_MS);
  try {
    const res = await fetch(`${base}${path}`, {
      headers: authHeaders(),
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

/** Where voice typing stands, off GET /components. Input: none. Output: the feature's state ("not_installed", "failed", "queued", "installing", "installed", ...), or "" when the daemon does not list it or could not be asked. */
export async function voiceTypingState(): Promise<string> {
  const c = await readJson<{ features?: { id: string; state: string }[] }>("/components");
  return c?.features?.find((x) => x.id === "transcribe")?.state ?? "";
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

/** Posts a question to the daemon. Input: the question text, the current context chip text, and the conversation to append the question to, or undefined to have the daemon open one. Output: the daemon's id for the resulting turn, and the conversation the question was stored in — the one that was named, or the one the daemon opened for a question that named none. Naming a conversation is what puts the turns already in it in front of the model, which is how a follow-up like "again" has anything to refer to. Throws on anything but a 2xx, same as a daemon that could not be reached — the caller's own catch already has to handle that case, so a 4xx/5xx body (which is not this shape) lands there too instead of being parsed as if it were. */
export async function ask(
  question: string,
  context: string,
  conversation?: string,
): Promise<{ id: string; conversationId: string }> {
  const res = await fetch(`${base}/ask`, {
    method: "POST",
    headers: authHeaders(),
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

/** Opens the daemon's SSE stream for the hover (see openStream in shared/wire.ts). Input: a callback for each event. Output: a stop function that closes the stream for good. */
export function events(onEvent: (ev: DaemonEvent) => void): () => void {
  return openStream(base, () => token, onEvent);
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
  const res = await post("/act", { goal });
  if (!res?.ok) return null;
  return ((await res.json()) as { id: string }).id;
}

/** Ends a running job now (POST /act/{id}/stop). Input: the job id. Output: nothing; a daemon that cannot be reached, or a job that has already ended, leaves nothing more for the window to do. */
export async function actStop(id: string): Promise<void> {
  await post(`/act/${id}/stop`);
}

/** Pauses or resumes a running job — one call for the single toggle the card shows (POST /act/{id}/pause or /act/{id}/resume). Input: the job id, and true to pause or false to resume. Output: nothing. */
export async function actPauseResume(
  id: string,
  pause: boolean,
): Promise<void> {
  await post(`/act/${id}/${pause ? "pause" : "resume"}`);
}

/** Answers the one question a stuck job asked (POST /act/{id}/answer). Input: the job id and the answer text. Output: nothing. */
export async function actAnswer(id: string, text: string): Promise<void> {
  await post(`/act/${id}/answer`, { text });
}

/** Checks whether the daemon is up. Input: none. Output: true if /status answers 200 with the token within 500ms, false otherwise. */
export async function probe(): Promise<boolean> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 500);
  try {
    const res = await fetch(`${base}/status`, {
      signal: controller.signal,
      headers: authHeaders(),
    });
    return res.ok;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}
