/** The rail beside a wide thread: a live job's plan and spend while one is running, or otherwise what the reply being read called and what it read. */

import { useJobQuery, type Turn } from "./api";
import { isJobLive } from "../shared/job";
import { costLine } from "./format";
import { Rail, RailBlock } from "./parts";
import type { JobRun } from "./store";
import { Quotes } from "./chat-reply";

/** The empty track. The rail's width is held even when there is nothing to put in it, for the same reason the page keeps its header and composer railed: the thread must not shift when the reply being read has no sources. */
function EmptyRail() {
  return <div aria-hidden />;
}

/** What a computer-use job in flight puts in the rail: the plan it wrote and what it has spent. A live job's plan and spend are not on the event stream at all — the plan is written once, early, on the daemon's own side, and never repeated on it — so this polls the job's own record instead of reading anything already in hand, and only while the job is live, so a finished job costs nothing. Input: the job. Output: the rail, or the empty track until the daemon has a plan or a round to report. */
function JobRail({ job }: { job: JobRun }) {
  // currentData rather than data: RTK Query keeps the previous job's result in data while a new job's own fetch is in flight, which would show the last job's plan and spend under the job that just started.
  const { currentData: jobData } = useJobQuery(job.id ?? "", {
    skip: !job.id,
    pollingInterval: isJobLive(job.state) ? 2000 : 0,
  });
  if (!jobData || (!jobData.plan && jobData.spend.rounds === 0)) return <EmptyRail />;
  return (
    <Rail label="What this job is doing">
      {jobData.plan ? <RailBlock title="Plan">{jobData.plan}</RailBlock> : null}
      {jobData.spend.rounds > 0 ? <RailBlock title="Spent so far">{costLine(jobData.spend)}</RailBlock> : null}
    </Rail>
  );
}

/** What the reply being read puts in the rail: the tools it called and the quotes behind it. Input: the turn. Output: the rail. */
function AnswerRail({ showing }: { showing: Turn }) {
  const tools = (showing.tools ?? []).filter(Boolean);
  const quotes = showing.evidence ?? [];
  return (
    <Rail label="What this answer is built on">
      {tools.length ? <RailBlock title="What it did">{tools.join(", ")}</RailBlock> : null}
      {quotes.length ? (
        <RailBlock title={`What it read (${quotes.length})`}>
          <Quotes evidence={quotes} compact />
        </RailBlock>
      ) : null}
    </Rail>
  );
}

/** Which of the two the rail is showing. A job in flight takes the rail over from the ask it displaced sources for, since it is the newest thing in the thread and the one worth reading about. Input: whether the thread wants a rail at all, the job in flight if there is one, and the reply currently being read. Output: the rail. Only rendered by the thread when the pane is wide, since a narrow pane reserves no track at all. */
export function ThreadRail({ sources, job, showing }: { sources: boolean; job?: JobRun; showing?: Turn }) {
  if (!sources) return <EmptyRail />;
  if (job) return <JobRail job={job} />;
  if (!showing) return <EmptyRail />;
  return <AnswerRail showing={showing} />;
}
