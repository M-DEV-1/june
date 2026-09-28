/** One day's page: its date, the daemon's own line about the day, the sentences of the page itself typeset as a document, and the work it raised. */

import type { DayView } from "./api";
import { dayRailed, keyed, pageHeading } from "./format";
import { Outline, Rail, RailBlock, Reading, useReading } from "./parts";
import { Raised } from "./day-raised";

/** The day's parts, in the order they are drawn, so the outline names what is actually on the page rather than a fixed list. Input: the page, and whether the morning brief is in the document rather than in the rail. Output: one entry per part, each with the id its heading carries. */
function daySections(page: DayView, briefInPage: boolean) {
  return [
    ...(briefInPage ? [{ id: "day-brief", text: "The morning brief" }] : []),
    { id: "day-page", text: briefInPage ? "What the day came to" : "The day" },
    ...(page.tasks?.length ? [{ id: "day-raised", text: "Raised that day" }] : []),
  ];
}

/** What sits beside the day in a wide pane: the daemon's own lines about it and an outline of the page. Input: the page and whether the brief is already in the document. Output: the rail. Only rendered when the day has something to put there, so its 280px does not sit empty beside the page and push the column off centre. */
function DayRail({ page, briefInPage }: { page: DayView; briefInPage: boolean }) {
  const sections = daySections(page, briefInPage);
  const { active, goTo } = useReading(sections.map((sec) => sec.id));
  const heading = (page.heading ?? "").trim();
  const brief = (page.brief ?? "").trim();
  const close = (page.close ?? "").trim();
  return (
    <Rail label="About this day">
      {heading ? <RailBlock title="What the day held">{heading}</RailBlock> : null}
      {brief ? <RailBlock title="The morning brief">{brief}</RailBlock> : null}
      {close && close !== page.page ? <RailBlock title="The evening close">{close}</RailBlock> : null}
      {sections.length > 1 ? (
        <RailBlock title="On this page">
          <Outline sections={sections} active={active} onPick={goTo} />
        </RailBlock>
      ) : null}
    </Rail>
  );
}

/** What the daemon wrote about the day, typeset as a document. Input: the page's own text and whether the morning brief is printed above it. Output: the paragraphs, the first one set as the lead, or one grey line for a day nothing was written about. */
function DayDocument({ text, briefInPage }: { text: string; briefInPage: boolean }) {
  const sentences = text
    .split(/\n+/)
    .map((s) => s.trim())
    .filter(Boolean);
  return (
    <div className="document mt-8">
      {briefInPage ? <h3 id="day-page">What the day came to</h3> : <span id="day-page" />}
      {sentences.length ? (
        keyed(sentences, (s) => s).map(({ key, item }, at) => (
          <p key={key} className={at === 0 ? "text-lead" : undefined}>
            {item}
          </p>
        ))
      ) : (
        <p className="text-muted-foreground">June wrote nothing for this day.</p>
      )}
    </div>
  );
}

/** One day's page: its date, the daemon's own line about the day, the sentences of the page itself typeset as a document, and the work it raised. Input: the page the daemon sent and whether the pane is wide enough for a rail. Output: the column, and beside it in a wide pane the day's own summary lines and an outline of its parts. */
export function Page({ page, wide }: { page: DayView; wide: boolean }) {
  const heading = (page.heading ?? "").trim();
  // In a wide pane the brief is in the rail, so the document does not carry it as well.
  const briefInPage = Boolean((page.brief ?? "").trim()) && !wide;
  // The header above asks dayRailed the same question, so the picker starts where the day's first word does.
  const rail = dayRailed(page) ? <DayRail page={page} briefInPage={briefInPage} /> : undefined;

  return (
    <Reading wide={wide} rail={rail}>
      <article>
        <h2 className="text-title text-foreground">{pageHeading(page.date)}</h2>
        {/* In a wide pane the rail carries the daemon's own line about the day, so it is not printed twice. */}
        {heading && !wide ? <p className="mt-2 text-meta text-muted-foreground">{heading}</p> : null}
        {/* The morning brief is what June said at the start of the day; the sentences under it are what it wrote at the end, so the brief goes first and is marked as its own part of the page rather than run together with the evening's account. */}
        {briefInPage ? (
          <div className="document mt-8">
            <h3 id="day-brief">The morning brief</h3>
            <p>{(page.brief ?? "").trim()}</p>
          </div>
        ) : null}
        <DayDocument text={page.page ?? ""} briefInPage={briefInPage} />
        <Raised tasks={page.tasks} />
      </article>
    </Reading>
  );
}
