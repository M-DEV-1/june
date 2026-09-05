/** The tick on a task, wherever a task is drawn: the Tasks list, the Days page's raised list, and the block of what a meeting left behind. Input: the task, as little of it as the tick needs so a day's raised item can use it too without carrying a whole Task. Output: the tick — an empty ring until it is done and a filled one after.
 *
 * The circle fills the instant it is clicked, but nothing is told to the daemon until UNDO_MS later: a second click inside that window puts the circle back and the daemon is never called at all, which is what makes a misclick free. Only once the window passes uncontested does the real status change go out — at which point the row it sits in, reading task.done rather than anything kept here, strikes the title through and the daemon's own reply is what a failed change is undone against.
 */

import { useEffect, useRef, useState } from "react";
import { Check } from "lucide-react";

import { useSetTaskStatusMutation, type Task } from "./api";
import { ui, useAppDispatch } from "./store";

/** How long a tick waits before it actually tells the daemon. A second click inside this window is an undo, not a second call. */
const UNDO_MS = 400;

export function TaskTick({ task }: { task: Pick<Task, "id" | "title" | "done"> }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  // What the circle is showing right now, ahead of task.done itself: undefined once nothing is pending, in which case the circle just reads task.done like any other row.
  const [pending, setPending] = useState<boolean | undefined>(undefined);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const done = pending ?? task.done;

  const click = () => {
    if (pending !== undefined) {
      // Inside the undo window: put the circle back and drop the pending change, nothing was ever sent.
      clearTimeout(timer.current);
      setPending(undefined);
      return;
    }
    const next = !task.done;
    setPending(next);
    timer.current = setTimeout(async () => {
      setPending(undefined);
      try {
        await setStatus({ id: task.id, status: next ? "done" : "open" }).unwrap();
      } catch {
        dispatch(ui.noticed("Could not change that task"));
      }
    }, UNDO_MS);
  };

  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={done}
      aria-label={done ? `Reopen ${task.title}` : `Mark ${task.title} done`}
      className="grid size-6 shrink-0 place-items-center rounded-sm outline-none hover:bg-hover focus-visible:ring-2 focus-visible:ring-ring"
      onClick={(e) => {
        e.stopPropagation();
        click();
      }}
    >
      <span
        className={`grid size-[15px] place-items-center rounded-full border transition-colors ${done ? "border-primary bg-primary text-primary-foreground" : "border-hairline-strong text-transparent"}`}
      >
        <Check className="size-2.5" strokeWidth={3} />
      </span>
    </button>
  );
}
