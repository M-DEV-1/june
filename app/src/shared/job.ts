/** The computer-use job rules both windows apply: the "do:" prefix that starts a job, whether a job's state word is still live, and how an "act" event's detail decodes. */

import type { Spend } from "./wire";

/** The goal of a "do:" question, which starts a computer-use job instead of an ask. Input: the text typed into the composer. Output: the goal with the prefix and any leading space stripped, or undefined for text that does not start with it, including "do:" with nothing after it, which names no goal to run. Case-insensitive, so "Do: reload the page" works the same as "do:". */
export function jobGoal(text: string): string | undefined {
  const m = /^do:\s*(.+)/is.exec(text.trim());
  return m ? m[1].trim() || undefined : undefined;
}

/** Whether a job's own state word is one it may still take a step from. Input: the state, straight off the wire (see actjob.State). Output: false for "done", "stopped", "failed" and "" (no job yet), true for every other word a daemon sends. */
export function isJobLive(state: string): boolean {
  return state !== "" && state !== "done" && state !== "stopped" && state !== "failed";
}

/** The shape of an "act" event's JSON detail (see internal/actjob.Event on the Go side). kind is "started", "plan", "step", "verified", "question", "answered", "paused", "resumed" or "done". */
export type ActDetail = { kind: string; state: string; text: string; step?: number; expect?: string; outcome?: string; held_before?: boolean; spend?: Spend };

/** Decodes one "act" event's detail. Input: the detail text off the wire. Output: the parts, or every field empty when the text will not parse, which never happens against a daemon that sent it but leaves nothing to throw on a malformed one. */
export function parseActDetail(detail: string | undefined): ActDetail {
  try {
    return JSON.parse(detail ?? "{}") as ActDetail;
  } catch {
    return { kind: "", state: "", text: "" };
  }
}
