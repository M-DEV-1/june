/** The Meetings screen: one recording's minutes at a time, with what it left the user to do beside them. The recording is chosen from the picker in the header, which carries a search, the time and who was there; there is no list of recordings beside the sidebar, because the sidebar is for chats. The minutes are stored as markdown, so they are read into headings, real lists and paragraphs and typeset as a document — not drawn as a column of grey labels, and not with their "##" and "**" showing.
 *
 * In a pane wide enough for it (parts.tsx's WIDE), who was there, when it ran, what it left the user to do and an outline of the minutes move into a rail beside the document, and the document itself goes up one step to 72 characters at 16px. Below that width the page is exactly what it was: one column with the same blocks stacked inside it.
 */

import { useMeetingsQuery, useTasksQuery } from "./api";
import { dayHeading, groupMeetings, hhmm, meetingLength, meetingTasks, meetingWho, meetingsShown, minutesLines } from "./format";
import { Blank, HEAD, Outline, PageHeader, Picker, Rail, RailBlock, Reading, Scroller, TAIL, useReading, useWide } from "./parts";
import { ui, useAppDispatch, useAppSelector } from "./store";
import { Owed } from "./meeting-owed";
import { Minutes, minutesBlocks, sectionId } from "./meeting-minutes";

/** The Meetings screen. Input: none. Output: the header with the recording picker in it, and under it the chosen recording's minutes, with what was around the meeting either beside them or above them depending on how much room the pane has. */
export function MeetingsScreen() {
  const dispatch = useAppDispatch();
  const { meetingId, query } = useAppSelector((s) => s.ui);
  const { data: meetings = [], isError } = useMeetingsQuery();
  const { data: tasks = [] } = useTasksQuery();
  const [wide, pane] = useWide();

  const now = new Date();
  const shown = meetingsShown(meetings, query.meetings);
  const selected = meetings.find((m) => m.id === meetingId) ?? shown[0];

  const blocks = minutesBlocks(minutesLines(selected?.minutes ?? "", selected?.title ?? ""));
  const sections = blocks.filter((b) => b.kind === "h").map((b, i) => ({ id: sectionId(i), text: (b as { text: string }).text }));
  const { active, goTo } = useReading(sections.map((sec) => sec.id));

  const owed = selected ? meetingTasks(tasks, selected) : [];
  const when = selected ? [dayHeading(selected.when, now), hhmm(selected.when)].filter(Boolean).join(" · ") : "";
  const ran = selected ? meetingLength(selected.duration_s) : "";
  const who = selected ? meetingWho(selected.attendees) : "";

  const options = groupMeetings(shown, now).flatMap((g) =>
    g.items.map((m) => ({ id: m.id, label: m.title, hint: [hhmm(m.when), meetingLength(m.duration_s)].filter(Boolean).join(" · "), group: g.label })),
  );

  const rail = selected ? (
    <Rail label="About this meeting">
      <RailBlock title="When">
        <div>{when}</div>
        {ran ? <div className="mt-0.5">{ran}</div> : null}
      </RailBlock>
      {who ? (
        <RailBlock title="Who was there">
          <ul className="flex flex-col gap-1">
            {selected.attendees.map((a, i) => (
              <li key={`${a.name}-${i}`} className="truncate" title={a.name}>
                {a.heard_only ? `${a.name} (heard)` : a.name}
              </li>
            ))}
          </ul>
        </RailBlock>
      ) : null}
      <Owed tasks={owed} inRail />
      {sections.length > 1 ? (
        <RailBlock title="In these minutes">
          <Outline sections={sections} active={active} onPick={goTo} />
        </RailBlock>
      ) : null}
    </Rail>
  ) : undefined;

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide} railed={Boolean(rail)}>
        <h1 className="shrink-0 text-ui font-medium">Meetings</h1>
        <Picker
          list="meetings"
          label="Choose a meeting"
          placeholder="Search meetings"
          options={options}
          selected={selected?.id}
          empty={meetings.length ? `Nothing matches “${query.meetings}”.` : "No meetings recorded yet."}
          onPick={(id) => dispatch(ui.meetingOpened(id))}
        />
      </PageHeader>
      <Scroller bodyClassName={selected ? `${HEAD} ${TAIL}` : "flex"}>
        {selected ? (
          <Reading wide={wide} rail={rail}>
            <article>
              <h2 className="text-title text-foreground">{selected.title}</h2>
              {/* In a wide pane the rail already says when it ran and who was on it, so the line under the title is not printed twice. */}
              {wide ? null : <p className="mt-2 text-meta text-muted-foreground">{[when, ran, who].filter(Boolean).join(" · ")}</p>}
              {wide ? null : <Owed tasks={owed} />}
              <div className="mt-8">
                <Minutes blocks={blocks} />
              </div>
            </article>
          </Reading>
        ) : (
          <Blank up={!isError} empty="No meetings recorded yet." hint="Ora writes minutes for a call once it has recorded one. Turn recording on in the config file and the next call lands here." />
        )}
      </Scroller>
    </div>
  );
}
