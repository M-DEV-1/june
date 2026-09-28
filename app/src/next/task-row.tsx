/** One task in the list: the tick, the title over as many lines as it needs, where it came from and when it was raised in a column of their own on the right, and the menu holding the other status changes. The title wraps rather than being cut off — a task read a day later has to say what it is, and a truncated one says nothing that its own hover tooltip has to finish.
 *
 * The row itself is a plain list item with no role of its own, and picking it is one button inside it. It was an option in a listbox once, which is wrong: an option may hold no interactive descendants, and every row holds two or three — the tick, the More menu, and on a noticed task the owner menu — so a screen reader in listbox mode could reach none of them and read all their labels as part of the row's own name instead.
 */

import { Check, MoreHorizontal, RotateCcw, Trash2, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { useDeleteTaskMutation, useSetTaskStatusMutation, type Task, type TaskStatus } from "./api";
import { shortWhen, taskDetail } from "./format";
import { ui, useAppDispatch } from "./store";
import { TaskTick } from "./task-tick";
import { OwnerControl } from "./task-owner";

/** Input: the task, whether it is the one the composer is aimed at, and the moment the dates are read against. Output: the row; a noticed task also gets the owner control, since a meeting's guess at who a task belongs to — including "Mine" — is exactly what the user needs to be able to correct. */
export function TaskRow({ task, selected, now }: { task: Task; selected: boolean; now: Date }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  const [deleteTask] = useDeleteTaskMutation();
  const detail = taskDetail(task);
  const when = task.when ? shortWhen(task.when, now) : "";
  // Only an action item June noticed can be dropped: the daemon answers 400 for a dropped task of the user's own, because user_tasks has nowhere to hold a third state.
  const droppable = task.source === "noticed";

  const set = async (status: TaskStatus) => {
    try {
      await setStatus({ id: task.id, status }).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not change that task", kind: "error" }));
    }
  };

  const remove = async () => {
    try {
      await deleteTask(task.id).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not delete that task", kind: "error" }));
    }
  };

  return (
    <li className={`group relative flex items-start gap-2.5 rounded-sm px-2 py-2 transition-colors ${selected ? "bg-selected" : "hover:bg-hover"}`}>
      <TaskTick task={task} />
      <button
        type="button"
        data-row-id={task.id}
        aria-current={selected ? "true" : undefined}
        onClick={() => dispatch(ui.taskOpened(task.id))}
        // The button's own ::after covers the whole row, so the padding, the gaps and the column saying when it was raised all pick the task — a row with no detail line is eight pixels tall, and only its title being clickable loses a third of its width. The ring is drawn on that same surface, so what the keyboard highlights and what the pointer hits are one rectangle. The More and owner menus sit above it by being positioned and coming later in the DOM; the tick comes earlier, so it carries a z-index of its own.
        className="min-w-0 flex-1 rounded-sm text-left outline-none after:absolute after:inset-0 after:rounded-sm after:content-[''] focus-visible:after:ring-2 focus-visible:after:ring-ring"
      >
        {/* Spans rather than divs: a button may only hold phrasing content, and some assistive technology flattens a block inside one oddly. */}
        <span className={`block text-ui break-words ${selected ? "font-medium" : ""} ${task.done ? "text-muted-foreground line-through" : ""}`}>
          {task.title}
        </span>
        {detail ? (
          <>
            {/* Two spans run together in the button's own name, which is read out as one sentence; a block would have separated them and an inline element does not. */}
            {" "}
            <span className="block break-words text-meta text-muted-foreground">
              {detail}
            </span>
          </>
        ) : null}
      </button>
      {when ? (
        <div className="hidden shrink-0 whitespace-nowrap pt-0.5 text-right text-meta text-muted-foreground tabular-nums sm:block">{when}</div>
      ) : null}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={`More for ${task.title}`}
            className="relative shrink-0 text-muted-foreground opacity-50 group-hover:opacity-100 focus-visible:opacity-100 aria-expanded:opacity-100"
          >
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
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
          ) : (
            // Only the user's own tasks are deleted: a noticed item is a line of a meeting's minutes, and Drop it above is what that is for. A ticked task used to have nowhere to go — the menu offered Reopen and nothing else — so a finished list only ever grew.
            <DropdownMenuItem variant="destructive" onClick={() => void remove()}>
              <Trash2 /> Delete
            </DropdownMenuItem>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
      {task.source === "noticed" ? <OwnerControl task={task} /> : null}
    </li>
  );
}
