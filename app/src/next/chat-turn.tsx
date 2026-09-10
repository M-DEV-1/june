/** The turn list: everything said in one conversation, oldest first, with a heading between one day and the next, the question in flight, and a "do:" job's own turn at the end. */

import { Fragment } from "react";

import { Skeleton } from "@/components/ui/skeleton";
import type { ConversationView, Turn } from "./api";
import { dayHeading, sourcedTurns, turnText } from "./format";
import {
  Blank,
  HEAD,
  MEASURE,
  Reading,
  Scroller,
  TAIL,
  useReading,
} from "./parts";
import type { JobRun, Run } from "./store";
import { ReplyMarkdown } from "./chat-markdown";
import { ReplyMeta } from "./chat-reply";
import { ThreadRail } from "./chat-rail";
import { JobTurn } from "./chat-job";
import { WorkingGrid } from "./working-grid";
import { Face } from "./face";

/** What the question in flight shows while it runs: the tools called so far as a list of steps down a rule, and under them the dot grid — the one shape that means Ora is busy, whether nothing has arrived yet or the answer is still streaming in. Input: the run. Output: the block. A tool step already says what it did, so the run's status words are left off when they only repeat the last one. */
function Working({ run }: { run: Run }) {
  const last = run.steps[run.steps.length - 1];
  const line = run.status && run.status !== last?.detail ? run.status : "";
  return (
    <>
      {run.steps.length ? (
        <ol className="flex flex-col gap-1.5 border-l border-hairline-strong pl-4">
          {run.steps.map((s, i) => (
            <li
              key={`${s.name}-${i}`}
              className="text-meta text-muted-foreground"
            >
              <span className="font-medium text-foreground">{s.name}</span>
              {s.detail ? <span className="ml-2">{s.detail}</span> : null}
            </li>
          ))}
        </ol>
      ) : null}
      {run.answer ? (
        <div>
          <p className="whitespace-pre-wrap">{run.answer}</p>
          <div className="mt-1.5">
            <WorkingGrid />
          </div>
        </div>
      ) : (
        <div className="flex gap-2.5">
          <Face state="thinking" className="text-micro" />
          <div className="flex flex-col gap-1.5">
            {line ? <p className="text-muted-foreground">{line}</p> : null}
            <WorkingGrid />
          </div>
        </div>
      )}
    </>
  );
}

/** What the thread shows while the daemon's answer is still on its way: three bars the shape of a short exchange, so the pane has the weight of a conversation rather than blinking from nothing to everything. Input: none. Output: the bars. */
function ThreadSkeleton() {
  return (
    <Scroller bodyClassName={`${HEAD} ${TAIL}`}>
      <div className={`flex flex-col gap-6 ${MEASURE}`} aria-hidden>
        <Skeleton className="ml-auto h-8 w-56 rounded-xl" />
        <div className="flex flex-col gap-2">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-[92%]" />
          <Skeleton className="h-4 w-[64%]" />
        </div>
      </div>
    </Scroller>
  );
}

/** The one line a screen reader hears about a conversation without being read every token as it streams in: the last thing actually said in it, once it has landed — a finished reply, or that the ask failed. It is silent about the run in flight, which is `run.answer` growing piece by piece, because announcing each piece would drown out everything else on the page; the region only moves once the streamed answer has settled into the conversation's own turns. Input: the turns loaded so far. Output: the sentence to announce, or "" when nothing has been said yet. */
function lastReplyAnnouncement(turns: Turn[]): string {
  const last = [...turns].reverse().find((t) => t.role !== "you");
  if (!last) return "";
  return last.kind === "error" ? "Could not answer that." : turnText(last);
}

/** Everything said in one conversation, oldest first, with a heading between one day and the next and the question in flight at the end. Input: the conversation, whether the daemon answered, whether it has not answered yet, the line to show when it holds nothing, the sentence under that line, the action to offer beside it, and the run in flight when it belongs to this conversation. Output: the scrolling thread, set at the window's one reading measure. */
export function Thread({
  view,
  up,
  loading,
  empty,
  hint,
  action,
  run,
  job,
  wide = false,
  sources = true,
  newest,
}: {
  view?: ConversationView;
  up: boolean;
  loading?: boolean;
  empty: string;
  hint?: string;
  action?: React.ReactNode;
  run?: Run;
  /** A "do:" job asked from this conversation, shown as the turn after it. */
  job?: JobRun;
  wide?: boolean;
  /** Whether a wide pane puts what a reply read in a rail beside the thread. True on Chats, where the thread is the page; false on Tasks, where the thread is the lower half of a split and the page is meant to stay one column. */
  sources?: boolean;
  /** Which conversation this is. Opening a different one shows its newest turn rather than the top of it. */
  newest?: string;
}) {
  const now = new Date();
  const turns = view?.turns ?? [];
  let lastDay = "";

  // In a wide pane what a reply read sits beside the thread rather than folded under it, for whichever reply is being read now. Only replies that read something or called something are worth following, so only those are watched — and the page asks sourcedTurns the same question to tell the composer how wide to be.
  const answered = sourcedTurns(turns);
  const { active } = useReading(
    wide && sources ? answered.map((t) => `turn-${t.id}`) : [],
  );
  const showing =
    answered.find((t) => `turn-${t.id}` === active) ??
    answered[answered.length - 1];

  // Read out loud once a reply has actually landed, and nowhere near the run in flight above — that one is streaming text a word at a time, which is not something a screen reader should be made to read piecemeal.
  const announce = (
    <div
      aria-live="polite"
      aria-atomic="true"
      aria-label="The latest reply"
      className="sr-only"
    >
      {lastReplyAnnouncement(turns)}
    </div>
  );

  if (loading && !turns.length && !run && !job)
    return (
      <>
        {announce}
        <ThreadSkeleton />
      </>
    );

  if (!turns.length && !run && !job) {
    return (
      <>
        {announce}
        <Scroller bodyClassName="flex">
          <Reading wide={wide} rail={wide ? <div aria-hidden /> : undefined}>
            <div className="flex h-full">
              <Blank up={up} empty={empty} hint={hint} action={action} />
            </div>
          </Reading>
        </Scroller>
      </>
    );
  }

  // In a narrow pane there is no track to reserve at all; in a wide one ThreadRail decides what fills it.
  const rail = wide ? <ThreadRail sources={sources} job={job} showing={showing} /> : undefined;

  return (
    // A thread starts at the top of the pane under the header and grows downwards, the way every other chat window works. A short one leaves the empty space below it rather than floating in the middle of a tall pane, and a long one simply scrolls, with the newest turn last.
    <>
      {announce}
      <Scroller
        newest={newest}
        bodyClassName={`flex flex-col justify-start gap-6 ${HEAD} ${TAIL}`}
      >
        <Reading wide={wide} rail={rail}>
          <div className="flex flex-col gap-6">
            {turns.map((t) => {
              const heading = dayHeading(t.when, now);
              const first = heading && heading !== lastDay;
              if (first) lastDay = heading;
              return (
                <Fragment key={t.id}>
                  {first ? (
                    <div className="flex items-center gap-3 pt-2 text-meta text-muted-foreground">
                      <span className="h-px flex-1 bg-border" />
                      {heading}
                      <span className="h-px flex-1 bg-border" />
                    </div>
                  ) : null}
                  {t.role === "you" ? (
                    // Your own message is the only surface in the thread; Ora's reply is prose on the page. The surface is a neutral one, because the accent is kept for the focus ring and the one action.
                    <div className="max-w-[85%] self-end rounded-xl bg-secondary px-3.5 py-2.5 whitespace-pre-wrap">
                      {t.text}
                    </div>
                  ) : (
                    <div
                      id={`turn-${t.id}`}
                      className={
                        t.kind === "error"
                          ? "border-l-2 border-destructive/40 pl-4"
                          : ""
                      }
                    >
                      <ReplyMarkdown text={turnText(t)} />
                      <ReplyMeta turn={t} folded={!wide || !sources} />
                    </div>
                  )}
                </Fragment>
              );
            })}

            {run ? (
              <>
                {/* The daemon writes the question into the conversation as soon as the ask starts, so anything that reads the conversation again while the answer is still streaming brings that question back as a turn — and the thread then drew it twice, once as a stored turn and once as the run's own bubble. The run's bubble is the one that gives way. */}
                {turns[turns.length - 1]?.text.trim() === run.question.trim() &&
                turns[turns.length - 1]?.role === "you" ? null : (
                  <div className="max-w-[85%] self-end rounded-xl bg-secondary px-3.5 py-2.5 whitespace-pre-wrap">
                    {run.question}
                  </div>
                )}
                <Working run={run} />
              </>
            ) : null}

            {job ? (
              <>
                <div className="max-w-[85%] self-end rounded-xl bg-secondary px-3.5 py-2.5 whitespace-pre-wrap">
                  {job.goal}
                </div>
                <JobTurn job={job} />
              </>
            ) : null}
          </div>
        </Reading>
      </Scroller>
    </>
  );
}
