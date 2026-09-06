/** One task in the list: the tick, the title on one line, where it came from and when it was raised in a column of their own on the right, and the menu holding the other status changes.
 *
 * The row itself is a plain list item with no role of its own, and picking it is one button inside it. It was an option in a listbox once, which is wrong: an option may hold no interactive descendants, and every row holds two or three — the tick, the More menu, and on a noticed task the owner menu — so a screen reader in listbox mode could reach none of them and read all their labels as part of the row's own name instead.
 */

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
  const when = task.when ? shortWhen(task.when, now) : "";
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
    <li
      className={`group flex items-center gap-2.5 rounded-sm px-2 transition-colors ${detail ? "h-11" : "h-8"} ${selected ? "bg-selected" : "hover:bg-hover"}`}
      // The whole row picks the task, not only the title: the padding, the gaps and the column saying when it was raised are part of the target, and a row with no detail line is eight pixels tall without them. A click that landed on one of the row's own controls is that control's alone — the menus included, whose items are not buttons and reach here through the React tree however far the portal moved them in the document. The keyboard has the button below, which is the row's one tab stop, so this adds no second way in for anything but a pointer.
      onClick={(e) => {
        if (!(e.target as HTMLElement).closest('button,[role="menu"]')) dispatch(ui.taskOpened(task.id));
      }}
    >
      <TaskTick task={task} />
      <button
        type="button"
        data-row-id={task.id}
        aria-current={selected ? "true" : undefined}
        onClick={() => dispatch(ui.taskOpened(task.id))}
        className="min-w-0 flex-1 self-stretch rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {/* Spans rather than divs: a button may only hold phrasing content, and some assistive technology flattens a block inside one oddly. */}
        <span className={`block truncate text-ui ${selected ? "font-medium" : ""} ${task.done ? "text-muted-foreground line-through" : ""}`} title={task.title}>
          {task.title}
        </span>
        {detail ? (
          <>
            {/* Two spans run together in the button's own name, which is read out as one sentence; a block would have separated them and an inline element does not. */}
            {" "}
            <span className="block truncate text-meta text-muted-foreground" title={detail}>
              {detail}
            </span>
          </>
        ) : null}
      </button>
      {when ? (
        <div className="hidden shrink-0 whitespace-nowrap text-right text-meta text-muted-foreground tabular-nums sm:block">{when}</div>
      ) : null}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={`More for ${task.title}`}
            className="text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 aria-expanded:opacity-100"
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
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      {task.source === "noticed" ? <OwnerControl task={task} /> : null}
    </li>
  );
}
