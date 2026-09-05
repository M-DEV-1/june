/** The rail beside a wide thread: a live job's plan and spend while one is running, or otherwise what the reply being read called and what it read. */

import { useJobQuery, type Turn } from "./api";
import { costLine, isJobLive } from "./format";
import { Rail, RailBlock } from "./parts";
import type { JobRun } from "./store";
import { Quotes } from "./chat-reply";

/** The rail's track stays even when there is nothing to put in it, for the same reason the page keeps its header and composer railed: the thread must not shift when the reply being read has no sources. A job in flight takes the rail over from the ask it displaced sources for, since it is the newest thing in the thread and the one worth reading about. Input: whether the thread wants a rail at all, the job in flight if there is one, and the reply currently being read. Output: the rail. Only rendered by the thread when the pane is wide, since a narrow pane reserves no track at all. A live job's plan and spend are not on the event stream at all — the plan is written once, early, on the daemon's own side, and never repeated on it — so this polls the job's own record instead of reading anything already in hand, and only while the job is live and the rail is actually showing, so a finished job costs nothing. */
export function ThreadRail({
  sources,
  job,
  showing,
}: {
  sources: boolean;
  job?: JobRun;
  showing?: Turn;
}) {
  const jobLive = Boolean(job && isJobLive(job.state));
  const { data: jobData } = useJobQuery(job?.id ?? "", {
    skip: !sources || !job?.id,
    pollingInterval: jobLive ? 2000 : 0,
  });

  if (job && sources) {
    if (!jobData || (!jobData.plan && jobData.spend.rounds === 0)) return <div aria-hidden />;
    return (
      <Rail label="What this job is doing">
        {jobData.plan ? <RailBlock title="Plan">{jobData.plan}</RailBlock> : null}
        {jobData.spend.rounds > 0 ? <RailBlock title="Spent so far">{costLine(jobData.spend)}</RailBlock> : null}
      </Rail>
    );
  }

  if (!(sources && showing)) return <div aria-hidden />;

  return (
    <Rail label="What this answer is built on">
      {(showing.tools ?? []).filter(Boolean).length ? (
        <RailBlock title="What it did">
          {(showing.tools ?? []).filter(Boolean).join(", ")}
        </RailBlock>
      ) : null}
      {(showing.evidence ?? []).length ? (
        <RailBlock title={`What it read (${(showing.evidence ?? []).length})`}>
          <Quotes evidence={showing.evidence ?? []} compact />
        </RailBlock>
      ) : null}
    </Rail>
  );
}
