/** The tick on a task, wherever a task is drawn: the Tasks list, the Days page's raised list, and the block of what a meeting left behind. Input: the task, as little of it as the tick needs so a day's raised item can use it too without carrying a whole Task. Output: the tick — an empty ring until it is done and a filled one after — which writes the new status straight away and says on one line when the daemon refuses it. */

import { Check } from "lucide-react";

import { useSetTaskStatusMutation, type Task } from "./api";
import { ui, useAppDispatch } from "./store";

export function TaskTick({ task }: { task: Pick<Task, "id" | "title" | "done"> }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={task.done}
      aria-label={task.done ? `Reopen ${task.title}` : `Mark ${task.title} done`}
      className="grid size-6 shrink-0 place-items-center rounded-sm outline-none hover:bg-hover focus-visible:ring-2 focus-visible:ring-ring"
      onClick={async (e) => {
        e.stopPropagation();
        try {
          await setStatus({ id: task.id, status: task.done ? "open" : "done" }).unwrap();
        } catch {
          dispatch(ui.noticed("Could not change that task"));
        }
      }}
    >
      <span
        className={`grid size-[15px] place-items-center rounded-full border transition-colors ${task.done ? "border-primary bg-primary text-primary-foreground" : "border-hairline-strong text-transparent"}`}
      >
        <Check className="size-2.5" strokeWidth={3} />
      </span>
    </button>
  );
}
