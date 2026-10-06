/** The composer at the foot of a thread, on Chats and on Tasks alike. */

import { useEffect, useRef, useState } from "react";
import { useStore } from "react-redux";
import { CornerDownLeft, Loader2, Mic } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  errorMessage,
  errorWord,
  useAnswerJobMutation,
  useAskMutation,
  useComponentsQuery,
  useStartDictationMutation,
  useStartJobMutation,
  useStopDictationMutation,
} from "./api";
import { isJobLive, jobGoal } from "../shared/job";
import { Reading, useOpenSettingsAt } from "./parts";
import { progress, ui, useAppDispatch, useAppSelector, type JobRun, type RootState } from "./store";

/** Everything that happens when the box is sent, which is three different things wearing one button: answering the question a stuck job is waiting on, starting a "do:" job, and asking an ordinary question. Input: the conversation the draft belongs to, the conversation it would be sent to, the brain and context to send alongside it, the way to open a conversation for a task that has none, whether this composer may let the daemon open one of its own, the draft itself, and the job in flight for this conversation. Output: the send, and whether there is anywhere to send to at all. */
function useSend({
  key,
  conversationId,
  brain,
  context,
  start,
  fresh,
  draft,
  job,
}: {
  key?: string;
  conversationId?: string;
  brain?: string;
  context?: string;
  start?: () => Promise<string | undefined>;
  fresh: boolean;
  draft: string;
  job?: JobRun;
}) {
  const dispatch = useAppDispatch();
  const [ask] = useAskMutation();
  const [startJob] = useStartJobMutation();
  const [answerJob] = useAnswerJobMutation();
  const ready = Boolean(conversationId || start || fresh);
  const send = async () => {
    const question = draft.trim();
    if (!question || !key || !ready) return;

    // A stuck job's one question is answered by whatever the composer sends next, not asked as a fresh question of the daemon's own model.
    if (job?.id && isJobLive(job.state) && job.question) {
      dispatch(ui.asked({ conversationId: key, text: "" }));
      try {
        await answerJob({ id: job.id, text: question }).unwrap();
      } catch {
        // What was typed exists nowhere else once the box has been cleared, so a write that did not go through gives it back rather than making the user write it again.
        dispatch(ui.asked({ conversationId: key, text: question }));
        dispatch(ui.noticed({ text: "Could not send that answer", kind: "error" }));
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
        dispatch(ui.noticed({ text: "Could not start that job", kind: "error" }));
      }
      return;
    }

    // A task June noticed owns no conversation until it is asked about; opening one is the same POST /conversations the rail's New chat used to make. A fresh chat draft owns none either, and names none at all: the daemon opens one of its own on an ask that names no conversation, which is what turns this composer's first message into the conversation itself.
    let id: string | undefined;
    if (conversationId) id = conversationId;
    else if (start) id = await start();
    else if (fresh) id = "";
    if (id === undefined) {
      dispatch(ui.noticed({ text: "Could not open a chat for this", kind: "error" }));
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
      dispatch(ui.noticed({ text: "Could not send that question", kind: "error" }));
    }
  };
  return { send, ready };
}

/** What the box says beside the microphone, and whether it offers the way to set voice typing up. */
type DictateNotice = { text: string; setUp?: boolean };

/** Where voice typing stands, off GET /components: "missing" when it was never set up, "failed" when its last set-up or update did not finish, "downloading" while one is on its way, and undefined when it is ready or the daemon does not say — a daemon that lists no such feature finds whisper some other way, so it is left to try. Only "missing" is sure to mean there is no whisper: "failed" and "downloading" also cover an update of a voice typing that still works (and on the daemon a failed job outranks installed), so those two only explain a failure once the daemon has tried. */
type VoiceTyping = "missing" | "failed" | "downloading" | undefined;

function useVoiceTyping(): VoiceTyping {
  const { data } = useComponentsQuery();
  const f = data?.features.find((x) => x.id === "transcribe");
  if (!f) return undefined;
  if (f.state === "queued" || f.state === "installing") return "downloading";
  if (f.state === "not_installed") return "missing";
  if (f.state === "failed") return "failed";
  return undefined;
}

/** The line for voice typing that was never set up, with the way to set it up. */
const NOT_SET_UP: DictateNotice = { text: "Voice typing isn't set up.", setUp: true };

/** What a failed start, stop or transcription means, in words. Input: what it rejected with (undefined for a failure the event stream reported), where voice typing stands, and the line for a failure nothing explains. Output: the notice. The daemon's not_set_up refusal and its JSON sentence come first, since the daemon has just tried; its plain-text failures are whisper's own error, a path and a file name, so they are not said, and where voice typing stands explains the failure instead. */
function dictationProblem(e: unknown, voiceTyping: VoiceTyping, fallback: string): DictateNotice {
  if (errorWord(e) === "not_set_up" || voiceTyping === "missing") return NOT_SET_UP;
  const said = errorMessage(e);
  if (said) return { text: said };
  if (voiceTyping === "failed") return { text: "Voice typing's set-up didn't finish.", setUp: true };
  if (voiceTyping === "downloading") return { text: "Voice typing is still downloading" };
  return { text: fallback };
}

/** How long the box says it is transcribing a recording the daemon had already closed on its own, after Stop found it gone. Its words come only on the event stream, and on most of the paths that lead here never: a decode that fails after the silence gate ended the recording broadcasts nothing, and neither does a recording another client's start threw away. A decode that works takes seconds, so a minute is many of those over; past it the box and the face go back to idle, and the words are still taken if they come later (dictationOverdue), because the decode can sit behind a whole meeting's on the GPU first and no fixed wait bounds that. */
const GONE_DICTATION_MS = 60_000;

/** Everything the microphone does for the composer, kept apart from sending: opening a recording on the daemon, closing it, and putting what was heard into the box. Input: the key the words go under. Output: what the recording is doing now — "listening", "transcribing" once Stop was pressed and whisper is still on it, or undefined with nothing open — the line to say beside the box when one could not be started or finished or heard nothing, the toggle the mic button and Space both call, and the way to stop waiting on words that are taking too long. The recording itself lives in the store (progress.dictating), so the face says June is listening while it is open. */
function useDictation(key: string | undefined) {
  const dispatch = useAppDispatch();
  const store = useStore<RootState>();
  const dictating = useAppSelector((s) => s.progress.dictating);
  const heard = useAppSelector((s) => s.progress.dictation);
  const [startDictation] = useStartDictationMutation();
  const [stopDictation] = useStopDictationMutation();
  const voiceTyping = useVoiceTyping();
  const failed = useAppSelector((s) => s.progress.dictationFailed);
  // A daemon error on start or stop, and a recording that heard nothing, are said beside the box for a few seconds rather than as a toast, since the box is exactly where the words were meant to land. Beside it rather than on its placeholder, which the half-typed question the words were meant to join hides. One that offers to set voice typing up stays longer, so there is time to reach the button.
  const [dictateNotice, setDictateNotice] = useState<DictateNotice | undefined>(undefined);
  const noticeTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(noticeTimer.current), []);
  const flashNotice = (notice: DictateNotice | string) => {
    const n = typeof notice === "string" ? { text: notice } : notice;
    clearTimeout(noticeTimer.current);
    setDictateNotice(n);
    noticeTimer.current = setTimeout(() => setDictateNotice(undefined), n.setUp ? 10_000 : 4000);
  };

  // A recording the daemon closed on its own and then could not transcribe is said here, off the store, since no stop reply of this window's will ever carry it.
  useEffect(() => {
    if (!failed) return;
    dispatch(progress.dictationFailureSaid());
    flashNotice(dictationProblem(undefined, voiceTyping, "Could not finish dictation"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [failed]);

  // The key and the draft are read when the words land, not when Stop was pressed: whisper takes seconds, and whatever was typed or opened in between is what the words belong with. Closed over, the draft as it stood at the click was written back with the words added, wiping anything typed since.
  const latestKey = useRef(key);
  useEffect(() => {
    latestKey.current = key;
  });

  /** Puts a finished dictation's words into the box. Input: the transcript, which is "" when the daemon heard nothing. Output: nothing; what is already half-typed keeps its place and the words are added after it. */
  const takeWords = (text: string) => {
    const at = latestKey.current;
    if (!text) {
      flashNotice("Heard nothing");
      return;
    }
    if (!at) return;
    const draft = store.getState().ui.ask[at] ?? "";
    dispatch(ui.asked({ conversationId: at, text: draft ? `${draft} ${text}` : text }));
  };

  const toggleDictate = async () => {
    const open = store.getState().progress.dictating;
    if (open?.transcribing) return;
    if (open) {
      dispatch(progress.dictationStopping());
      try {
        const reply = await stopDictation(open.id).unwrap();
        // The recording had already ended on its own and may still be being transcribed: its words come on the stream if at all, and the box says it is transcribing until they do or GONE_DICTATION_MS passes, rather than saying straight away that it heard nothing.
        if (reply.gone) {
          setTimeout(() => {
            if (store.getState().progress.dictating?.id !== open.id) return;
            dispatch(progress.dictationOverdue());
            flashNotice("Dictation is taking too long");
          }, GONE_DICTATION_MS);
          return;
        }
        // The same words also come on the stream; the store takes whichever lands first and lets the recording go, so the other finds it gone and the words are typed once.
        dispatch(progress.dictationHeard({ id: open.id, text: reply.text }));
      } catch (e) {
        if (store.getState().progress.dictating?.id === open.id) dispatch(progress.dictationEnded());
        flashNotice(dictationProblem(e, voiceTyping, "Could not finish dictation"));
      }
      return;
    }
    // Opening the microphone without voice typing set up let a person talk for a minute and then lose every word, so the box says so before anything is recorded. Only when it was never set up: a failed or downloading update can leave a whisper that works, which only the daemon can tell.
    if (voiceTyping === "missing") {
      flashNotice(NOT_SET_UP);
      return;
    }
    try {
      const { id } = await startDictation().unwrap();
      setDictateNotice(undefined);
      dispatch(progress.dictationStarted(id));
    } catch (e) {
      flashNotice(dictationProblem(e, voiceTyping, "Could not start dictation"));
    }
  };

  /** Gives up on words still being transcribed, from the Transcribing button or Escape, so a recording whose words never come — the daemon restarted, another client's start threw it away, or its decode failed with nobody told — never holds the mic button and Enter for longer than the user wants. Its words are dropped if they come after all: giving up was asked for. */
  const stopWaiting = () => {
    if (store.getState().progress.dictating?.transcribing) dispatch(progress.dictationEnded());
  };

  // Whichever of the stop reply and the daemon's own "dictation" event brought the words, they land here, off the store: the daemon closes a recording itself once it has heard enough silence, or at the two-minute cap, and then the event is the only one to bring them. They are taken once, read off the store rather than the render, so StrictMode's second run of this effect finds them already taken. The hover window matches this, at finishDictation in src/main.ts.
  useEffect(() => {
    const words = store.getState().progress.dictation;
    if (!words) return;
    dispatch(progress.dictationTaken());
    takeWords(words.text);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [heard]);

  const open = Boolean(dictating);
  // Escape stops a dictation from anywhere, not only from inside the box, because the box does not have to be focused for one to be running; once Stop has been pressed it stops waiting on the words instead. Read off a ref rather than closed over: the listener is only rebuilt when a recording opens or closes, so a conversation switched to in between would otherwise have Escape write the words under the conversation that was open when the dictation started.
  const latestEscape = useRef(() => {});
  // Written in an effect rather than during render: a render React throws away must not leave its version of the handler behind, and every keypress happens after a commit, so the ref is never read before this has run.
  useEffect(() => {
    latestEscape.current = () => {
      if (store.getState().progress.dictating?.transcribing) stopWaiting();
      else void toggleDictate();
    };
  });
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        latestEscape.current();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);

  const phase: "listening" | "transcribing" | undefined = dictating ? (dictating.transcribing ? "transcribing" : "listening") : undefined;
  return { dictating: phase, notice: dictateNotice, toggle: toggleDictate, stopWaiting };
}

/** The composer at the foot of a thread, on Chats and on Tasks alike: a card that floats over the page rather than a bar ruled off from it, and one that follows the reading column above it rather than the width of the window. It is a text area, not a single line, so Enter sends and Shift+Enter starts a new line, and it grows with what is typed up to a third of the window before it scrolls on its own.
 *
 * Input: the conversation the question belongs to, which is undefined when nothing is open; the key what is half-typed is kept under, which is the conversation when there is one and the task when there is not yet; the brain that conversation names; the placeholder, and the one to show instead while there is nowhere to send to; the passage to send alongside the question, which is how the Tasks page says which task is being talked about; and the function that opens a conversation on the first question about something that has none, which the Tasks page hands over and Chats does not. Output: the composer. What is typed lives in the store per key, so switching away and back does not lose it.
 */
export function Composer({
  conversationId,
  draftKey,
  brain,
  placeholder = "Ask June, or give it something to do",
  idle = "Open a chat first, or start a new one",
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
  /** What the box says while there is nowhere to send to: on Tasks that is no task picked, which "Open a chat first" did not describe. */
  idle?: string;
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
  const openSettingsAt = useOpenSettingsAt();
  const draft = useAppSelector((s) => (key ? (s.ui.ask[key] ?? "") : ""));
  const mineJob = useAppSelector((s) => (key ? s.progress.jobs[key] : undefined));
  const running =
    useAppSelector((s) => Boolean(s.progress.run)) || Boolean(mineJob && isJobLive(mineJob.state) && !mineJob.question);
  const { send, ready } = useSend({ key, conversationId, brain, context, start, fresh, draft, job: mineJob });
  const box = useRef<HTMLTextAreaElement>(null);
  const { dictating, notice: dictateNotice, toggle: toggleDictate, stopWaiting } = useDictation(key);

  // The box is as tall as what is in it, up to a third of the window, after which it scrolls. WebKitGTK has no field-sizing, so the height is measured rather than declared: reset to nothing first, or a line that was deleted would leave the box tall.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    el.style.height = "0px";
    el.style.height = `${Math.min(el.scrollHeight, Math.round(window.innerHeight / 3))}px`;
  }, [draft]);

  return (
    <div className="shrink-0 pb-6">
      {/* The spacer stands in for the rail so the composer sits under the words above it rather than under the middle of the pane. */}
      <Reading
        wide={wide}
        rail={wide && railed ? <div aria-hidden /> : undefined}
      >
        <div className="flex items-end gap-1 rounded-2xl border border-hairline-strong bg-card p-1.5 shadow-lg transition-shadow focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/40">
          {dictating === "transcribing" ? (
            // Stop has been pressed and whisper is still on it. Said here, beside the box, because the box itself may already hold a half-typed question that would hide a placeholder; and a button, because words that never come must not hold the mic and Enter until the wait runs out.
            <Tooltip>
              <TooltipTrigger asChild>
                <button
                  type="button"
                  aria-label="Stop waiting for the words"
                  onClick={stopWaiting}
                  className="flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2 text-ui text-muted-foreground hover:text-foreground"
                >
                  <Loader2 aria-hidden className="size-3.5 shrink-0 animate-spin" />
                  Transcribing…
                </button>
              </TooltipTrigger>
              <TooltipContent side="top">Escape stops waiting</TooltipContent>
            </Tooltip>
          ) : dictating ? (
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
            <>
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
              {dictateNotice ? (
                <span role="status" className="flex h-8 shrink-0 items-center gap-1.5 px-1 text-ui text-muted-foreground">
                  {dictateNotice.text}
                  {dictateNotice.setUp ? (
                    <button type="button" onClick={() => openSettingsAt("local-features")} className="text-foreground underline underline-offset-2 outline-none hover:text-primary focus-visible:ring-2 focus-visible:ring-ring">
                      Set it up
                    </button>
                  ) : null}
                </span>
              ) : null}
            </>
          )}
          <Textarea
            ref={box}
            rows={1}
            value={draft}
            disabled={!ready}
            // Read-only rather than disabled while the microphone is open: a disabled box drops the caret and leaves the accessibility tree altogether, so a screen reader loses the field the words are about to land in. Editable again once Stop is pressed: the words are added to whatever the box holds when they land, so typing on while whisper runs loses nothing.
            readOnly={dictating === "listening"}
            aria-busy={Boolean(dictating)}
            placeholder={ready ? placeholder : idle}
            aria-label="Ask June"
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
              // While a dictation is open, or its words are still being transcribed, it belongs to the dictation instead: the box is read-only or editable rather than disabled, so it takes the keypress and would stop it here, on its way to the document listener above that is the only thing that ends a recording or stops waiting on one — wiping the very text the transcript was about to be added to and leaving the microphone open.
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
