/** The tick on a task, wherever a task is drawn: the Tasks list, the Days page's raised list, and the block of what a meeting left behind. Input: the task, as little of it as the tick needs so a day's raised item can use it too without carrying a whole Task. Output: the tick — an empty ring until it is done and a filled one after.
 *
 * The circle fills the instant it is clicked, but nothing is told to the daemon until UNDO_MS later: a second click inside that window puts the circle back and the daemon is never called at all, which is what makes a misclick free. Only once the window passes uncontested does the real status change go out, and the circle holds what it is showing until the daemon has answered — at which point the row it sits in, reading task.done rather than anything kept here, strikes the title through and the daemon's own reply is what a failed change is undone against. A click while that request is in flight does nothing, and the tick says so rather than swallowing it: the change has already gone out, so there is nothing left to undo.
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
  // True from the moment the change goes out until the daemon has answered it. A click in that time is ignored: the change is already gone, so there is nothing left to undo, and treating it as a fresh tick would send the opposite status straight after the first one. It is state rather than a ref so the button can redraw as disabled while it holds — a control that swallows a click without saying anything reads as broken.
  const [sending, setSending] = useState(false);
  useEffect(() => () => clearTimeout(timer.current), []);

  const done = pending ?? task.done;

  const click = () => {
    if (sending) {
      return;
    }
    if (pending !== undefined) {
      // Inside the undo window: put the circle back and drop the pending change, nothing was ever sent.
      clearTimeout(timer.current);
      setPending(undefined);
      return;
    }
    const next = !task.done;
    setPending(next);
    timer.current = setTimeout(async () => {
      setSending(true);
      try {
        await setStatus({ id: task.id, status: next ? "done" : "open" }).unwrap();
      } catch {
        dispatch(ui.noticed("Could not change that task"));
      } finally {
        // The circle only goes back to reading task.done once the daemon has answered; dropping the pending state any earlier empties the circle again for the whole round trip.
        setSending(false);
        setPending(undefined);
      }
    }, UNDO_MS);
  };

  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={done}
      aria-disabled={sending}
      disabled={sending}
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
