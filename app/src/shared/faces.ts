/** June's face: one kaomoji per state, shared by the main window (src/next/face.tsx) and the hover window (src/main.ts) so the two never show different faces for the same thing. A state with more than one face alternates through them.
 *
 * Three sets stacked, newest first. Each state leads with the face he picked off the specimen sheet on 2026-09-12; behind it sits the minimal face drawn from that day's design sheets, and behind that the hand-picked set of 2026-09-07. Nothing was retired, because he liked all of it: face() walks each list on a timer, so every face still gets its turn, and the one that leads is the one a still frame shows.
 * The first face of each state is the one that matters most, since it is what a single reading takes.
 */

export type JuneState = "watching" | "listening" | "thinking" | "speaking" | "done" | "refused" | "asleep" | "dreaming" | "noticed" | "recording";

/** The faces for each state, in the order they alternate. */
export const FACES: Record<JuneState, string[]> = {
  asleep: ["(-‿-)", "(￣o￣) zzZ"],
  watching: ["( •_•)>", "(•‿•)", "(・ω・)", "(¬‿¬)"],
  thinking: ["(￢_￢)…", "(-.-)", "(ノ_<。)", "( ・・)?", "(￣ω￣;)"],
  speaking: ["(-‿^)", "(^▽^)"],
  done: ["(•‿•)b", "(^‿^)", "(^-^)b"],
  noticed: ["(-‿-)!", "( ‘o’)"],
  listening: ["(•_•)", "( ･ω･)ﾉ", "(°ロ°)"],
  refused: ["(・_・;)", "(-_-)", "(￣ヘ￣)", "(ಠ_ಠ)"],
  dreaming: ["(-, -)…", "(˘‿˘)", "(￣.￣)…", "(っ˘ω˘ς )"],
  recording: ["(≖‿≖)", "(●_●)", "( ✧≖ ͜ʖ≖)"],
};

/** How often a state with several faces changes face, in milliseconds. */
export const FACE_TICK_MS = 4000;

/** Input: the state and a tick counter that grows over time. Output: the face to show now, wrapping through the state's list. */
export function face(state: JuneState, tick = 0): string {
  const list = FACES[state];
  return list[tick % list.length];
}

/** The overlay's label pill mood, one of the four the pointer drawings use: "point" for ink showing where something is, "act" for a press about to happen, "done" for a thing finished, and "neutral" for anything else. */
export type OverlayMood = "point" | "act" | "done" | "neutral";

