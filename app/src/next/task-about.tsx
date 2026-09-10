/** The head of a task's detail pane: the task's title, when it was raised, the meeting it came out of, and what that meeting was about, so a line like "Continue transition-risk work" can be placed a week later by somebody who has forgotten the call it came from. The words are the meeting's own — the "What the meeting covered" section of its minutes — because a task's title is one bullet and one bullet is not enough to recognise the work by.
 *
 * Nothing here is fetched for the task itself: GET /meetings already carries every recording's minutes, and the meeting a task came from is found in that list by the name and date its own detail line carries.
 */

import type { Meeting, Task } from "./api";
import { dayHeading, hhmm, minutesSection, shortWhen } from "./format";
import { ui, useAppDispatch } from "./store";

/** The heading in a set of minutes that says what the meeting was about, which is the part that places a task raised in it. */
const COVERED = "What the meeting covered";

/** The lines of context themselves. Input: the meeting the task came from. Output: up to six of what it covered, or nothing when the minutes have no such section. */
function covered(meeting?: Meeting) {
  if (!meeting) return [];
  return minutesSection(meeting.minutes ?? "", COVERED).slice(0, 6);
}

/** Input: the picked task, the meeting it was raised in if there is one, and the moment its dates are read against. Output: the head of the detail pane — the title, one line saying when and where from, and the meeting's own summary folded under a native disclosure. Nothing at all when no task is picked. */
export function TaskAbout({ task, meeting, now }: { task?: Task; meeting?: Meeting; now: Date }) {
  const dispatch = useAppDispatch();
  if (!task) return null;
  const lines = covered(meeting);
  const when = task.when ? `${dayHeading(task.when, now)} · ${hhmm(task.when)}` : shortWhen(task.when ?? "", now);
  const from = task.source === "you" ? "You set it yourself" : meeting?.title || (task.detail ?? "").trim();

  return (
    <header className="flex flex-col gap-2 border-b border-hairline px-8 pt-6 pb-5">
      <h2 className={`text-doc font-medium text-foreground ${task.done ? "line-through text-muted-foreground" : ""}`}>{task.title}</h2>
      <p className="text-meta text-muted-foreground">
        {when}
        {from ? (
          <>
            {" · "}
            {meeting ? (
              <button
                type="button"
                onClick={() => dispatch(ui.meetingOpened(meeting.id))}
                className="underline decoration-hairline-strong underline-offset-4 hover:text-foreground"
              >
                {from}
              </button>
            ) : (
              from
            )}
          </>
        ) : null}
      </p>
      {lines.length ? (
        <details className="text-meta text-muted-foreground">
          <summary className="cursor-pointer select-none hover:text-foreground">What that meeting covered</summary>
          <ul className="mt-2 flex flex-col gap-2">
            {lines.map((line, i) => (
              <li key={i}>
                {line.lead ? <span className="font-medium text-foreground">{line.lead} — </span> : null}
                {line.text}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </header>
  );
}
