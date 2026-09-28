/** Dictation keys: one key press opens the daemon's microphone, and a second one, or the daemon's own silence gate, closes it and gives back what was said. The two HTTP calls are startDictation and stopDictation in daemon.ts; this file only decides what a key means. */

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
