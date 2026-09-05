/** One day's page: its date, the daemon's own line about the day, the sentences of the page itself typeset as a document, and the work it raised. */

import type { DayView } from "./api";
import { dayRailed, pageHeading } from "./format";
import { Outline, Rail, RailBlock, Reading, useReading } from "./parts";
import { Raised } from "./day-raised";

/** One day's page: its date, the daemon's own line about the day, the sentences of the page itself typeset as a document, and the work it raised. Input: the page the daemon sent and whether the pane is wide enough for a rail. Output: the column, and beside it in a wide pane the day's own summary lines and an outline of its parts. */
export function Page({ page, wide }: { page: DayView; wide: boolean }) {
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
