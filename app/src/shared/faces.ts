/** Ora's face: one kaomoji per state, shared by the main window (src/next/face.tsx) and the hover window (src/main.ts) so the two never show different faces for the same thing. The set was picked by hand on 2026-09-07 from a sheet of four candidates per state; a state with more than one face alternates through them. */

export type OraState = "watching" | "listening" | "thinking" | "speaking" | "done" | "refused" | "asleep" | "dreaming" | "noticed" | "recording";

/** The faces for each state, in the order they alternate. */
export const FACES: Record<OraState, string[]> = {
  watching: ["(・ω・)", "(¬‿¬)"],
  listening: ["( ･ω･)ﾉ", "(°ロ°)"],
  thinking: ["(ノ_<。)", "( ・・)?", "(￣ω￣;)"],
  speaking: ["(^▽^)"],
  done: ["(^-^)b"],
  refused: ["(￣ヘ￣)", "(ಠ_ಠ)"],
  asleep: ["(￣o￣) zzZ"],
  dreaming: ["(￣.￣)…", "(っ˘ω˘ς )"],
  noticed: ["( ‘o’)"],
  recording: ["( ✧≖ ͜ʖ≖)"],
};

/** How often a state with several faces changes face, in milliseconds. */
export const FACE_TICK_MS = 4000;

/** Input: the state and a tick counter that grows over time. Output: the face to show now, wrapping through the state's list. */
export function face(state: OraState, tick = 0): string {
  const list = FACES[state];
  return list[tick % list.length];
}
