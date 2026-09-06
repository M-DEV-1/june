import { TOKEN_HEADER } from "./daemon";
/** Dictation: one key press opens the daemon's microphone, and a second one — or the daemon's own silence gate — closes it and gives back what was said. HTTP and key handling only, no DOM state of its own. */

function headers(token: string | undefined): Record<string, string> {
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (token) h[TOKEN_HEADER] = token;
  return h;
}

/** Opens the microphone on the daemon. Input: the daemon's base URL and the IPC token. Output: the id of the recording, which stopDictation and the "dictation" event both carry. Throws if the daemon would not start one. */
export async function startDictation(
  base: string,
  token: string | undefined,
): Promise<string> {
  const res = await fetch(`${base}/dictate/start`, {
    method: "POST",
    headers: headers(token),
  });
  if (!res.ok) throw new Error(`dictation did not start: ${res.status}`);
  const body = (await res.json()) as { id: string };
  return body.id;
}

/** Closes the microphone and waits for the transcript. Input: the daemon's base URL, the IPC token, and the id startDictation returned. Output: what was said, trimmed; an empty string when the daemon has no such recording open any more (the silence gate or the two-minute limit already ended it and sent the text on the event stream instead, or a later dictation replaced it). Throws if whisper failed. */
export async function stopDictation(
  base: string,
  token: string | undefined,
  id: string,
): Promise<string> {
  const res = await fetch(`${base}/dictate/stop`, {
    method: "POST",
    headers: headers(token),
    body: JSON.stringify({ id }),
  });
  if (res.status === 404) return "";
  if (!res.ok) throw new Error(`dictation failed: ${res.status}`);
  const body = (await res.json()) as { text: string };
  return (body.text ?? "").trim();
}

/** The part of a keyboard event that decides what a key means for dictation. */
export type DictationKeyEvent = {
  key: string;
  repeat?: boolean;
  shiftKey?: boolean;
  ctrlKey?: boolean;
  altKey?: boolean;
  metaKey?: boolean;
};

/** What one key press means for dictation, which is a toggle: the space bar starts it while the input is empty, and the space bar, Enter or Escape ends the one that is running. Input: the key event, whether the input is empty, whether a dictation is already open, and whether a stop for it has already gone to the daemon and not come back. Output: "start", "stop", or "" for a key that belongs to whatever else was listening.
 * Auto-repeat says nothing, so a key left down starts exactly one recording rather than flickering it on and off, and a key with a modifier says nothing either, so Shift+Space can mean a live voice session. A key pressed while a stop is still in flight says nothing either: the daemon has already closed that recording, so a second stop 404s and answers "", which would be taken as the transcript and drop the words the first stop is still waiting for. */
export function dictationKey(
  e: DictationKeyEvent,
  inputEmpty: boolean,
  dictating: boolean,
  stopping = false,
): "start" | "stop" | "" {
  if (e.repeat || e.shiftKey || e.ctrlKey || e.altKey || e.metaKey) return "";
  if (stopping) return "";
  if (dictating)
    return e.key === " " || e.key === "Enter" || e.key === "Escape"
      ? "stop"
      : "";
  return e.key === " " && inputEmpty ? "start" : "";
}
