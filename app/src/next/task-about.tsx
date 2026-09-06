/** What is beside a task: when it was raised, the meeting it came out of, and what that meeting was about, so a line like "Continue transition-risk work" can be placed a week later by somebody who has forgotten the call it came from. The words are the meeting's own — the "What the meeting covered" section of its minutes — because a task's title is one bullet and one bullet is not enough to recognise the work by.
 *
 * In a wide pane this is the rail beside the list; below that width the same blocks are drawn as one bordered card under the list. Nothing here is fetched for the task itself: GET /meetings already carries every recording's minutes, and the meeting a task came from is found in that list by the name and date its own detail line carries.
 */

import type { Meeting, Task } from "./api";
import { dayHeading, hhmm, minutesSection, shortWhen } from "./format";
import { Rail, RailBlock } from "./parts";
import { ui, useAppDispatch } from "./store";

/** The heading in a set of minutes that says what the meeting was about, which is the part that places a task raised in it. */
const COVERED = "What the meeting covered";

/** The lines of context themselves. Input: the meeting the task came from. Output: up to COVERED_LINES of what it covered, or nothing when the minutes have no such section. */
function covered(meeting?: Meeting) {
  if (!meeting) return [];
  return minutesSection(meeting.minutes ?? "", COVERED).slice(0, 6);
}

/** Input: the picked task, the meeting it was raised in if there is one, and the moment its dates are read against. Output: the blocks — when, where from, and what that meeting covered. Returns nothing at all when no task is picked, so neither the rail nor the card is drawn over an empty selection. */
export function TaskAbout({ task, meeting, now, inRail }: { task?: Task; meeting?: Meeting; now: Date; inRail?: boolean }) {
  const dispatch = useAppDispatch();
  if (!task) return null;
  const lines = covered(meeting);
  const when = task.when ? `${dayHeading(task.when, now)} · ${hhmm(task.when)}` : "";
  const from = task.source === "you" ? "You set it yourself" : meeting?.title || (task.detail ?? "").trim();

  const blocks = (
    <>
      <RailBlock title="Raised">
        <div>{when || shortWhen(task.when ?? "", now)}</div>
      </RailBlock>
      {from ? (
        <RailBlock title="From">
          {meeting ? (
            <button
              type="button"
              onClick={() => dispatch(ui.meetingOpened(meeting.id))}
              className="text-left underline decoration-hairline-strong underline-offset-4 hover:text-foreground"
            >
              {from}
            </button>
          ) : (
            <div>{from}</div>
          )}
        </RailBlock>
      ) : null}
      {lines.length ? (
        <RailBlock title="What that meeting covered">
          <ul className="flex flex-col gap-2">
            {lines.map((line, i) => (
              <li key={i}>
                {line.lead ? <span className="font-medium text-foreground">{line.lead} — </span> : null}
                {line.text}
              </li>
            ))}
          </ul>
        </RailBlock>
      ) : null}
    </>
  );

  if (inRail) return <Rail label="About this task">{blocks}</Rail>;
  return <div className="mt-6 flex flex-col gap-5 rounded-md border border-hairline p-4">{blocks}</div>;
}
