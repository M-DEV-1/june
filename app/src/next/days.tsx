/** The Days screen: one day's page at a time — the sentences Ora wrote that night, under the day's own date and the one line the daemon writes about what the day held. The day is chosen from the picker in the header, which carries a search and each day's counts; there is no list of days beside the sidebar, because the sidebar is for chats. */

import { Check } from "lucide-react";

import { useDayQuery, useDaysQuery, type DayView } from "./api";
import { activeDays, dayCounts, dayRailed, dayShort, daysShown, groupDays, pageHeading } from "./format";
import { Blank, Group, HEAD, Outline, PageHeader, Picker, Rail, RailBlock, Reading, Scroller, SectionHeading, TAIL, useReading, useWide } from "./parts";
import { ui, useAppDispatch, useAppSelector } from "./store";

/** The work a day raised, as it reads on the page: a tick that says what became of each one, and nothing to click, since a day's page is a record of what happened rather than a place to change it. Input: the day's tasks. Output: the block, or nothing when the day raised none. */
function Raised({ tasks }: { tasks: DayView["tasks"] }) {
  if (!tasks?.length) return null;
  return (
    <div id="day-raised" className="mt-10">
      <SectionHeading>Raised that day</SectionHeading>
      <Group>
        <ul className="divide-y">
          {tasks.map((t, i) => (
            <li key={`${t.title}-${i}`} className="flex items-center gap-2.5 px-3 py-2 text-ui">
              <span
                className={`grid size-[15px] shrink-0 place-items-center rounded-full border ${t.done ? "border-primary bg-primary text-primary-foreground" : "border-hairline-strong text-transparent"}`}
              >
                <Check aria-label={t.done ? "done" : "still open"} className="size-2.5" strokeWidth={3} />
              </span>
              <span className={t.done ? "text-muted-foreground line-through" : ""}>{t.title}</span>
            </li>
          ))}
        </ul>
      </Group>
    </div>
  );
}

/** One day's page: its date, the daemon's own line about the day, the sentences of the page itself typeset as a document, and the work it raised. Input: the page the daemon sent and whether the pane is wide enough for a rail. Output: the column, and beside it in a wide pane the day's own summary lines and an outline of its parts. */
function Page({ page, wide }: { page: DayView; wide: boolean }) {
  const sentences = (page.page ?? "")
    .split(/\n+/)
    .map((s) => s.trim())
    .filter(Boolean);
  const heading = (page.heading ?? "").trim();
  const brief = (page.brief ?? "").trim();
  const close = (page.close ?? "").trim();

  // In a wide pane the brief is in the rail, so the document does not carry it as well.
  const briefInPage = Boolean(brief) && !wide;
  // The day's parts, in the order they are drawn, so the outline names what is actually on the page rather than a fixed list.
  const sections = [
    ...(briefInPage ? [{ id: "day-brief", text: "The morning brief" }] : []),
    { id: "day-page", text: briefInPage ? "What the day came to" : "The day" },
    ...(page.tasks?.length ? [{ id: "day-raised", text: "Raised that day" }] : []),
  ];
  const { active, goTo } = useReading(sections.map((sec) => sec.id));

  // A day the daemon wrote nothing around gets no rail at all, so its 280px does not sit empty beside the page and push the column off centre. The header above asks dayRailed the same question, so the picker starts where the day's first word does.
  const outlined = sections.length > 1;
  const rail = dayRailed(page) ? (
    <Rail label="About this day">
      {heading ? <RailBlock title="What the day held">{heading}</RailBlock> : null}
      {brief ? <RailBlock title="The morning brief">{brief}</RailBlock> : null}
      {close && close !== page.page ? <RailBlock title="The evening close">{close}</RailBlock> : null}
      {outlined ? (
        <RailBlock title="On this page">
          <Outline sections={sections} active={active} onPick={goTo} />
        </RailBlock>
      ) : null}
    </Rail>
  ) : undefined;

  return (
    <Reading wide={wide} rail={rail}>
      <article>
        <h2 className="text-title text-foreground">{pageHeading(page.date)}</h2>
        {/* In a wide pane the rail carries the daemon's own line about the day, so it is not printed twice. */}
        {heading && !wide ? <p className="mt-2 text-meta text-muted-foreground">{heading}</p> : null}
        {/* The morning brief is what Ora said at the start of the day; the sentences under it are what it wrote at the end, so the brief goes first and is marked as its own part of the page rather than run together with the evening's account. */}
        {briefInPage ? (
          <div className="document mt-8">
            <h3 id="day-brief">The morning brief</h3>
            <p>{brief}</p>
          </div>
        ) : null}
        <div className="document mt-8">
          {briefInPage ? <h3 id="day-page">What the day came to</h3> : <span id="day-page" />}
          {sentences.length ? (
            sentences.map((s, i) => (
              <p key={i} className={i === 0 ? "text-lead" : undefined}>
                {s}
              </p>
            ))
          ) : (
            <p className="text-muted-foreground">Ora wrote nothing for this day.</p>
          )}
        </div>
        <Raised tasks={page.tasks} />
      </article>
    </Reading>
  );
}

/** The Days screen. Input: none. Output: the header with the day picker in it and the chosen day's page under it. Today is in the list before the nightly loop has written its page, so the window opens on the newest day that actually has one. */
export function DaysScreen() {
  const dispatch = useAppDispatch();
  const { date, query } = useAppSelector((s) => s.ui);
  const { data: days = [], isError } = useDaysQuery();
  const [wide, pane] = useWide();

  const listed = daysShown(activeDays(days), query.days);
  const chosen = date ?? (days.find((d) => d.has_page) ?? days[0])?.date;
  const { data: page } = useDayQuery(chosen ?? "", { skip: !chosen });

  // Each row carries what the day holds: the counts the daemon reported, and for a day the nightly loop has not written yet, that it has no page.
  const options = groupDays(listed).flatMap((g) => g.items.map((d) => ({ id: d.date, label: dayShort(d.date), hint: dayCounts(d) || (d.has_page ? "" : "no page yet"), group: g.label })));

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide} railed={Boolean(page && dayRailed(page))}>
        <h1 className="shrink-0 text-ui font-medium">Days</h1>
        <Picker
          list="days"
          label="Choose a day"
          placeholder="Search days"
          options={options}
          selected={chosen}
          empty={activeDays(days).length ? `Nothing matches “${query.days}”.` : "No days written yet."}
          onPick={(id) => dispatch(ui.dayOpened(id))}
        />
      </PageHeader>
      <Scroller bodyClassName={page ? `${HEAD} ${TAIL}` : "flex"}>
        {page ? (
          <Page page={page} wide={wide} />
        ) : (
          <Blank
            up={!isError}
            empty="No days written yet."
            hint="Ora writes a page for the day each night, from what it saw and heard. The first one appears after the first full day it has been running."
          />
        )}
      </Scroller>
    </div>
  );
}
