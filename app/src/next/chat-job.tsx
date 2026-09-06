/** What a "do:" job shows in the thread while it runs and once it ends: the state word, the ticking clock, the Stop/Pause controls, the step list, the stuck question and the closing line with what it cost. */

import { useEffect, useState } from "react";
import { Check, Circle, Pause, Play, Square, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useSetJobPauseMutation, useStopJobMutation } from "./api";
import { costLine, isJobLive, jobStateWord, took } from "./format";
import type { JobRun } from "./store";

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

/** What a "do:" job shows while it runs and once it ends: the state word and, while it is still going, the ticking clock and the Stop/Pause controls beside it; the steps taken so far, each with its own tick or cross; the one question a stuck job is waiting on, whose answer is whatever the composer sends next; and, once it has ended, the closing sentence and what it cost. Input: the job. Output: the block. */
export function JobTurn({ job }: { job: JobRun }) {
  const [stopJob] = useStopJobMutation();
  const [setJobPause] = useSetJobPauseMutation();
  const live = isJobLive(job.state);
  const paused = job.state === "paused";

  return (
    <div>
      <div className="flex items-center gap-2 text-meta text-work">
        <span className="font-medium">{jobStateWord(job.state)}</span>
        <Elapsed startedAt={job.startedAt} live={live} />
        {live && job.id ? (
          <div className="ml-auto flex items-center gap-0.5 text-muted-foreground">
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={paused ? "Resume" : "Pause"}
                  onClick={() => void setJobPause({ id: job.id!, pause: !paused })}
                >
                  {paused ? <Play /> : <Pause />}
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top">{paused ? "Resume" : "Pause"}</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label="Stop" onClick={() => void stopJob(job.id!)}>
                  <Square />
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top">Stop</TooltipContent>
            </Tooltip>
          </div>
        ) : null}
      </div>
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
