/** Tests for the pure helpers every screen draws through: the date labels, the groups the sidebar and the rails are headed by, the searches and the minutes reader. */

import { describe, expect, it } from "vitest";

import type {
  ConversationSummary,
  DaySummary,
  Meeting,
  Task,
  Turn,
} from "./api";
import { truncateAtWord } from "../shared/errorline";
import {
  activeDays,
  cachedInput,
  perQuestion,
  atBottom,
  dayRailed,
  daysShown,
  modelEffort,
  groupConversations,
  groupLabel,
  meetingTasks,
  keyed,
  minutesLines,
  noticeAge,
  shortWhen,
  sourcedTurns,
  taskDetail,
  tasksShown,
  turnText,
} from "./format";

/** The moment every test that needs one compares against: Friday 4 September 2026, a quarter past three in the afternoon. */
const now = new Date("2026-09-04T15:15:00");

/** One conversation, with only the fields a test cares about spelled out. */
function conv(over: Partial<ConversationSummary> = {}): ConversationSummary {
  return {
    id: "c1",
    title: "A chat",
    brain: "",
    last: "",
    updated: now.toISOString(),
    ...over,
  };
}

/** One day row of GET /days. */
function day(over: Partial<DaySummary> = {}): DaySummary {
  return {
    date: "2026-09-03",
    title: "",
    has_page: false,
    seen: 0,
    meetings: 0,
    meeting_minutes: 0,
    ...over,
  };
}

/** One task of GET /tasks. */
function task(over: Partial<Task> = {}): Task {
  return {
    id: "t1",
    title: "Book the flight",
    source: "you",
    owner: "me",
    when: now.toISOString(),
    done: false,
    conversation_id: "c1",
    detail: "",
    ...over,
  };
}

/** One recording of GET /meetings. */
function meeting(over: Partial<Meeting> = {}): Meeting {
  return {
    id: "m1",
    title: "Standup",
    when: now.toISOString(),
    duration_s: 0,
    minutes: "",
    attendees: [],
    ...over,
  };
}

/** One of June's turns. */
function turn(over: Partial<Turn> = {}): Turn {
  return {
    id: "1",
    role: "june",
    text: "",
    kind: "ask",
    evidence: [],
    tools: [],
    when: now.toISOString(),
    reason: "",
    ...over,
  };
}

describe("clock and date labels", () => {
  it("says the time for today, the weekday within the week, and the date beyond it", () => {
    expect(shortWhen("2026-09-04T09:07:00", now)).toBe("09:07");
    expect(shortWhen("2026-09-01T09:07:00", now)).toBe("Tuesday");
    // The month's short name is the machine's own, so the assertion is on what it says rather than on where the locale puts the number.
    expect(shortWhen("2026-08-02T09:07:00", now)).toContain("Aug");
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
      [
        conv({ id: "a" }),
        conv({ id: "b", updated: "2026-09-03T10:00:00" }),
        conv({ id: "c", updated: "2026-09-03T09:00:00" }),
        conv({ id: "d", updated: "2026-08-02T09:00:00" }),
      ],
      now,
    );
    expect(groups.map((g) => g.label)).toEqual([
      "Today",
      "Yesterday",
      "August 2026",
    ]);
    expect(groups[1].items.map((c) => c.id)).toEqual(["b", "c"]);
  });

  it("lists only the days that hold something, and keeps one that reports no counts at all", () => {
    const kept = activeDays([
      day({ date: "1", has_page: true }),
      day({ date: "2", seen: 12 }),
      day({ date: "3" }),
      { date: "4", title: "", has_page: false } as DaySummary,
    ]);
    expect(kept.map((d) => d.date)).toEqual(["1", "2", "4"]);
  });
});

describe("the searches over each list", () => {
  it("matches a task on its title and on where it came from", () => {
    const list = [
      task({ id: "a" }),
      task({ id: "b", title: "Other", detail: "Meridian call" }),
    ];
    expect(tasksShown(list, "meridian").map((t) => t.id)).toEqual(["b"]);
  });

  it("matches a day on its stored date and on the way the rail writes it", () => {
    expect(daysShown([day({ date: "2026-09-04" })], "friday")).toHaveLength(1);
    expect(daysShown([day({ date: "2026-09-04" })], "2026-09")).toHaveLength(1);
    expect(daysShown([day({ date: "2026-09-04" })], "monday")).toHaveLength(0);
  });
});

describe("tasks", () => {
  it("shows where a noticed task came from and says nothing when the detail only repeats the title", () => {
    expect(taskDetail(task({ source: "noticed", detail: "Meridian call" }))).toBe(
      "from Meridian call",
    );
    expect(taskDetail(task({ detail: "Book the flight" }))).toBe("");
    expect(taskDetail(task({ detail: "" }))).toBe("");
  });
});

describe("what a failed ask reads as", () => {
  it("cuts a long first line on a word boundary and says there is more", () => {
    const long = `${"word ".repeat(60)}end`;
    const { line, more } = truncateAtWord(long);
    expect(more).toBe(true);
    expect(line.endsWith("…")).toBe(true);
    expect(line.length).toBeLessThanOrEqual(151);
  });

  it("says there is more when only the first of several lines is shown", () => {
    expect(truncateAtWord("first line\nsecond line")).toEqual({
      line: "first line",
      more: true,
    });
  });

  it("reads a failed turn as the daemon's own sentence, and falls back to one line of the provider's error", () => {
    expect(
      turnText(
        turn({
          kind: "error",
          text: "raw provider json",
          reason: "The model would not answer.",
        }),
      ),
    ).toBe("The model would not answer.");
    expect(
      turnText(turn({ kind: "error", text: "raw provider json", reason: "" })),
    ).toBe("raw provider json");
    expect(turnText(turn({ text: "an answer" }))).toBe("an answer");
  });
});

describe("meetings", () => {
  it("reads the minutes into headings, bullets and paragraphs and drops the heading that repeats the title", () => {
    const lines = minutesLines(
      "# Standup\n\n## What was said\n- **Vexil** will send the file\nA plain sentence.\n",
      "Standup",
    );
    expect(lines).toMatchObject([
      { kind: "h", text: "What was said" },
      { kind: "bullet", text: "Vexil will send the file" },
      { kind: "text", text: "A plain sentence." },
    ]);
  });

  it("pins the tasks that meeting raised and leaves everything else out", () => {
    const call = meeting({ title: "Meridian call" });
    const list = [
      task({ id: "mine", source: "you", detail: "" }),
      task({ id: "owed", source: "noticed", detail: "Meridian call" }),
      task({ id: "elsewhere", source: "noticed", detail: "Standup" }),
    ];
    expect(meetingTasks(list, call).map((t) => t.id)).toEqual(["owed"]);
    expect(meetingTasks(list, meeting({ title: "" }))).toEqual([]);
  });

  it("pins only the instance of a recurring meeting that raised them", () => {
    // The daemon writes a noticed task's provenance as "<meeting>, YYYY-MM-DD" (raisedIn in internal/ipc/tasks.go), which is the only thing telling Monday's standup from Tuesday's.
    // Built from local parts, so the day the window reads off `when` is 2026-09-07 whatever zone the test runs in.
    const monday = meeting({
      title: "Daily standup",
      when: new Date(2026, 8, 7, 9, 30).toISOString(),
    });
    const list = [
      task({ id: "monday", source: "noticed", detail: "Daily standup, 2026-09-07" }),
      task({ id: "tuesday", source: "noticed", detail: "Daily standup, 2026-09-08" }),
      task({ id: "undated", source: "noticed", detail: "Daily standup" }),
    ];
    expect(meetingTasks(list, monday).map((t) => t.id)).toEqual([
      "monday",
      "undated",
    ]);
  });
});

describe("what a question costs", () => {
  it("reads as nothing rather than dividing by zero on a machine that has asked nothing", () => {
    expect(perQuestion(0, 0)).toBe(0);
    expect(perQuestion(1000, 0)).toBe(0);
    expect(perQuestion(Number.NaN, 4)).toBe(0);
  });
});

describe("how much of the input came out of the cache", () => {
  it("says it has nothing to report when the daemon sends no cached figure, rather than claiming nothing was cached", () => {
    expect(
      cachedInput([{ input_tokens: 1000 }, { input_tokens: 200 }]),
    ).toEqual({ cached: 0, input: 0, has: false });
    expect(cachedInput([])).toEqual({ cached: 0, input: 0, has: false });
  });

  it("counts a call that reported nothing cached, because that is a real zero", () => {
    expect(
      cachedInput([{ input_tokens: 400, cached_input_tokens: 0 }]),
    ).toEqual({ cached: 0, input: 400, has: true });
  });
});

describe("whether a day has anything to put beside it", () => {
  const bare = {
    date: "2026-09-04",
    brief: "",
    close: "",
    page: "Quiet.",
    you: [],
    tasks: [],
    heading: "",
  };

  it("says yes to any one of the day's own line, its brief, its close, or work it raised", () => {
    expect(dayRailed({ ...bare, heading: "366 things seen" })).toBe(true);
    expect(dayRailed({ ...bare, brief: "Deploy #5632 today." })).toBe(true);
    expect(dayRailed({ ...bare, close: "It went well." })).toBe(true);
    expect(
      dayRailed({
        ...bare,
        tasks: [
          {
            id: "1",
            title: "Send the file",
            done: false,
            status: "open",
            owner: "me",
          },
        ],
      }),
    ).toBe(true);
  });

  it("does not count a close that is only the page over again, since the rail would print it twice", () => {
    expect(dayRailed({ ...bare, close: "Quiet." })).toBe(false);
  });
});

describe("whether a thread that has grown should follow its newest turn", () => {
  it("leaves a reader who scrolled up where they are", () => {
    expect(
      atBottom({ scrollHeight: 4000, scrollTop: 3079, clientHeight: 800 }),
    ).toBe(false);
    expect(
      atBottom({ scrollHeight: 4000, scrollTop: 0, clientHeight: 800 }),
    ).toBe(false);
  });

  it("counts a thread too short to scroll as being at its newest turn", () => {
    expect(
      atBottom({ scrollHeight: 600, scrollTop: 0, clientHeight: 800 }),
    ).toBe(true);
  });
});

describe("the replies worth a rail beside them", () => {
  const base = {
    kind: "ask" as const,
    when: "2026-09-05T07:00:00Z",
    reason: "",
    evidence: [],
    tools: [],
  };

  it("counts a reply that read something or called something, and nothing else", () => {
    const turns = [
      { ...base, id: "t1", role: "you" as const, text: "what did she say?" },
      {
        ...base,
        id: "t2",
        role: "june" as const,
        text: "She said Friday.",
        evidence: [{ title: "Meridian call", meta: "meeting", body: "Friday" }],
      },
      {
        ...base,
        id: "t3",
        role: "june" as const,
        text: "Just answered.",
        tools: ["search_memory"],
      },
      {
        ...base,
        id: "t4",
        role: "june" as const,
        text: "Nothing read, nothing called.",
      },
      {
        ...base,
        id: "t5",
        role: "june" as const,
        text: "An empty tool name is not a step.",
        tools: [""],
      },
    ];
    expect(sourcedTurns(turns).map((t) => t.id)).toEqual(["t2", "t3"]);
  });
});

describe("reading the daemon's real minutes", () => {
  // The opening of every set of minutes the daemon writes: the title as a heading, then the same title again as a bold line with the date on it. The page already prints both above the document.
  const opening =
    "# Meridian statement pattern analysis\n**Meridian statement pattern analysis — Fri 4 Sep 2026, 14:30–14:58**\n\n## Your part\n";

  it("drops both the heading and the bold line that only repeat the title and its date", () => {
    const lines = minutesLines(opening, "Meridian statement pattern analysis");
    expect(lines).toMatchObject([{ kind: "h", text: "Your part" }]);
  });

  it("keeps a first line that is not the title", () => {
    const lines = minutesLines(
      "**Fri 4 Sep 2026, 14:30–14:58**\n## Your part\n",
      "Meridian statement pattern analysis",
    );
    expect(lines[0]).toMatchObject({
      kind: "text",
      text: "Fri 4 Sep 2026, 14:30–14:58",
    });
  });

  it("turns a bullet too long to be a bullet into a paragraph, keeping its lead phrase apart from the text", () => {
    const long = "x".repeat(340);
    const lines = minutesLines(`- **You said** — ${long}`);
    expect(lines).toMatchObject([{ kind: "text", text: long, lead: "You said" }]);
  });

  it("leaves a short bullet a bullet, and still keeps its lead phrase apart", () => {
    const lines = minutesLines(
      "- **Said to you** — Vexil agreed the pattern set is good enough.",
    );
    expect(lines).toMatchObject([
      {
        kind: "bullet",
        text: "Vexil agreed the pattern set is good enough.",
        lead: "Said to you",
      },
    ]);
  });

  it("leaves a long bullet with no lead phrase a paragraph with no lead", () => {
    const long = `Presented the output of an AI-agent pipeline. ${"y".repeat(300)}`;
    expect(minutesLines(`- ${long}`)).toMatchObject([{ kind: "text", text: long }]);
  });

  it("keeps an indented bullet a bullet however long it runs, because it belongs to the label above it", () => {
    // The daemon writes "You now owe" as a label with its items indented under it; one item running past the paragraph length must not break that list in half.
    const owed = [
      "- **You now owe**",
      `  - ${"a".repeat(320)}`,
      `  - ${"b".repeat(180)}`,
    ].join("\n");
    expect(minutesLines(owed).map((l) => l.kind)).toEqual([
      "label",
      "bullet",
      "bullet",
    ]);
  });

  it('reads the real shape of "You now owe": the label names the list under it rather than being an item of it', () => {
    const owed = [
      "## Your part",
      "- **You now owe**",
      "  - Build a single Excel workbook of the Meridian statement patterns.",
      "  - Check the remaining rows against the template.",
    ].join("\n");
    expect(minutesLines(owed)).toMatchObject([
      { kind: "h", text: "Your part" },
      { kind: "label", text: "You now owe" },
      {
        kind: "bullet",
        text: "Build a single Excel workbook of the Meridian statement patterns.",
      },
      {
        kind: "bullet",
        text: "Check the remaining rows against the template.",
      },
    ]);
  });

  it("makes a label of any bullet with items indented under it, keeping its lead phrase apart", () => {
    const lines = minutesLines(
      [
        "- **Vexil Quorin** — took two things away",
        "  - Combine the two tables into one.",
        "- Someone else said something short.",
      ].join("\n"),
    );
    expect(lines.map((l) => l.kind)).toEqual(["label", "bullet", "bullet"]);
    expect(lines[0]).toMatchObject({
      kind: "label",
      text: "took two things away",
      lead: "Vexil Quorin",
    });
  });

  it("does not mistake a dash inside a sentence for a lead phrase", () => {
    expect(minutesLines("- Vexil — who leads the work — agreed.")).toMatchObject([
      { kind: "bullet", text: "Vexil — who leads the work — agreed." },
    ]);
  });
});

describe("taskDetail on a task the user typed", () => {
  it("says nothing for the source line 'you said', since a task the user typed needs no reminder of that", () => {
    expect(
      taskDetail({
        id: "7",
        title: "Book the flight",
        detail: "you said",
        source: "asked",
        done: false,
        owner: "me",
      } as never),
    ).toBe("");
  });
});

describe("modelEffort", () => {
  it("splits the effort Antigravity encodes in the model id off the model itself", () => {
    expect(modelEffort("gemini-3.8-flash-high")).toEqual({ model: "gemini-3.8-flash", effort: "high" });
    expect(modelEffort("gpt-oss-120b-medium")).toEqual({ model: "gpt-oss-120b", effort: "medium" });
  });

  it("leaves a model that carries no effort suffix whole", () => {
    expect(modelEffort("claude-opus-4-6-thinking")).toEqual({ model: "claude-opus-4-6-thinking", effort: "" });
    expect(modelEffort("sonnet")).toEqual({ model: "sonnet", effort: "" });
  });
});

// The notice card says how long ago the notice landed, the way the design sheets of 2026-09-12 draw it: "now", then "5m ago", then "12m ago". A notice carries no time of its own from the daemon, so the card stamps its arrival and reads it against the clock; without this the card would either say nothing or keep saying "now" for an hour.
describe("noticeAge", () => {
  const at = new Date("2026-09-12T14:00:00").getTime();
  // A clock that has gone backwards — the machine resyncing, or a notice stamped a moment in the future — must not read "-1m ago".
  it("says now when the stamp is ahead of the clock", () => {
    expect(noticeAge(at, new Date("2026-09-12T13:59:00"))).toBe("now");
  });
});

describe("keyed", () => {
  it("gives the same text a key each time it appears, so a repeated paragraph is still its own row", () => {
    expect(keyed(["a", "b", "a"], (s) => s).map((k) => k.key)).toEqual(["a", "b", "a#1"]);
  });
});
