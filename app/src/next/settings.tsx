/** The Settings screen: how the window looks, where the hover opens, the hotkey that opens it, what the daemon is allowed to watch, which brain answers and on which model, what the daemon is running on this machine, and the token ledger. Every value is read off GET /settings, GET /brains, GET /status and GET /usage, and every control writes through the route that owns the setting, except the theme and the hover position, which the window and the hover share through localStorage rather than through the daemon (see HOVER_POSITION_KEY in src/winplace.ts). The page is a stack of grouped cards, the way a desktop settings pane is built, rather than a run of rows under grey capitals. */

import { Fragment, useState } from "react";
import { RotateCcw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { HOVER_POSITION_KEY, storedHoverPosition, type HoverPosition } from "../winplace";
import { useBrainsQuery, usePickBrainMutation, useSetCaptureMutation, useSetClaudeUsageFromLoginMutation, useSettingsQuery, useTrackerQuery, useUsageQuery, type SettingsView, type Usage, type UsageWindow } from "./api";
import { bytes, cachedInput, compact, hhmm, hotkeyKeys, perQuestion, tokens, took } from "./format";
import { Blank, Group, HEAD, PageHeader, Reading, Scroller, SectionHeading, TAIL, useWide } from "./parts";
import { settings as settingsUi, ui, useAppDispatch, useAppSelector, type Theme } from "./store";

/** The keys the installer registers, drawn when the daemon reports no accelerator of its own. */
const INSTALLED_HOTKEY = ["Ctrl", "Alt", "Space"];

/** What to do before Ora can answer at all, drawn only while the daemon still says none of the four ways of answering text is set up. Input: none — it reads GET /settings itself, so it can sit at the top of Settings and inside the empty Chats pane without either of them passing it anything. Output: the panel, or nothing once the daemon reports an empty step list. "Check again" re-reads /settings, which is what a person does after running one of the commands in another window.
 *
 * The steps are the daemon's own sentences and are shown verbatim as a real list; the window does not restate them, because then there would be two places to fix when a step changes.
 */
export function FirstRunPanel() {
  const { data: daemon, refetch, isFetching } = useSettingsQuery();
  const steps = daemon?.first_run?.steps ?? [];
  if (!steps.length) return null;
  return (
    <Group className="border-work/40 bg-work/5">
      <div className="px-4 py-3.5">
        <h2 className="text-doc text-foreground">Ora cannot answer yet</h2>
        <p className="mt-1.5 text-read text-muted-foreground">It needs one way to answer text. Any one of these is enough.</p>
        <ul className="mt-3 list-disc pl-5 text-read marker:text-muted-foreground">
          {steps.map((s) => (
            <li key={s} className="mt-1.5">
              {s}
            </li>
          ))}
        </ul>
        <Button variant="outline" size="sm" className="mt-4" disabled={isFetching} onClick={() => void refetch()}>
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

/** One label-and-value line in the machine block. Input: the label and the value. Output: the line, or nothing when there is no value, so a field the daemon could not fill draws no row. */
function Fact({ label, value }: { label: string; value: string }) {
  if (!value) return null;
  return (
    <div className="flex justify-between gap-6 px-3.5 py-2 text-ui">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className="max-w-[62%] truncate text-right" title={value}>
        {value}
      </span>
    </div>
  );
}

/** What the daemon is running and what it has written down. Input: the /settings answer. Output: the block. */
function Machine({ s }: { s: SettingsView }) {
  const started = new Date(s.daemon_started);
  return (
    <section className="mt-10">
      <SectionHeading>This machine</SectionHeading>
      <Group>
        <div className="divide-y">
          <Fact label="Answers with" value={s.brain} />
          <Fact label="Understands meaning with" value={s.embed_model === "none" ? "nothing — search is words only" : s.embed_model} />
          <Fact label="Hears with" value={s.voice_model} />
          <Fact label="Everything is kept in" value={s.data_dir} />
          <Fact label="What it has written" value={`${bytes(s.store_bytes)} of memory · ${bytes(s.recordings_bytes)} of audio · ${bytes(s.models_bytes)} of models`} />
          <Fact label="Audio kept for" value={s.keep_audio_days < 0 ? "as long as you leave it there" : `${s.keep_audio_days} days`} />
          <Fact label="Running since" value={Number.isNaN(started.getTime()) ? "" : `${hhmm(s.daemon_started)}, build ${s.version}`} />
        </div>
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
      <div>
        <h3 className="mb-2 text-ui font-medium">Recent calls</h3>
        {/* Each row is already one whole question, not one round of it: the daemon sums a turn's tokens over every round of its tool loop before it ever files a row (see TokenUsage in internal/agent/ask.go and recordTokenUse in internal/ipc/ipc.go), so In/Out/Total here are what that question cost end to end. What the daemon does not carry yet is how many rounds a question took — TurnTrace has no round counter, so that count is not drawn here rather than guessed from tool-call counts, which would undercount a round that called more than one tool. The bar behind Total is that row's share of the priciest row in this list, so an expensive question is the tall bar rather than a number a person has to read every row to compare. */}
        {usage.recent?.length ? (
          // Fixed layout gives every column the share of the table's width its header names below, so a long question is capped by that share and truncates in its own cell instead of stretching the table and squeezing In/Out/Total/Took into each other — a real question can run to a full sentence, not the short fixture text auto layout is normally proven against.
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
                      <td className="truncate py-1.5">
                        <span className="font-medium">{c.provider}</span> <span className="text-muted-foreground">{[c.model, c.channel].filter(Boolean).join(" · ")}</span>
                        {c.question ? (
                          <div className="truncate text-meta text-muted-foreground" title={c.question}>
                            {c.question}
                          </div>
                        ) : null}
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
      dispatch(ui.noticed(on ? "Could not start watching" : "Could not pause watching"));
    }
  };

  const pickModel = async (brain: string, model: string) => {
    try {
      await pickBrain({ brain, model }).unwrap();
    } catch {
      dispatch(ui.noticed("Could not change the model"));
    }
  };

  const toggleClaudeUsage = async (on: boolean) => {
    try {
      await setClaudeUsageFromLogin(on).unwrap();
    } catch {
      dispatch(ui.noticed("Could not change that setting"));
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
                    <kbd key={k} className="rounded-xs border bg-muted px-1.5 py-0.5 text-micro text-muted-foreground uppercase">
                      {k}
                    </kbd>
                  ))}
                </div>
              </Row>
              <Row label="Watching the screen" hint="what Ora sees is what it can remember">
                <Switch checked={watching} aria-label="Watching the screen" onCheckedChange={(on) => void watch(on)} />
              </Row>
              <Row label="Recording meetings" hint="turned on in the config file, not from here">
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span>
                      <Switch checked={daemon?.meetings_enabled ?? false} disabled aria-label="Recording meetings" />
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="left">The daemon reads this from its config; there is no route to change it from the window.</TooltipContent>
                </Tooltip>
              </Row>
            </div>
          </Group>

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
                                {m}
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
