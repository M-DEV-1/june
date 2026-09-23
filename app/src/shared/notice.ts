import { clockTime } from "./clock";

/** The Done/snooze suffix a notice's action turns into, shared by the hover's own noticeActionLine (state.ts) and the React window's noticeActionMessage (next/format.ts), which each put the same suffix in a different place in their own line.
 * Input: the action and until fields off a notice, and the moment to compare until against. Output: "Done", "Snoozed until " plus the clock time when until falls on the same day as now, "Snoozed until tomorrow " plus the clock time otherwise, "Snoozed" when until does not parse, or undefined when the notice carries no action yet. */
export function noticeActionSuffix(
  n: { action?: string; until?: string },
  now: Date,
): string | undefined {
  if (n.action === "done") return "Done";
  if (n.action !== "snoozed" || !n.until) return undefined;
  const until = new Date(n.until);
  if (Number.isNaN(until.getTime())) return "Snoozed";
  const sameDay = until.toDateString() === now.toDateString();
  return `Snoozed until ${sameDay ? clockTime(until) : `tomorrow ${clockTime(until)}`}`;
}
