/** The work a day raised, as it reads on the Days page. */

import type { DayView } from "./api";
import { TaskTick } from "./tasks";
import { Group, SectionHeading } from "./parts";

/** The work a day raised, as it reads on the page: the same tick the Tasks screen draws, wired to the same POST /tasks/{id}/done, so ticking one here and ticking it there are the same action on the same row — the "Task" tag both screens' queries carry means either one refetches the other. Input: the day's tasks. Output: the block, or nothing when the day raised none. */
export function Raised({ tasks }: { tasks: DayView["tasks"] }) {
  if (!tasks?.length) return null;
  return (
    <div id="day-raised" className="mt-10">
      <SectionHeading>Raised that day</SectionHeading>
      <Group>
        <ul className="divide-y">
          {tasks.map((t) => (
            <li key={t.id} className="flex items-center gap-2.5 px-3 py-2 text-ui">
              <TaskTick task={{ id: t.id, title: t.title, done: t.done }} />
              <span className={t.done ? "text-muted-foreground line-through" : ""}>{t.title}</span>
            </li>
          ))}
        </ul>
      </Group>
    </div>
  );
}
