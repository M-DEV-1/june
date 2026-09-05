/** Tests for the pure helpers every screen draws through: the date labels, the groups the sidebar and the rails are headed by, the searches, the minutes reader and the number formats. */

import { describe, expect, it } from "vitest";

import type { ConversationSummary, DaySummary, Meeting, Task, Turn } from "./api";
import {
  activeDays,
  bytes,
  cachedInput,
  perQuestion,
  chatsShown,
  compact,
  dayCounts,
  dayHeading,
  dayShort,
  atBottom,
  dayRailed,
  daysShown,
  errorLine,
  groupConversations,
  groupDays,
  groupLabel,
  groupMeetings,
  hhmm,
  hits,
  hotkeyKeys,
  meetingTasks,
  meetingLength,
  meetingWho,
  meetingsShown,
  minutesLines,
  pageHeading,
  shortWhen,
  sourcedTurns,
  taskContext,
  taskDetail,
  tasksShown,
  tokens,
  took,
  turnText,
} from "./format";

/** The moment every test that needs one compares against: Friday 4 September 2026, a quarter past three in the afternoon. */
const now = new Date("2026-09-04T15:15:00");

/** One conversation, with only the fields a test cares about spelled out. */
function conv(over: Partial<ConversationSummary> = {}): ConversationSummary {
  return { id: "c1", title: "A chat", brain: "", last: "", updated: now.toISOString(), ...over };
}

/** One day row of GET /days. */
function day(over: Partial<DaySummary> = {}): DaySummary {
  return { date: "2026-09-03", title: "", has_page: false, seen: 0, meetings: 0, meeting_minutes: 0, ...over };
}

/** One task of GET /tasks. */
function task(over: Partial<Task> = {}): Task {
  return { id: "t1", title: "Book the flight", source: "you", when: now.toISOString(), done: false, conversation_id: "c1", detail: "", ...over };
}

/** One recording of GET /meetings. */
function meeting(over: Partial<Meeting> = {}): Meeting {
  return { id: "m1", title: "Standup", when: now.toISOString(), duration_s: 0, minutes: "", attendees: [], ...over };
}

/** One of Ora's turns. */
function turn(over: Partial<Turn> = {}): Turn {
  return { id: "1", role: "ora", text: "", kind: "ask", evidence: [], tools: [], when: now.toISOString(), reason: "", ...over };
}

describe("clock and date labels", () => {
  it("writes a time as a 24-hour clock and leaves anything unparseable alone", () => {
    expect(hhmm("2026-09-04T09:07:00")).toBe("09:07");
    expect(hhmm("not a time")).toBe("not a time");
    expect(hhmm("")).toBe("");
  });

  it("says the time for today, the weekday within the week, and the date beyond it", () => {
    expect(shortWhen("2026-09-04T09:07:00", now)).toBe("09:07");
    expect(shortWhen("2026-09-01T09:07:00", now)).toBe("Tuesday");
    // The month's short name is the machine's own, so the assertion is on what it says rather than on where the locale puts the number.
    expect(shortWhen("2026-08-02T09:07:00", now)).toContain("Aug");
  });

  it("marks today's heading as today and leaves another day's alone", () => {
    expect(dayHeading("2026-09-04T09:00:00", now)).toContain("Friday");
    expect(dayHeading("2026-09-04T09:00:00", now)).toContain("· today");
    expect(dayHeading("2026-09-02T09:00:00", now)).toContain("Wednesday");
    expect(dayHeading("2026-09-02T09:00:00", now)).not.toContain("today");
  });

  it("writes a day's own date long on its page and short in the rail", () => {
    expect(pageHeading("2026-09-04")).toContain("September");
    expect(pageHeading("2026-09-04")).toContain("Friday");
    expect(dayShort("2026-09-04")).toBe("Friday 4");
    expect(dayShort("rubbish")).toBe("rubbish");
  });
});

describe("the headings a list is grouped under", () => {
  it("puts a conversation under today, yesterday, the week, or its month", () => {
    expect(groupLabel("2026-09-04T01:00:00", now)).toBe("Today");
    expect(groupLabel("2026-09-03T23:00:00", now)).toBe("Yesterday");
    expect(groupLabel("2026-08-31T10:00:00", now)).toBe("Previous 7 days");
    expect(groupLabel("2026-08-02T10:00:00", now)).toBe("August 2026");
    expect(groupLabel("not a date", now)).toBe("Earlier");
  });

  it("keeps the order the conversations came in and heads each stretch once", () => {
    const groups = groupConversations(
      [conv({ id: "a" }), conv({ id: "b", updated: "2026-09-03T10:00:00" }), conv({ id: "c", updated: "2026-09-03T09:00:00" }), conv({ id: "d", updated: "2026-08-02T09:00:00" })],
      now,
    );
    expect(groups.map((g) => g.label)).toEqual(["Today", "Yesterday", "August 2026"]);
    expect(groups[1].items.map((c) => c.id)).toEqual(["b", "c"]);
  });

  it("groups meetings the same way conversations are grouped", () => {
    const groups = groupMeetings([meeting({ id: "m1" }), meeting({ id: "m2", when: "2026-08-02T09:00:00" })], now);
    expect(groups.map((g) => g.label)).toEqual(["Today", "August 2026"]);
  });

  it("groups days one month at a time", () => {
    const groups = groupDays([day({ date: "2026-09-03" }), day({ date: "2026-09-01" }), day({ date: "2026-08-30" })]);
    expect(groups.map((g) => g.label)).toEqual(["September 2026", "August 2026"]);
    expect(groups[0].items).toHaveLength(2);
  });

  it("lists only the days that hold something, and keeps one that reports no counts at all", () => {
    const kept = activeDays([day({ date: "1", has_page: true }), day({ date: "2", seen: 12 }), day({ date: "3" }), { date: "4", title: "", has_page: false } as DaySummary]);
    expect(kept.map((d) => d.date)).toEqual(["1", "2", "4"]);
  });

  it("says what a day with no page of its own holds", () => {
    expect(dayCounts(day({ seen: 60, meetings: 1, meeting_minutes: 28 }))).toBe("60 seen · 1 call, 28 min");
    expect(dayCounts(day({ meetings: 2 }))).toBe("2 calls");
    expect(dayCounts(day())).toBe("");
  });
});

describe("the searches over each list", () => {
  it("keeps everything when nothing is typed and matches without case", () => {
    expect(hits("", "anything")).toBe(true);
    expect(hits(" FLIGHT ", "book the flight")).toBe(true);
    expect(hits("train", "book the flight")).toBe(false);
  });

  it("matches a conversation on its title and on the line under it", () => {
    const list = [conv({ id: "a", title: "Flights" }), conv({ id: "b", title: "Other", last: "the flight is booked" }), conv({ id: "c", title: "Nothing" })];
    expect(chatsShown(list, "flight").map((c) => c.id)).toEqual(["a", "b"]);
  });

  it("matches a task on its title and on where it came from", () => {
    const list = [task({ id: "a" }), task({ id: "b", title: "Other", detail: "TCFD call" })];
    expect(tasksShown(list, "tcfd").map((t) => t.id)).toEqual(["b"]);
  });

  it("matches a day on its stored date and on the way the rail writes it", () => {
    expect(daysShown([day({ date: "2026-09-04" })], "friday")).toHaveLength(1);
    expect(daysShown([day({ date: "2026-09-04" })], "2026-09")).toHaveLength(1);
    expect(daysShown([day({ date: "2026-09-04" })], "monday")).toHaveLength(0);
  });

  it("matches a meeting on its title and on who was there", () => {
    const list = [meeting({ id: "a", attendees: [{ name: "Priya", heard_only: false }] }), meeting({ id: "b", title: "Other" })];
    expect(meetingsShown(list, "priya").map((m) => m.id)).toEqual(["a"]);
  });
});

describe("tasks", () => {
  it("shows where a noticed task came from and says nothing when the detail only repeats the title", () => {
    expect(taskDetail(task({ source: "noticed", detail: "TCFD call" }))).toBe("from TCFD call");
    expect(taskDetail(task({ detail: "Book the flight" }))).toBe("");
    expect(taskDetail(task({ detail: "" }))).toBe("");
  });

  it("says what the composer should send along with a question about a task", () => {
    expect(taskContext(task({ title: "Book the flight" }))).toBe('This is about one thing on the user\'s list: "Book the flight". The user set it themselves.');
    expect(taskContext(task({ source: "noticed", detail: "TCFD call" }))).toBe('This is about one thing on the user\'s list: "Book the flight". Ora noticed it in TCFD call.');
    expect(taskContext(task({ source: "noticed", detail: "" }))).toBe('This is about one thing on the user\'s list: "Book the flight". Ora noticed it.');
    expect(taskContext(undefined)).toBe("");
  });
});

describe("what a failed ask reads as", () => {
  it("keeps a short message whole and says nothing was left out", () => {
    expect(errorLine("the model refused")).toEqual({ line: "the model refused", more: false });
  });

  it("cuts a long first line on a word boundary and says there is more", () => {
    const long = `${"word ".repeat(60)}end`;
    const { line, more } = errorLine(long);
    expect(more).toBe(true);
    expect(line.endsWith("…")).toBe(true);
    expect(line.length).toBeLessThanOrEqual(151);
  });

  it("says there is more when only the first of several lines is shown", () => {
    expect(errorLine("first line\nsecond line")).toEqual({ line: "first line", more: true });
  });

  it("reads a failed turn as the daemon's own sentence, and falls back to one line of the provider's error", () => {
    expect(turnText(turn({ kind: "error", text: "raw provider json", reason: "The model would not answer." }))).toBe("The model would not answer.");
    expect(turnText(turn({ kind: "error", text: "raw provider json", reason: "" }))).toBe("raw provider json");
    expect(turnText(turn({ text: "an answer" }))).toBe("an answer");
  });
});

describe("meetings", () => {
  it("says how long a recording ran, and says nothing when it never reported a length", () => {
    expect(meetingLength(1680)).toBe("28 min");
    expect(meetingLength(4020)).toBe("1 h 07");
    expect(meetingLength(0)).toBe("");
  });

  it("names who was there and marks a name that was only heard", () => {
    expect(meetingWho([{ name: "Priya", heard_only: false }, { name: "Sam", heard_only: true }])).toBe("Priya, Sam (heard)");
    expect(meetingWho([])).toBe("");
  });

  it("reads the minutes into headings, bullets and paragraphs and drops the heading that repeats the title", () => {
    const lines = minutesLines("# Standup\n\n## What was said\n- **Priya** will send the file\nA plain sentence.\n", "Standup");
    expect(lines).toEqual([
      { kind: "h", text: "What was said" },
      { kind: "bullet", text: "Priya will send the file" },
      { kind: "text", text: "A plain sentence." },
    ]);
  });

  it("pins the tasks that meeting raised and leaves everything else out", () => {
    const call = meeting({ title: "TCFD call" });
    const list = [
      task({ id: "mine", source: "you", detail: "" }),
      task({ id: "owed", source: "noticed", detail: "TCFD call" }),
      task({ id: "elsewhere", source: "noticed", detail: "Standup" }),
    ];
    expect(meetingTasks(list, call).map((t) => t.id)).toEqual(["owed"]);
    expect(meetingTasks(list, meeting({ title: "" }))).toEqual([]);
  });
});

describe("numbers as a person writes them", () => {
  it("sizes bytes in powers of 1024", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(900)).toBe("900 B");
    expect(bytes(22020096)).toBe("21.0 MB");
    expect(bytes(-1)).toBe("0 B");
  });

  it("groups token counts in threes and shortens them for a bar", () => {
    expect(tokens(1020)).toBe("1,020");
    expect(tokens(Number.NaN)).toBe("0");
    expect(compact(940)).toBe("940");
    expect(compact(12400)).toBe("12.4k");
    expect(compact(1300000)).toBe("1.3M");
  });

  it("says how long a call took in the unit that fits", () => {
    expect(took(820)).toBe("820ms");
    expect(took(2400)).toBe("2.4s");
    expect(took(64000)).toBe("1m 04s");
    expect(took(0)).toBe("0ms");
  });

  it("reads a GNOME accelerator as the keys to draw", () => {
    expect(hotkeyKeys("<Control><Alt>space")).toEqual(["Ctrl", "Alt", "Space"]);
    expect(hotkeyKeys("<Super>k")).toEqual(["Super", "K"]);
    expect(hotkeyKeys("")).toEqual([]);
  });
});

describe("what a question costs", () => {
  it("averages the tokens of a window over the calls that spent them", () => {
    // 15,400 tokens over 9 calls is 1,711 a question, which is what the figure beside the three totals reads.
    expect(perQuestion(15400, 9)).toBe(1711);
    expect(perQuestion(1600, 3)).toBe(533);
  });

  it("reads as nothing rather than dividing by zero on a machine that has asked nothing", () => {
    expect(perQuestion(0, 0)).toBe(0);
    expect(perQuestion(1000, 0)).toBe(0);
    expect(perQuestion(Number.NaN, 4)).toBe(0);
  });
});

describe("how much of the input came out of the cache", () => {
  it("adds up only the calls that reported a cached figure", () => {
    const calls = [
      { input_tokens: 1000, cached_input_tokens: 800 },
      { input_tokens: 500, cached_input_tokens: 100 },
    ];
    expect(cachedInput(calls)).toEqual({ cached: 900, input: 1500, has: true });
  });

  it("says it has nothing to report when the daemon sends no cached figure, rather than claiming nothing was cached", () => {
    expect(cachedInput([{ input_tokens: 1000 }, { input_tokens: 200 }])).toEqual({ cached: 0, input: 0, has: false });
    expect(cachedInput([])).toEqual({ cached: 0, input: 0, has: false });
  });

  it("counts a call that reported nothing cached, because that is a real zero", () => {
    expect(cachedInput([{ input_tokens: 400, cached_input_tokens: 0 }])).toEqual({ cached: 0, input: 400, has: true });
  });
});

describe("whether a day has anything to put beside it", () => {
  const bare = { date: "2026-09-04", brief: "", close: "", page: "Quiet.", you: [], tasks: [], heading: "" };

  it("says no to a day the daemon wrote nothing around", () => {
    expect(dayRailed(bare)).toBe(false);
  });

  it("says yes to any one of the day's own line, its brief, its close, or work it raised", () => {
    expect(dayRailed({ ...bare, heading: "366 things seen" })).toBe(true);
    expect(dayRailed({ ...bare, brief: "Deploy #5632 today." })).toBe(true);
    expect(dayRailed({ ...bare, close: "It went well." })).toBe(true);
    expect(dayRailed({ ...bare, tasks: [{ title: "Send the file", done: false }] })).toBe(true);
  });

  it("does not count a close that is only the page over again, since the rail would print it twice", () => {
    expect(dayRailed({ ...bare, close: "Quiet." })).toBe(false);
  });
});

describe("whether a thread that has grown should follow its newest turn", () => {
  it("follows while the newest turn is in view, or within a screen's last 120px of it", () => {
    expect(atBottom({ scrollHeight: 4000, scrollTop: 3200, clientHeight: 800 })).toBe(true);
    expect(atBottom({ scrollHeight: 4000, scrollTop: 3080, clientHeight: 800 })).toBe(true);
  });

  it("leaves a reader who scrolled up where they are", () => {
    expect(atBottom({ scrollHeight: 4000, scrollTop: 3079, clientHeight: 800 })).toBe(false);
    expect(atBottom({ scrollHeight: 4000, scrollTop: 0, clientHeight: 800 })).toBe(false);
  });

  it("counts a thread too short to scroll as being at its newest turn", () => {
    expect(atBottom({ scrollHeight: 600, scrollTop: 0, clientHeight: 800 })).toBe(true);
  });
});

describe("the replies worth a rail beside them", () => {
  const base = { kind: "ask" as const, when: "2026-09-05T07:00:00Z", reason: "", evidence: [], tools: [] };

  it("counts a reply that read something or called something, and nothing else", () => {
    const turns = [
      { ...base, id: "t1", role: "you" as const, text: "what did she say?" },
      { ...base, id: "t2", role: "ora" as const, text: "She said Friday.", evidence: [{ title: "TCFD call", meta: "meeting", body: "Friday" }] },
      { ...base, id: "t3", role: "ora" as const, text: "Just answered.", tools: ["search_memory"] },
      { ...base, id: "t4", role: "ora" as const, text: "Nothing read, nothing called." },
      { ...base, id: "t5", role: "ora" as const, text: "An empty tool name is not a step.", tools: [""] },
    ];
    expect(sourcedTurns(turns).map((t) => t.id)).toEqual(["t2", "t3"]);
  });

  it("says a thread of plain replies has none, which is what drops the rail", () => {
    expect(sourcedTurns([{ ...base, id: "t1", role: "ora", text: "pong" }])).toEqual([]);
    expect(sourcedTurns([])).toEqual([]);
  });
});

describe("reading the daemon's real minutes", () => {
  // The opening of every set of minutes the daemon writes: the title as a heading, then the same title again as a bold line with the date on it. The page already prints both above the document.
  const opening = "# TCFD statement pattern analysis\n**TCFD statement pattern analysis — Fri 4 Sep 2026, 14:30–14:58**\n\n## Your part\n";

  it("drops both the heading and the bold line that only repeat the title and its date", () => {
    const lines = minutesLines(opening, "TCFD statement pattern analysis");
    expect(lines).toEqual([{ kind: "h", text: "Your part" }]);
  });

  it("keeps a first line that is not the title", () => {
    const lines = minutesLines("**Fri 4 Sep 2026, 14:30–14:58**\n## Your part\n", "TCFD statement pattern analysis");
    expect(lines[0]).toEqual({ kind: "text", text: "Fri 4 Sep 2026, 14:30–14:58" });
  });

  it("turns a bullet too long to be a bullet into a paragraph, keeping its lead phrase apart from the text", () => {
    const long = "x".repeat(340);
    const lines = minutesLines(`- **You said** — ${long}`);
    expect(lines).toEqual([{ kind: "text", text: long, lead: "You said" }]);
  });

  it("leaves a short bullet a bullet, and still keeps its lead phrase apart", () => {
    const lines = minutesLines("- **Said to you** — Priya agreed the pattern set is good enough.");
    expect(lines).toEqual([{ kind: "bullet", text: "Priya agreed the pattern set is good enough.", lead: "Said to you" }]);
  });

  it("leaves a long bullet with no lead phrase a paragraph with no lead", () => {
    const long = `Presented the output of an AI-agent pipeline. ${"y".repeat(300)}`;
    expect(minutesLines(`- ${long}`)).toEqual([{ kind: "text", text: long }]);
  });

  it("keeps an indented bullet a bullet however long it runs, because it belongs to the label above it", () => {
    // The daemon writes "You now owe" as a label with its items indented under it; one item running past the paragraph length must not break that list in half.
    const owed = ["- **You now owe**", `  - ${"a".repeat(320)}`, `  - ${"b".repeat(180)}`].join("\n");
    expect(minutesLines(owed).map((l) => l.kind)).toEqual(["label", "bullet", "bullet"]);
  });

  it("reads the real shape of \"You now owe\": the label names the list under it rather than being an item of it", () => {
    const owed = ["## Your part", "- **You now owe**", "  - Build a single Excel workbook of the TCFD statement patterns.", "  - Continue researching vulnerability scoring methodology."].join("\n");
    expect(minutesLines(owed)).toEqual([
      { kind: "h", text: "Your part" },
      { kind: "label", text: "You now owe" },
      { kind: "bullet", text: "Build a single Excel workbook of the TCFD statement patterns." },
      { kind: "bullet", text: "Continue researching vulnerability scoring methodology." },
    ]);
  });

  it("makes a label of any bullet with items indented under it, keeping its lead phrase apart", () => {
    const lines = minutesLines(["- **Priya Shah** — took two things away", "  - Combine the two tables into one.", "- Someone else said something short."].join("\n"));
    expect(lines.map((l) => l.kind)).toEqual(["label", "bullet", "bullet"]);
    expect(lines[0]).toEqual({ kind: "label", text: "took two things away", lead: "Priya Shah" });
  });

  it("leaves a bullet with no indented items a bullet, even when a bullet at the same depth follows it", () => {
    expect(minutesLines(["- One thing.", "- Another thing."].join("\n")).map((l) => l.kind)).toEqual(["bullet", "bullet"]);
  });

  it("does not mistake a dash inside a sentence for a lead phrase", () => {
    expect(minutesLines("- Priya — who leads the work — agreed.")).toEqual([{ kind: "bullet", text: "Priya — who leads the work — agreed." }]);
  });
});
