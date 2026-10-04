/** First-run setup: the screens a fresh install opens on instead of Chats — what June is and what leaves the computer, which brain it thinks with, a microphone check, the optional local features, and what to remember — and the screen the window shows while the daemon restarts. App.tsx draws it while GET /setup says done:false.
 *
 * Which screen the person was on is the one thing kept in this window (localStorage), so a restart in the middle of setup — which saving a Gemini key always causes — comes back to the same screen. Everything a screen says is read off the daemon again, so it comes back showing what the restart changed.
 */

import { useEffect, useRef, useState, type ReactNode } from "react";
import { ArrowUpRight, Check, Loader2, Mic } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./alert-dialog";
import {
  errorMessage,
  errorStatus,
  errorWord,
  juneApi,
  useComponentsQuery,
  useFreshBrainsQuery,
  useMicTestMutation,
  useOpenMicSettingsMutation,
  useRemoveGeminiKeyMutation,
  useSaveGeminiKeyMutation,
  useSaveSettingsMutation,
  useSettingsQuery,
  type Brain,
  type MicTest,
  type SetupView,
} from "./api";
import { Face } from "./face";
import { hotkeyKeys } from "./format";
import { FeatureList, restartBlockedLine, useFeaturesUnderWay, useRestartBlocker, type UnderWay } from "./onboarding-features";
import { Group, NO_SHORTCUT, ON_WINDOWS, useHotkeyUnavailable, useOpenLink } from "./parts";
import { DRAFT_CHAT, progress, setupUi, ui, useAppDispatch, useAppSelector, type AppDispatch, type RestartWhy } from "./store";

/** Where a person gets a Gemini key: Google's own page, which makes one in two clicks for any Google account. */
const GEMINI_KEY_URL = "https://aistudio.google.com/apikey";

/** The question the last screen offers to ask first. It needs nothing June has not got the moment setup ends — the screen is right there — so the first answer works on a machine with no memory yet. */
const FIRST_QUESTION = "What's on my screen right now?";

/** The screens, in order. */
type Step = "welcome" | "brain" | "mic" | "features" | "done";
const STEPS: Step[] = ["welcome", "brain", "mic", "features", "done"];

/** The localStorage key the screen showing is kept under. */
const STEP_KEY = "june.setup.step";

/** The logins June can answer questions through, under the names a person knows them by: Codex is how ChatGPT's plan is reached, and nobody outside a terminal calls it Codex. In the order the router tries them while none is picked (automaticRank in internal/ipc/brains.go), so the first one signed in is the one that answers. Grok is not here: the router sends it no questions (internal/agent/router.go), and a "Signed in" row for it read as a brain June could answer with. app is where a lapsed login is signed back into, which for ChatGPT's plan is the Codex app and not chatgpt.com: a browser sign-in gives June nothing. */
const LOGINS: { id: string; name: string; app: string }[] = [
  { id: "codex", name: "ChatGPT", app: "Codex" },
  { id: "antigravity", name: "Antigravity", app: "Antigravity" },
  { id: "claude", name: "Claude", app: "Claude Code" },
];

/** Where Google says what it does with what a key sends it: on the free tier it may use it to improve its products and have people read it. */
const GEMINI_TERMS_URL = "https://ai.google.dev/gemini-api/terms";

/** The screen last shown, or the first one. Storage can be blocked, which is the same as nothing stored. */
function readStep(): Step {
  try {
    const s = localStorage.getItem(STEP_KEY) as Step | null;
    return s && STEPS.includes(s) ? s : "welcome";
  } catch {
    return "welcome";
  }
}

/** Records the screen showing, or with undefined forgets it once setup is over, so a later reinstall starts at the beginning. */
function writeStep(s?: Step): void {
  try {
    if (s) localStorage.setItem(STEP_KEY, s);
    else localStorage.removeItem(STEP_KEY);
  } catch {
    /* storage blocked */
  }
}

/** A login June found: signed in, or there but refused. */
type Login = { id: string; name: string; account: string; expired: boolean; note: string };

/** The logins worth a row. Input: GET /brains' rows. Output: the signed-in ones, and the ones a provider refused (an expired or revoked login), which are worth naming because the fix is one sign-in away. A tool that is simply not installed gets no row: four rows saying "not found" would read to someone who has never heard of them as four things wrong. The daemon's note is only read to tell a refused login from a missing one; what the row says is the window's own, because the note names the provider's internal tool and a terminal command, under a row this window has given the name the person knows. */
function loginsFound(brains: Brain[]): Login[] {
  return LOGINS.flatMap(({ id, name, app }): Login[] => {
    const b = brains.find((x) => x.id === id);
    if (!b) return [];
    if (b.signed_in) return [{ id, name, account: b.account, expired: false, note: "" }];
    if (!/expired|refused|sign in again|log in again/i.test(b.limits_note ?? "")) return [];
    return [{ id, name, account: "", expired: true, note: `Open ${app} and sign in again, then press Check again below.` }];
  });
}

/** What went wrong with a key, as the box under it says it: the sentence, and optionally the reason the daemon or Google gave on a quieter line under it. unchecked is true when Google could not be asked at all, which is when saving it unchecked is worth offering. */
type KeyProblem = { text: string; detail?: string; unchecked?: boolean };

/** The advice for a key that is not one, or one Google says is not valid: almost always a copy that missed part of it. */
const COPY_AGAIN = "Copy it again from Google AI Studio — it starts with AIza — and paste the whole thing.";

/** What a refused Gemini key means. Input: what the save or the removal rejected with. Output: the problem. A 409 is read by its word first: it is both a key set somewhere June cannot override and a restart that would cut something short, and the daemon's own sentence names which, and where. A 422 carries the reason as Google gave it, and Google refuses a well-copied key too — the API not turned on for a key made in the Cloud console, a key restricted to other APIs, a country it does not serve — so the reason is shown, and the copy-it-again advice only where it fits. */
function keyProblem(e: unknown): KeyProblem {
  const status = errorStatus(e);
  const word = errorWord(e);
  if (word === "recording" || word === "processing" || word === "downloading")
    return { text: errorMessage(e) || (word === "downloading" ? restartBlockedLine("downloading", "Changing the key") : "Changing the key restarts June, which would cut short the meeting it is recording or writing up. Try again once it's done.") };
  if (status === 422 || word === "invalid_key") {
    const reason = errorMessage(e);
    if (!reason) return { text: `Google didn't accept that key. ${COPY_AGAIN}` };
    if (/not a gemini api key/i.test(reason)) return { text: reason };
    return { text: "Google didn't accept that key.", detail: /api key not valid|api_key_invalid/i.test(reason) ? `${COPY_AGAIN} (Google said: ${reason.replace(/\.$/, "")}.)` : `Google said: ${reason}` };
  }
  if (status === 502 || word === "unreachable") return { text: "June couldn't reach Google to check the key. Check your internet connection and try again — or save it without checking.", unchecked: true };
  if (status === 409 || word === "env_var_set")
    return {
      text:
        errorMessage(e) ||
        (ON_WINDOWS
          ? "This computer already gives every program a Gemini key of its own, in Windows' environment variables, and that one always wins. Change or remove it there, then restart June."
          : "This computer already gives every program a Gemini key of its own, in your login environment, and that one always wins. Change or remove it there, then restart June."),
    };
  if (status === 404) return { text: "This version of June can't save a key from here yet." };
  if (status === undefined) return { text: "June isn't answering. Try again in a moment." };
  return { text: errorMessage(e) || "The key wasn't saved. Try again in a moment." };
}

/** The Gemini key: how to get one, the box to paste it in, and once one is saved, that it is. Input: whether a key is saved, and whether this is Settings, which tucks the box away behind a button and offers Remove. Output: the block. Saving restarts June, because the key is read once at start; the window shows the restart screen until it is back. */
export function GeminiKey({ saved, manage = false }: { saved: boolean; manage?: boolean }) {
  const dispatch = useAppDispatch();
  const openLink = useOpenLink();
  const [save, { isLoading: checking }] = useSaveGeminiKeyMutation();
  const [removeKey] = useRemoveGeminiKeyMutation();
  const [key, setKey] = useState("");
  const [open, setOpen] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [problem, setProblem] = useState<KeyProblem | undefined>(undefined);
  const showForm = manage || saved ? open : true;
  // Saving or removing the key restarts June, so both wait for what a restart would cut short.
  const blocker = useRestartBlocker();
  const held = restartBlockedLine(blocker, saved ? "Changing the key" : "Saving a key");

  /** What a saved or removed key leads to: a restart, which the daemon always does, or — from a daemon that did not — a fresh read of what changed. */
  const after = (restarting: boolean) => {
    if (restarting) dispatch(setupUi.restartBegan("key"));
    else dispatch(juneApi.util.invalidateTags(["Setup", "Brain", "Settings"]));
  };

  const submit = async (force = false) => {
    const k = key.trim();
    if (!k || checking || held) return;
    setProblem(undefined);
    try {
      const r = await save({ key: k, force }).unwrap();
      setKey("");
      setOpen(false);
      after(Boolean(r?.restarting));
    } catch (e) {
      setProblem(keyProblem(e));
    }
  };

  const remove = async () => {
    setConfirming(false);
    setProblem(undefined);
    try {
      const r = await removeKey().unwrap();
      after(Boolean(r?.restarting));
    } catch (e) {
      const status = errorStatus(e);
      setProblem(status === 409 ? keyProblem(e) : status === undefined ? { text: "June isn't answering. Try again in a moment." } : { text: "The key wasn't removed.", detail: errorMessage(e) || "Try again in a moment." });
    }
  };

  return (
    <div>
      {saved || manage ? (
        <div className="flex items-center justify-between gap-4">
          <div className="min-w-0">
            <div className="flex items-center gap-1.5 text-ui">
              {saved ? <Check aria-hidden className="size-3.5 text-primary" /> : null}
              {saved ? "Gemini key saved" : "No Gemini key"}
            </div>
            <div className="mt-0.5 text-meta text-muted-foreground">{saved ? "June can answer, and you can talk to it out loud." : "Free from Google. Also lets you talk to June out loud."}</div>
          </div>
          <div className="flex shrink-0 gap-1">
            {!open ? (
              <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>
                {saved ? "Use a different key" : "Add a key"}
              </Button>
            ) : null}
            {saved && manage ? (
              <Button variant="ghost" size="sm" disabled={Boolean(held)} title={held || undefined} onClick={() => setConfirming(true)}>
                Remove
              </Button>
            ) : null}
          </div>
        </div>
      ) : null}
      {showForm ? (
        <div className={saved || manage ? "mt-4" : ""}>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <Button variant="outline" size="sm" onClick={() => openLink(GEMINI_KEY_URL)}>
              Get a free key <ArrowUpRight />
            </Button>
            <span className="text-meta text-muted-foreground">Sign in with Google, press Create API key, and copy it.</span>
          </div>
          <form
            className="mt-3 flex gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            {/* A password box, because a key on screen is a key in every screenshot and screen share; pasting is the only way anyone enters one anyway. */}
            <Input
              type="password"
              value={key}
              onChange={(e) => {
                setKey(e.target.value);
                setProblem(undefined);
              }}
              placeholder="Paste your key here"
              aria-label="Gemini key"
              aria-invalid={problem && !problem.unchecked ? true : undefined}
              autoComplete="off"
              spellCheck={false}
              disabled={checking}
              className="h-9 font-mono placeholder:font-sans"
            />
            <Button type="submit" size="lg" disabled={!key.trim() || checking || Boolean(held)}>
              {checking ? <Loader2 className="animate-spin" /> : null}
              {checking ? "Checking…" : "Save"}
            </Button>
            {saved || manage ? (
              <Button
                type="button"
                variant="ghost"
                size="lg"
                disabled={checking}
                onClick={() => {
                  setOpen(false);
                  setKey("");
                  setProblem(undefined);
                }}
              >
                Cancel
              </Button>
            ) : null}
          </form>
          {checking ? <p className="mt-2 text-meta text-muted-foreground">Checking the key with Google…</p> : null}
        </div>
      ) : null}
      {held && (showForm || (saved && manage)) && !problem ? <p className="mt-2 text-meta text-muted-foreground">{held}</p> : null}
      {problem ? (
        <div role="alert" className="mt-2">
          <p className="text-meta text-destructive">{problem.text}</p>
          {problem.detail ? <p className="mt-1 text-meta break-words text-muted-foreground">{problem.detail}</p> : null}
          {problem.unchecked ? (
            <div className="mt-2 flex gap-2">
              <Button variant="outline" size="sm" disabled={Boolean(held)} onClick={() => void submit(false)}>
                Try again
              </Button>
              <Button variant="ghost" size="sm" disabled={Boolean(held)} onClick={() => void submit(true)}>
                Save without checking
              </Button>
            </div>
          ) : null}
        </div>
      ) : null}
      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove the Gemini key?</AlertDialogTitle>
            <AlertDialogDescription>June restarts to forget it. Talking to June out loud stops working until you add a key again.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep it</AlertDialogCancel>
            <AlertDialogAction onClick={() => void remove()}>Remove</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** Every way June can be given a brain on this machine: the logins it found, and a Gemini key. Input: whether a key is saved. Output: the block. Shared by the setup screen and the panels Settings and Chats draw while nothing can answer, so there is one set of instructions, and none of them sends a person to a terminal or a config file. */
export function BrainChoices({ geminiKey }: { geminiKey: boolean }) {
  const { data, isFetching, refetch } = useFreshBrainsQuery();
  const openLink = useOpenLink();
  const logins = loginsFound(data?.brains ?? []);
  return (
    <div className="flex flex-col gap-6">
      {logins.length ? (
        <section>
          <h2 className="mb-2 text-ui font-medium">Found on this PC</h2>
          <Group>
            <div className="divide-y">
              {logins.map((l) => (
                <div key={l.id} className="flex items-start justify-between gap-4 px-4 py-3">
                  <div className="min-w-0">
                    <div className="text-ui">{l.name}</div>
                    {l.expired ? <div className="mt-0.5 text-meta text-muted-foreground">{l.note}</div> : null}
                  </div>
                  {l.expired ? (
                    <span className="shrink-0 text-meta text-destructive">Login expired</span>
                  ) : (
                    <span className="flex shrink-0 items-center gap-1 text-meta text-muted-foreground">
                      <Check aria-hidden className="size-3.5 text-primary" />
                      {l.account ? `Signed in · ${l.account}` : "Signed in"}
                    </span>
                  )}
                </div>
              ))}
            </div>
          </Group>
        </section>
      ) : null}
      <section>
        <h2 className="mb-2 text-ui font-medium">
          Gemini key <span className="font-normal text-muted-foreground">· free</span>
        </h2>
        <Group>
          <div className="px-4 py-3.5">
            <GeminiKey saved={geminiKey} />
          </div>
        </Group>
        {/* Said where the key is offered, not only in the installer's notes: the free tier is the one most people will pick, and Google's terms for it ask that nothing personal be sent — which notes about a screen are. */}
        <p className="mt-2 text-meta text-muted-foreground">
          With a key, Google also gets your voice while you talk to June, screenshots June can't read as text, and June's notes on what you've been doing. On the free tier, Google may use all of it to improve its products, and people at Google may read it. Turning on billing for the key's Google project stops that.{" "}
          <TextLink onClick={() => openLink(GEMINI_TERMS_URL)}>Google's terms</TextLink>
        </p>
      </section>
      {/* Only the command-line logins count — signing in to claude.ai or chatgpt.com in a browser gives June nothing — so with no row to fix, the line names the apps rather than inviting a sign-in June cannot see. */}
      <p className="text-meta text-muted-foreground">
        {logins.length ? "Fixed a login, or signed in to another? " : "Use Claude Code, Codex or Antigravity on this PC? Sign in to it there, then "}
        <TextLink disabled={isFetching} onClick={() => void refetch()}>
          {isFetching ? "Checking…" : logins.length ? "Check again" : "check again"}
        </TextLink>
      </p>
    </div>
  );
}

/** The start-at-sign-in switch, on setup's last screen and in Settings. Input: what GET /setup says now, and the switch's accessible name. Output: the switch, with a line under it when the change did not take. GET /setup is read again after the write rather than trusted, because a daemon that does not know the field answers 200 and changes nothing, and the switch would flip and quietly flip back. */
export function AutostartSwitch({ on, label }: { on: boolean; label: string }) {
  const dispatch = useAppDispatch();
  const [save] = useSaveSettingsMutation();
  const [want, setWant] = useState<boolean | undefined>(undefined);
  const [problem, setProblem] = useState("");
  const flip = async (next: boolean) => {
    setWant(next);
    setProblem("");
    try {
      await save({ autostart: next }).unwrap();
      const now = await dispatch(juneApi.endpoints.setup.initiate(undefined, { subscribe: false, forceRefetch: true })).unwrap();
      if (now.autostart !== next) setProblem("This version of June can't change that from here.");
    } catch {
      setProblem("That didn't change. Try again in a moment.");
    } finally {
      setWant(undefined);
    }
  };
  return (
    <div className="flex flex-col items-end gap-1">
      <Switch checked={want ?? on} aria-label={label} onCheckedChange={(v) => void flip(v)} />
      {problem ? <span className="text-meta text-destructive">{problem}</span> : null}
    </div>
  );
}

/** A link-styled button inside a sentence. */
function TextLink({ onClick, disabled, children }: { onClick: () => void; disabled?: boolean; children: ReactNode }) {
  return (
    <button type="button" disabled={disabled} onClick={onClick} className="text-foreground underline underline-offset-2 outline-none hover:text-primary focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60">
      {children}
    </button>
  );
}

/** One screen's frame: an optional face, the heading, the sentence under it, the body, and the row of buttons at the foot. Input: those. Output: the screen. The heading takes focus when the screen changes, so a screen reader says where the person has got to rather than staying on the button that was pressed. */
function StepFrame({ face, title, lead, children, footer }: { face?: "watching" | "done"; title: string; lead?: ReactNode; children?: ReactNode; footer: ReactNode }) {
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), [title]);
  return (
    <div className="flex flex-col">
      {face ? (
        <div className="mb-6">
          <Face state={face} className="text-title" />
        </div>
      ) : null}
      <h1 ref={heading} tabIndex={-1} className="text-title text-foreground outline-none">
        {title}
      </h1>
      {lead ? <p className="mt-2 text-read text-muted-foreground">{lead}</p> : null}
      {children ? <div className="mt-8">{children}</div> : null}
      <div className="mt-10 flex flex-wrap items-center gap-2">{footer}</div>
    </div>
  );
}

/** The ghost Back button every screen after the first carries on the left of its foot. */
function Back({ onClick }: { onClick: () => void }) {
  return (
    <Button variant="ghost" size="lg" onClick={onClick}>
      Back
    </Button>
  );
}

/** A label on the left and what it means on the right, the way the welcome and last screens lay out what to know, with a switch at the far right when it is a choice rather than a fact. */
function Fact({ label, children, control }: { label: string; children: ReactNode; control?: ReactNode }) {
  return (
    <div className={`grid gap-1 px-4 py-3 sm:gap-4 ${control ? "sm:grid-cols-[11rem_1fr_auto]" : "sm:grid-cols-[11rem_1fr]"}`}>
      <div className="text-ui font-medium">{label}</div>
      <div className="text-ui text-muted-foreground">{children}</div>
      {control ? <div className="sm:justify-self-end">{control}</div> : null}
    </div>
  );
}

function WelcomeStep({ setup, onNext }: { setup: SetupView; onNext: () => void }) {
  // Checking every login can take fifteen seconds, and the next screen has nothing to offer until it is done, so it starts while this one is read. A subscription rather than a prefetch: a prefetch holds nothing, so its answer was dropped a minute after it came and someone who read this screen for longer waited for the whole check again.
  useFreshBrainsQuery();
  return (
    <StepFrame
      face="watching"
      title="Hi, I'm June"
      lead="I keep notes on what happens on this computer — what's on your screen, what's said in your meetings — so you can ask me about any of it later."
      footer={
        <Button size="lg" className="ml-auto" onClick={onNext}>
          Get started
        </Button>
      }
    >
      <Group>
        <div className="divide-y">
          {/* Kept to what packaging/windows/privacy.txt says, which is what the installer shows: the memory upkeep and the overnight run send notes about the screen to the AI without a question being asked, so "only when you ask" would be untrue. */}
          <Fact label="Kept on this PC">
            Your notes, recordings and memory are saved here{setup.data_dir ? <>, in <code className="font-mono text-[0.92em] break-all">{setup.data_dir}</code></> : null}. June's makers get nothing: no analytics, no account.
          </Fact>
          <Fact label="Sent to your AI">
            Your questions, and the notes needed to answer them, go to the AI you pick next — ChatGPT, Antigravity, Claude or Google Gemini. June also sends it notes on what you've been doing, in the background, to keep its memory tidy and write up your meetings. Each service keeps what it gets under its own privacy terms.
          </Fact>
          <Fact label="Not watching yet">June doesn't look at your screen until you finish setting up. It takes about three minutes.</Fact>
        </div>
      </Group>
    </StepFrame>
  );
}

/** The login June will answer with when there is no Gemini key, off a fresh GET /brains. Input: its rows. Output: the login, or undefined when none can answer. The row the daemon marks default is the one the router actually answers on (a brain picked in Settings, or the first signed in of automaticRank); a signed-in login in LOGINS' order stands in only when that row is not one of them, such as a picked brain whose login has since lapsed, which the router passes over. */
function answeringLogin(brains: Brain[]): { id: string; name: string } | undefined {
  const usable = LOGINS.filter(({ id }) => brains.some((b) => b.id === id && b.signed_in));
  const def = brains.find((b) => b.default && b.signed_in);
  return usable.find((l) => l.id === def?.id) ?? usable[0];
}

function BrainStep({ setup, onBack, onNext }: { setup: SetupView; onBack: () => void; onNext: () => void }) {
  const { data, isError } = useFreshBrainsQuery();
  const alt = data ? answeringLogin(data.brains) : undefined;
  // GET /setup's brain_ready is read without asking any provider, so it says yes to a login a provider has since refused; it stands in only when the fresh check could not be had at all.
  const answers = setup.gemini_key || (data ? alt !== undefined : setup.brain_ready);
  let next: ReactNode;
  if (setup.gemini_key) next = <Button size="lg" onClick={onNext}>Continue</Button>;
  // Held back until the logins have been checked, which takes a few seconds: a Continue that then turned into "Use Claude instead" under the pointer read as the button changing its mind. Said on the button itself, because an empty foot read as a screen with no way on.
  else if (!data && !isError)
    next = (
      <Button variant="outline" size="lg" disabled>
        <Loader2 className="animate-spin" /> Checking your AI apps…
      </Button>
    );
  else if (alt) next = <Button variant="outline" size="lg" onClick={onNext}>Use {alt.name} instead</Button>;
  else if (answers) next = <Button size="lg" onClick={onNext}>Continue</Button>;
  else next = <Button variant="ghost" size="lg" onClick={onNext}>Skip for now</Button>;
  return (
    <StepFrame
      title="Give June a brain"
      lead="June needs an AI to think with. The quickest is a key from Google — free to get, and it also lets you talk to June out loud."
      footer={
        <>
          <Back onClick={onBack} />
          <div className="ml-auto">{next}</div>
        </>
      }
    >
      <BrainChoices geminiKey={setup.gemini_key} />
      {!setup.gemini_key && alt ? (
        <p className="mt-6 text-ui text-muted-foreground">You can skip the key and let June answer with {alt.name}. It just won't be able to talk out loud.</p>
      ) : null}
      {!answers && (data || isError) ? (
        <p className="mt-6 text-ui text-muted-foreground">Without one, June can't answer questions yet. You can add one later in Settings.</p>
      ) : null}
    </StepFrame>
  );
}

/** The microphone's level as a row of bars that rise left to right, lit up to how loud the room is. Input: the level, 0 to 1. Output: the meter. The square root spreads ordinary speech, which sits low on a linear scale, across the bars. */
function Meter({ level }: { level: number }) {
  const bars = 18;
  const lit = Math.round(Math.sqrt(Math.max(0, Math.min(1, level))) * bars);
  return (
    <div aria-hidden className="flex h-6 items-end gap-[3px]">
      {Array.from({ length: bars }, (_, i) => (
        <span key={i} className={`w-1.5 rounded-xs transition-colors duration-75 ${i < lit ? "bg-primary" : "bg-muted-foreground/20"}`} style={{ height: `${35 + (i / (bars - 1)) * 65}%` }} />
      ))}
    </div>
  );
}

/** Why the microphone test did not run, in words. Input: what it rejected with. Output: the sentence. */
function micProblem(e: unknown): string {
  const status = errorStatus(e);
  if (status === 404) return "This version of June can't test the microphone from here yet.";
  if (status === undefined) return "June isn't answering. Try again in a moment.";
  if (errorWord(e) === "busy") return "A microphone test is already running. Wait a moment, then test again.";
  return couldNotOpen(errorMessage(e));
}

/** What a microphone that would not open means. Input: the reason the daemon gave, or "". Output: the sentence. */
function couldNotOpen(reason: string): string {
  return `June couldn't open a microphone${reason ? ` (${reason.replace(/\.$/, "")})` : ""}. Check one is plugged in, then test again.`;
}

function MicStep({ onBack, onNext }: { onBack: () => void; onNext: () => void }) {
  const dispatch = useAppDispatch();
  const level = useAppSelector((s) => s.setup.micLevel);
  const [test, { isLoading: testing }] = useMicTestMutation();
  const [openSettings] = useOpenMicSettingsMutation();
  const [result, setResult] = useState<MicTest | undefined>(undefined);
  const [failure, setFailure] = useState("");
  const [settingsProblem, setSettingsProblem] = useState("");

  const run = async () => {
    setResult(undefined);
    setFailure("");
    dispatch(setupUi.micLevelReset());
    try {
      const r = await test().unwrap();
      // A microphone that would not open still answers 200, with the reason in error, since the consent reading beside it is worth having either way. Blocked is read first: a microphone Windows blocks often fails to open for that very reason, and the privacy settings are the fix, not checking the plug.
      if (r.error && !r.heard && r.consent !== "blocked") setFailure(couldNotOpen(r.error));
      else setResult(r);
    } catch (e) {
      setFailure(micProblem(e));
    } finally {
      dispatch(setupUi.micLevelReset());
    }
  };

  const blocked = result?.consent === "blocked" && !result.heard;
  /** Opens the system's own microphone settings, saying beside the button — not in place of the advice above it — when there is none June can open. */
  const settings = (label: string) => (
    <>
      <Button
        variant="outline"
        size="sm"
        className="mt-3"
        onClick={() => {
          setSettingsProblem("");
          void openSettings()
            .unwrap()
            .catch((e) => setSettingsProblem(errorMessage(e) || (ON_WINDOWS ? "June couldn't open the settings. Open Settings → Privacy & security → Microphone yourself." : "June couldn't open the settings. Open your system's sound settings yourself.")));
        }}
      >
        {label} <ArrowUpRight />
      </Button>
      {settingsProblem ? <p className="mt-2 text-meta text-destructive">{settingsProblem}</p> : null}
    </>
  );
  let verdict: ReactNode = null;
  if (testing) verdict = <p className="text-ui text-muted-foreground">Listening… say a few words.</p>;
  else if (failure) verdict = <p className="text-ui text-destructive">{failure}</p>;
  else if (result?.heard)
    verdict = (
      <p className="flex items-center gap-1.5 text-ui">
        <Check aria-hidden className="size-4 text-primary" /> June can hear you{result.device ? ` through ${result.device}` : ""}.
      </p>
    );
  else if (blocked)
    verdict = (
      <div>
        <p className="text-ui text-destructive">{ON_WINDOWS ? "Windows is blocking the microphone for desktop apps." : "The system is blocking the microphone."}</p>
        <p className="mt-1 text-ui text-muted-foreground">
          {ON_WINDOWS
            ? "In the settings that open, turn on “Microphone access”, and under it “Let desktop apps access your microphone”. Then come back and test again."
            : "Allow microphone access in the settings that open, then come back and test again."}
        </p>
        {result?.error ? <p className="mt-1 text-meta break-words text-muted-foreground">The microphone wouldn't open: {result.error.replace(/\.$/, "")}.</p> : null}
        {settings("Open privacy settings")}
      </div>
    );
  else if (result)
    verdict = (
      <div>
        <p className="text-ui">June didn't hear anything{result.device ? ` from ${result.device}` : ""}.</p>
        <p className="mt-1 text-ui text-muted-foreground">Check that the microphone isn't muted or unplugged, then test again.</p>
        {/* Windows' page is about permission, which a silent test has already shown is granted; a Linux desktop's opens on the input device and its level, which is where a muted microphone is unmuted. */}
        {ON_WINDOWS ? null : settings("Open sound settings")}
      </div>
    );

  return (
    <StepFrame
      title="Check your microphone"
      lead="June listens when you dictate, talk to it, or record a meeting. Press Test and say a few words."
      footer={
        <>
          <Back onClick={onBack} />
          <div className="ml-auto">
            {result?.heard ? (
              <Button size="lg" onClick={onNext}>
                Continue
              </Button>
            ) : (
              <Button variant="ghost" size="lg" onClick={onNext}>
                Skip for now
              </Button>
            )}
          </div>
        </>
      }
    >
      <Group>
        <div className="flex items-center gap-4 px-4 py-4">
          <Mic aria-hidden className="size-5 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1">
            <Meter level={testing ? level : 0} />
          </div>
          <Button variant={result?.heard ? "outline" : "default"} size="lg" disabled={testing} onClick={() => void run()}>
            {testing ? <Loader2 className="animate-spin" /> : null}
            {testing ? "Listening" : result || failure ? "Test again" : "Test"}
          </Button>
        </div>
      </Group>
      <div className="mt-4 min-h-12" aria-live="polite">
        {verdict}
      </div>
    </StepFrame>
  );
}

function FeaturesStep({ onBack, onNext }: { onBack: () => void; onNext: () => void }) {
  return (
    <StepFrame
      title="Add local features"
      lead="Optional. These run on this PC instead of online. Each is a one-time download — set up any you like, or none. Downloads keep going in the background while you finish."
      footer={
        <>
          <Back onClick={onBack} />
          <Button size="lg" className="ml-auto" onClick={onNext}>
            Continue
          </Button>
        </>
      }
    >
      <FeatureList />
    </StepFrame>
  );
}

/** Asks the first question from a fresh chat, the same way the composer sends a fresh draft's first message (see useSend in chat-composer.tsx): tracked under the draft key until the daemon says which conversation it opened, then opened. Kept apart from the composer because setup has finished and gone by the time the answer arrives, and nothing of this screen is mounted to see it through. Input: the store's dispatch and the question. Output: nothing; a question that is not taken is put back in the box with the rail's one notice line, as the composer does. */
async function askFirst(dispatch: AppDispatch, question: string): Promise<void> {
  dispatch(progress.askSent({ conversationId: DRAFT_CHAT, question }));
  try {
    const res = await dispatch(juneApi.endpoints.ask.initiate({ question, conversation_id: "" })).unwrap();
    dispatch(progress.askAccepted({ askId: res.id, conversationId: res.conversation_id }));
    if (res.conversation_id) dispatch(ui.conversationOpened(res.conversation_id));
  } catch {
    dispatch(progress.askFailed());
    dispatch(ui.asked({ conversationId: DRAFT_CHAT, text: question }));
    dispatch(ui.noticed({ text: "Could not send that question", kind: "error" }));
  }
}

/** The localStorage key the first question waits under while the daemon restarts. A restart closes this window and the daemon opens a fresh one with an empty store (stopWindow in cmd/tray_*.go), so neither the composer nor anything else in Redux outlives it. */
const FIRST_QUESTION_KEY = "june.setup.firstQuestion";

/** How long a waiting first question stays worth asking. A restart takes seconds; a question still there a day later is from a window that never came back, and asking it then would come out of nowhere. */
const FIRST_QUESTION_TTL_MS = 10 * 60 * 1000;

function keepFirstQuestion(question: string): void {
  try {
    localStorage.setItem(FIRST_QUESTION_KEY, JSON.stringify({ question, at: Date.now() }));
  } catch {
    /* storage blocked: the question is lost with the window, as it would be anyway */
  }
}

/** Takes the waiting first question, if there is one still worth asking, and forgets it, so it is asked once whichever window gets there first. */
function takeFirstQuestion(): string {
  try {
    const raw = localStorage.getItem(FIRST_QUESTION_KEY);
    if (raw === null) return "";
    localStorage.removeItem(FIRST_QUESTION_KEY);
    const v = JSON.parse(raw) as { question?: unknown; at?: unknown };
    return typeof v.question === "string" && typeof v.at === "number" && Date.now() - v.at < FIRST_QUESTION_TTL_MS ? v.question : "";
  } catch {
    return "";
  }
}

/** Asks the first question setup's last screen was finished with, once the restart that finishing caused is over. Input: whether GET /setup says setup is done, and whether the window is still waiting on a restart. Output: none. Mounted for the whole window, since it is a new window, or this one after the restart screen, that gets to ask it. */
export function useFirstQuestionAfterRestart(done: boolean, restarting: boolean): void {
  const dispatch = useAppDispatch();
  useEffect(() => {
    if (!done || restarting) return;
    const question = takeFirstQuestion();
    if (!question) return;
    dispatch(ui.chatDraftOpened());
    void askFirst(dispatch, question);
  }, [done, restarting, dispatch]);
}

/** What the last screen says about the features still downloading. Input: them. Output: the sentence, which says when each switches on: by itself when it is done, or — for one that needs a restart — once June restarts, which the daemon does by itself after the downloads when setup ended with a restart owed (POST /setup/complete puts it off while anything downloads). */
function stillDownloading(list: UnderWay[], restartOwed: boolean): string {
  const one = list.length === 1;
  const names = list.map((u) => (u.pct === undefined ? u.title : `${u.title} (${u.pct}%)`)).join(", ");
  const going = `${names}. ${one ? "It keeps" : "They keep"} downloading in the background`;
  if (restartOwed) return `${going}. Once ${one ? "it's" : "they're"} done, June restarts by itself for a few seconds to switch everything on.`;
  const later = list.filter((u) => u.needsRestart);
  if (!later.length) return `${going} and ${one ? "switches on by itself when it's" : "switch on by themselves when they're"} done.`;
  if (later.length === list.length) return `${going} and ${one ? "is" : "are"} switched on the next time June starts.`;
  return `${going}. ${later.map((u) => u.title).join(" and ")} ${later.length === 1 ? "is" : "are"} switched on the next time June starts; the rest as soon as they're done.`;
}

function DoneStep({ setup, onBack }: { setup: SetupView; onBack: () => void }) {
  const dispatch = useAppDispatch();
  const underWay = useFeaturesUnderWay();
  const { data: brains } = useFreshBrainsQuery();
  const { data: daemon } = useSettingsQuery();
  const [saveSettings] = useSaveSettingsMutation();
  const [fallbackProblem, setFallbackProblem] = useState("");
  const hotkeyGone = useHotkeyUnavailable();
  const [finishing, setFinishing] = useState(false);
  const [failed, setFailed] = useState<{ text: string; detail?: string } | undefined>(undefined);
  // The same reading as the brain screen's: a key, or a login a provider has not refused. Offering a question to a June that cannot answer it made the first thing a new person saw in Chats an error.
  const canAnswer = setup.gemini_key || (brains ? answeringLogin(brains.brains) !== undefined : setup.brain_ready);
  // Windows' window registers Ctrl+Alt+Space itself; on Linux a hotkey is only there when GNOME's keybinding for June is, and "" from GET /setup means there is none to press.
  const keys = hotkeyKeys(setup.hotkey || (ON_WINDOWS ? "Ctrl+Alt+Space" : ""));
  // Falling back only means something with a second way to answer: a key and a login, or two logins.
  const ways = (setup.gemini_key ? 1 : 0) + LOGINS.filter(({ id }) => brains?.brains.some((b) => b.id === id && b.signed_in)).length;
  const fallback = daemon?.allow_fallback;

  /** Writes the fall-back choice, saying under it when the daemon would not take it. */
  const setFallback = async (on: boolean) => {
    setFallbackProblem("");
    try {
      await saveSettings({ allow_fallback: on }).unwrap();
    } catch {
      setFallbackProblem("That didn't change. You can change it later in Settings → Brain.");
    }
  };

  /** Ends setup — which is what starts June watching the screen — and opens Chats, asking the first question when one was picked. When an earlier change was waiting on a restart and nothing is downloading, the daemon restarts now instead, and the question waits in localStorage for the window that comes back. */
  const finish = async (question?: string) => {
    setFinishing(true);
    setFailed(undefined);
    let restarting = false;
    try {
      const r = await dispatch(juneApi.endpoints.completeSetup.initiate()).unwrap();
      restarting = Boolean(r?.restarting);
    } catch (e) {
      setFinishing(false);
      // A 500 is June's settings file refusing the write — locked by another program, or not June's to write — which trying again in a moment does not fix, so the reason the daemon gave is shown with it.
      const reason = errorMessage(e);
      setFailed(errorStatus(e) === undefined ? { text: "June isn't answering. Try again in a moment." } : reason ? { text: "Setup couldn't be finished, because June couldn't save its settings.", detail: reason } : { text: "Setup couldn't be finished. Try again in a moment." });
      return;
    }
    writeStep();
    if (question && restarting) keepFirstQuestion(question);
    else if (question) {
      dispatch(ui.chatDraftOpened());
      void askFirst(dispatch, question);
    }
    if (restarting) dispatch(setupUi.restartBegan("restart"));
    // The window leaves setup the moment the daemon says it is done rather than a round trip later, which is when the question above starts streaming into Chats.
    dispatch(
      juneApi.util.updateQueryData("setup", undefined, (d) => {
        d.done = true;
      }),
    );
    dispatch(juneApi.util.invalidateTags(["Setup", "Tracker", "Settings"]));
  };

  return (
    <StepFrame
      face="done"
      title="June is ready"
      lead="A few things to remember:"
      footer={
        <>
          <Back onClick={onBack} />
          <Button size="lg" className="ml-auto" disabled={finishing} onClick={() => void finish()}>
            {finishing ? <Loader2 className="animate-spin" /> : null} Start using June
          </Button>
        </>
      }
    >
      <Group>
        <div className="divide-y">
          <Fact label="Open June">
            {hotkeyGone ? (
              NO_SHORTCUT
            ) : keys.length ? (
              <span className="inline-flex flex-wrap items-center gap-1">
                Press
                {keys.map((k) => (
                  <kbd key={k} className="rounded-xs border border-hairline-strong px-1.5 py-0.5 text-micro text-foreground uppercase">
                    {k}
                  </kbd>
                ))}
                from anywhere.
              </span>
            ) : (
              "Open it from your apps menu whenever you need it."
            )}
          </Fact>
          {/* A switch rather than a sentence: the installer's tick box for this is easy to miss, and this is the last place setup can ask. */}
          <Fact label={ON_WINDOWS ? "Starts with Windows" : "Starts when you log in"} control={<AutostartSwitch on={setup.autostart} label={ON_WINDOWS ? "Start June with Windows" : "Start June when you log in"} />}>
            {setup.autostart ? "June opens in the background when you sign in, so it is always keeping notes." : `June runs when you open it${ON_WINDOWS ? " from the Start menu" : ""}. It won't start on its own when you sign in.`}
          </Fact>
          <Fact label="Watching your screen">Starts when you press Start using June. You can pause it any time, for a few minutes or for longer, from the button beside June's face at the top left.</Fact>
          {fallback !== undefined && ways > 1 ? (
            <Fact
              label="Background work"
              control={<Switch checked={fallback} aria-label="If your chosen AI can't answer, try my other signed-in AIs" onCheckedChange={(on) => void setFallback(on)} />}
            >
              {/* Background work only: the daemon applies this to its own duties (agent.DutyFallbackAllowed), not to the questions a person asks. */}
              {fallback
                ? "When the AI you picked can't do June's background work — tidying its notes, writing up meetings — your other signed-in AIs do it, which can use up their plans' allowance."
                : "June's background work — tidying its notes, writing up meetings — is done only by the AI you picked."}
              {fallbackProblem ? <span className="mt-1 block text-meta text-destructive">{fallbackProblem}</span> : null}
            </Fact>
          ) : null}
          {/* GNOME loads a newly installed extension only at the next login, and screen control needs it. */}
          {setup.window_frames === false ? <Fact label="Screen control">Log out once and back in to turn on screen control.</Fact> : null}
          {underWay.length ? <Fact label="Still downloading">{stillDownloading(underWay, setup.restart_pending)}</Fact> : null}
        </div>
      </Group>
      {canAnswer ? (
        <div className="mt-8">
          <h2 className="mb-2 text-ui font-medium">Try asking</h2>
          <button
            type="button"
            disabled={finishing}
            onClick={() => void finish(FIRST_QUESTION)}
            className="rounded-sm border border-hairline-strong bg-card px-3 py-1.5 text-meta text-foreground transition-colors hover:bg-hover focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none disabled:opacity-60"
          >
            {FIRST_QUESTION}
          </button>
        </div>
      ) : (
        <p className="mt-8 text-ui text-muted-foreground">June starts keeping notes now, but can't answer questions until it has a brain. Add a Gemini key or sign in to one of your AI apps any time in Settings → Brain.</p>
      )}
      {failed ? (
        <div role="alert" className="mt-4">
          <p className="text-meta text-destructive">{failed.text}</p>
          {failed.detail ? <p className="mt-1 text-meta break-words text-muted-foreground">{failed.detail}</p> : null}
        </div>
      ) : null}
    </StepFrame>
  );
}

/** The dots across the top that say how far through setup the person is. Input: how many screens there are and which is showing. Output: the dots, named for a screen reader as "Step 2 of 5". */
function Dots({ count, at }: { count: number; at: number }) {
  return (
    <div role="img" aria-label={`Step ${at + 1} of ${count}`} className="flex items-center gap-1.5">
      {Array.from({ length: count }, (_, i) => (
        <span key={i} className={`size-1.5 rounded-full transition-colors ${i === at ? "bg-primary" : i < at ? "bg-primary/40" : "bg-muted-foreground/25"}`} />
      ))}
    </div>
  );
}

/** First-run setup. Input: what GET /setup said. Output: the whole window. */
export function Onboarding({ setup }: { setup: SetupView }) {
  const { data: comps, isError: componentsFailed } = useComponentsQuery();
  // The local features screen is left out where the daemon offers none, or never answered the route at all, as one too old to have it does. Not on a failed refetch with a list already read: isError stays set across one, and every focus refetches, so a person mid-download on that screen was thrown past it.
  const noFeatures = comps ? comps.features.length === 0 : componentsFailed;
  const steps = STEPS.filter((s) => s !== "features" || !noFeatures);
  const [stored, setStored] = useState<Step>(readStep);
  const step = steps.includes(stored) ? stored : "done";
  const at = steps.indexOf(step);
  const go = (s: Step) => {
    setStored(s);
    writeStep(s);
  };
  const next = () => go(steps[Math.min(at + 1, steps.length - 1)]);
  const back = () => go(steps[Math.max(at - 1, 0)]);

  let body: ReactNode;
  switch (step) {
    case "brain":
      body = <BrainStep setup={setup} onBack={back} onNext={next} />;
      break;
    case "mic":
      body = <MicStep onBack={back} onNext={next} />;
      break;
    case "features":
      body = <FeaturesStep onBack={back} onNext={next} />;
      break;
    case "done":
      body = <DoneStep setup={setup} onBack={back} />;
      break;
    default:
      body = <WelcomeStep setup={setup} onNext={next} />;
  }

  return (
    <div className="flex h-svh flex-col bg-background">
      <header className="flex h-12 shrink-0 items-center border-b px-6">
        <span className="text-ui font-medium">Setting up June</span>
        <div className="ml-auto">
          <Dots count={steps.length} at={at} />
        </div>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <main className="mx-auto w-full max-w-[36rem] px-8 pt-14 pb-20">{body}</main>
      </div>
    </div>
  );
}

/** What the window shows while the daemon restarts, which takes the window down with it in the desktop app and brings a new one up in its place; in a browser tab the same window waits and carries on. Input: why it is restarting, whether it has been gone long enough to say what to do if it never comes back, and, once the restart is known not to be happening, why not ("" when the daemon gave no reason). Output: the whole window. */
export function RestartScreen({ why, slow, failed }: { why: RestartWhy; slow: boolean; failed?: string }) {
  const dispatch = useAppDispatch();
  if (failed !== undefined) {
    const update = why === "update";
    return (
      <div role="alert" className="flex h-svh flex-col items-center justify-center gap-3 bg-background px-8 text-center">
        <Face state="refused" className="text-title" />
        <p className="text-doc text-foreground">{update ? "June didn't update" : "June couldn't restart"}</p>
        {failed ? (
          <p className="max-w-[52ch] text-meta break-words text-muted-foreground">
            {failed.charAt(0).toUpperCase() + failed.slice(1).replace(/\.$/, "")}.
          </p>
        ) : null}
        <p className="max-w-[46ch] text-read text-muted-foreground">
          {update
            ? "It's still running the version you had. Try the update again later, or download it yourself from the release page."
            : `What you changed is saved, and takes effect the next time June starts. To start it again now, quit June${ON_WINDOWS ? " from its icon in the system tray (click ^ by the clock if you don't see it)" : ""}, then open it again.`}
        </p>
        <Button
          size="lg"
          className="mt-3"
          onClick={() => {
            dispatch(setupUi.restartEnded());
            dispatch(juneApi.util.invalidateTags(["Setup", "Component", "Settings", "Brain", "Update"]));
          }}
        >
          Back to June
        </Button>
      </div>
    );
  }
  return (
    <div role="status" className="flex h-svh flex-col items-center justify-center gap-3 bg-background px-8 text-center">
      <Face state="thinking" className="text-title" />
      <p className="text-doc text-foreground">{why === "update" ? "Updating June…" : "Restarting June…"}</p>
      <p className="max-w-[46ch] text-read text-muted-foreground">{why === "update" ? "The new version opens on its own in a moment." : "This takes a few seconds."}</p>
      {slow ? (
        <p className="max-w-[46ch] text-read text-muted-foreground">
          This is taking longer than it should. If June doesn't come back, open it again{ON_WINDOWS ? " from the Start menu" : ""}.
        </p>
      ) : null}
    </div>
  );
}
