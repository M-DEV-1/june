/** What is running right now, in the sidebar, so a job that takes minutes is visible from whichever screen the user has walked on to.
 *
 * The step list lives inside the chat turn that opened the job, which meant leaving that chat lost sight of a task still moving things on the real desktop. This sits beside the notice card, in the one place that is on every screen, and carries the least that is worth carrying: what the job is, how far through it is against its own estimate, and the way to stop it.
 */

import { Square } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useStopJobMutation } from "./api";
import { isJobLive, stepLine } from "./format";
import { Face } from "./face";
import { ui, useAppDispatch, useAppSelector, type JobRun } from "./store";

/** One running job's row: the face, what it is, where it has got to, and Stop. */
function RunningJob({ job }: { job: JobRun }) {
  const dispatch = useAppDispatch();
  const [stopJob] = useStopJobMutation();
  const where = stepLine(job);
  const lines = (
    <>
      <span className="block truncate text-meta text-foreground">{job.goal}</span>
      {/* A stuck job has asked something, and saying so here matters more than the count: it is why nothing is moving. */}
      <span className="block truncate text-meta text-muted-foreground">{job.question ? "waiting on you" : where}</span>
    </>
  );
  return (
    <div className="flex items-start gap-2">
      <Face state={job.question ? "noticed" : "thinking"} />
      {/* A job started by speaking was asked from no chat, so there is nothing to open and the row is only something to read. */}
      {job.conversationId ? (
        <button
          type="button"
          // The row opens the chat the job was asked from, which is where its whole step list is.
          onClick={() => dispatch(ui.conversationOpened(job.conversationId))}
          className="min-w-0 flex-1 rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {lines}
        </button>
      ) : (
        <div className="min-w-0 flex-1">{lines}</div>
      )}
      {job.id ? (
        <Button variant="ghost" size="icon-xs" aria-label={`Stop ${job.goal}`} className="shrink-0 text-muted-foreground" onClick={() => stopJob(job.id!).unwrap().catch(() => dispatch(ui.noticed({ text: "Could not stop that job", kind: "error" })))}>
          <Square />
        </Button>
      ) : null}
    </div>
  );
}

/** Input: none; it reads the jobs in flight off the store. Output: the strip, or nothing at all when none are running — an empty box saying "nothing is running" is noise on a rail that is mostly chats. */
export function RunningNow() {
  const jobs = useAppSelector((s) => s.progress.jobs);
  const live = Object.values(jobs).filter((j) => isJobLive(j.state));
  if (live.length === 0) return null;
  return (
    <div
      role="status"
      aria-label="Running now"
      className="flex flex-col gap-2 rounded-lg border bg-card px-2.5 py-2 group-data-[collapsible=icon]:hidden"
    >
      {live.map((job) => (
        <RunningJob key={job.id ?? job.conversationId} job={job} />
      ))}
    </div>
  );
}
