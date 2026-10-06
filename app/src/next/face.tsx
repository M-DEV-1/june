/** June's face as a chip: the kaomoji for a state in a small mono box, alternating on a timer for a state with more than one face. It is an image to a screen reader, named "june is <state>", because the glyphs themselves read as noise. */

import { useEffect, useState } from "react";

import { FACES, FACE_TICK_MS, face, type JuneState } from "../shared/faces";
import { useAppSelector } from "./store";

/** Input: the state, and the chip's text size class (text-meta by default; text-micro for a signature). Output: the chip. */
export function Face({ state, className = "text-meta" }: { state: JuneState; className?: string }) {
  const [tick, setTick] = useState(0);
  const many = FACES[state].length > 1;
  useEffect(() => {
    if (!many) return;
    const id = setInterval(() => setTick((t) => t + 1), FACE_TICK_MS);
    return () => clearInterval(id);
  }, [many, state]);
  return (
    <span role="img" aria-label={`june is ${state}`} className={`inline-flex shrink-0 items-center whitespace-nowrap rounded-xs border border-hairline-strong bg-card px-1.5 py-0.5 font-mono leading-tight text-foreground ${className}`}>
      {face(state, tick)}
    </span>
  );
}

/** How long the face says done or refused after a question ends before going back to whatever else is true. */
export const ENDED_FACE_MS = 5000;

/** What June is doing right now, read off the store, most pressing first: asleep when the daemon does not answer, and refused when it answers but will not take this window's key; listening while this window's dictation is open or a voice session listens; speaking and thinking off the voice session; thinking while a question is in flight or a dictation is being transcribed; done or refused for a few seconds after one ends; noticed while a notice waits for an answer; recording while a meeting is captured; dreaming during the nightly run; paused while the user has stopped it watching the screen; watching otherwise. Input: whether the daemon answered the last read, and whether watching is paused. Output: the state. */
export function useJuneState(up: boolean, paused = false): JuneState {
  const { run, dictating, voice, recording, dreaming, ended, refused } = useAppSelector((s) => s.progress);
  const liveNotice = useAppSelector((s) => s.ui.liveNotice);
  // A re-render is forced once the done or refused moment is over, since nothing else in the store changes at that instant.
  const [, setPast] = useState(0);
  useEffect(() => {
    if (!ended) return;
    const left = ended.at + ENDED_FACE_MS - Date.now();
    if (left <= 0) return;
    const id = setTimeout(() => setPast(Date.now()), left);
    return () => clearTimeout(id);
  }, [ended]);
  if (!up) return refused ? "refused" : "asleep";
  if ((dictating && !dictating.transcribing) || voice === "listening") return "listening";
  if (voice === "speaking") return "speaking";
  if (run || voice === "thinking" || dictating?.transcribing) return "thinking";
  if (ended && Date.now() - ended.at < ENDED_FACE_MS) return ended.ok ? "done" : "refused";
  if (liveNotice) return "noticed";
  if (recording) return "recording";
  if (dreaming) return "dreaming";
  // Said where "watching" would be, because the rail's "watching" stayed up through a pause and a person who paused on Tuesday wondered on Friday why June remembered nothing.
  return paused ? "paused" : "watching";
}
