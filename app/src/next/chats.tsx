/** The Chats screen: the open conversation's turns, the question in flight while it runs, and the composer where the next one is typed. The thread and the composer are exported because the Tasks screen talks to a task through the same two pieces, with the task itself sent along as context. */

import { Fragment, useEffect, useRef, useState } from "react";
import { Check, ChevronDown, ChevronRight, Circle, CornerDownLeft, Pause, Play, Square, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  useAnswerJobMutation,
  useAskMutation,
  useBrainsQuery,
  useConversationQuery,
  useConversationsQuery,
  useJobQuery,
  useSetJobPauseMutation,
  useSettingsQuery,
  useStartJobMutation,
  useStopJobMutation,
  type ConversationView,
  type Evidence,
  type Turn,
} from "./api";
import { costLine, dayHeading, hhmm, isJobLive, jobGoal, jobStateWord, sourcedTurns, took, turnText } from "./format";
import {
  Blank,
  HEAD,
  MEASURE,
  PageHeader,
  Rail,
  RailBlock,
  Reading,
  Scroller,
  TAIL,
  BrainPicker,
  useReading,
  useWide,
} from "./parts";
import { FirstRunPanel } from "./settings";
import {
  DRAFT_CHAT,
  progress,
  ui,
  useAppDispatch,
  useAppSelector,
  type JobRun,
  type Run,
} from "./store";

/** One of the quotes behind an answer, drawn as the thing that was read rather than as a field: the source and when it was on one grey line, the words themselves on a sunken block under it. Input: the rows the daemon sent. Output: the stack of them. */
function Quotes({
  evidence,
  compact,
}: {
  evidence: Evidence[];
  compact?: boolean;
}) {
  return (
    <div
      className={compact ? "flex flex-col gap-3" : "mt-3 flex flex-col gap-2"}
    >
      {evidence.map((e, i) => (
        <div
          key={`${e.title}-${i}`}
          className={
            compact ? "border-l pl-3" : "rounded-md bg-sunken px-3 py-2"
          }
        >
          <div className="flex justify-between gap-3 text-meta text-muted-foreground">
            <span className="truncate" title={e.title}>
              {e.title}
            </span>
            <time className="shrink-0">{e.meta}</time>
          </div>
          <p
            className={
              compact ? "mt-1 text-meta text-foreground" : "mt-1 text-read"
            }
          >
            {e.body}
          </p>
        </div>
      ))}
    </div>
  );
}

/** The row that folds a reply's sources away and back. Input: how many there are, whether they are showing, what the row is called, and what to do when it is clicked. Output: the row — a chevron, the word, and the count — which is the same control on an answer's quotes and on a failed ask's provider message. */
function Fold({
  label,
  count,
  open,
  onToggle,
}: {
  label: string;
  count?: number;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      aria-expanded={open}
      onClick={onToggle}
      className="-ml-1.5 mt-1 flex items-center gap-1 rounded-sm px-1.5 py-1 text-meta text-muted-foreground outline-none transition-colors hover:bg-hover hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
    >
      {open ? (
        <ChevronDown className="size-3.5" />
      ) : (
        <ChevronRight className="size-3.5" />
      )}
      {label}
      {count === undefined ? null : (
        <span className="rounded-xs bg-muted px-1 tabular-nums">{count}</span>
      )}
    </button>
  );
}

/** What sits under one of Ora's replies: the grey line saying when it was said and which tools it called, and the fold holding what it read. Input: the turn. Output: the line, the fold, and the quotes themselves when the fold is open. A failed ask gets the same shape, except that what the fold holds is the provider's whole message rather than what was read. */
function ReplyMeta({ turn, folded = true }: { turn: Turn; folded?: boolean }) {
  const dispatch = useAppDispatch();
  const open = useAppSelector((s) => s.ui.openRails.includes(turn.id));
  const time = hhmm(turn.when);

  if (turn.kind === "error") {
    const whole =
      (turn.text ?? "").trim() !== (turn.reason ?? "").trim() &&
      (turn.text ?? "").trim().length > 0;
    return (
      <>
        <div className="mt-1.5 text-meta text-muted-foreground">
          {time} · could not answer
        </div>
        {whole && folded ? (
          <Fold
            label="The whole message"
            open={open}
            onToggle={() => dispatch(ui.railToggled(turn.id))}
          />
        ) : null}
        {open && whole && folded ? (
          <pre className="mt-2 max-h-[220px] overflow-auto rounded-md bg-sunken p-3 font-mono text-meta whitespace-pre-wrap">
            {turn.text}
          </pre>
        ) : null}
      </>
    );
  }

  const evidence = turn.evidence ?? [];
  const tools = (turn.tools ?? []).filter(Boolean);
  return (
    <>
      <div className="mt-1.5 text-meta text-muted-foreground">
        {time}
        {tools.length && folded ? ` · ${tools.join(", ")}` : ""}
        {evidence.length ? "" : " · read nothing — treat it that way"}
      </div>
      {evidence.length && folded ? (
        <Fold
          label="Sources"
          count={evidence.length}
          open={open}
          onToggle={() => dispatch(ui.railToggled(turn.id))}
        />
      ) : null}
      {open && evidence.length && folded ? (
        <Quotes evidence={evidence} />
      ) : null}
    </>
  );
}

/** What the question in flight shows while it runs: the tools called so far as a list of steps down a rule, and under them one amber line — the working line while there is nothing to read, and "still writing" once the answer has started arriving. Input: the run. Output: the block. A tool step already says what it did, so the line above the steps is left off rather than repeating the last one, which is what keeps it to one amber line and not two. */
function Working({ run }: { run: Run }) {
  const last = run.steps[run.steps.length - 1];
  const line =
    run.status && run.status !== last?.detail
      ? run.status
      : run.steps.length
        ? ""
        : "working…";
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
          <div className="mt-1.5 text-meta text-work">still writing</div>
        </div>
      ) : line ? (
        <p className="text-work">{line}</p>
      ) : null}
    </>
  );
}

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

/** One row of a job's live step list: a tick, a cross or a plain circle for a step still running, the action it took, the change it was written down to expect, and — once wait_for has checked it — why that check went the way it did. Input: the step. Output: the row. */
function JobStepView({ step }: { step: JobRun["steps"][number] }) {
  const mark =
    step.outcome === "pass" ? (
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
function JobTurn({ job }: { job: JobRun }) {
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

/** Everything said in one conversation, oldest first, with a heading between one day and the next and the question in flight at the end. Input: the conversation, whether the daemon answered, whether it has not answered yet, the line to show when it holds nothing, the sentence under that line, the action to offer beside it, and the run in flight when it belongs to this conversation. Output: the scrolling thread, set at the window's one reading measure. */
/** The one line a screen reader hears about a conversation without being read every token as it streams in: the last thing actually said in it, once it has landed — a finished reply, or that the ask failed. It is silent about the run in flight, which is `run.answer` growing piece by piece, because announcing each piece would drown out everything else on the page; the region only moves once the streamed answer has settled into the conversation's own turns. Input: the turns loaded so far. Output: the sentence to announce, or "" when nothing has been said yet. */
function lastReplyAnnouncement(turns: Turn[]): string {
  const last = [...turns].reverse().find((t) => t.role !== "you");
  if (!last) return "";
  return last.kind === "error" ? "Could not answer that." : turnText(last);
}

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

  // A live job's plan and spend are not on the event stream at all — the plan is written once, early, on the daemon's own side, and never repeated on it — so the rail beside a job turn polls the job's own record instead of reading anything already in hand. Only polled while the job is live and the rail is actually showing, so a narrow pane or a finished job costs nothing. Called unconditionally, ahead of every early return below, so the hooks this component calls are the same on every render.
  const jobLive = Boolean(job && isJobLive(job.state));
  const { data: jobData } = useJobQuery(job?.id ?? "", {
    skip: !wide || !sources || !job?.id,
    pollingInterval: jobLive ? 2000 : 0,
  });

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

  // The rail's track stays even when there is nothing to put in it, for the same reason the page keeps its header and composer railed: the thread must not shift when the reply being read has no sources. A job in flight takes the rail over from the ask it displaced sources for, since it is the newest thing in the thread and the one worth reading about.
  const rail = !wide
    ? undefined
    : job && sources
      ? jobData && (jobData.plan || jobData.spend.rounds > 0) ? (
          <Rail label="What this job is doing">
            {jobData.plan ? <RailBlock title="Plan">{jobData.plan}</RailBlock> : null}
            {jobData.spend.rounds > 0 ? <RailBlock title="Spent so far">{costLine(jobData.spend)}</RailBlock> : null}
          </Rail>
        ) : (
          <div aria-hidden />
        )
      : !(sources && showing)
        ? (
          <div aria-hidden />
        )
        : (
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
                      <p className="whitespace-pre-wrap">{turnText(t)}</p>
                      <ReplyMeta turn={t} folded={!wide || !sources} />
                    </div>
                  )}
                </Fragment>
              );
            })}

            {run ? (
              <>
                <div className="max-w-[85%] self-end rounded-xl bg-secondary px-3.5 py-2.5 whitespace-pre-wrap">
                  {run.question}
                </div>
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

/** The composer at the foot of a thread, on Chats and on Tasks alike: a card that floats over the page rather than a bar ruled off from it, and one that follows the reading column above it rather than the width of the window. It is a text area, not a single line, so Enter sends and Shift+Enter starts a new line, and it grows with what is typed up to a third of the window before it scrolls on its own.
 *
 * Input: the conversation the question belongs to, which is undefined when nothing is open; the key what is half-typed is kept under, which is the conversation when there is one and the task when there is not yet; the brain that conversation names; the placeholder; the passage to send alongside the question, which is how the Tasks page says which task is being talked about; and the function that opens a conversation on the first question about something that has none, which the Tasks page hands over and Chats does not. Output: the composer. What is typed lives in the store per key, so switching away and back does not lose it.
 */
export function Composer({
  conversationId,
  draftKey,
  brain,
  placeholder = "Ask Ora, or give it something to do",
  context,
  start,
  wide = false,
  railed = false,
  fresh = false,
}: {
  conversationId?: string;
  draftKey?: string;
  brain?: string;
  placeholder?: string;
  context?: string;
  start?: () => Promise<string | undefined>;
  wide?: boolean;
  /** Whether the thread above this composer has a rail beside it. When it has, the composer reserves the same width so it sits under the words rather than under the middle of the pane; when it has not, it is the same single centred column the thread is. */
  railed?: boolean;
  /** Whether this composer may open a conversation of its own on the first message it sends, naming none to the daemon and letting it open one — a fresh chat draft, which Chats opens on "New chat" instead of creating the conversation up front. Tasks never sets this: its own `start` is what opens a conversation there, named after the task. */
  fresh?: boolean;
}) {
  const dispatch = useAppDispatch();
  const key = draftKey ?? conversationId;
  const draft = useAppSelector((s) => (key ? (s.ui.ask[key] ?? "") : ""));
  const job = useAppSelector((s) => s.progress.job);
  const mineJob = job && key && job.conversationId === key ? job : undefined;
  const running =
    useAppSelector((s) => Boolean(s.progress.run)) || Boolean(mineJob && isJobLive(mineJob.state) && !mineJob.question);
  const [ask] = useAskMutation();
  const [startJob] = useStartJobMutation();
  const [answerJob] = useAnswerJobMutation();
  const ready = Boolean(conversationId || start || fresh);
  const box = useRef<HTMLTextAreaElement>(null);

  // The box is as tall as what is in it, up to a third of the window, after which it scrolls. WebKitGTK has no field-sizing, so the height is measured rather than declared: reset to nothing first, or a line that was deleted would leave the box tall.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    el.style.height = "0px";
    el.style.height = `${Math.min(el.scrollHeight, Math.round(window.innerHeight / 3))}px`;
  }, [draft]);

  const send = async () => {
    const question = draft.trim();
    if (!question || !key || !ready) return;

    // A stuck job's one question is answered by whatever the composer sends next, not asked as a fresh question of the daemon's own model.
    if (mineJob?.id && isJobLive(mineJob.state) && mineJob.question) {
      dispatch(ui.asked({ conversationId: key, text: "" }));
      try {
        await answerJob({ id: mineJob.id, text: question }).unwrap();
      } catch {
        dispatch(ui.noticed("Could not send that answer"));
      }
      return;
    }

    // "do: …" starts a computer-use job instead of asking a question, from an open chat or from a fresh draft alike — a job opens no conversation of its own either way.
    const goal = jobGoal(question);
    if (goal) {
      dispatch(ui.asked({ conversationId: key, text: "" }));
      dispatch(progress.jobSent({ conversationId: key, goal }));
      try {
        const res = await startJob({ goal, brain }).unwrap();
        dispatch(progress.jobAccepted({ id: res.id, conversationId: key }));
      } catch {
        dispatch(progress.jobFailed());
        dispatch(ui.noticed("Could not start that job"));
      }
      return;
    }

    // A task Ora noticed owns no conversation until it is asked about; opening one is the same POST /conversations the rail's New chat used to make. A fresh chat draft owns none either, and names none at all: the daemon opens one of its own on an ask that names no conversation, which is what turns this composer's first message into the conversation itself.
    let id: string | undefined;
    if (conversationId) id = conversationId;
    else if (start) id = await start();
    else if (fresh) id = "";
    if (id === undefined) {
      dispatch(ui.noticed("Could not open a chat for this"));
      return;
    }
    dispatch(ui.asked({ conversationId: key, text: "" }));
    // The run is tracked under the draft's own key until the daemon says which conversation it actually opened — id is "" for that first message, so there is nothing else to track it under yet.
    dispatch(progress.askSent({ conversationId: id || key, question }));
    try {
      const res = await ask({
        question,
        conversation_id: id,
        brain,
        context,
      }).unwrap();
      dispatch(
        progress.askAccepted({
          askId: res.id,
          conversationId: res.conversation_id,
        }),
      );
      if (fresh && !conversationId && res.conversation_id) dispatch(ui.conversationOpened(res.conversation_id));
    } catch {
      dispatch(progress.askFailed());
      dispatch(ui.noticed("Could not send that question"));
    }
  };

  return (
    <div className="shrink-0 pb-6">
      {/* The spacer stands in for the rail so the composer sits under the words above it rather than under the middle of the pane. */}
      <Reading
        wide={wide}
        rail={wide && railed ? <div aria-hidden /> : undefined}
      >
        <div className="flex items-end gap-1 rounded-xl border bg-card p-1.5 shadow-lg transition-shadow focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/40">
          <Textarea
            ref={box}
            rows={1}
            value={draft}
            disabled={!ready}
            placeholder={
              ready ? placeholder : "Open a chat first, or start a new one"
            }
            aria-label="Ask Ora"
            className="min-h-0 resize-none border-0 bg-transparent px-2 py-1.5 text-read shadow-none focus-visible:border-0 focus-visible:ring-0 disabled:bg-transparent dark:bg-transparent dark:disabled:bg-transparent"
            onChange={(e) =>
              key &&
              dispatch(ui.asked({ conversationId: key, text: e.target.value }))
            }
            onKeyDown={(e) => {
              // Enter sends and Shift+Enter starts a line, which is the way round every chat window has settled on.
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void send();
                return;
              }
              // Escape gives up the draft — straight away when it is one line, because there is barely anything to lose, and behind a confirm once it runs past one, so a longer draft is not thrown away by a stray keypress. Handled here rather than left to fall through to the window's own Escape, which knows nothing about what is half-typed in this box.
              if (e.key !== "Escape" || !key || !draft) return;
              if (draft.includes("\n") && !window.confirm("Clear this draft?"))
                return;
              e.stopPropagation();
              dispatch(ui.asked({ conversationId: key, text: "" }));
            }}
          />
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                size="icon-sm"
                className="rounded-lg"
                aria-label="Send"
                disabled={!ready || !draft.trim() || running}
                onClick={() => void send()}
              >
                <CornerDownLeft />
              </Button>
            </TooltipTrigger>
            <TooltipContent side="top">
              Enter sends it, Shift+Enter starts a line
            </TooltipContent>
          </Tooltip>
        </div>
      </Reading>
    </div>
  );
}

/** The Chats screen. Input: none — the conversation showing and the question in flight both come from the store. Output: the header, the thread and the composer, on one page with nothing beside it. */
export function ChatsScreen() {
  const conversationId = useAppSelector((s) => s.ui.conversationId);
  const chatDraft = useAppSelector((s) => s.ui.chatDraft);
  const run = useAppSelector((s) => s.progress.run);
  const job = useAppSelector((s) => s.progress.job);
  // A chat draft shows as though nothing were picked, even though conversationId still names whatever was open before "New chat" — cleared would only have App.tsx's own "keep some chat picked" effect put it straight back the instant one exists.
  const shownId = chatDraft ? undefined : conversationId;
  const {
    data: convs = [],
    isError,
    isLoading: listLoading,
  } = useConversationsQuery();
  // currentData rather than data: RTK Query keeps the previous arg's result in data while a skipped query stays uninitialized, which would leave a draft showing the conversation it was opened over instead of nothing.
  const { currentData: view, isLoading } = useConversationQuery(shownId ?? "", {
    skip: !shownId,
  });
  const { data: brains = [] } = useBrainsQuery();
  const { data: daemon } = useSettingsQuery();
  const [wide, pane] = useWide();

  // Nothing a person types can be answered until one of the four ways of answering text is set up, so while the daemon still lists steps the pane says so instead of showing an empty thread.
  const setUp = !daemon?.first_run?.steps?.length;
  const current = convs.find((c) => c.id === shownId);
  // In a wide pane the rail's track is always reserved, filled or not, so the header, the thread and the composer sit at the same place in every chat: a column that moved left the moment a reply called a tool, and back when the next chat had none, read as the page jumping about (2026-09-05).
  const railed = wide;
  // A fresh draft has no conversationId yet, tracked under this sentinel until the first message sent from it opens a real one and the composer's own send() moves everything over to that id.
  const key = shownId ?? DRAFT_CHAT;
  const mine = run && run.conversationId === key ? run : undefined;
  const mineJob = job && job.conversationId === key ? job : undefined;

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide} railed={railed}>
        <h1 className="truncate text-ui font-medium" title={current?.title}>
          {current?.title ?? "Ora"}
        </h1>
        <div className="ml-auto">
          <BrainPicker
            current={view?.brain ?? current?.brain ?? ""}
            brains={brains}
          />
        </div>
      </PageHeader>
      {setUp ? (
        <Thread
          view={view}
          up={!isError}
          loading={isLoading || listLoading}
          empty={
            shownId
              ? "Nothing said in this chat yet."
              : "Pick a chat on the left, or start a new one."
          }
          hint="Ask a question below and Ora answers from what it has seen and heard."
          run={mine}
          job={mineJob}
          wide={wide}
          newest={key}
        />
      ) : (
        <Scroller bodyClassName={`${HEAD} ${TAIL}`}>
          <div className={MEASURE}>
            <FirstRunPanel />
          </div>
        </Scroller>
      )}
      <Composer
        conversationId={shownId}
        draftKey={key}
        brain={view?.brain}
        wide={wide}
        railed={railed}
        fresh={!shownId}
      />
    </div>
  );
}
