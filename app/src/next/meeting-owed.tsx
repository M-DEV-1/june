/** What a meeting left the user to do, drawn either as a page section or as a rail block. */

import type { Task } from "./api";
import { TaskTick } from "./tasks";
import { Group, RailBlock, SectionHeading } from "./parts";

/** What the meeting left the user to do, as a card. Input: the tasks this meeting raised, which GET /tasks has already narrowed to the ones that are the user's own and still open, and whether it is being drawn in the rail rather than in the page. Output: the card, with each item tickable in place, or nothing when the meeting raised none for them. Anything the meeting left with somebody else is in the minutes, which is where the model wrote it. */
export function Owed({ tasks, inRail }: { tasks: Task[]; inRail?: boolean }) {
  if (!tasks.length) return null;
  const list = (
    <Group>
      <ul className="divide-y">
        {tasks.map((t) => (
          <li key={t.id} className="flex items-center gap-2.5 px-3 py-2">
            <TaskTick task={t} />
            <span className={`min-w-0 flex-1 text-ui ${t.done ? "text-muted-foreground line-through" : ""}`}>{t.title}</span>
          </li>
        ))}
      </ul>
    </Group>
  );
  if (inRail) return <RailBlock title="What you owe from this">{list}</RailBlock>;
  return (
    <div className="mt-6">
      <SectionHeading>What you owe from this</SectionHeading>
      {list}
    </div>
  );
}
