/** One task in the list: the tick, the title on one line, where it came from and when it was raised in a column of their own on the right, and the menu holding the other status changes. */

import { Check, MoreHorizontal, RotateCcw, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { useSetTaskStatusMutation, type Task, type TaskStatus } from "./api";
import { shortWhen, taskDetail } from "./format";
import { ui, useAppDispatch } from "./store";
import { TaskTick } from "./task-tick";
import { OwnerControl } from "./task-owner";

/** Input: the task, whether it is the one the composer is aimed at, and the moment the dates are read against. Output: the row; a noticed task also gets the owner control, since a meeting's guess at who a task belongs to — including "Mine" — is exactly what the user needs to be able to correct. */
export function TaskRow({ task, selected, now }: { task: Task; selected: boolean; now: Date }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  const detail = taskDetail(task);
  const meta = [detail, task.when ? shortWhen(task.when, now) : ""].filter(Boolean).join(" · ");
  // Only an action item Ora noticed can be dropped: the daemon answers 400 for a dropped task of the user's own, because user_tasks has nowhere to hold a third state.
  const droppable = task.source === "noticed";

  const set = async (status: TaskStatus) => {
    try {
      await setStatus({ id: task.id, status }).unwrap();
    } catch {
      dispatch(ui.noticed("Could not change that task"));
    }
  };

  return (
    <div
      role="option"
      aria-selected={selected}
      data-row-id={task.id}
      tabIndex={0}
      onClick={() => dispatch(ui.taskOpened(task.id))}
      onKeyDown={(e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        e.preventDefault();
        dispatch(ui.taskOpened(task.id));
      }}
      className={`group flex h-8 items-center gap-2.5 rounded-sm px-2 outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring ${selected ? "bg-selected" : "hover:bg-hover"}`}
    >
      <TaskTick task={task} />
      <div className={`min-w-0 flex-1 truncate text-ui ${selected ? "font-medium" : ""} ${task.done ? "text-muted-foreground line-through" : ""}`} title={task.title}>
        {task.title}
      </div>
      {meta ? (
        <div className="hidden max-w-[26ch] shrink-0 truncate text-meta text-muted-foreground sm:block" title={meta}>
          {meta}
        </div>
      ) : null}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={`More for ${task.title}`}
            className="text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 aria-expanded:opacity-100"
            onClick={(e) => e.stopPropagation()}
          >
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()}>
          {task.done ? (
            <DropdownMenuItem onClick={() => void set("open")}>
              <RotateCcw /> Reopen
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem onClick={() => void set("done")}>
              <Check /> Mark done
            </DropdownMenuItem>
          )}
          {droppable ? (
            <DropdownMenuItem variant="destructive" onClick={() => void set("dropped")}>
              <X /> Drop it
            </DropdownMenuItem>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      {task.source === "noticed" ? <OwnerControl task={task} /> : null}
    </div>
  );
}
