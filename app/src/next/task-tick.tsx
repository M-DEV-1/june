/** The tick on a task, wherever a task is drawn: the Tasks list, the Days page's raised list, and the block of what a meeting left behind. Input: the task, as little of it as the tick needs so a day's raised item can use it too without carrying a whole Task. Output: the tick — an empty ring until it is done and a filled one after.
 *
 * The circle fills the instant it is clicked, but nothing is told to the daemon until UNDO_MS later: a second click inside that window puts the circle back and the daemon is never called at all, which is what makes a misclick free. Only once the window passes uncontested does the real status change go out, and the circle holds what it is showing until the row it sits in says the same thing — not until the daemon answers, which on the Days page is a read of the day earlier than the row changes, or until HOLD_MS has passed for a row that never comes to agree. A refused change is the one case the row never catches up on, so the circle is put back there by hand. A click while a request is in flight does nothing, and the tick says so rather than swallowing it: the change has already gone out, so there is nothing left to undo. A click after that request has been answered, while the circle is still waiting for the row, is a fresh tick asking for the opposite — the change it would undo has already happened.
 */

import { useEffect, useRef, useState } from "react";
import { Check } from "lucide-react";

import { useSetTaskStatusMutation, type Task } from "./api";
import { ui, useAppDispatch } from "./store";

/** How long a tick waits before it actually tells the daemon. A second click inside this window is an undo, not a second call. */
const UNDO_MS = 400;

/** How long the circle holds a change the daemon has already taken before it gives up waiting for the row to say the same thing. A day read from a table the change did not reach, or a refetch that errored, would otherwise leave the circle showing something the row never comes round to. */
const HOLD_MS = 4000;

export function TaskTick({ task }: { task: Pick<Task, "id" | "title" | "done"> }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  // What the circle is showing right now, ahead of task.done itself: undefined once nothing is pending, in which case the circle just reads task.done like any other row.
  const [pending, setPending] = useState<boolean | undefined>(undefined);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  // True from the moment the change goes out until the daemon has answered it. A click in that time is ignored: the change is already gone, so there is nothing left to undo, and treating it as a fresh tick would send the opposite status straight after the first one. It is state rather than a ref so the button can redraw as disabled while it holds — a control that swallows a click without saying anything reads as broken.
  const [sending, setSending] = useState(false);
  // Whether the pending change has already gone to the daemon. A click before that is an undo, since nothing was sent; a click after it is a fresh tick asking for the opposite, because the change it would undo has already happened.
  const [sent, setSent] = useState(false);
  useEffect(() => () => clearTimeout(timer.current), []);
  // The pending circle is dropped only once the row underneath is showing the same thing, not when the daemon answers. On the Tasks screen setTaskStatus patches the tasks caches as the request goes out, so the two agree at once and this fires on the next render. On the Days page the row comes from day.tasks, which changes only when GET /days/{date} is read again, and clearing any earlier empties the circle for the whole of that read.
  useEffect(() => {
    if (pending !== undefined && task.done === pending) {
      setPending(undefined);
      setSent(false);
    }
  }, [task.done, pending]);
  // A row that never comes to agree would hold the circle for ever, so the wait is bounded and the circle then goes back to reading the row like any other.
  useEffect(() => {
    if (!sent) return;
    const id = setTimeout(() => {
      setPending(undefined);
      setSent(false);
    }, HOLD_MS);
    return () => clearTimeout(id);
  }, [sent]);

  const done = pending ?? task.done;

  const click = () => {
    if (sending) {
      return;
    }
    if (pending !== undefined && !sent) {
      // Inside the undo window: put the circle back and drop the pending change, nothing was ever sent.
      clearTimeout(timer.current);
      setPending(undefined);
      return;
    }
    const next = !done;
    setPending(next);
    setSent(false);
    timer.current = setTimeout(async () => {
      setSending(true);
      setSent(true);
      try {
        await setStatus({ id: task.id, status: next ? "done" : "open" }).unwrap();
      } catch {
        // A refused change never reaches the row, so nothing else will ever clear the circle: put it back here.
        dispatch(ui.noticed("Could not change that task"));
        setPending(undefined);
        setSent(false);
      } finally {
        setSending(false);
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
