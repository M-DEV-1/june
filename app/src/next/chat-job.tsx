/** What a "do:" job shows in the thread while it runs and once it ends: the state word, the ticking clock, the Stop/Pause controls, the plan it opened with, the step list, the stuck question and the closing line with what it cost. */

import { useEffect, useState } from "react";
import { Check, Circle, Pause, Play, Square, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useSetJobPauseMutation, useStopJobMutation } from "./api";
import { isJobLive } from "../shared/job";
import { costLine, jobStateWord, took } from "./format";
import { ui, useAppDispatch, type JobRun } from "./store";

/** The elapsed time on a job's own heading, ticking on its own second by second so the step list under it does not re-render along with it. Input: when the job started, and whether it is still live. Output: "3.2s", "1m 04s" — took() reused from the token ledger. Stops ticking the moment it is handed live=false, which is also the render where the closing text and cost line take this line's place. */
function Elapsed({ startedAt, live }: { startedAt: number; live: boolean }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!live) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [live]);
  if (!live) return null;
  return <span>{took(now - startedAt)}</span>;
}

/** One row of a job's live step list: a tick, a cross, a muted mark for a check that already held, or a plain circle for a step still running, the action it took, the change it was written down to expect, and — once wait_for has checked it — why that check went the way it did. Input: the step. Output: the row. */
function JobStepView({ step }: { step: JobRun["steps"][number] }) {
  const mark = step.heldBefore ? (
    // A check that was already true before the step took it says nothing about what the step did, so it is neither ticked nor crossed; the words are in the mark's own place rather than in an icon, since there is no shape that reads as "this proves nothing".
    <span className="mt-0.5 shrink-0" title="The check already held before this step, so it says nothing about what the step did">
      already held
    </span>
  ) : step.outcome === "pass" ? (
    <Check className="mt-0.5 size-3.5 shrink-0 text-work" />
  ) : step.outcome === "fail" ? (
    <X className="mt-0.5 size-3.5 shrink-0 text-destructive" />
  ) : (
    <Circle className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
  );
  return (
    <li className="flex items-start gap-2 text-meta text-muted-foreground">
      {mark}
      <div>
        <div className="text-foreground">{step.text}</div>
        {step.expect ? <div>expecting {step.expect}</div> : null}
        {step.why ? <div>{step.why}</div> : null}
      </div>
    </li>
  );
}

/** What a job still running offers: pause it or let it carry on, and stop it outright. Input: the daemon's id for the job and whether it is paused now. Output: the two buttons. Only drawn while the job is live and the daemon has answered with an id, since neither call can be made without one. */
function JobControls({ id, paused }: { id: string; paused: boolean }) {
  const dispatch = useAppDispatch();
  const [stopJob] = useStopJobMutation();
  const [setJobPause] = useSetJobPauseMutation();
  // A refused press leaves the job exactly as it was, so the rail says so rather than the button looking like it took.
  const failed = (text: string) => () => dispatch(ui.noticed({ text, kind: "error" }));
  return (
    <div className="ml-auto flex items-center gap-0.5 text-muted-foreground">
      <Tooltip>
        <TooltipTrigger asChild>
          <Button variant="ghost" size="icon-sm" aria-label={paused ? "Resume" : "Pause"} onClick={() => setJobPause({ id, pause: !paused }).unwrap().catch(failed(paused ? "Could not resume that job" : "Could not pause that job"))}>
            {paused ? <Play /> : <Pause />}
          </Button>
        </TooltipTrigger>
        <TooltipContent side="top">{paused ? "Resume" : "Pause"}</TooltipContent>
      </Tooltip>
      <Tooltip>
        <TooltipTrigger asChild>
          <Button variant="ghost" size="icon-sm" aria-label="Stop" onClick={() => stopJob(id).unwrap().catch(failed("Could not stop that job"))}>
            <Square />
          </Button>
        </TooltipTrigger>
        <TooltipContent side="top">Stop</TooltipContent>
      </Tooltip>
    </div>
  );
}

/** What a "do:" job shows while it runs and once it ends: the state word and, while it is still going, the ticking clock and the Stop/Pause controls beside it; the plan the model wrote on its first round, with its own guess at how many steps the job would take; the steps taken so far, each with its own tick or cross; the one question a stuck job is waiting on, whose answer is whatever the composer sends next; and, once it has ended, the closing sentence and what it cost. Input: the job. Output: the block. */
export function JobTurn({ job }: { job: JobRun }) {
  const live = isJobLive(job.state);
  const paused = job.state === "paused";

  return (
    <div>
      <div className="flex items-center gap-2 text-meta text-work">
        <span className="font-medium">{jobStateWord(job.state)}</span>
        <Elapsed startedAt={job.startedAt} live={live} />
        {live && job.id ? <JobControls id={job.id} paused={paused} /> : null}
      </div>
      {job.plan ? (
        <div className="mt-2 text-meta text-muted-foreground">
          <p className="text-foreground">{job.plan}</p>
          {job.estimate ? <p>about {job.estimate} steps</p> : null}
        </div>
      ) : null}
      {job.steps.length ? (
        <ol className="mt-2 flex flex-col gap-1.5 border-l border-hairline-strong pl-4">
          {job.steps.map((s) => (
            <JobStepView key={s.n} step={s} />
          ))}
        </ol>
      ) : null}
      {job.question ? <p className="mt-2 text-work">{job.question}</p> : null}
      {job.say ? (
        <div className="mt-2">
          <p className="whitespace-pre-wrap">{job.say}</p>
          {job.spend ? <p className="mt-1.5 text-meta text-muted-foreground">{costLine(job.spend)}</p> : null}
        </div>
      ) : null}
    </div>
  );
}
