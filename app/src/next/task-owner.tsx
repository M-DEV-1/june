/** The control beside a noticed row that lets the user say whose task it really is — a meeting's guess is only ever a guess, "Mine" included, and this is how he corrects it. */

import { ArrowRightLeft } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { useSetTaskOwnerMutation, type Task, type TaskOwner } from "./api";
import { ui, useAppDispatch } from "./store";

/** ownerLabels names the three classes the way the menu shows them, in the order Mine, Theirs, Unclear. */
const ownerLabels: Record<TaskOwner, string> = { me: "Mine", them: "Theirs", unclear: "Unclear" };

/** Input: the task. Output: a menu of Mine / Theirs / Unclear; picking one writes it through PATCH /tasks/{id} and says on one line when the daemon refuses it. */
export function OwnerControl({ task }: { task: Task }) {
  const dispatch = useAppDispatch();
  const [setOwner] = useSetTaskOwnerMutation();

  const set = async (owner: TaskOwner) => {
    // Picking the class the minutes already guessed still writes it: task.owner is the derived guess, not a stored choice, and only a stored choice survives a later change to the user's identity.
    try {
      await setOwner({ id: task.id, owner }).unwrap();
    } catch {
      dispatch(ui.noticed("Could not change who owns that task"));
    }
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon-xs"
          aria-label={`${task.title}: ${ownerLabels[task.owner]} — change who owns it`}
          className="shrink-0 text-muted-foreground opacity-50 group-hover:opacity-100 focus-visible:opacity-100 aria-expanded:opacity-100"
          onClick={(e) => e.stopPropagation()}
        >
          <ArrowRightLeft />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()}>
        {(Object.keys(ownerLabels) as TaskOwner[]).map((owner) => (
          <DropdownMenuItem key={owner} onClick={() => void set(owner)}>
            {ownerLabels[owner]}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
