/** The Routines screen: user-authored scheduled instructions Ora checks on its own — "every weekday at 8, tell me the one thing I must do today", "when Priya replies about the venue, tell me" — added as free text plus a schedule, and a list of what is running with what each last said, a way to run one right now, and a way to drop it. */

import { Play, Trash2 } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
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

  const add = async () => {
    const t = text.trim();
    const s = schedule.trim();
    if (!t || !s) return;
    try {
      await createRoutine({ text: t, schedule: s }).unwrap();
      setText("");
      setSchedule("");
    } catch {
      dispatch(ui.noticed("Could not add that routine"));
    }
  };

  return (
    <div className="flex flex-col gap-2 rounded-lg border bg-card p-3">
      <Input
        value={text}
        aria-label="Instruction"
        placeholder="What should Ora tell you? “Tell me the one thing I must do today”"
        onChange={(e) => setText(e.target.value)}
      />
      <div className="flex gap-2">
        <Input
          value={schedule}
          aria-label="Schedule"
          className="flex-1"
          placeholder="When? “weekdays at 8”, “every 3 hours”, “when Priya replies about the venue”"
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
    </div>
  );
}

/** One routine's row: its instruction and schedule, what it last said or that it has never run, and the run-now and delete actions. Input: the routine. Output: the row. */
function RoutineRow({ routine }: { routine: Routine }) {
  const dispatch = useAppDispatch();
  const [runRoutine, { isLoading: running }] = useRunRoutineMutation();
  const [deleteRoutine] = useDeleteRoutineMutation();

  const run = async () => {
    try {
      const { answer } = await runRoutine(routine.id).unwrap();
      dispatch(
        ui.noticed(
          answer === "NOTHING" ? "Nothing worth saying right now" : answer,
        ),
      );
    } catch {
      dispatch(ui.noticed("Could not run that routine"));
    }
  };

  const remove = async () => {
    try {
      await deleteRoutine(routine.id).unwrap();
    } catch {
      dispatch(ui.noticed("Could not remove that routine"));
    }
  };

  const last = !routine.last_run
    ? "Never run yet"
    : `Last said: ${routine.last_answer === "NOTHING" ? "nothing worth saying" : routine.last_answer} · ${shortWhen(routine.last_run)}`;

  return (
    <li className="flex flex-col gap-1 px-3 py-2.5">
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
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Remove "${routine.text}"`}
            onClick={() => void remove()}
          >
            <Trash2 />
          </Button>
        </div>
      </div>
      <p className="text-meta text-muted-foreground">{last}</p>
    </li>
  );
}

/** The Routines screen. Input: none. Output: the box to write a new one and the list of every routine with what it last said. */
export function RoutinesScreen() {
  const { data: routines = [], isError } = useRoutinesQuery();
  const [wide, pane] = useWide();

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide}>
        <h1 className="text-ui font-medium">Routines</h1>
      </PageHeader>
      <Scroller bodyClassName={`pt-4 ${TAIL}`}>
        <Reading wide={wide}>
          <div className="flex flex-col gap-4">
            <NewRoutine />
            {routines.length === 0 ? (
              <Nothing
                up={!isError}
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
