/** The Settings screen: how the window looks, where the hover opens, the hotkey that opens it, what the daemon is allowed to watch, which brain answers and on which model, what the daemon is running on this machine, and the token ledger. Every value is read off GET /settings, GET /brains, GET /status and GET /usage, and every control writes through the route that owns the setting, except the theme and the hover position, which the window and the hover share through localStorage rather than through the daemon (see HOVER_POSITION_KEY in src/winplace.ts). The page is a stack of grouped cards, the way a desktop settings pane is built, rather than a run of rows under grey capitals. */

import { Fragment, useState } from "react";
import { Check, ChevronDown, Loader2, Play, RotateCcw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Switch } from "@/components/ui/switch";
import { HOVER_POSITION_KEY, storedHoverPosition, type HoverPosition } from "../winplace";
import {
  errorStatus,
  useBrainsQuery,
  usePickBrainMutation,
  usePreviewVoiceMutation,
  useSetCaptureMutation,
  useSetClaudeUsageFromLoginMutation,
  useSetLiveModelMutation,
  useSetVoiceMutation,
  useSettingsQuery,
  useTrackerQuery,
  useUsageQuery,
  useVoicesQuery,
  SEARCH_PROVIDERS,
  type LiveModel,
  type ProviderLimits,
  type SettingsView,
  type Usage,
  type UsageWindow,
  type Voice,
} from "./api";
import { bytes, cachedInput, compact, hhmm, hotkeyKeys, modelEffort, perQuestion, tokens, took } from "./format";
import { Blank, Group, HEAD, PageHeader, Reading, Scroller, SectionHeading, TAIL, useWide } from "./parts";
import { settings as settingsUi, ui, useAppDispatch, useAppSelector, type Theme } from "./store";

/** The keys the installer registers, drawn when the daemon reports no accelerator of its own. */
const INSTALLED_HOTKEY = ["Ctrl", "Alt", "Space"];

/** A machine value inside an otherwise plain sentence: an ALL_CAPS environment variable, a path starting with ~/ or /, or a `claude login`-style command. Three client-side rules rather than markup the daemon would have to start sending, since the steps are its own sentences read verbatim. */
const CODE_TOKEN = /\b[A-Z][A-Z_]{3,}\b|(?:~\/|\/)\S*[^\s.,;:]|\bclaude \w+\b/g;

/** Wraps the machine-specific tokens in a sentence — an env var, a path, a command — in <code>, so a person can tell what to type apart from the sentence telling them to type it. Input: the sentence. Output: the same text, split around each match, with the match itself in a <code> element. */
function codeSpans(text: string): React.ReactNode[] {
  const out: React.ReactNode[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  const re = new RegExp(CODE_TOKEN);
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(
      <code key={m.index} className="font-mono text-[0.92em]">
        {m[0]}
      </code>,
    );
    last = m.index + m[0].length;
  }
  out.push(text.slice(last));
  return out;
}

/** What to do before Ora can answer at all, drawn only while the daemon still says none of the four ways of answering text is set up. Input: none — it reads GET /settings itself, so it can sit at the top of Settings and inside the empty Chats pane without either of them passing it anything. Output: the panel, or nothing once the daemon reports an empty step list. "Check again" re-reads /settings, which is what a person does after running one of the commands in another window.
 *
 * The steps are the daemon's own sentences and are shown verbatim as a real list; the window does not restate them, because then there would be two places to fix when a step changes.
 */
export function FirstRunPanel() {
  const { data: daemon, refetch, isFetching } = useSettingsQuery();
  const steps = daemon?.first_run?.steps ?? [];
  if (!steps.length) return null;
  return <SetUpCard lead="Ora needs a model before it can answer." ways={steps.map(codeSpans)} checking={isFetching} onCheck={() => void refetch()} />;
}

/** The chat page's card while GET /brains lists no brain signed in: every way Ora can be given one, since it works with whichever the user already has. Input: the daemon's data directory, whose env file is the one every ora command reads (loadEnvFiles in cmd/root.go). Output: the card. "Check again" re-reads /brains; a key added to the env file is only read when the daemon starts, which is why that line says to restart Ora. */
export function NoBrainPanel({ dataDir }: { dataDir: string }) {
  const { refetch, isFetching } = useBrainsQuery();
  const ways = [
    <>Claude Code: run <code>claude</code>, then type <code>/login</code>.</>,
    <>Codex: run <code>codex login</code>.</>,
    <>Antigravity: run <code>agy</code> and sign in.</>,
    <>Grok: run <code>grok</code> and sign in.</>,
    <>Gemini: put <code>GEMINI_API_KEY=your-key</code> in <code>{`${dataDir}/env`}</code>, then restart Ora.</>,
  ];
  return <SetUpCard lead="No brain is signed in on this machine." ways={ways} checking={isFetching} onCheck={() => void refetch()} />;
}

/** The card both set-up panels draw. Input: the sentence under the heading, the ways to fix it, whether a re-read is in flight, and what "Check again" does. Output: the card. */
function SetUpCard({ lead, ways, checking, onCheck }: { lead: string; ways: React.ReactNode[]; checking: boolean; onCheck: () => void }) {
  return (
    <Group>
      <div className="px-4 py-3.5">
        <h2 className="text-doc text-foreground">Ora cannot answer yet</h2>
        <p className="mt-1.5 text-read text-muted-foreground">{lead} Any one of these will do.</p>
        <ul className="mt-3 list-disc pl-5 text-read marker:text-muted-foreground [&_code]:font-mono [&_code]:text-[0.92em]">
          {ways.map((w, i) => (
            <li key={i} className="mt-1.5">
              {w}
            </li>
          ))}
        </ul>
        <Button size="sm" className="mt-4" disabled={checking} onClick={onCheck}>
          <RotateCcw /> Check again
        </Button>
      </div>
    </Group>
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
          <Fact label="Speech to text" hint="hears with" value={s.voice_model} mono />
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

/** The voice picker: a single select showing the current voice and the trait that says how it sounds, with every one of Gemini Live's thirty prebuilt voices behind it the same way, and a play button beside it that previews whichever voice is currently picked. Input: none — it reads GET /voices itself. Output: the section. */
function VoiceSection() {
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
      // The daemon says exactly what went wrong — a spent daily allowance, a line the model refused, no speaker — and "Could not play that voice" threw all three away. It is shown as it came, with the generic line kept only for a failure that carried no words of its own.
      dispatch(ui.noticed({ text: errorStatus(e) === 503 ? "This machine has no speaker to play it through" : errorSentence(e) || "Could not play that voice", kind: "error" }));
    }
  };

  return (
    <section className="mt-10">
      <SectionHeading>Voice</SectionHeading>
      <p className="mb-3 text-meta text-muted-foreground">Heard the next time a live voice session starts, not the one already running — the daemon reads this when it dials.</p>
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
                    <td className="py-1.5 pl-4">{m.model}</td>
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
  if (!spent) return <Blank up={up} empty="Nothing asked yet" hint="No tokens have been spent on this machine. Ask Ora something and what it cost appears here." />;
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
                        <span className="font-medium">{c.provider}</span> <span className="text-muted-foreground">{[c.model, c.channel].filter(Boolean).join(" · ")}</span>
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
  const { data: brains = [] } = useBrainsQuery();
  const { data: tracker } = useTrackerQuery();
  const { data: usage } = useUsageQuery();
  const [pickBrain] = usePickBrainMutation();
  const [setCapture] = useSetCaptureMutation();
  const [setClaudeUsageFromLogin] = useSetClaudeUsageFromLoginMutation();
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

  const toggleClaudeUsage = async (on: boolean) => {
    try {
      await setClaudeUsageFromLogin(on).unwrap();
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
          {/* Nothing else on this page matters while Ora cannot answer, so the steps go above the first group and disappear on their own once the daemon reports none left. */}
          {daemon?.first_run?.steps?.length ? (
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
                <Row label="Hotkey" hint="the GNOME shortcut that opens this window">
                  <div className="flex gap-1">
                    {keys.map((k) => (
                      <kbd key={k} className="rounded-xs border border-hairline-strong px-1.5 py-0.5 text-micro text-muted-foreground uppercase">
                        {k}
                      </kbd>
                    ))}
                  </div>
                </Row>
                <Row label="Watching the screen" hint="what Ora sees is what it can remember">
                  <Switch checked={watching} aria-label="Watching the screen" onCheckedChange={(on) => void watch(on)} />
                </Row>
                <Row label="Recording meetings" hint="turned on in the config file, not from here">
                  <span className="text-ui text-muted-foreground">{daemon?.meetings_enabled ? "On" : "Off"}</span>
                </Row>
              </div>
            </Group>
          </section>

          <section className="mt-10">
            <SectionHeading>Brain</SectionHeading>
            <Group>
              <div className="border-b">
                <Row label="Show Claude plan usage" hint="Reads your Claude Code login's usage from an undocumented Anthropic endpoint. Turn off if you would rather it did not.">
                  <Switch
                    checked={daemon?.claude_usage_from_login ?? true}
                    aria-label="Show Claude plan usage"
                    onCheckedChange={(on) => void toggleClaudeUsage(on)}
                  />
                </Row>
              </div>
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
                            b.models.map((m) => (
                              <button
                                key={m}
                                type="button"
                                aria-pressed={m === (b.model || b.models[0])}
                                onClick={() => void pickModel(b.id, m)}
                                className={`rounded-full px-2.5 py-1 text-meta outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring ${m === (b.model || b.models[0]) ? "bg-primary/15 text-foreground" : "bg-muted text-muted-foreground hover:bg-hover hover:text-foreground"}`}
                              >
                                {modelEffort(m).model}
                                {/* Antigravity has no effort setting of its own: it publishes one model id per effort, so the suffix is the dial and is drawn as its own word rather than buried in the id. */}
                                {modelEffort(m).effort ? (
                                  <span className="ml-1 opacity-60">· {modelEffort(m).effort}</span>
                                ) : null}
                              </button>
                            ))
                          )}
                        </div>
                      ) : null}
                      {b.note ? <p className="mt-1.5 text-meta text-muted-foreground">{b.note}</p> : null}
                    </div>
                  ))}
                </div>
              )}
            </Group>
          </section>

          <VoiceSection />

          {daemon ? <Machine s={daemon} /> : null}

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
