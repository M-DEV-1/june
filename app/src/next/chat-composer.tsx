/** The composer at the foot of a thread, on Chats and on Tasks alike. */

import { useEffect, useRef, useState } from "react";
import { CornerDownLeft, Mic } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  useAnswerJobMutation,
  useAskMutation,
  useStartDictationMutation,
  useStartJobMutation,
  useStopDictationMutation,
} from "./api";
import { isJobLive, jobGoal } from "./format";
import { Reading } from "./parts";
import { progress, ui, useAppDispatch, useAppSelector } from "./store";

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
  const mineJob = useAppSelector((s) => (key ? s.progress.jobs[key] : undefined));
  const dictation = useAppSelector((s) => s.progress.dictation);
  const running =
    useAppSelector((s) => Boolean(s.progress.run)) || Boolean(mineJob && isJobLive(mineJob.state) && !mineJob.question);
  const [ask] = useAskMutation();
  const [startJob] = useStartJobMutation();
  const [answerJob] = useAnswerJobMutation();
  const [startDictation] = useStartDictationMutation();
  const [stopDictation] = useStopDictationMutation();
  const ready = Boolean(conversationId || start || fresh);
  const box = useRef<HTMLTextAreaElement>(null);

  // The box is as tall as what is in it, up to a third of the window, after which it scrolls. WebKitGTK has no field-sizing, so the height is measured rather than declared: reset to nothing first, or a line that was deleted would leave the box tall.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    el.style.height = "0px";
    el.style.height = `${Math.min(el.scrollHeight, Math.round(window.innerHeight / 3))}px`;
  }, [draft]);

  // The id of the recording open on the daemon, undefined while nothing is being dictated. A daemon error on start or stop is said on the composer's own placeholder for a few seconds rather than as a toast, since the box is exactly where the words were meant to land.
  const [dictating, setDictating] = useState<string | undefined>(undefined);
  const [dictateNotice, setDictateNotice] = useState<string | undefined>(undefined);
  const noticeTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(noticeTimer.current), []);
  const flashNotice = (text: string) => {
    clearTimeout(noticeTimer.current);
    setDictateNotice(text);
    noticeTimer.current = setTimeout(() => setDictateNotice(undefined), 4000);
  };

  /** Puts a finished dictation's words into the box. Input: the transcript, which is "" when the daemon heard nothing. Output: nothing; what is already half-typed keeps its place and the words are added after it. */
  const takeWords = (text: string) => {
    if (text && key) dispatch(ui.asked({ conversationId: key, text: draft ? `${draft} ${text}` : text }));
  };

  const toggleDictate = async () => {
    if (dictating) {
      const id = dictating;
      setDictating(undefined);
      try {
        takeWords((await stopDictation(id).unwrap()).text);
      } catch {
        // A daemon that had already closed the recording itself is not a failure and never reaches here: stopDictation answers that 404 with no words rather than an error, since the words are already arriving on the stream.
        flashNotice("Could not finish dictation");
      }
      return;
    }
    try {
      const { id } = await startDictation().unwrap();
      setDictating(id);
    } catch {
      flashNotice("Could not start dictation");
    }
  };

  // The daemon closes a recording itself once it has heard enough silence, or at the two-minute cap, and broadcasts the transcript as a "dictation" event; the stop route then has nothing left to answer with. Whichever of the two arrives first is the one applied: clearing `dictating` here is what keeps the stop reply from adding the same words a second time, since that path only runs while a recording is still open. The hover window matches this, at finishDictation in src/main.ts.
  useEffect(() => {
    if (!dictation || dictation.id !== dictating) return;
    setDictating(undefined);
    takeWords(dictation.text);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dictation, dictating]);

  // Escape stops a dictation from anywhere, not only from inside the box, because the box does not have to be focused for one to be running. Read off a ref rather than closed over: the listener is only rebuilt when `dictating` changes, so a conversation switched to in between would otherwise have Escape write the words under the conversation that was open when the dictation started.
  const latestToggle = useRef(toggleDictate);
  latestToggle.current = toggleDictate;
  useEffect(() => {
    if (!dictating) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        void latestToggle.current();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [dictating]);

  const send = async () => {
    const question = draft.trim();
    if (!question || !key || !ready) return;

    // A stuck job's one question is answered by whatever the composer sends next, not asked as a fresh question of the daemon's own model.
    if (mineJob?.id && isJobLive(mineJob.state) && mineJob.question) {
      dispatch(ui.asked({ conversationId: key, text: "" }));
      try {
        await answerJob({ id: mineJob.id, text: question }).unwrap();
      } catch {
        // What was typed exists nowhere else once the box has been cleared, so a write that did not go through gives it back rather than making the user write it again.
        dispatch(ui.asked({ conversationId: key, text: question }));
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
        dispatch(progress.jobFailed(key));
        dispatch(ui.asked({ conversationId: key, text: question }));
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
      dispatch(ui.asked({ conversationId: key, text: question }));
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
        <div className="flex items-end gap-1 rounded-2xl border border-hairline-strong bg-card p-1.5 shadow-lg transition-shadow focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/40">
          {dictating ? (
            <button
              type="button"
              aria-label="Stop dictation"
              onClick={() => void toggleDictate()}
              className="flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2 text-ui text-destructive"
            >
              <span aria-hidden className="size-1.5 shrink-0 rounded-full bg-destructive" />
              Listening…
            </button>
          ) : (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  className="rounded-lg text-muted-foreground hover:text-foreground"
                  aria-label="Dictate"
                  disabled={!ready}
                  onClick={() => void toggleDictate()}
                >
                  <Mic />
                </Button>
              </TooltipTrigger>
              <TooltipContent side="top">Space dictates, on an empty box</TooltipContent>
            </Tooltip>
          )}
          <Textarea
            ref={box}
            rows={1}
            value={draft}
            disabled={!ready}
            // Read-only rather than disabled while a dictation is open: a disabled box drops the caret and leaves the accessibility tree altogether, so a screen reader loses the field the words are about to land in.
            readOnly={Boolean(dictating)}
            aria-busy={Boolean(dictating)}
            placeholder={
              dictateNotice ?? (ready ? placeholder : "Open a chat first, or start a new one")
            }
            aria-label="Ask Ora"
            className="min-h-0 resize-none border-0 bg-transparent px-2 py-1.5 text-lead font-medium shadow-none focus-visible:border-0 focus-visible:ring-0 disabled:bg-transparent dark:bg-transparent dark:disabled:bg-transparent"
            onChange={(e) =>
              key &&
              dispatch(ui.asked({ conversationId: key, text: e.target.value }))
            }
            onKeyDown={(e) => {
              // Enter sends and Shift+Enter starts a line, which is the way round every chat window has settled on.
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                // Nothing is sent while a dictation is open: the words still to come are part of the question. Nothing is sent under a question already running either — the same condition the Send button is disabled on, since a second run replaces the first and the first's own finish would then take the second off the screen.
                if (!dictating && !running) void send();
                return;
              }
              // Space on an empty box is the same as clicking the mic, the same key the hover window has always used to start one; a modifier or a held-down key means something else, same as there.
              if (e.key === " " && ready && !draft && !e.repeat && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
                e.preventDefault();
                void toggleDictate();
                return;
              }
              // Escape gives up the draft — straight away when it is one line, because there is barely anything to lose, and behind a confirm once it runs past one, so a longer draft is not thrown away by a stray keypress. Handled here rather than left to fall through to the window's own Escape, which knows nothing about what is half-typed in this box.
              //
              // While a dictation is open it belongs to the dictation instead: the box is read-only rather than disabled now, so it takes the keypress and would stop it here, on its way to the document listener above that is the only thing that ends a recording — wiping the very text the transcript was about to be added to and leaving the microphone open.
              if (e.key !== "Escape" || dictating || !key || !draft) return;
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
                variant={draft.trim() ? "default" : "ghost"}
                className={draft.trim() ? "rounded-lg" : "rounded-lg text-muted-foreground"}
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
