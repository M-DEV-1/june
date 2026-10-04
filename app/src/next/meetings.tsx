/** The Meetings screen: one recording's minutes at a time, with what it left the user to do beside them. The recording is chosen from the picker in the header, which carries a search, the time and who was there; there is no list of recordings beside the sidebar, because the sidebar is for chats. The minutes are stored as markdown, so they are read into headings, real lists and paragraphs and typeset as a document — not drawn as a column of grey labels, and not with their "##" and "**" showing.
 *
 * In a pane wide enough for it (parts.tsx's WIDE), who was there, when it ran, what it left the user to do and an outline of the minutes move into a rail beside the document, and the document itself goes up one step to 72 characters at 16px. Below that width the page is exactly what it was: one column with the same blocks stacked inside it.
 */

import { useState } from "react";
import { Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./alert-dialog";
import { useDeleteMeetingMutation, useMeetingsQuery, useSettingsQuery, useTasksQuery, type Meeting, type Task } from "./api";
import { dayHeading, groupMeetings, hhmm, keyed, meetingLength, meetingTasks, meetingWho, meetingsShown, minutesBlocks, minutesLines, type MinutesBlock } from "./format";
import { Blank, HEAD, Outline, PageHeader, Picker, Rail, RailBlock, Reading, Scroller, TAIL, useReading, useWide } from "./parts";
import { ui, useAppDispatch, useAppSelector } from "./store";
import { Owed } from "./meeting-owed";
import { Minutes } from "./meeting-minutes";

/** The question asked before a recording's write-up is removed, the same as deleting a chat: minutes are written once from audio that may since have been cleared, and there is no undo. Input: the recording, the one to open in its place, and whether the question is being asked. Output: the dialog. */
function DeleteMeeting({ meeting, next, open, onOpenChange }: { meeting?: Meeting; next: string; open: boolean; onOpenChange: (open: boolean) => void }) {
  const dispatch = useAppDispatch();
  const [removeMeeting] = useDeleteMeetingMutation();
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete “{meeting?.title}”?</AlertDialogTitle>
          <AlertDialogDescription>This removes the write-up. The recording itself stays on disk, so the minutes can be written again from it.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Keep it</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => {
              if (!meeting) return;
              // The next recording in the list is opened by hand: the one showing has just gone, and leaving the screen pointed at it would show a blank document until the list came back.
              void removeMeeting(meeting.id)
                .unwrap()
                .then(() => dispatch(ui.meetingOpened(next)))
                .catch(() => dispatch(ui.noticed({ text: "Could not delete", kind: "error" })));
            }}
          >
            Delete it
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/** What sits beside a meeting in a wide pane: when it ran, who was on it, what it left the user owing, and an outline of the minutes. Input: the meeting, the three lines about it, the tasks it raised for the user, and the minutes already gathered into blocks. Output: the rail. */
function MeetingRail({ meeting, when, ran, owed, blocks }: { meeting: Meeting; when: string; ran: string; owed: Task[]; blocks: MinutesBlock[] }) {
  const who = meetingWho(meeting.attendees);
  const sections = blocks.flatMap((b) => (b.kind === "h" ? [{ id: b.id, text: b.text }] : []));
  const { active, goTo } = useReading(sections.map((sec) => sec.id));
  return (
    <Rail label="About this meeting">
      <RailBlock title="When">
        <div>{when}</div>
        {ran ? <div className="mt-0.5">{ran}</div> : null}
      </RailBlock>
      {who ? (
        <RailBlock title="Who was there">
          <ul className="flex flex-col gap-1">
            {keyed(meeting.attendees, (a) => a.name).map(({ key, item: a }) => (
              <li key={key} className="truncate" title={a.name}>
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
  );
}

/** One recording as it reads on the page: its title, the minutes typeset as a document, and in a wide pane the rail beside them. Input: the recording, every task the daemon has (the ones it raised are picked out here), whether the pane is wide, and the moment the dates are read against. Output: the document and its rail. */
function MeetingView({ meeting, tasks, wide, now }: { meeting: Meeting; tasks: Task[]; wide: boolean; now: Date }) {
  const blocks = minutesBlocks(minutesLines(meeting.minutes ?? "", meeting.title ?? ""));
  const owed = meetingTasks(tasks, meeting);
  const when = [dayHeading(meeting.when, now), hhmm(meeting.when)].filter(Boolean).join(" · ");
  const ran = meetingLength(meeting.duration_s);
  return (
    <Reading wide={wide} rail={wide ? <MeetingRail meeting={meeting} when={when} ran={ran} owed={owed} blocks={blocks} /> : undefined}>
      <article>
        <h2 className="text-title text-foreground">{meeting.title}</h2>
        {/* In a wide pane the rail already says when it ran and who was on it, so the line under the title is not printed twice. */}
        {wide ? null : <p className="mt-2 text-meta text-muted-foreground">{[when, ran, meetingWho(meeting.attendees)].filter(Boolean).join(" · ")}</p>}
        {wide ? null : <Owed tasks={owed} />}
        <div className="mt-8">
          <Minutes blocks={blocks} />
        </div>
      </article>
    </Reading>
  );
}

/** The Meetings screen. Input: none. Output: the header with the recording picker in it, and under it the chosen recording's minutes, with what was around the meeting either beside them or above them depending on how much room the pane has. */
export function MeetingsScreen() {
  const dispatch = useAppDispatch();
  const { meetingId, query } = useAppSelector((s) => s.ui);
  const { data: meetings = [], isError, isLoading } = useMeetingsQuery();
  const { data: tasks = [] } = useTasksQuery();
  const { data: daemon } = useSettingsQuery();
  const [asking, setAsking] = useState(false);
  const [wide, pane] = useWide();

  const now = new Date();
  const shown = meetingsShown(meetings, query.meetings);
  const selected = meetings.find((m) => m.id === meetingId) ?? shown[0];

  const options = groupMeetings(shown, now).flatMap((g) =>
    g.items.map((m) => ({ id: m.id, label: m.title, hint: [hhmm(m.when), meetingLength(m.duration_s)].filter(Boolean).join(" · "), group: g.label })),
  );

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide} railed={Boolean(selected)}>
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
        {/* Only offered while a recording is open, because it is that recording it deletes. Named for the meeting so it is not a bare bin icon beside a picker holding thirty of them. */}
        {selected ? (
          <Button variant="ghost" size="icon" className="shrink-0 text-muted-foreground hover:text-destructive" aria-label={`Delete ${selected.title}`} onClick={() => setAsking(true)}>
            <Trash2 />
          </Button>
        ) : null}
      </PageHeader>
      <Scroller bodyClassName={selected ? `${HEAD} ${TAIL}` : "flex"}>
        {selected ? (
          <MeetingView meeting={selected} tasks={tasks} wide={wide} now={now} />
        ) : (
          <Blank
            up={!isError}
            loading={isLoading}
            empty="No meetings recorded yet."
            // meetings_enabled is the daemon's own "offers to record, or records" (meetings.offer, on unless turned off, or meetings.auto_record), so the hint says what will actually happen on the next call rather than asking for a setting that is already on. It names the Settings control rather than the config file, which a person should never have to open.
            // meetings_enabled is fixed when the daemon starts, while meetings_offer is the setting as it is now: Off holds at once, and Ask only from the next start, so the setting decides "off" and the pair decides "not yet".
            hint={
              daemon?.meetings_offer === "off" || (daemon?.meetings_offer === undefined && daemon?.meetings_enabled === false)
                ? "June is set not to offer to record calls. Start a recording from the tray, or set Offer to record calls back to Ask in Settings, and the minutes land here."
                : daemon?.meetings_enabled === false
                  ? "June starts offering to record calls the next time it starts. Until then, start a recording from the tray, and the minutes land here."
                  : "When June notices a call it offers to record it, or start one from the tray. The minutes land here once it has written them up."
            }
          />
        )}
      </Scroller>

      <DeleteMeeting meeting={selected} next={meetings.find((m) => m.id !== selected?.id)?.id ?? ""} open={asking} onOpenChange={setAsking} />
    </div>
  );
}
