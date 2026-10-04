/** The Settings screen: how the window looks, where the hover opens, the hotkey that opens it, what the daemon is allowed to watch, which brain answers and on which model and the Gemini key, the local features downloaded onto this machine, what the daemon is running on it, which version of June this is, and the token ledger. Every value is read off GET /settings, GET /setup, GET /brains, GET /status, GET /components, GET /update and GET /usage, and every control writes through the route that owns the setting, except the theme and the hover position, which the window and the hover share through localStorage rather than through the daemon (see HOVER_POSITION_KEY in src/winplace.ts). The page is a stack of grouped cards, the way a desktop settings pane is built, rather than a run of rows under grey capitals. */

import { Fragment, useState } from "react";
import { Check, ChevronDown, Loader2, Play } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Switch } from "@/components/ui/switch";
import { HOVER_POSITION_KEY, storedHoverPosition, type HoverPosition } from "../winplace";
import {
  errorMessage,
  errorStatus,
  errorWord,
  useBrainsQuery,
  usePickBrainMutation,
  usePreviewVoiceMutation,
  useSaveSettingsMutation,
  useSetCaptureMutation,
  useSetLiveModelMutation,
  useSetVoiceMutation,
  useSettingsQuery,
  useSetupQuery,
  useTrackerQuery,
  useUsageQuery,
  useVoicesQuery,
  SEARCH_PROVIDERS,
  type LiveModel,
  type ProviderLimits,
  type SettingsChange,
  type SettingsView,
  type Usage,
  type UsageWindow,
  type Voice,
} from "./api";
import { bytes, cachedInput, compact, hhmm, hotkeyKeys, modelEffort, perQuestion, tokens, took } from "./format";
import { AutostartSwitch, BrainChoices, GeminiKey } from "./onboarding";
import { FeatureList } from "./onboarding-features";
import { Blank, BrainPicker, Group, HEAD, NO_SHORTCUT, ON_WINDOWS, PageHeader, Reading, Scroller, SectionHeading, TAIL, useHotkeyUnavailable, useWide } from "./parts";
import { settings as settingsUi, ui, useAppDispatch, useAppSelector, type Theme } from "./store";
import { UpdateStatus } from "./update";

/** The keys the installer registers, drawn when the daemon reports no accelerator of its own. */
const INSTALLED_HOTKEY = ["Ctrl", "Alt", "Space"];

/** What each brain is, in a line a person can read. The daemon's own notes are written for whoever maintains June — a command to run, a file to edit, a policy to cite — so the window says it in its own words, and says nothing for a brain it has no line for. */
const BRAIN_NOTES: Record<string, string> = {
  antigravity: "Answers on the Google plan you already pay for.",
  gemini: "Answers on your Gemini key. Google's free tier allows a limited number of requests a day.",
  codex: "Answers on your ChatGPT plan, through the Codex app's sign-in.",
  claude: "Answers on your Claude plan, through Claude Code's sign-in.",
  grok: "Grok offers no choice of model, so there is nothing to pick here.",
  ollama: "Runs a model on this computer, so nothing leaves it.",
};

/** What to do before June can answer at all, drawn only while the daemon still says none of the ways of answering text is set up. Input: none — it reads GET /settings itself, so it can sit at the top of Settings without being passed anything. Output: the panel, or nothing once the daemon reports an empty step list. The daemon's own steps say which file and which command; the panel offers the same ways first-run setup does instead, none of which needs a terminal or a text editor. */
export function FirstRunPanel() {
  const { data: daemon } = useSettingsQuery();
  if (!daemon?.first_run?.steps?.length) return null;
  return <SetUpCard lead="June needs a brain before it can answer." />;
}

/** The chat page's card while GET /brains lists no brain signed in: the ways June can be given one, the same as first-run setup's. Input: none. Output: the card. */
export function NoBrainPanel() {
  return <SetUpCard lead="No brain is signed in on this computer." />;
}

/** The card both set-up panels draw. Input: the sentence under the heading. Output: the card, with the logins June found and the Gemini key box under it. */
function SetUpCard({ lead }: { lead: string }) {
  const { data: setup } = useSetupQuery();
  return (
    <div>
      <h2 className="text-doc text-foreground">June can't answer yet</h2>
      <p className="mt-1.5 mb-5 text-read text-muted-foreground">{lead} The quickest is a key from Google — free to get, and it also lets you talk to June out loud.</p>
      <BrainChoices geminiKey={setup?.gemini_key ?? false} />
    </div>
  );
}

/** One labelled row of a group. Input: the label, the sentence under it when there is one, and the control. Output: the row. */
function Row({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-6 px-3.5 py-2.5">
      <div className="min-w-0">
        <div className="text-ui">{label}</div>
        {hint ? <div className="mt-0.5 text-meta text-muted-foreground">{hint}</div> : null}
      </div>
      <div className="shrink-0">{children}</div>
    </div>
  );
}

/** One label-and-value line in the machine block, shaped like a settings Row: a noun for a label, the sentence fragment that used to be the label as the subtitle under it, and the value on the right. Input: the label, the phrase under it when there is one, the value, and whether the value is something the machine chose rather than prose. Output: the line, or nothing when there is no value, so a field the daemon could not fill draws no row. The value wraps onto as many lines as it needs — a data directory or a model name can run long, and a truncated one would say nothing that its own hover tooltip had to finish. */
function Fact({ label, hint, value, mono }: { label: string; hint?: string; value: string; mono?: boolean }) {
  if (!value) return null;
  return (
    <div className="flex items-start justify-between gap-6 px-3.5 py-2">
      <div className="min-w-0 shrink-0">
        <div className="text-ui">{label}</div>
        {hint ? <div className="mt-0.5 text-meta text-muted-foreground">{hint}</div> : null}
      </div>
      {mono ? (
        <code className="min-w-0 break-words text-right font-mono text-[0.92em]">{value}</code>
      ) : (
        <span className="min-w-0 break-words text-right text-ui">{value}</span>
      )}
    </div>
  );
}

/** What the daemon is running and what it has written down. Input: the /settings answer. Output: the block. */
function Machine({ s }: { s: SettingsView }) {
  const started = new Date(s.daemon_started);
  // With no embedding model the value is a sentence about what search falls back to, not a model id, so it stays in the body face.
  const noEmbeddings = s.embed_model === "none";
  return (
    <section className="mt-10">
      <SectionHeading>This machine</SectionHeading>
      <Group>
        <div className="divide-y">
          <Fact label="Model" hint="answers with" value={s.brain} mono />
          <Fact label="Embeddings" hint="understands meaning with" value={noEmbeddings ? "nothing — search is words only" : s.embed_model} mono={!noEmbeddings} />
          <Fact label="Live voice" hint="talks through" value={s.voice_model} mono />
          <Fact label="Location" hint="everything is kept in" value={s.data_dir} mono />
          <Fact label="What it has written" value={`${bytes(s.store_bytes)} of memory · ${bytes(s.recordings_bytes)} of audio · ${bytes(s.models_bytes)} of models`} />
          <Fact label="Audio kept for" value={s.keep_audio_days < 0 ? "as long as you leave it there" : `${s.keep_audio_days} days`} />
          <Fact label="Running since" value={Number.isNaN(started.getTime()) ? "" : `${hhmm(s.daemon_started)}, build ${s.version}`} />
        </div>
      </Group>
    </section>
  );
}

/** The sentence the daemon sent with a failure. Input: whatever the mutation rejected with. Output: the plain-text body it carried, trimmed to one line, or "" when it carried none — RTK Query puts a text/plain error body in data and reports the status as PARSING_ERROR, since it expected JSON. */
function errorSentence(e: unknown): string {
  if (!e || typeof e !== "object" || !("data" in e)) return "";
  const data = (e as { data: unknown }).data;
  if (typeof data !== "string") return "";
  const line = data.split("\n")[0].trim();
  return line.length > 200 ? line.slice(0, 200) + "…" : line;
}

/** Why a voice could not be played, in words. Input: what the preview rejected with, and whether a Gemini key is saved. Output: the sentence. A 503 means no speaker or no key, and the daemon's sentence for the second names a file and a variable, so the window says both in its own words: told apart by the error word a newer daemon sends, then by the daemon's sentence, then by whether a key is saved at all. Anything else keeps the daemon's own sentence — a spent daily allowance, a line the model refused — since that is the one that says what happened. */
function previewProblem(e: unknown, geminiKey: boolean): string {
  const said = errorMessage(e) || errorSentence(e);
  if (errorStatus(e) !== 503) return said || "Could not play that voice";
  const word = errorWord(e);
  if (word === "no_speaker" || /speaker/i.test(said)) return "This machine has no speaker to play it through";
  if (word === "no_key" || /gemini|key/i.test(said) || !geminiKey) return "Hearing a voice needs a Gemini key. Add one under Brain.";
  return "This machine has no speaker to play it through";
}

/** The voice picker: a single select showing the current voice and the trait that says how it sounds, with every one of Gemini Live's thirty prebuilt voices behind it the same way, and a play button beside it that previews whichever voice is currently picked. Input: whether a Gemini key is saved, undefined while that is not known. Output: the section. */
function VoiceSection({ geminiKey }: { geminiKey?: boolean }) {
  const dispatch = useAppDispatch();
  const { data } = useVoicesQuery();
  const voices = data?.voices ?? [];
  const models = data?.models ?? [];
  const [setVoice] = useSetVoiceMutation();
  const [setLiveModel] = useSetLiveModelMutation();
  const [previewVoice, { isLoading: playing }] = usePreviewVoiceMutation();
  const current = voices.find((v) => v.current);
  const currentModel = models.find((m) => m.current);

  const pick = async (voice: Voice) => {
    try {
      await setVoice(voice.name).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not change the voice", kind: "error" }));
    }
  };

  const pickModel = async (model: LiveModel) => {
    try {
      await setLiveModel(model.name).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not change the voice model", kind: "error" }));
    }
  };

  const preview = async () => {
    if (!current) return;
    try {
      await previewVoice(current.name).unwrap();
    } catch (e) {
      dispatch(ui.noticed({ text: previewProblem(e, geminiKey ?? true), kind: "error" }));
    }
  };

  return (
    <section className="mt-10">
      <SectionHeading>Voice</SectionHeading>
      <p className="mb-3 text-meta text-muted-foreground">Heard the next time a live voice session starts, not the one already running — the daemon reads this when it dials.</p>
      {/* Said before the play button is pressed rather than after: with no key, talking out loud and hearing a voice you have not played before both need one. */}
      {geminiKey === false ? <p className="mb-3 text-meta text-muted-foreground">Talking to June out loud, and hearing a voice you haven't played before, need a Gemini key. Add one under Brain.</p> : null}
      <Group>
        {models.length > 0 ? (
          <Row label="Model">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="sm" aria-label="Model" className="gap-1 px-1.5 font-normal text-muted-foreground hover:text-foreground">
                  <span className="text-foreground">{currentModel ? currentModel.label : "not set"}</span>
                  <ChevronDown className="opacity-60" />
                </Button>
              </DropdownMenuTrigger>
              {/* A handful of models, so nothing here scrolls. Each trait is a full sentence about the trade — latency against tone — so it sits on its own line under the label rather than crowding beside it the way a voice's one-word trait does. */}
              <DropdownMenuContent align="end" className="w-72">
                {models.map((m) => (
                  <DropdownMenuItem key={m.name} onClick={() => void pickModel(m)} className="items-start gap-2 whitespace-nowrap">
                    <Check className={`mt-0.5 size-3.5 shrink-0 ${m.current ? "" : "invisible"}`} />
                    <span className="flex flex-col whitespace-normal">
                      <span className={m.current ? "font-medium text-foreground" : undefined}>{m.label}</span>
                      <span className="text-meta text-muted-foreground">{m.trait}</span>
                    </span>
                  </DropdownMenuItem>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>
          </Row>
        ) : null}
        {voices.length === 0 ? (
          <p className="px-3.5 py-3 text-ui text-muted-foreground">No voices reported.</p>
        ) : (
          <Row label="Voice">
            <div className="flex items-center gap-2">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="sm" aria-label="Voice" className="gap-1 px-1.5 font-normal text-muted-foreground hover:text-foreground">
                    <span className="text-foreground">{current ? `${current.name} — ${current.trait}` : "not set"}</span>
                    <ChevronDown className="opacity-60" />
                  </Button>
                </DropdownMenuTrigger>
                {/* Thirty voices is more than a menu can show at once, so it is bounded and scrolls inside itself rather than covering the page. Each row is one line — a wrapped name reads as two entries — and the one in use carries a tick in a gutter every row reserves, so nothing shifts sideways as the selection moves. */}
                <DropdownMenuContent align="end" className="max-h-72 w-56 overflow-y-auto">
                  {voices.map((v) => (
                    <DropdownMenuItem key={v.name} onClick={() => void pick(v)} className="gap-2 whitespace-nowrap">
                      <Check className={`size-3.5 shrink-0 ${v.current ? "" : "invisible"}`} />
                      <span className={v.current ? "font-medium text-foreground" : undefined}>
                        {v.name} — {v.trait}
                      </span>
                    </DropdownMenuItem>
                  ))}
                </DropdownMenuContent>
              </DropdownMenu>
              <Button variant="ghost" size="icon-sm" aria-label={current ? `Play ${current.name}` : "Play"} disabled={playing || !current} onClick={() => void preview()}>
                {playing ? <Loader2 className="animate-spin" /> : <Play />}
              </Button>
            </div>
          </Row>
        )}
      </Group>
    </section>
  );
}

/** The four number columns of one usage-table row: calls, in, out, total. Module scope — it closes over nothing but its own argument, so there is no reason to rebuild it on every render. */
function cells(r: { calls: number; input_tokens: number; output_tokens: number; total_tokens: number }) {
  return (
    <>
      <td className="py-1.5 text-right tabular-nums">{tokens(r.calls)}</td>
      <td className="py-1.5 text-right tabular-nums">{tokens(r.input_tokens)}</td>
      <td className="py-1.5 text-right tabular-nums">{tokens(r.output_tokens)}</td>
      <td className="py-1.5 text-right tabular-nums">{tokens(r.total_tokens)}</td>
    </>
  );
}

/** What a ledger row calls the model a call ran on. Input: the provider and the model the daemon filed. Output: the model, or "default model" when the call named none and the daemon filed it under the provider's own name instead — an unpinned Antigravity run is filed as model "agy", which drew a model row reading exactly like the provider row above it. */
function usageModel(provider: string, model: string): string {
  return !model || model === provider ? "default model" : model;
}

/** One window of the ledger as a table: a row per provider with that provider's models under it. Input: the window and the line to show when nothing was spent in it. Output: the table, or that one line. */
function UsageTable({ window: w, empty }: { window: UsageWindow; empty: string }) {
  const providers = w?.providers ?? [];
  if (!providers.length) return <p className="text-ui text-muted-foreground">{empty}</p>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[26rem] text-ui">
        <thead>
          <tr className="border-b text-meta text-muted-foreground">
            <th className="py-1.5 text-left font-normal">Provider</th>
            <th className="py-1.5 text-right font-normal">Calls</th>
            <th className="py-1.5 text-right font-normal">In</th>
            <th className="py-1.5 text-right font-normal">Out</th>
            <th className="py-1.5 text-right font-normal">Total</th>
          </tr>
        </thead>
        <tbody>
          {providers.map((p) => (
            <Fragment key={p.provider}>
              <tr className="border-b">
                <td className="py-1.5 font-medium">{p.provider}</td>
                {cells(p)}
              </tr>
              {(w.models ?? [])
                .filter((m) => m.provider === p.provider)
                .map((m) => (
                  <tr key={`${p.provider}-${m.model}`} className="border-b text-muted-foreground">
                    <td className="py-1.5 pl-4">{usageModel(m.provider, m.model)}</td>
                    {cells(m)}
                  </tr>
                ))}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** One of the figures at the head of the ledger. Input: the figure and what it counts. Output: the pair, the number above the word. */
function Figure({ value, label }: { value: string; label: string }) {
  return (
    <div>
      <div className="text-figure tabular-nums">{value}</div>
      <div className="mt-0.5 text-meta text-muted-foreground">{label}</div>
    </div>
  );
}

/** Adds up one window of the ledger. Input: the window. Output: its calls and its tokens across every provider in it. */
function totals(w?: UsageWindow): { calls: number; tokens: number } {
  return (w?.providers ?? []).reduce((sum, p) => ({ calls: sum.calls + (p.calls || 0), tokens: sum.tokens + (p.total_tokens || 0) }), { calls: 0, tokens: 0 });
}

/** The web search providers' own plan allowances, one block per engine branch() can reach the web through. Input: the limits map GET /usage carries, keyed by provider id and holding brains alongside the search engines. Output: the section, or null when the daemon reported no search provider at all — which is what a machine with neither EXA_API_KEY nor TAVILY_API_KEY set answers, and also what one that simply has not searched this month answers.
 *
 * This exists because a search is the one call in the ledger that costs no tokens. It spends a request against a monthly plan, so the four token tables draw Exa and Tavily as rows of zeros and the count has nowhere else to appear. The daemon already works both figures out — Exa's from its own ledger, Tavily's off that provider's usage endpoint — and each one's source line says which, because the two numbers are not read the same way and a person comparing them should know that.
 */
function SearchSpend({ limits }: { limits?: Record<string, ProviderLimits> }) {
  const found = SEARCH_PROVIDERS.map((id) => [id, limits?.[id]] as const).filter(([, p]) => p && p.limits.length > 0);
  if (!found.length) return null;
  return (
    <div role="group" aria-label="Web search">
      <h3 className="mb-2 text-ui font-medium">Web search</h3>
      <div className="flex flex-col gap-4">
        {found.map(([id, provider]) => (
          <div key={id} className="flex flex-col gap-1">
            <span className="text-ui">{id.charAt(0).toUpperCase() + id.slice(1)}</span>
            {provider!.limits.map((l) => {
              // used_fraction is 0 when no ceiling for this provider is known, which the daemon means as "there is no bar to draw" rather than "nothing was spent" — see exaProviderLimits in internal/ipc/usage.go. The source line below carries the call count either way, so that case loses no number.
              const pct = Math.round(l.used_fraction * 100);
              return (
                <Fragment key={l.window}>
                  {l.used_fraction > 0 ? (
                    <div className="flex items-baseline justify-between gap-3 text-meta text-muted-foreground">
                      <span>{l.window === "monthly" ? "Monthly" : l.window}</span>
                      <span>{pct}%</span>
                    </div>
                  ) : null}
                  {l.used_fraction > 0 ? (
                    <div className="h-[3px] w-full overflow-hidden rounded-full bg-muted">
                      <div
                        data-testid={`search-bar-${id}-${l.window}`}
                        className={`h-full rounded-full ${l.used_fraction >= 0.9 ? "bg-destructive" : "bg-primary"}`}
                        style={{ width: `${Math.min(100, Math.max(0, pct))}%` }}
                      />
                    </div>
                  ) : null}
                  <p className="text-meta text-muted-foreground">{l.source}</p>
                </Fragment>
              );
            })}
          </div>
        ))}
      </div>
    </div>
  );
}

/** The token ledger: the figures saying what has been spent and what one question costs, the week as a bar a day, and then the tables and the call log behind them. Input: what GET /usage answered and whether it answered at all. Output: the section; a machine that has never called a provider gets one sentence rather than four empty tables.
 *
 * The daemon reports counts and never a price, so what a question costs is said in tokens rather than in money; putting a currency here would mean this window carrying its own price list for every provider, which would be wrong the day a provider changes one.
 */
export function UsageLedger({ usage, up }: { usage?: Usage; up: boolean }) {
  const spent = usage && (usage.today.providers.length > 0 || usage.week.providers.length > 0 || usage.recent.length > 0);
  if (!spent) return <Blank up={up} empty="Nothing asked yet" hint="No tokens have been spent on this machine. Ask June something and what it cost appears here." />;
  const max = (usage.days ?? []).reduce((m, d) => Math.max(m, d.total_tokens || 0), 0);
  const today = totals(usage.today);
  const week = totals(usage.week);
  // The priciest of the calls actually drawn below, which is what each row's bar is scaled against.
  const maxRecent = (usage.recent ?? []).reduce((m, c) => Math.max(m, c.total_tokens || 0), 0);
  // How much of what was sent the provider answered out of its own cache. Only drawn once a daemon actually reports the figure; until then the window says nothing rather than claiming nothing was cached.
  const cache = cachedInput(usage.recent ?? []);
  // The plan's own allowance, when the daemon reports one. Past four fifths is worth one quiet line, because the next thing a person does is decide whether to keep asking.
  const budget = usage.budget_used_fraction;
  const tight = typeof budget === "number" && budget > 0.8;
  return (
    <div className="flex flex-col gap-8">
      <div>
        <div className="flex flex-wrap gap-x-12 gap-y-5 border-b pb-5">
          <Figure value={compact(today.tokens)} label="today" />
          <Figure value={tokens(today.calls)} label={today.calls === 1 ? "call today" : "calls today"} />
          <Figure value={compact(week.tokens)} label="this week" />
          <Figure value={compact(perQuestion(week.tokens, week.calls))} label="a question, this week" />
        </div>
        {cache.has && cache.input > 0 ? (
          <p className="mt-3 text-meta text-muted-foreground">
            Of {tokens(cache.input)} tokens sent in the calls below, {tokens(cache.cached)} came back out of the provider's cache and {tokens(Math.max(0, cache.input - cache.cached))} were read
            afresh.
          </p>
        ) : null}
        {tight ? (
          <p role="status" className="mt-3 text-meta text-work">
            {Math.round((budget as number) * 100)}% of this plan's allowance has gone.
          </p>
        ) : null}
        {usage.days?.length ? (
          <div className="mt-6 flex items-end gap-2">
            {usage.days.map((d) => {
              const label = new Date(`${d.day}T00:00:00`);
              const name = Number.isNaN(label.getTime()) ? d.day : label.toLocaleDateString(undefined, { weekday: "short" });
              // A percentage resolves only against a parent of a stated height, which is why the bar sits in a box of its own 72px tall rather than in the column that also holds the two labels; the whole day's column would otherwise be auto-height and every bar would come out at nothing. A day with anything spent at all keeps 2px, so a quiet day reads as quiet rather than as a day the daemon was off.
              const share = max > 0 ? Math.max(d.total_tokens ? 2 : 0, Math.round(((d.total_tokens || 0) / max) * 72)) : 0;
              return (
                <div key={d.day} className="flex flex-1 flex-col items-center gap-1.5" title={`${d.day}: ${tokens(d.total_tokens)} tokens over ${tokens(d.calls)} calls`}>
                  <span className="text-micro text-muted-foreground">{compact(d.total_tokens)}</span>
                  <div className="flex h-[72px] w-full items-end">
                    <div className="w-full rounded-xs bg-primary/30" style={{ height: `${share}px` }} />
                  </div>
                  <span className="text-micro text-muted-foreground">{name}</span>
                </div>
              );
            })}
          </div>
        ) : null}
      </div>
      <div>
        <h3 className="mb-2 text-ui font-medium">Today</h3>
        <UsageTable window={usage.today} empty="Nothing spent today." />
      </div>
      <div>
        <h3 className="mb-2 text-ui font-medium">Last seven days</h3>
        <UsageTable window={usage.week} empty="Nothing spent this week." />
      </div>
      <SearchSpend limits={usage.limits} />
      <div>
        <h3 className="mb-2 text-ui font-medium">Recent calls</h3>
        {/* Each row is already one whole question, not one round of it: the daemon sums a turn's tokens over every round of its tool loop before it ever files a row (see TokenUsage in internal/agent/ask.go and recordTokenUse in internal/ipc/ipc.go), so In/Out/Total here are what that question cost end to end. What the daemon does not carry yet is how many rounds a question took — TurnTrace has no round counter, so that count is not drawn here rather than guessed from tool-call counts, which would undercount a round that called more than one tool. The bar behind Total is that row's share of the priciest row in this list, so an expensive question is the tall bar rather than a number a person has to read every row to compare. */}
        {usage.recent?.length ? (
          // Fixed layout gives every column the share of the table's width its header names below, so a long question wraps onto more lines in its own cell instead of stretching the table and squeezing In/Out/Total/Took into each other — a real question can run to a full sentence, not the short fixture text auto layout is normally proven against.
          <div className="overflow-x-auto">
            <table className="w-full min-w-[30rem] table-fixed text-ui">
              <thead>
                <tr className="border-b text-meta text-muted-foreground">
                  <th className="w-14 py-1.5 text-left font-normal">Time</th>
                  <th className="py-1.5 text-left font-normal">Provider</th>
                  <th className="w-20 py-1.5 text-right font-normal">In</th>
                  <th className="w-20 py-1.5 text-right font-normal">Out</th>
                  <th className="w-20 py-1.5 text-right font-normal">Total</th>
                  <th className="w-14 py-1.5 text-right font-normal">Took</th>
                </tr>
              </thead>
              <tbody>
                {usage.recent.map((c) => {
                  const share = maxRecent > 0 ? Math.round(((c.total_tokens || 0) / maxRecent) * 100) : 0;
                  return (
                    <tr key={c.id} className="border-b align-top">
                      <td className="py-1.5 tabular-nums text-muted-foreground">{hhmm(c.when)}</td>
                      <td className="break-words py-1.5">
                        <span className="font-medium">{c.provider}</span> <span className="text-muted-foreground">{[c.model && usageModel(c.provider, c.model), c.channel].filter(Boolean).join(" · ")}</span>
                        {c.question ? <div className="text-meta text-muted-foreground">{c.question}</div> : null}
                      </td>
                      <td className="py-1.5 text-right tabular-nums">{tokens(c.input_tokens)}</td>
                      <td className="py-1.5 text-right tabular-nums">{tokens(c.output_tokens)}</td>
                      <td
                        data-testid={`cost-bar-${c.id}`}
                        className="py-1.5 text-right tabular-nums"
                        style={{ backgroundImage: `linear-gradient(to left, color-mix(in srgb, var(--primary) 18%, transparent) 0 ${share}%, transparent ${share}%)` }}
                      >
                        {tokens(c.total_tokens)}
                      </td>
                      <td className="py-1.5 text-right tabular-nums text-muted-foreground">{took(c.duration_ms)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="text-ui text-muted-foreground">No calls yet.</p>
        )}
      </div>
    </div>
  );
}

/** The Settings screen. Input: none. Output: the page. */
export function SettingsScreen() {
  const dispatch = useAppDispatch();
  const theme = useAppSelector((s) => s.settings.theme);
  const { data: daemon, isError } = useSettingsQuery();
  const { data: setup } = useSetupQuery();
  const { data: brainList } = useBrainsQuery();
  const brains = brainList?.brains ?? [];
  const { data: tracker } = useTrackerQuery();
  const { data: usage } = useUsageQuery();
  const [pickBrain] = usePickBrainMutation();
  const [setCapture] = useSetCaptureMutation();
  const [saveSettings] = useSaveSettingsMutation();
  const hotkeyGone = useHotkeyUnavailable();
  const [wide, pane] = useWide();
  // Where the hover opens, kept in the localStorage key both windows share rather than on the daemon; read once on mount, same as the hover itself re-reads it on every open.
  const [hoverPosition, setHoverPosition] = useState<HoverPosition>(() => storedHoverPosition());

  const pickPosition = (v: HoverPosition) => {
    setHoverPosition(v);
    try {
      localStorage.setItem(HOVER_POSITION_KEY, v);
    } catch {
      /* storage blocked */
    }
  };

  // The daemon reads the accelerator live off gsettings; the keys the installer registers are the fallback for a daemon that reports none.
  const live = hotkeyKeys(daemon?.hotkey ?? "");
  const keys = live.length ? live : INSTALLED_HOTKEY;
  // /status is the live answer and /settings is what was true when the page was read, so the live one wins when it is there.
  const watching = tracker ? !tracker.paused : (daemon?.capture_enabled ?? false);
  const pausedUntil = tracker?.paused ? (tracker.paused_until ?? "") : "";
  const firstRun = Boolean(daemon?.first_run?.steps?.length);

  const watch = async (on: boolean) => {
    try {
      await setCapture(on).unwrap();
    } catch {
      dispatch(ui.noticed({ text: on ? "Could not start watching" : "Could not pause watching", kind: "error" }));
    }
  };

  const pickModel = async (brain: string, model: string) => {
    try {
      // default false: choosing a brain's model remembers it for that brain and leaves which brain answers alone.
      await pickBrain({ brain, model, default: false }).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not change the model", kind: "error" }));
    }
  };

  /** Writes one of the daemon's settings, and says on the rail when it did not go through; the control shows what the daemon reads back either way. */
  const save = async (change: SettingsChange) => {
    try {
      await saveSettings(change).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not change that setting", kind: "error" }));
    }
  };

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide}>
        <h1 className="text-ui font-medium">Settings</h1>
      </PageHeader>
      <Scroller bodyClassName={`${HEAD} ${TAIL}`}>
        <Reading wide={wide}>
          {/* Nothing else on this page matters while June cannot answer, so the ways to fix it go above the first group and disappear on their own once the daemon reports nothing left to set up. */}
          {firstRun ? (
            <div className="mb-8">
              <FirstRunPanel />
            </div>
          ) : null}
          <section>
            <SectionHeading>This window</SectionHeading>
            <Group>
              <div className="divide-y">
                <Row label="Look">
                  {/* Manual activation: an arrow key moves along the three without changing the window's colours until Enter or Space picks one. */}
                  <Tabs value={theme} activationMode="manual" onValueChange={(v) => dispatch(settingsUi.themePicked(v as Theme))}>
                    <TabsList aria-label="Look">
                      <TabsTrigger value="light">Light</TabsTrigger>
                      <TabsTrigger value="dark">Dark</TabsTrigger>
                      <TabsTrigger value="system">System</TabsTrigger>
                    </TabsList>
                  </Tabs>
                </Row>
                <Row label="Position" hint="where the hover opens on screen">
                  <Tabs value={hoverPosition} activationMode="manual" onValueChange={(v) => pickPosition(v as HoverPosition)}>
                    <TabsList aria-label="Position">
                      <TabsTrigger value="top">Top</TabsTrigger>
                      <TabsTrigger value="center">Center</TabsTrigger>
                      <TabsTrigger value="bottom">Bottom</TabsTrigger>
                    </TabsList>
                  </Tabs>
                </Row>
                {/* Another program holding the keys leaves them doing nothing, so the row says so, and where June opens from instead, rather than drawing keys that will not work. */}
                <Row label="Hotkey" hint={hotkeyGone ? NO_SHORTCUT : "the shortcut that opens this window"}>
                  <div className={`flex gap-1 ${hotkeyGone ? "line-through opacity-60" : ""}`}>
                    {keys.map((k) => (
                      <kbd key={k} className="rounded-xs border border-hairline-strong px-1.5 py-0.5 text-micro text-muted-foreground uppercase">
                        {k}
                      </kbd>
                    ))}
                  </div>
                </Row>
                {setup ? (
                  <Row label={ON_WINDOWS ? "Start with Windows" : "Start when you log in"} hint="June opens in the background when you sign in, so it is always keeping notes">
                    <AutostartSwitch on={setup.autostart} label={ON_WINDOWS ? "Start with Windows" : "Start when you log in"} />
                  </Row>
                ) : null}
                <Row label="Watching the screen" hint={pausedUntil ? `Paused until ${hhmm(pausedUntil)} — it starts again by itself` : "what June sees is what it can remember"}>
                  <Switch checked={watching} aria-label="Watching the screen" onCheckedChange={(on) => void watch(on)} />
                </Row>
                {daemon?.meetings_offer ? (
                  // The daemon starts its call watcher only when offering was on as it started (cmd/daemon.go), and meetings_enabled says whether it is running; Ask to Off holds at once, Off to Ask only from the next start, so the row says so rather than looking as if it took.
                  <Row
                    label="Offer to record calls"
                    hint={
                      daemon.meetings_offer === "ask" && !daemon.meetings_enabled
                        ? "June starts asking the next time it starts — after a restart, or when you next sign in"
                        : "when another app uses the microphone for a while, June asks whether to record the call"
                    }
                  >
                    <Tabs value={daemon.meetings_offer} activationMode="manual" onValueChange={(v) => void save({ meetings_offer: v === "off" ? "off" : "ask" })}>
                      <TabsList aria-label="Offer to record calls">
                        <TabsTrigger value="ask">Ask</TabsTrigger>
                        <TabsTrigger value="off">Off</TabsTrigger>
                      </TabsList>
                    </Tabs>
                  </Row>
                ) : (
                  <Row label="Recording meetings" hint="whether June offers to record your calls">
                    <span className="text-ui text-muted-foreground">{daemon?.meetings_enabled ? "On" : "Off"}</span>
                  </Row>
                )}
              </div>
            </Group>
          </section>

          {/* The id is where an answer that failed for want of a key or a login sends the person. */}
          <section id="brain" className="mt-10 scroll-mt-8">
            <SectionHeading>Brain</SectionHeading>
            <Group>
              {/* The panel at the top already holds the key box while nothing can answer, and two boxes for one key would ask which one counts. A daemon too old to answer GET /setup cannot take a key from here at all. */}
              {setup && !firstRun ? (
                <div className="border-b px-3.5 py-3">
                  <GeminiKey saved={setup.gemini_key} manage />
                </div>
              ) : null}
              <div className="border-b">
                <Row label="Show Claude plan usage" hint="Reads your Claude Code login's usage from an undocumented Anthropic endpoint. Turn off if you would rather it did not.">
                  <Switch
                    checked={daemon?.claude_usage_from_login ?? true}
                    aria-label="Show Claude plan usage"
                    onCheckedChange={(on) => void save({ claude_usage_from_login: on })}
                  />
                </Row>
              </div>
              {daemon?.allow_fallback !== undefined ? (
                <div className="border-b">
                  {/* The daemon reads this for background work only (agent.DutyFallbackAllowed); a question you ask is still handed on when the AI you picked is out of allowance or signed out, so the hint names what the switch covers and promises nothing about questions. */}
                  <Row label="If your chosen AI can't answer, try my other signed-in AIs" hint="Covers June's background work: tidying its notes and writing up meetings. On, that can use up the allowance on your other plans; off, only the AI you picked does it.">
                    <Switch checked={daemon.allow_fallback} aria-label="If your chosen AI can't answer, try my other signed-in AIs" onCheckedChange={(on) => void save({ allow_fallback: on })} />
                  </Row>
                </div>
              ) : null}
              {brains.length > 0 ? (
                <div className="border-b">
                  {/* The same control as the header of Chats and Tasks, so the way back to letting June pick is in Settings too and not only in a chat's header. */}
                  <Row label="Default brain" hint="what a chat that names none is answered by">
                    <BrainPicker current="" brains={brains} automatic={brainList?.automatic} />
                  </Row>
                </div>
              ) : null}
              {brains.length === 0 ? (
                <p className="px-3.5 py-3 text-ui text-muted-foreground">{isError ? "Not connected." : "No brains reported."}</p>
              ) : (
                <div className="divide-y">
                  {brains.map((b) => (
                    <div key={b.id} className="px-3.5 py-3">
                      <div className="flex items-baseline justify-between gap-4">
                        <span className="text-ui font-medium">{b.name}</span>
                        <span className={`text-meta ${b.signed_in ? "text-muted-foreground" : "text-destructive"}`}>
                          {b.signed_in ? (b.account ? `signed in · ${b.account}` : "signed in") : "not signed in"}
                        </span>
                      </div>
                      {b.signed_in ? (
                        <div className="mt-2 flex flex-wrap gap-1">
                          {(b.models ?? []).length === 0 ? (
                            <span className="text-meta text-muted-foreground">no model choice exposed</span>
                          ) : (
                            // "" leads the row as "default model": a brain nobody pinned a model for runs on its own default, which is the chip lit for it, and pressing it is the way back to that after pinning one. Lighting the first listed model instead claimed a pin that was never made.
                            ["", ...b.models].map((m) => (
                              <button
                                key={m || "default"}
                                type="button"
                                aria-pressed={m === b.model}
                                onClick={() => void pickModel(b.id, m)}
                                className={`rounded-full px-2.5 py-1 text-meta outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring ${m === b.model ? "bg-primary/15 text-foreground" : "bg-muted text-muted-foreground hover:bg-hover hover:text-foreground"}`}
                              >
                                {m ? modelEffort(m).model : "default model"}
                                {/* Antigravity has no effort setting of its own: it publishes one model id per effort, so the suffix is the dial and is drawn as its own word rather than buried in the id. */}
                                {m && modelEffort(m).effort ? (
                                  <span className="ml-1 opacity-60">· {modelEffort(m).effort}</span>
                                ) : null}
                              </button>
                            ))
                          )}
                        </div>
                      ) : null}
                      {BRAIN_NOTES[b.id] ? <p className="mt-1.5 text-meta text-muted-foreground">{BRAIN_NOTES[b.id]}</p> : null}
                    </div>
                  ))}
                </div>
              )}
            </Group>
          </section>

          <VoiceSection geminiKey={setup?.gemini_key} />

          {/* The id is where the composer's "Set it up" for voice typing lands. */}
          <section id="local-features" className="mt-10 scroll-mt-8">
            <SectionHeading aside="run on this PC, not online">Local features</SectionHeading>
            <FeatureList manage />
          </section>

          {daemon ? <Machine s={daemon} /> : null}

          <section className="mt-10">
            <SectionHeading>About June</SectionHeading>
            <Group>
              <div className="divide-y">
                <Row label="Version" hint={daemon?.update_check === false ? "automatic checks are off; Check now still asks" : "June looks for a newer one once a day"}>
                  <UpdateStatus version={setup?.version || daemon?.version || ""} />
                </Row>
                {daemon?.update_check !== undefined ? (
                  <Row label="Check for updates" hint="once a day, June asks GitHub whether a newer version is out">
                    <Switch checked={daemon.update_check} aria-label="Check for updates" onCheckedChange={(on) => void save({ update_check: on })} />
                  </Row>
                ) : null}
              </div>
            </Group>
          </section>

          {/* The ledger is the last section of this page rather than a destination of its own, and it opens on the figures and the week's bars rather than on a table. */}
          <section className="mt-10">
            <SectionHeading aside="counts, not prices — the daemon reports no money">Token use</SectionHeading>
            <UsageLedger usage={usage} up={!isError} />
          </section>
        </Reading>
      </Scroller>
    </div>
  );
}
