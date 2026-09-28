/** The Routines screen: user-authored scheduled instructions Ora checks on its own — "every weekday at 8, tell me the one thing I must do today", "when Vexil replies about the venue, tell me" — added as free text plus a schedule, and a list of what is running with what each last said, a way to run one right now, and a way to drop it. */

import { MoreHorizontal, Play, Trash2 } from "lucide-react";
import { useId, useState } from "react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import {
  useCreateRoutineMutation,
  useDeleteRoutineMutation,
  useRoutinesQuery,
  useRunRoutineMutation,
  type Routine,
} from "./api";
import { shortWhen } from "./format";
import {
  Group,
  HEAD,
  Nothing,
  PageHeader,
  Reading,
  Scroller,
  TAIL,
  useWide,
} from "./parts";
import { ui, useAppDispatch } from "./store";

/** The box a new routine is written in: the instruction and the schedule it runs on, added together — the daemon refuses either one blank, so the button stays off until both are filled in. Input: none. Output: the two fields and the button. */
function NewRoutine() {
  const dispatch = useAppDispatch();
  const [text, setText] = useState("");
  const [schedule, setSchedule] = useState("");
  const [createRoutine, { isLoading }] = useCreateRoutineMutation();
  const textId = useId();
  const scheduleId = useId();
  const hintId = useId();

  const add = async () => {
    const t = text.trim();
    const s = schedule.trim();
    if (!t || !s) return;
    try {
      await createRoutine({ text: t, schedule: s }).unwrap();
      setText("");
      setSchedule("");
    } catch {
      dispatch(ui.noticed({ text: "Could not add that routine", kind: "error" }));
    }
  };

  return (
    <div className="flex flex-col gap-3 rounded-lg border bg-card p-3">
      <div className="flex flex-col gap-1.5">
        <label htmlFor={textId} className="text-meta text-muted-foreground">
          Instruction
        </label>
        <Input
          id={textId}
          value={text}
          placeholder="Tell me the one thing I must do today"
          onChange={(e) => setText(e.target.value)}
        />
      </div>
      <div className="flex flex-col gap-1.5">
        <label htmlFor={scheduleId} className="text-meta text-muted-foreground">
          When
        </label>
        <div className="flex gap-2">
          <Input
            id={scheduleId}
            value={schedule}
            className="flex-1"
            placeholder="weekdays at 8"
            aria-describedby={hintId}
            onChange={(e) => setSchedule(e.target.value)}
            onKeyDown={(e) => {
              if (e.key !== "Enter") return;
              e.preventDefault();
              void add();
            }}
          />
          <Button
            disabled={!text.trim() || !schedule.trim() || isLoading}
            onClick={() => void add()}
          >
            Add
          </Button>
        </div>
        <p id={hintId} className="text-meta text-muted-foreground">
          A clock time, an interval, or something to wait for: “weekdays at 8”, “every 3 hours”, “when Vexil replies about the venue”.
        </p>
      </div>
    </div>
  );
}

/** One routine's row: its instruction and schedule, what it last said or that it has never run, a run-now button, and an overflow menu holding the one destructive action so it is not adjacent to the safe one at the same weight. Input: the routine. Output: the row. */
function RoutineRow({ routine }: { routine: Routine }) {
  const dispatch = useAppDispatch();
  const [runRoutine, { isLoading: running }] = useRunRoutineMutation();
  const [deleteRoutine] = useDeleteRoutineMutation();

  const run = async () => {
    try {
      await runRoutine(routine.id).unwrap();
      // The daemon answers as soon as the run has started, not when it has finished; what the routine says arrives as a notice, the same as on its own schedule.
      dispatch(ui.noticed({ text: "Running…", kind: "info" }));
    } catch {
      dispatch(ui.noticed({ text: "Could not run that routine", kind: "error" }));
    }
  };

  const remove = async () => {
    try {
      await deleteRoutine(routine.id).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not remove that routine", kind: "error" }));
    }
  };

  const last = !routine.last_run
    ? "Never run yet"
    : `Last said: ${routine.last_answer === "NOTHING" ? "nothing worth saying" : routine.last_answer} · ${shortWhen(routine.last_run)}`;

  return (
    <li className="flex flex-col gap-1 px-3.5 py-2.5">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <p className="text-ui">{routine.text}</p>
          <p className="text-meta text-muted-foreground">{routine.schedule}</p>
        </div>
        <div className="flex shrink-0 gap-1">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Run "${routine.text}" now`}
            disabled={running}
            onClick={() => void run()}
          >
            <Play />
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`More for "${routine.text}"`}
              >
                <MoreHorizontal />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem
                variant="destructive"
                onClick={() => void remove()}
              >
                <Trash2 /> Remove
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <p className="text-meta text-muted-foreground">{last}</p>
    </li>
  );
}

/** The Routines screen. Input: none. Output: the box to write a new one and the list of every routine with what it last said. */
export function RoutinesScreen() {
  const { data: routines = [], isError, isLoading } = useRoutinesQuery();
  const [wide, pane] = useWide();

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide}>
        <h1 className="text-ui font-medium">Routines</h1>
      </PageHeader>
      <Scroller bodyClassName={`${HEAD} ${TAIL}`}>
        <Reading wide={wide}>
          <div className="flex flex-col gap-4">
            <NewRoutine />
            {routines.length === 0 ? (
              <Nothing
                up={!isError}
                loading={isLoading}
                empty="No routines yet. Write one above."
              />
            ) : (
              <Group>
                <ul role="list" aria-label="Routines" className="divide-y">
                  {routines.map((r) => (
                    <RoutineRow key={r.id} routine={r} />
                  ))}
                </ul>
              </Group>
            )}
          </div>
        </Reading>
      </Scroller>
    </div>
  );
}
