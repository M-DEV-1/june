/** The section below Mine for a task raised by a meeting that is not clearly the user's own — collapsed behind a count by default, since a noticed item is something to watch rather than something already on the list. */

import { useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";

import type { Task } from "./api";
import { Nothing } from "./parts";
import { TaskRow } from "./task-row";

/** Input: the tasks not owned by the user that the current search still matches, how many there are with no search applied at all (which is what decides whether the section exists), who is picked, and the moment for their dates. Output: the disclosure, and the rows once it is opened; nothing at all when there is nothing being watched. */
export function TheirsSection({ tasks, total, selectedId, now }: { tasks: Task[]; total: number; selectedId?: string; now: Date }) {
  const [open, setOpen] = useState(false);
  if (!total) return null;
  return (
    <div className="mt-2">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-1.5 rounded-sm px-2 py-1.5 text-left text-ui text-muted-foreground hover:bg-hover"
      >
        {open ? <ChevronDown className="size-3.5 shrink-0" /> : <ChevronRight className="size-3.5 shrink-0" />}
        <span>Theirs, watching</span>
        <span className="text-meta">({tasks.length})</span>
      </button>
      {open ? (
        tasks.length === 0 ? (
          <Nothing up empty="Nothing watching." />
        ) : (
          <ul role="list" aria-label="Theirs, watching" className="-mx-2 flex flex-col">
            {tasks.map((t) => (
              <TaskRow key={t.id} task={t} selected={t.id === selectedId} now={now} />
            ))}
          </ul>
        )
      ) : null}
    </div>
  );
}
