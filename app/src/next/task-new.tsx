/** The box above the list where a new task is typed. */

import { Plus } from "lucide-react";

import { Input } from "@/components/ui/input";
import { useCreateTaskMutation } from "./api";
import { ui, useAppDispatch, useAppSelector } from "./store";

/** Input: none. Output: the field; Enter adds the task and picks what the daemon named after it, so the composer is already aimed at the thing that was just written down. */
export function NewTask() {
  const dispatch = useAppDispatch();
  const draft = useAppSelector((s) => s.ui.newTask);
  const [createTask] = useCreateTaskMutation();

  const add = async () => {
    const title = draft.trim();
    if (!title) return;
    dispatch(ui.taskTyped(""));
    try {
      const made = await createTask(title).unwrap();
      dispatch(ui.taskOpened(made.id));
    } catch {
      dispatch(ui.noticed({ text: "Could not add that task", kind: "error" }));
    }
  };

  return (
    <div className="group relative shrink-0 border-b border-hairline hover:bg-hover">
      <Plus className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
      <Input
        value={draft}
        placeholder="Give June something to do"
        aria-label="Give June something to do"
        className="h-8 rounded-none border-none bg-transparent pl-8 text-ui text-muted-foreground shadow-none focus-visible:text-foreground focus-visible:ring-0"
        onChange={(e) => dispatch(ui.taskTyped(e.target.value))}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          void add();
        }}
      />
    </div>
  );
}
