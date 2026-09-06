/** Every pure function the React window's screens need to turn what the daemon sent into what a person reads: the date labels, the sidebar's groups, the searches over each list, the minutes reader, and the number formats. Nothing here touches React, Redux or the network — data in, a string or a list out — which is what makes it testable on its own and shared by every screen. The behaviour is the current window's, taken from src/app/render.ts rather than invented again. */

import { truncateAtWord } from "../shared/errorline";
import { noticeActionSuffix } from "../shared/notice";
import type {
  ConversationSummary,
  DaySummary,
  DayView,
  Meeting,
  Notice,
  Spend,
  Task,
  Turn,
} from "./api";

/** Renders a Date as a 24-hour clock time in this window's own style, "HH:MM". The one place hhmm and noticeActionMessage below both turn a Date into a clock face, so the two never drift apart on padding or separators. */
function hhmmFromDate(d: Date): string {
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

/** Formats a timestamp as a 24-hour clock time. Input: an RFC3339 string. Output: "HH:MM", or the input unchanged when it does not parse. */
export function hhmm(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso || "";
  return hhmmFromDate(d);
}

/** The rail-line message a notice becomes once the user has pressed Done or a snooze button on the desktop notification it was also posted as (see internal/proactive/notify.go's chose/snooze/markDone, which send the same notice back with action and until filled in). body is the routine's or task's own text for those two kinds, so the line reads "Send the invoice: Done" or "Priya replied about the venue.: Snoozed until 18:00". Input: the notice, and the moment to compare its until against. Output: the message, or undefined for a notice with no action, which this window does not show at all. The suffix after the colon is the same one the hover's own noticeActionLine computes (see shared/notice.ts); this window only adds the body prefix and renders the clock time in its own 24-hour style. */
export function noticeActionMessage(
  n: Pick<Notice, "body" | "action" | "until">,
  now: Date = new Date(),
): string | undefined {
  const suffix = noticeActionSuffix(n, now, hhmmFromDate);
  return suffix === undefined ? undefined : `${n.body}: ${suffix}`;
}

/** The short label a list row shows beside its title. Input: a timestamp and the moment to compare it against. Output: "15:12" for today, the weekday name within the last week, and "2 Sep" for anything older. */
export function shortWhen(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso || "";
  if (d.toDateString() === now.toDateString()) return hhmm(iso);
  const days = (now.getTime() - d.getTime()) / 86400000;
  if (days >= 0 && days < 7)
    return d.toLocaleDateString(undefined, { weekday: "long" });
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
}

/** The heading that separates one day of a conversation from the next. Input: a timestamp and the moment to compare it against. Output: "Friday 4 September", with " · today" added when it is today. */
export function dayHeading(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso || "";
  const label = d.toLocaleDateString(undefined, {
    weekday: "long",
    day: "numeric",
    month: "long",
  });
  return d.toDateString() === now.toDateString() ? `${label} · today` : label;
}

/** Formats a day's own date as the heading on its page. Input: a YYYY-MM-DD date. Output: "Friday, 4 September", or the input unchanged when it does not parse. */
export function pageHeading(date: string): string {
  const d = new Date(`${date}T00:00:00`);
  if (Number.isNaN(d.getTime())) return date || "";
  return d.toLocaleDateString(undefined, {
    weekday: "long",
    day: "numeric",
    month: "long",
  });
}

/** Formats a day's date the compact way the rail on the left shows it. Input: a YYYY-MM-DD date. Output: "Friday 4", or the input unchanged when it does not parse. */
export function dayShort(date: string): string {
  const d = new Date(`${date}T00:00:00`);
  if (Number.isNaN(d.getTime())) return date || "";
  return `${d.toLocaleDateString(undefined, { weekday: "long" })} ${d.getDate()}`;
}

/** How many whole calendar days back a timestamp is. Input: the date and the moment to count from. Output: 0 for today, 1 for yesterday, and so on; a date in the future counts as 0. */
function daysBack(d: Date, now: Date): number {
  const midnight = new Date(
    now.getFullYear(),
    now.getMonth(),
    now.getDate(),
  ).getTime();
  const its = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  return Math.max(0, Math.round((midnight - its) / 86400000));
}

/** Which heading a conversation sits under in the sidebar. Input: when it was last touched and the moment to compare against. Output: "Today", "Yesterday", "Previous 7 days", or the month it fell in as "August 2026". */
export function groupLabel(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "Earlier";
  const back = daysBack(d, now);
  if (back === 0) return "Today";
  if (back === 1) return "Yesterday";
  if (back < 7) return "Previous 7 days";
  return d.toLocaleDateString(undefined, { month: "long", year: "numeric" });
}

/** One headed block of a list. */
export type Group<T> = { label: string; items: T[] };

/** Buckets rows under headings, keeping the order the rows came in and the order the headings first appear. Input: the rows and the heading each one belongs under. Output: one entry per heading. */
function groupBy<T>(rows: T[], label: (row: T) => string): Group<T>[] {
  const out: Group<T>[] = [];
  for (const row of rows) {
    const key = label(row);
    const group = out.find((g) => g.label === key);
    if (group) group.items.push(row);
    else out.push({ label: key, items: [row] });
  }
  return out;
}

/** Buckets the conversation list into the sidebar's headed groups. Input: the conversations as the daemon sent them, newest first, and the moment to compare against. Output: one entry per heading in the order the headings first appear. */
export function groupConversations(
  convs: ConversationSummary[],
  now: Date = new Date(),
): Group<ConversationSummary>[] {
  return groupBy(convs, (c) => groupLabel(c.updated, now));
}

/** Buckets meetings into one group per stretch of time, the same headings a conversation gets. Input: the meetings, newest first, and the moment to compare against. Output: one entry per heading. */
export function groupMeetings(
  list: Meeting[],
  now: Date = new Date(),
): Group<Meeting>[] {
  return groupBy(list, (m) => groupLabel(m.when, now));
}

/** The days worth listing. Input: the rows GET /days sent. Output: the ones that hold something — a page Ora wrote, screen time recorded, or a meeting kept. A row that reports none of the three counts at all is kept, since a daemon that does not send them has not said the day is empty. */
export function activeDays(days: DaySummary[]): DaySummary[] {
  return days.filter((d) => {
    if (d.has_page) return true;
    const counted =
      d.seen !== undefined ||
      d.meetings !== undefined ||
      d.meeting_minutes !== undefined;
    if (!counted) return true;
    return (
      (d.seen ?? 0) > 0 || (d.meetings ?? 0) > 0 || (d.meeting_minutes ?? 0) > 0
    );
  });
}

/** Buckets days into one group per calendar month. Input: the days, newest first. Output: one entry per month in that order, headed "August 2026". */
export function groupDays(days: DaySummary[]): Group<DaySummary>[] {
  return groupBy(days, (d) => {
    const date = new Date(`${d.date}T00:00:00`);
    return Number.isNaN(date.getTime())
      ? "Earlier"
      : date.toLocaleDateString(undefined, { month: "long", year: "numeric" });
  });
}

/** What a day with no page of its own says under its date. Input: the day's counts. Output: "60 seen · 1 call, 28 min", the parts the daemon actually reported, or "" when it reported none. */
export function dayCounts(d: DaySummary): string {
  const parts: string[] = [];
  if (d.seen) parts.push(`${d.seen} seen`);
  if (d.meetings) {
    const minutes = d.meeting_minutes ? `, ${d.meeting_minutes} min` : "";
    parts.push(`${d.meetings} call${d.meetings === 1 ? "" : "s"}${minutes}`);
  }
  return parts.join(" · ");
}

/** Reads a GNOME accelerator as the keys to draw. Input: a binding such as "<Control><Alt>space", or "". Output: ["Ctrl", "Alt", "Space"], or an empty list when nothing is bound. */
export function hotkeyKeys(accel: string): string[] {
  const names: Record<string, string> = {
    control: "Ctrl",
    ctrl: "Ctrl",
    primary: "Ctrl",
    alt: "Alt",
    shift: "Shift",
    super: "Super",
    meta: "Super",
    space: "Space",
  };
  return String(accel ?? "")
    .replace(/[<>]/g, " ")
    .trim()
    .split(/\s+/)
    .filter(Boolean)
    .map(
      (k) => names[k.toLowerCase()] ?? (k.length === 1 ? k.toUpperCase() : k),
    );
}

/** How much of a failed ask's message is shown before it is folded away. The daemon stores the provider's whole error, which runs past a thousand characters. */
/** The one line a failed ask reads as. Input: the turn's stored text, which is the provider's whole error. Output: its first line, cut at 150 characters on a word boundary with an ellipsis, and whether anything was left out; the same cut the hover makes, from shared/errorline.ts. */
export function errorLine(text: string): { line: string; more: boolean } {
  return truncateAtWord(text ?? "");
}

/** What one of Ora's turns reads as. Input: the turn. Output: its text, replaced for a failed ask by the daemon's own plain sentence, or by one line of the provider's error when the daemon sent none. */
export function turnText(turn: Turn): string {
  if (turn.kind !== "error") return turn.text ?? "";
  return (turn.reason ?? "").trim() || errorLine(turn.text ?? "").line;
}

/** Formats a size the way a person says it. Input: a byte count. Output: "0 B", "21.0 MB", "4.0 GB" — one decimal place above a kilobyte, powers of 1024. */
export function bytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return unit === 0
    ? `${Math.round(value)} B`
    : `${value.toFixed(1)} ${units[unit]}`;
}

/** Writes a token count in full, digits grouped in threes. Input: a count. Output: "1,020"; anything that is not a finite positive number reads as "0", so a missing field never draws NaN. */
export function tokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  return Math.round(n)
    .toString()
    .replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** Shortens a token count to what fits on a bar. Input: a count. Output: "940", "12.4k", "1.3M" — one decimal place under a hundred of the unit, none above it. */
export function compact(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  const unit = (value: number, suffix: string) =>
    `${value < 100 ? value.toFixed(1) : Math.round(value)}${suffix}`;
  if (n >= 1e6) return unit(n / 1e6, "M");
  if (n >= 1000) return unit(n / 1000, "k");
  return String(Math.round(n));
}

/** Says how long a call took. Input: milliseconds. Output: "820ms" under a second, "2.4s" under a minute, and "1m 04s" above it. */
export function took(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "0ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  const minutes = Math.floor(ms / 60000);
  const seconds = Math.round((ms % 60000) / 1000);
  return `${minutes}m ${String(seconds).padStart(2, "0")}s`;
}

/** The second line under a task, when there is one worth showing. Input: the task. Output: its detail, or "" when the detail only repeats the title — GET /tasks sends the whole stored note a noticed task's title was parsed out of, which is not worth a second line. A task Ora noticed says where it came from, since its detail is the meeting or note that raised it. */
export function taskDetail(task: Task): string {
  const detail = (task.detail ?? "").trim();
  if (!detail || !task.title) return "";
  if (detail === task.title || detail.includes(task.title)) return "";
  // A task the user typed carries "you said" as its source; the row already reads as theirs, so the words add nothing.
  if (detail === "you said") return "";
  return task.source === "noticed" ? `from ${detail}` : detail;
}

/** Says how long a meeting ran. Input: its length in seconds. Output: "28 min", "1 h 07", or "" when the recording never reported a length, so a row says nothing rather than "0 min". */
export function meetingLength(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min`;
  return `${Math.floor(minutes / 60)} h ${String(minutes % 60).padStart(2, "0")}`;
}

/** Who was in a meeting, as one line. Input: the attendees the daemon read out of the minutes. Output: their names joined by commas, with a name the recogniser only heard marked "(heard)"; "" when nobody was named. */
export function meetingWho(
  attendees: { name: string; heard_only: boolean }[],
): string {
  return (attendees ?? [])
    .map((a) => (a.heard_only ? `${a.name} (heard)` : a.name))
    .join(", ");
}

/** One line of the minutes as it should be drawn. kind is "h" for one of the minutes' own headings, "bullet" for a list item, "label" for a line that names the items indented under it, and "text" for a paragraph. lead is the bold phrase the daemon opens some bullets with — "You said", "Said to you", a person's name — which names whose part the line is and is set in medium weight ahead of the text rather than left inside it. */
export type MinutesLine = {
  kind: "h" | "bullet" | "label" | "text";
  text: string;
  lead?: string;
};

/** How long a top-level bullet may run before it stops being a list item. The daemon's "Your part" section writes a whole briefing into one bullet — the real ones run to 1,300 characters — and a paragraph that long with a marker beside it reads as a wall rather than as a list, so past this it is drawn as a paragraph. An indented bullet is exempt: it belongs to the label above it and stays in that list at any length. */
const LONG_BULLET = 300;

/** Reads the markdown the daemon stores minutes as into lines to draw. Input: the minutes verbatim and the meeting's title. Output: one entry per non-blank line, with the leading #, - or * taken off and the ** emphasis markers dropped.
 *
 * Four things are decided here rather than in the page. The minutes open with the title as a heading and then repeat it as a bold line carrying the date; the page prints both above the document already, so any opening line that starts with the title is left out. A bullet that opens with a bold phrase and a dash has that phrase lifted out as its lead. A bullet longer than LONG_BULLET is a paragraph, not a list item. And a bullet with items indented under it — "You now owe" is the daemon's — is a label naming that list rather than the first item of it.
 */
export function minutesLines(minutes: string, title = ""): MinutesLine[] {
  const out: MinutesLine[] = [];
  const rows = String(minutes ?? "")
    .split("\n")
    .filter((raw) => raw.trim());
  const indent = (raw: string) => raw.length - raw.trimStart().length;
  for (const [at, raw] of rows.entries()) {
    const line = raw.trim();
    // An item written under a label is indented; however long it runs it stays part of that label's list, or one long item would break the list in two.
    const nested = indent(raw) > 0;
    const heading = /^#{1,6}\s+(.*)$/.exec(line);
    const bullet = /^[-*]\s+(.*)$/.exec(line);
    // A bullet with more deeply indented lines under it names them rather than standing beside them, so it is drawn as a label with no marker and they follow as its list.
    const labels =
      Boolean(bullet) &&
      at + 1 < rows.length &&
      indent(rows[at + 1]) > indent(raw);
    const whole = heading?.[1] ?? bullet?.[1] ?? line;
    // Only a phrase that is bold and followed by a dash is a lead; a dash in the middle of a sentence is punctuation.
    const led = /^\*\*(.+?)\*\*\s*[—–-]\s+(.+)$/s.exec(whole);
    const body = (led ? led[2] : whole).replace(/\*\*/g, "").trim();
    const lead = led ? led[1].replace(/\*\*/g, "").trim() : undefined;
    if (!body) continue;
    if (out.length === 0 && title && body.startsWith(title)) continue;
    const kind = heading
      ? "h"
      : labels
        ? "label"
        : bullet && (nested || body.length <= LONG_BULLET)
          ? "bullet"
          : "text";
    out.push(lead ? { kind, text: body, lead } : { kind, text: body });
  }
  return out;
}

/** The replies a rail can be built beside. Input: every turn of a conversation. Output: the ones Ora sent that read something or called something, in the order they came; a plain reply is not one, and neither is anything the user said. An empty string in the tools list is not a step, which is what the daemon sends for a turn that called nothing.
 *
 * The Chats page and its composer each have to decide whether there is a rail before either of them is drawn, and they have to reach the same answer or the box you type in does not line up with the words above it. This is that one answer.
 */
export function sourcedTurns(turns: Turn[]): Turn[] {
  return (turns ?? []).filter(
    (t) =>
      t.role !== "you" &&
      ((t.evidence ?? []).length > 0 ||
        (t.tools ?? []).filter(Boolean).length > 0),
  );
}

/** The goal of a "do:" question, which starts a computer-use job instead of an ask. Input: the text typed into the composer. Output: the goal with the prefix and any leading space stripped, or undefined for text that does not start with it — including "do:" with nothing after it, which names no goal to run. Case-insensitive, so "Do: reload the page" works the same as "do:". */
export function jobGoal(text: string): string | undefined {
  const m = /^do:\s*(.+)/is.exec(text.trim());
  return m ? m[1].trim() || undefined : undefined;
}

/** Whether a job's own state word is one it may still take a step from. Input: the state, straight off the wire (see actjob.State). Output: false for "done", "stopped", "failed" and "" (no job yet), true for every other word a daemon sends. */
export function isJobLive(state: string): boolean {
  return (
    state !== "" &&
    state !== "done" &&
    state !== "stopped" &&
    state !== "failed"
  );
}

/** The plain word a job's state reads as: the line above its live step list, and the sidebar row's subtitle while it runs. Input: the state. Output: the word. */
export function jobStateWord(state: string): string {
  switch (state) {
    case "planning":
      return "Planning…";
    case "stepping":
      return "Working…";
    case "verifying":
      return "Checking…";
    case "paused":
      return "Paused";
    case "stuck":
      return "Waiting on you";
    case "done":
      return "Done";
    case "stopped":
      return "Stopped";
    case "failed":
      return "Could not finish";
    default:
      return state;
  }
}

/** The line under a finished job: what it cost. Input: the spend. Output: "3 rounds · 15.4k in · 8.2k cached · 640 out", with each model's own input-plus-output tokens added in parentheses once more than one model did any of the rounds — comparing two brains on the same task is the point of keeping the split at all. */
export function costLine(spend: Spend): string {
  const parts = [
    `${spend.rounds} round${spend.rounds === 1 ? "" : "s"}`,
    `${compact(spend.input)} in`,
    `${compact(spend.cached)} cached`,
    `${compact(spend.output)} out`,
  ];
  const models = Object.entries(spend.by_model ?? {});
  if (models.length > 1) {
    const per = models
      .map(([name, u]) => `${name} ${compact(u.input + u.output)}`)
      .join(", ");
    return `${parts.join(" · ")} (${per})`;
  }
  return parts.join(" · ");
}

/** Whether a day has anything to put in the rail beside its page. Input: the day the daemon sent. Output: true when it wrote a line about the day, a morning brief, an evening close that is not just the page over again, or raised any work — the four things the rail carries. A day with none of them gets no rail, and its page is one centred column.
 *
 * The Days header and the day's own page both have to know this before either is drawn, or the picker in the header does not start where the day's first word does.
 */
export function dayRailed(page: DayView): boolean {
  const close = (page.close ?? "").trim();
  return Boolean(
    (page.heading ?? "").trim() ||
    (page.brief ?? "").trim() ||
    (close && close !== page.page) ||
    page.tasks?.length,
  );
}

/** How near the end of a thread a reader has to be for it to keep following the newest turn. 120px — about one turn — so somebody reading the last answer while the next one lands stays with it, and somebody who has scrolled up to read something is left where they are. */
export const STICK = 120;

/** Whether a thread that has just grown should be pulled back to its newest turn. Input: the scrolling region's own three numbers. Output: true while the end of the thread is in view or within STICK pixels below it, which includes a thread too short to scroll at all. */
export function atBottom(view: {
  scrollHeight: number;
  scrollTop: number;
  clientHeight: number;
}): boolean {
  return view.scrollHeight - view.scrollTop - view.clientHeight <= STICK;
}

/** Whether a row matches what was typed into a search field. Input: the query and the row's own text — its title, the line under it, whatever the field is meant to search. Output: true when the query is blank, and otherwise true when any one field contains it, ignoring case and surrounding spaces. */
export function hits(
  query: string,
  ...fields: (string | undefined)[]
): boolean {
  const q = (query ?? "").trim().toLowerCase();
  if (!q) return true;
  return fields.some((f) => (f ?? "").toLowerCase().includes(q));
}

/** The conversations a search leaves showing, matched on title and on the line under it. Input: the whole list and the query. Output: the ones that match, in the order they came. */
export function chatsShown(
  convs: ConversationSummary[],
  query: string,
): ConversationSummary[] {
  return convs.filter((c) => hits(query, c.title, c.last));
}

/** The tasks a search leaves showing, matched on title and on where the task came from. Input: the whole list and the query. Output: the ones that match, in the order they came. */
export function tasksShown(list: Task[], query: string): Task[] {
  return list.filter((t) => hits(query, t.title, t.detail));
}

/** The days a search leaves showing, matched on the date both as it is stored and as the rail writes it, and on the day's title. Input: the days worth listing and the query. Output: the ones that match, in the order they came. */
export function daysShown(list: DaySummary[], query: string): DaySummary[] {
  return list.filter((d) => hits(query, d.date, d.title, dayShort(d.date)));
}

/** The meetings a search leaves showing, matched on the title and on who was there. Input: the whole list and the query. Output: the ones that match, in the order they came. */
export function meetingsShown(list: Meeting[], query: string): Meeting[] {
  return list.filter((m) => hits(query, m.title, meetingWho(m.attendees)));
}

/** The tasks one meeting raised, as the Meetings page pins them above the minutes. Input: everything GET /tasks answered and the meeting. Output: the action items whose provenance is this meeting's own name on this meeting's own day, in the order the daemon listed them. The daemon writes that provenance as "<meeting>, YYYY-MM-DD" (raisedIn in internal/ipc/tasks.go), and the day is what keeps Monday's standup from listing everything every standup ever raised. GET /tasks only ever holds the items that are the user's own and still open, so this needs no filter of its own for either; the items other people took away are left in the minutes text, which is where the model wrote them.
 */
export function meetingTasks(tasks: Task[], meeting: Meeting): Task[] {
  const title = (meeting?.title ?? "").trim();
  if (!title) return [];
  const d = new Date(meeting?.when ?? "");
  const day = Number.isNaN(d.getTime())
    ? ""
    : `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  return tasks.filter((t) => {
    if (t.source !== "noticed") return false;
    const detail = (t.detail ?? "").trim();
    const dated = detail.match(/^(.*), (\d{4}-\d{2}-\d{2})$/);
    const name = dated ? dated[1] : detail;
    if (name !== title) return false;
    // Every instance of a recurring meeting carries the same name, so the day is what tells Monday's standup from Tuesday's. A task filed before the daemon carried the date, or a meeting whose own date will not parse, falls back to the name alone.
    return !dated || !day || dated[2] === day;
  });
}

/** The meeting a noticed task was raised in. Input: every meeting the daemon knows about and the task. Output: the meeting whose name and day match the task's detail line ("Lodestone X growth strategy briefing, 2026-08-31"), or undefined for a task the user typed in and for one whose meeting is no longer on file. The match is meetingTasks' own, asked the other way round, so a task and its meeting agree on which of them belongs to the other. */
export function taskMeeting(meetings: Meeting[], task?: Task): Meeting | undefined {
  if (!task || task.source !== "noticed") return undefined;
  return meetings.find((m) => meetingTasks([task], m).length > 0);
}

/** The lines of one section of a set of minutes. Input: the minutes markdown and the heading to look for, written without its "##". Output: the lines under that heading, read the same way the Meetings page reads them, stopping at the next heading; none when the minutes have no such section. */
export function minutesSection(minutes: string, heading: string): MinutesLine[] {
  const want = heading.trim().toLowerCase();
  const out: MinutesLine[] = [];
  let inside = false;
  for (const line of minutesLines(minutes)) {
    if (line.kind === "h") {
      inside = line.text.trim().toLowerCase() === want;
      continue;
    }
    if (inside) out.push(line);
  }
  return out;
}

/** What the composer on the Tasks page sends alongside a question, so the answer is about the task rather than about nothing. Input: the task. Output: one short passage naming the task and where it came from, or "" when there is no task. The conversation the question goes into carries the rest of the context by itself, which is why none of it is repeated here.
 */
export function taskContext(task?: Task): string {
  if (!task?.title) return "";
  const from = (task.detail ?? "").trim();
  const raised =
    task.source === "noticed"
      ? `Ora noticed it${from ? ` in ${from}` : ""}.`
      : "The user set it themselves.";
  return `This is about one thing on the user's list: "${task.title}". ${raised}`;
}

/** What one question costs on average over a window of the ledger. Input: the tokens spent in that window and the number of calls that spent them. Output: the tokens one call cost, rounded; 0 when nothing has been called, so a machine that has asked nothing reads as nothing rather than dividing by zero. */
export function perQuestion(totalTokens: number, calls: number): number {
  if (!Number.isFinite(totalTokens) || !Number.isFinite(calls) || calls <= 0)
    return 0;
  return Math.round(totalTokens / calls);
}

/** How much of the input a provider answered out of its own cache, over the calls that reported the figure at all. Input: the calls. Output: the cached tokens, the input tokens they were part of, and whether any call reported a cached figure; a daemon that does not send the field yet reports has:false and the window then says nothing about caching rather than claiming nothing was cached. */
export function cachedInput(
  calls: { input_tokens: number; cached_input_tokens?: number }[],
): { cached: number; input: number; has: boolean } {
  let cached = 0;
  let input = 0;
  let has = false;
  for (const c of calls ?? []) {
    if (c.cached_input_tokens === undefined) continue;
    has = true;
    cached += c.cached_input_tokens || 0;
    input += c.input_tokens || 0;
  }
  return { cached, input, has };
}
