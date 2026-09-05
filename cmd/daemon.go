package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"ora/internal/act"
	"ora/internal/actjob"
	"ora/internal/agent"
	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/dream"
	"ora/internal/embed"
	"ora/internal/ipc"
	"ora/internal/ipctoken"
	"ora/internal/memory"
	"ora/internal/proactive"
	"ora/internal/recorder"
	"ora/internal/study"
	"ora/internal/tally"
	"ora/internal/tracker"
	"ora/internal/vector"
	"ora/internal/window"
)

// meetingRecorder is the tray's handle on the meeting recorder. startDaemonServices assigns it once the store exists, before registerSNI runs; it stays nil if the daemon never got that far, and every read of it is nil-safe.
var meetingRecorder *recorder.Recorder

// DaemonPort is the loopback port the daemon binds and every client in this package dials. Overridable via ORA_PORT so a second daemon (a test, a dry run) can run beside the live one without fighting it for the port. Defaults to 6942.
var DaemonPort = func() string {
	if p := os.Getenv("ORA_PORT"); p != "" {
		return p
	}
	return "6942"
}()

// pingHandler answers with this process's build identity — the client compares it against its own to detect a daemon that's been running since before the most recent rebuild (see checkDaemonBuildMismatch in root.go). Extracted as a named function so it's testable in isolation from the rest of the daemon's mux.
func pingHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buildIdentity))
}

// maxDeriveStateNotes bounds how many relevance-ranked notes feed the 5-minute working-state derive, instead of the full notes table.
const maxDeriveStateNotes = 10

// maxDeriveStateEpisodes bounds how many tracked episodes the change gate reads to build its app-and-window-title signature. Two hundred: this store logged at most 60 episodes in its busiest hour on 2026-09-04, so it covers well over the ten-minute window the gate compares across, and the signature is a set so reading extra rows only costs the query.
const maxDeriveStateEpisodes = 200

// codexFallbackBrain holds the hand-over brain the unattended jobs use when Gemini answers 429 or 503. It is published once the daemon has built the ask agent, which happens after those jobs are wired, so it is read through an atomic rather than captured directly.
var codexFallbackBrain atomic.Pointer[brain.Brain]

// publishCodexFallback makes b the brain every unattended job hands over to on a quota or overload failure. Input: the Codex-backed brain; called once during startup.
func publishCodexFallback(b brain.Brain) {
	codexFallbackBrain.Store(&b)
}

// backgroundFallbackBrain returns the hand-over brain the unattended jobs should use, resolved at call time. Output: a Brain that answers through Codex once one has been published, and fails with a clear message before that.
func backgroundFallbackBrain() brain.Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		b := codexFallbackBrain.Load()
		if b == nil {
			return "", fmt.Errorf("codex fallback is not wired yet")
		}
		return (*b)(ctx, prompt)
	}
}

// geminiRequestGate adapts brain.WithDailyQuota's Brain-shaped daily quota check into the Allow(model string) error shape memory.GeminiSummarizer's request gate expects, so its direct genai calls are metered against the same shared daily count as every Brain-wrapped call site above. The wrapped primary is a no-op: WithDailyQuota calls it only once its own quota check has already passed, so by the time it runs the count has already advanced and there is nothing left to do.
type geminiRequestGate struct {
	state *brain.QuotaState
	opts  brain.QuotaOptions
	// forAsks marks the interactive band, which may spend the share the quota keeps back from the nightly jobs.
	forAsks bool
}

// Allow reports whether a request against model may proceed in this gate's band: background like every unattended job, or interactive when forAsks is set.
func (g *geminiRequestGate) Allow(model string) error {
	noop := func(context.Context, string) (string, error) { return "", nil }
	_, err := brain.WithDailyQuota(g.state, model, g.forAsks, g.opts, noop)(context.Background(), "")
	return err
}

// reconcileEmbedCap bounds how many backfill embeds one ReconcileVectors sweep performs, to protect API quota on a large dirty store — the sweep runs again on the next trigger (startup / note consolidation) and picks up where it left off.
const reconcileEmbedCap = 200

// localReconcileEmbedCap is the same bound when the embedder is the local llama-server rather than a metered API. The cap exists to protect a quota; with a free embedder there is no quota to protect, and a small cap only means a backlog that never drains. Chunking made that backlog real — a store of 4,866 captures needs about 8,800 passage vectors, so at 200 a sweep it would take dozens of restarts to catch up.
const localReconcileEmbedCap = 5000

// reconcileCap picks the sweep's budget from whether embedding costs money.
func reconcileCap(embedsFree bool) int {
	if embedsFree {
		return localReconcileEmbedCap
	}
	return reconcileEmbedCap
}

// embedderAdapter adapts an embed.Embedder's Embed (which takes embed.TaskType) to the plain-string task param db.Store.SetEmbedder expects.
// internal/db can't import internal/embed, so the adapter lives here instead.
type embedderAdapter struct {
	inner embed.Embedder
}

func (e *embedderAdapter) Embed(ctx context.Context, task string, text string) ([]float32, error) {
	return e.inner.Embed(ctx, embed.TaskType(task), text)
}

// vectorIndexAdapter adapts *vector.ChromemIndex to db.Store.SetVectorIndex, translating vector.Result into db.Result field by field.
type vectorIndexAdapter struct {
	inner *vector.ChromemIndex
}

func (v *vectorIndexAdapter) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	return v.inner.Add(ctx, id, content, embedding, metadata)
}

func (v *vectorIndexAdapter) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	results, err := v.inner.Search(ctx, queryEmbedding, n, where)
	if err != nil {
		return nil, err
	}
	out := make([]db.Result, len(results))
	for i, r := range results {
		out[i] = db.Result{
			ID:         r.ID,
			Content:    r.Content,
			Metadata:   r.Metadata,
			Similarity: r.Similarity,
		}
	}
	return out, nil
}

func (v *vectorIndexAdapter) Delete(ctx context.Context, id string) error {
	return v.inner.Delete(ctx, id)
}

func (v *vectorIndexAdapter) IDs() []string { return v.inner.IDs() }

// brainProviderName names a configured brain for the tally counters (see internal/tally.Wrap): the config's own provider constant when it's one of the recognised CLIs, "gemini" for the default/empty/explicit-Gemini-API case.
func brainProviderName(cfg config.BrainConfig) string {
	switch cfg.Provider {
	case config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI:
		return cfg.Provider
	default:
		return "gemini"
	}
}

// every runs fn on a ticker every interval until ctx is done — the ticker/select/ctx.Done skeleton every one of the daemon's background jobs otherwise repeated by hand. Each job's own logging/error-handling stays inside its fn closure; name is only for the stop-log line below.
func every(ctx context.Context, interval time.Duration, name string, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fn()
		case <-ctx.Done():
			slog.Debug("background job stopped", "job", name)
			return
		}
	}
}

// closeWithin runs one shutdown step and comes back at the bound whether or not the step finished, so a step that hangs delays the daemon's exit by that bound and no more. Input: the step's name for the log line, how long it may take, and the step itself. Output: none — a step still running when its bound passes is left behind and logged, which is the right trade at shutdown: the process is about to exit and the alternative is a daemon that never releases its port.
func closeWithin(name string, bound time.Duration, step func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		step()
	}()
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		slog.Warn("a shutdown step did not finish in time and was left behind", "step", name, "bound", bound)
	}
}

func runDaemon(ctx context.Context, shutdownObs func(context.Context) error) error {
	slog.Info("Starting Ora Daemon...")

	// port binding instance lock to prevent double spawning
	listener, err := net.Listen("tcp", "127.0.0.1:"+DaemonPort)
	if err != nil {
		slog.Warn("failed to bind daemon port", "port", DaemonPort, "error", err)
		return nil
	}

	runDaemonSupervisor(ctx, listener)
	return nil
}

// startDaemonServices wires up all background services and the IPC HTTP server.
// The caller owns listener lifetime; on error the caller is responsible for closing it.
// The returned stop func shuts down the server and closes the db — safe to call once.
// The returned *tracker.Daemon allows the tray to pause/resume tracking.
func startDaemonServices(ctx context.Context, listener net.Listener) (stop func(), daemonOut *tracker.Daemon, err error) {
	startTime := time.Now()
	store, err := db.New(filepath.Join(config.DataDir(), "db"))
	if err != nil {
		slog.Error("failed to init db", "error", err)
		return nil, nil, err
	}

	// The query_store tool's description carries the store's own schema and the vocabulary of every column that holds only a handful of values. It is read here rather than written into the tool, because a written one goes stale silently: the hand-written version claimed notes.kind included "action_item" when the real value is "action", and a query counting open action items returned zero against twenty-five real ones.
	if schema, err := store.DescribeSchema(ctx); err != nil {
		slog.Warn("could not read the store's schema for the query tool", "error", err)
	} else {
		agent.SetStoreSchema(schema)
	}

	trackerImpl, err := tracker.New()
	if err != nil {
		slog.Error("failed to init tracker", "error", err)
		store.Close()
		return nil, nil, err
	}

	appConfig := config.LoadConfig()

	// Every unattended job reads its model through config.BackgroundModel, so the choice lives in the config file rather than in each call site. Without this they all run on DefaultBackgroundModel.
	config.SetBackgroundModels(appConfig.BackgroundModels)

	// The config file is the switch for start-on-login: make the on-disk login entry agree with it on every daemon start, so a config edited by hand (or an entry left behind by an older build) is corrected here rather than drifting.
	reconcileAutostart(appConfig.Autostart)

	// Names the tray mark as the desktop window's dock icon (GNOME otherwise shows a generic gear); never fatal.
	if err := installDesktopEntry(); err != nil {
		slog.Warn("failed to install desktop entry", "error", err)
	}

	apiKey := os.Getenv("GEMINI_API_KEY")

	// geminiQuota is the free-tier daily request ceiling shared by every Gemini-routed brain in this function — the summarizer's own direct calls below, plus evening close/morning brief and dream further down — persisted under the data dir so it survives a daemon restart; see internal/brain/quota.go. None of these are the interactive ask (that path lives in internal/agent and calls Gemini directly, bypassing this package), so everything here is metered as background, not asks.
	geminiQuota := brain.NewQuotaState(config.DataDir())
	geminiQuotaOpts := brain.DefaultQuotaOptions()

	// brainUsage keeps what each provider says about the user's own allowance, so the brain picker can draw a bar per brain. Codex fills it from the rate-limit headers of every response Ora already makes; Claude is read from its OAuth usage endpoint when the picker is opened; Gemini's is computed from geminiQuota below. It persists under the data dir beside brain_quota.json, so the bars are there the moment the window opens after a restart.
	brainUsage := brain.NewUsageStore(config.DataDir())
	agent.SetUsageRecorder(brainUsage)

	meetingRecorder = recorder.New(config.DataDir(), store, apiKey)
	// A meeting write-up that hits a spent daily allowance is finished by Codex instead of being dropped.
	meetingRecorder.SetMinutesFallback(backgroundFallbackBrain())

	// Ora watches the microphone rather than the meeting apps: a call is the one thing that always takes it, and watching it needs no list of which applications count as a meeting.
	if appConfig.Meetings.OfferEnabled() || appConfig.Meetings.AutoRecord {
		go recorder.WatchForMeetings(ctx, meetingRecorder, appConfig.Meetings.AutoRecord)
	}

	summarizer, err := memory.NewGeminiSummarizer(apiKey)
	if err != nil {
		slog.Warn("failed to init summarizer, semantic memory disabled", "error", err)
	}
	if summarizer != nil {
		// A background summary whose Gemini call comes back 429 or 503 is handed to Codex rather than lost, the same rule the user's own asks already follow.
		summarizer.SetBackgroundFallback(memory.TextBackend(backgroundFallbackBrain()))
		// The summarizer's own direct Gemini calls (ReconcileNotes, AttributeThreads, DeriveState, ConsolidateNotes, AnalyzeScreen) count against the same shared daily cap as every other background brain here, rather than running unmetered.
		summarizer.SetRequestGate(&geminiRequestGate{state: geminiQuota, opts: geminiQuotaOpts})
		// The compiler otherwise reads the user's name off a calendar entry and files them as somebody they met.
		summarizer.SetIdentity(func(ctx context.Context) string {
			entries, err := store.PersonalContext(ctx)
			if err != nil {
				return ""
			}
			for _, e := range entries {
				if e.Subject == "identity" {
					return e.Content
				}
			}
			return ""
		})
	}
	compiler := memory.NewCompiler(summarizer, store)

	// vecIndex is nil unless the block below succeeds — declared here (not just inside the block) so the /vector/* IPC handlers further down can serve the client's hybrid search over the same index the daemon itself uses, instead of each opening chromem separately (two processes opening the same chromem dir risks torn reads/corruption).
	var vecIndex *vector.ChromemIndex

	// embedsFree says the embedder is the local llama-server rather than a metered API. Declared out here for the same reason vecIndex is: it is set inside the block below and read by the reconciliation sweeps further down, which size their budget by it.
	embedsFree := false

	// The embedding engine is the local llama-server child process, and only that: there is no API-backed embedder any more. It stays nil when no local embedder is configured, which is what the shutdown path and the /embed IPC handler key off, and means no semantic half at all — HybridSearch already falls back to lexical-only when Store has no embedder/vector index set.
	embedEngine := embed.NewEngine(appConfig.Embed)
	if embedEngine == nil {
		slog.Warn("no local embedder configured (embed.llama_server / embed.model_path), hybrid search degrades to lexical-only")
	}

	// The embedding server above answers only /v1/embeddings, so it cannot write the working state; this is a second llama-server on its own port running the instruction-tuned GGUF, spawned on the first derive and reaped when it goes idle.
	textEngine := embed.NewTextEngine(appConfig)
	if textEngine == nil {
		slog.Warn("no local text model configured (local_text.model_path / dream.model_path), the working-state derive stays on the metered API")
	} else if summarizer != nil {
		// This is what takes the five-minute working-state job off the user's free-tier daily request allowance entirely.
		summarizer.SetStateBackend(textEngine.Generate)
	}

	if embedEngine != nil {
		// A whisper GPU decode and the embedding server share one small card; when the card is short, the recorder may evict an idle embedding server (it respawns on the next embed).
		recorder.SetGPUReleaser(embedEngine.StopIfIdle)
		// 10000 = the deck's agreed pruning cap for the vector index.
		index, err := vector.NewChromemIndex(filepath.Join(config.DataDir(), "vectors"), config.LocalEmbedDim, 10000)
		if err != nil {
			slog.Warn("failed to init vector index, hybrid search degrades to lexical-only", "error", err)
		} else {
			vecIndex = index
			store.SetEmbedder(&embedderAdapter{inner: embedEngine})
			// The local engine costs CPU rather than API calls, which is what lets reconciliation backfill episodes of any age instead of only the last ten days.
			store.SetEmbedsAreFree(true)
			embedsFree = true
			store.SetVectorSimilarityFloor(float32(appConfig.Embed.Floor()))
			// The floor the act run reference block scores past screen questions against, on the same embedder's scale as the one above.
			store.SetActRunSimilarityFloor(appConfig.Embed.ActRunFloor())
			store.SetVectorIndex(&vectorIndexAdapter{inner: vecIndex})

			// Startup sweep: heals a store carried over from before targeted vector deletes existed (orphaned notes/summaries/thinned episodes) and backfills anything wired in later (e.g. client-side note saves) that never got a vector. Async — a sweep of a large dirty store can spend real time on embeds and must not delay the rest of startup.
			go func() {
				report, err := store.ReconcileVectors(ctx, reconcileCap(embedsFree))
				if err != nil {
					slog.Error("startup vector reconciliation failed", "error", err)
					return
				}
				slog.Info("startup vector reconciliation complete", "deleted", report.Deleted, "backfilled", report.Backfilled)
			}()
		}
	}

	eventChan := make(chan tracker.Activity, 100)
	daemon := tracker.NewDaemon(trackerImpl, 2*time.Second, appConfig.Tracker.DwellTime*time.Millisecond, appConfig.Tracker.Blocklist, eventChan)

	// vision tier: when accessibility text is too thin (browsers, video, games), the tracker grabs a screenshot and asks the model to describe it.
	// Gated by cost guards inside the daemon (thinTextThreshold + minVisionInterval). Disabled when no model is available.
	if summarizer != nil {
		daemon.SetVisionFn(func(ctx context.Context, png []byte) tracker.Sight {
			s := summarizer.AnalyzeScreen(ctx, png)
			return tracker.Sight{UserActivity: s.UserActivity, VisibleText: s.VisibleText, Summary: s.Summary}
		})

		// prompt for screenshot permission up front (Linux portal) so the first real vision capture doesn't silently fail waiting on consent.
		// Blocks on the dialog, so run it off the startup path. No-op on other platforms.
		go tracker.WarmUpScreenshotPermission(ctx)
	}

	// tracker loop entry point
	go daemon.Start(ctx)

	// hourly safety-net flush: catches long idle sessions where no new activities fire
	go every(ctx, time.Hour, "safety-net-flush", func() {
		if compiler != nil {
			compiler.ForceFlush(ctx)
		}
	})

	// roll up fine-grained summaries older than 7 days into daily digests every 12 h.
	// skipped when no API key is available (summarizer == nil).
	if summarizer != nil {
		compactor := memory.NewCompactor(summarizer, store)
		go every(ctx, 12*time.Hour, "episodic-compaction", func() {
			if err := compactor.Compact(ctx, 7*24*time.Hour); err != nil {
				slog.Error("episodic compaction failed", "error", err)
			} else {
				slog.Info("episodic compaction complete")
			}
		})

		// consolidate the notes table every 6 h: merge near-duplicates and drop transient task detail that leaked in as "facts."
		noteCompactor := memory.NewNoteCompactor(summarizer, store)
		go every(ctx, 6*time.Hour, "note-consolidation", func() {
			if err := noteCompactor.Compact(ctx); err != nil {
				slog.Error("note consolidation failed", "error", err)
			} else {
				slog.Info("note consolidation complete")
				// ReplaceAllNotes (inside Compact) renumbers every note with no vector for the new rows — this sweep backfills them and cleans up anything else that's drifted.
				if report, err := store.ReconcileVectors(ctx, reconcileCap(embedsFree)); err != nil {
					slog.Error("post-consolidation vector reconciliation failed", "error", err)
				} else {
					slog.Info("post-consolidation vector reconciliation complete", "deleted", report.Deleted, "backfilled", report.Backfilled)
				}
			}
		})

		// recompute the working-state cache from recent summaries + notes.
		// The tick stays at five minutes, but memory.StateGate decides whether the call is actually made: at least ten minutes since the last derive, and either a window the user was not in before or enough new summaries to be worth re-reading. Before this gate every tick that found a single new summary made an API call, which is how one unattended job spent a 500-request day by noon on 2026-09-04.
		var gate memory.StateGate
		var lastDerive time.Time
		go every(ctx, 5*time.Minute, "working-state-derive", func() {
			now := time.Now()
			// The window starts at the last derive, or one interval back on the first tick, so the signature describes the episodes this derive would actually be covering.
			since := lastDerive
			if since.IsZero() {
				since = now.Add(-memory.StateInterval)
			}
			episodes, _ := store.EpisodesInWindow(ctx, since, now, maxDeriveStateEpisodes)
			windows := make([]string, 0, len(episodes))
			for _, e := range episodes {
				windows = append(windows, e.App+"|"+e.Title)
			}
			signature := memory.EpisodeSignature(windows)
			// A running total rather than a count since the last derive, so the gate can measure how many arrived between one derive and the next.
			summaryCount, _ := store.CountSummariesSince(ctx, time.Time{})
			if !gate.ShouldDerive(now, signature, summaryCount) {
				return
			}
			recent, _ := store.RecentSummaries(ctx, 10)
			// prepend concurrent live threads so the working state reflects everything in flight (watching + coding), not just the latest summary.
			liveThreads, _ := store.GetLiveThreads(ctx, 6)
			threadLines := make([]string, 0, len(liveThreads))
			for _, t := range liveThreads {
				if t.State != "" {
					threadLines = append(threadLines, fmt.Sprintf("Ongoing %s — %s: %s", t.Kind, t.Subject, t.State))
				} else {
					threadLines = append(threadLines, fmt.Sprintf("Ongoing %s — %s", t.Kind, t.Subject))
				}
			}
			recent = append(threadLines, recent...)
			// relevance-gated, not the whole notes table — same fix GetImplicitContext already applies for identity notes.
			noteStrings, _ := store.RelevantNotes(ctx, strings.Join(recent, " "), maxDeriveStateNotes)
			state, err := summarizer.DeriveState(ctx, recent, noteStrings)
			if err != nil {
				slog.Warn("working-state derive failed", "error", err)
				return
			}
			if state != "" {
				if err := store.SetWorkingState(ctx, state); err != nil {
					slog.Error("set working state failed", "error", err)
				}
			}
			// Only a derive that produced something moves the gate on, so a failed call does not start the ten-minute floor running and lose the change that earned it.
			gate.Derived(now, signature, summaryCount)
			lastDerive = now
		})
	}

	// proactive seams: the evening close writes Ora's diary for the day and the morning brief meets the first activity after the configured hour. One goroutine, per-minute condition checks, everything best-effort.
	mainBrain := tally.Wrap(brainProviderName(appConfig.Brain), brain.Metered(appConfig.Brain, apiKey, geminiQuota, false, geminiQuotaOpts), store)
	// Meeting minutes previously built their own unmetered brain per meeting; pinning to the meeting-minutes job's own model (as defaultBrain did unmetered) and metering it against the same shared quota means an unattended write-up spends the day's allowance in the same place it always spent it, just counted now.
	meetingRecorder.SetBrain(tally.Wrap(brainProviderName(appConfig.Brain), brain.Metered(config.BackgroundBrainConfig(appConfig.Brain, config.JobMeetingMinutes), apiKey, geminiQuota, false, geminiQuotaOpts), store))
	scheduler := proactive.New(store, mainBrain, proactive.NotifySend, appConfig.Proactive)
	// One-click answers to the morning brief's question about an item that has gone quiet. The notification blocks until it is answered, so the scheduler asks from its own goroutine.
	scheduler.SetAsk(proactive.NotifySendAsk)
	// Every other notice is posted straight to the desktop's notification service, carrying the buttons a macOS reminder carries: Open in Ora, Done, In an hour, This evening, Tomorrow. Presses arrive back over the session bus, so nothing blocks waiting for one, and a machine with no session bus falls back to notify-send.
	scheduler.SetNotifier(proactive.NewNotifier(ctx))
	// "Open in Ora", and a click on the notification body itself, do what the tray's own Open Ora item does.
	scheduler.SetOpenWindow(func() {
		authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
	})
	// "Done" on a task notice closes it through the daemon's own POST /tasks/{id}/done, so a task ticked off from a notification takes the one path that already knows a task the user typed in from an action item a meeting raised.
	scheduler.SetTaskDone(func(ctx context.Context, id string) error {
		url := "http://127.0.0.1:" + DaemonPort + "/tasks/" + id + "/done"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"done":true}`))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		attachIPCToken(req, ipctoken.DefaultPath)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("closing task %s: %w", id, proactive.ErrTaskGone)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("closing task %s answered %s", id, resp.Status)
		}
		return nil
	})
	// Sunday-only: render the week's self-accounting log, then run the distillation study pass over the same replay/trace material evals/main.go's track 6 uses — on the daemon's own main brain (claude-cli sonnet by default), which deliberately rides the user's Claude workday window rather than running overnight.
	scheduler.SetWeeklyStudy(func(ctx context.Context, now time.Time) error {
		if err := tally.RunWeeklyLog(ctx, store, now); err != nil {
			slog.Warn("weekly system log failed", "error", err)
		}
		replays, _ := filepath.Glob("evals/replays/*.md")
		traces, _ := filepath.Glob(filepath.Join(config.DataDir(), "dreams", "*.jsonl"))
		if res, err := study.Study(ctx, mainBrain, replays, traces, filepath.Join(config.DataDir(), "study")); err != nil {
			slog.Warn("weekly distillation study failed", "error", err)
		} else {
			slog.Info("weekly distillation study complete", "replays", res.ReplaysRead, "traces", res.TracesRead, "lessons_added", res.LessonsAdded)
		}
		return nil
	})
	go scheduler.Run(ctx)

	// overnight dreaming: while the machine idles on mains between the dream hour and the morning brief, test the diary's accumulated hypotheses, adopt new ones, rewrite the understanding doc, and leave a morning report in the diary. Judge-only this slice — every call goes to the brain.
	dreamBriefHour, _ := appConfig.Proactive.Hours()
	dreamer := dream.New(store, tally.Wrap(brainProviderName(appConfig.Brain), brain.Metered(appConfig.Brain, apiKey, geminiQuota, false, geminiQuotaOpts), store), dream.Probes{
		OnAC:              recorder.OnACPower,
		SessionLocked:     tracker.SessionLocked,
		RecorderQuiescent: meetingRecorder.Quiescent,
		InputIdle:         tracker.InputIdle,
	}, appConfig.Dream.DreamHour(), dreamBriefHour)
	// Touching this file makes the next tick dream immediately, gates bypassed — the way to watch a run without leaving the machine.
	dreamer.ForceMarker = filepath.Join(config.DataDir(), "dream-now")
	// Night traces: every dream brain call's raw reply lands in <data>/dreams/<night>.jsonl as raw material for a later distillation pass.
	dreamer.DataDir = config.DataDir()
	// A dream brain of its own (grok, agy) frees the night from the Claude window curfew, since it spends none of the user's Claude usage.
	if p := appConfig.Dream.Brain.Provider; p != "" {
		// On the Gemini API the night runs on the model config.BackgroundModel names for the dream job, and a quota or overload failure hands the stage to Codex.
		dreamBrainConfig := config.BackgroundBrainConfig(appConfig.Dream.Brain, config.JobDream)
		dreamer.SetBrain(tally.Wrap(brainProviderName(appConfig.Dream.Brain), brain.WithCodexFallback(brain.Metered(dreamBrainConfig, apiKey, geminiQuota, false, geminiQuotaOpts), backgroundFallbackBrain()), store))
		dreamer.CurfewExempt = p != config.BrainClaudeCLI
	}
	// Nightly dual-run: a local Gemma shadows the primary dream brain on the same prompts, replies logged for comparison, never acted on. The daemon owns the llama-server child for exactly one night's run — started before the stages, stopped after. The binary is the same llama-server the embedder already runs (appConfig.Embed.LlamaServer); a config with a dream model_path but no embed.llama_server falls back to PATH.
	if appConfig.Dream.ModelPath != "" {
		port := appConfig.Dream.DreamPort()
		binary := appConfig.Embed.LlamaServer
		if binary == "" {
			binary = "llama-server"
		}
		dreamer.ShadowLifecycle = dream.NewLlamaServerLifecycle(binary, appConfig.Dream.ModelPath, port, appConfig.Dream.Device)
		dreamer.Shadow = tally.Wrap("llama-shadow", brain.LlamaServer(fmt.Sprintf("http://127.0.0.1:%d", port), 600), store)
		if embedEngine != nil {
			dreamer.GPUReleaser = embedEngine.StopIfIdle
		}
	}
	go every(ctx, 5*time.Minute, "dreaming", func() { dreamer.Tick(ctx) })

	// age out old episode JPEGs every 24 hours. Screen text is no longer cleared here: all captured text ever recorded is only 7.4 MB, and it is the source of truth for memory, so there is no storage reason to lose it.
	go every(ctx, 24*time.Hour, "episode-aging", func() {
		const keepImagesFor = 14 * 24 * time.Hour
		// JPEGs are the storage hog. Drop every vision thumbnail older than two weeks; keep the structured description.
		dropped, err := store.AgeEpisodeImages(ctx, keepImagesFor)
		if err != nil {
			slog.Error("episode image aging failed", "error", err)
		} else {
			slog.Info("episode image aging complete", "dropped_jpegs", dropped)
		}
	})

	go func() {
		for ev := range eventChan {
			if _, err := store.WriteEpisode(ctx, db.EpisodeWrite{
				App: ev.App, Title: ev.Title, ScreenText: ev.ScreenText,
				UserActivity: ev.UserActivity, VisibleText: ev.VisibleText, ImageJPEG: ev.ImageJPEG,
				ExtraJPEG: ev.ExtraJPEG,
			}); err != nil {
				slog.Error("log episode failed", "error", err)
			}
			if compiler != nil {
				compiler.Ingest(ctx, ev)
			}
		}
	}()

	// IPC token: every handler below except /ping requires it (see requireIPCToken) — without this, any local process (or, since browsers can reach 127.0.0.1, any webpage) could read the live activity buffer, inject/wipe "memories" via /vector/add|delete, or toggle tracking. Regenerated on every startup so a leftover/stale process's copy stops working.
	ipcToken, err := ipctoken.Generate(ipctoken.DefaultPath)
	if err != nil {
		slog.Error("failed to generate IPC auth token, daemon IPC will be unreachable", "error", err)
	}
	// Every authenticated request is by definition a live TUI client, so the auth wrapper doubles as the presence signal that pins the embedding server in memory and warms it. The daemon has no other notion of a client session, and adding one just for this would be more machinery than a timestamp.
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return requireIPCToken(ipcToken, func(w http.ResponseWriter, r *http.Request) {
			if embedEngine != nil {
				embedEngine.MarkClientPresence(ctx)
			}
			h(w, r)
		})
	}

	// askAgent answers /ask questions the same way the CLI's text-eval path does: no mic/speaker (text only), the daemon's own store as the ContextReader, no compiler (buffer context isn't needed here).
	askAgent := agent.NewAgent(nil, nil, store, nil, apiKey)
	// The window's asks and the live voice session spend the interactive share of the same daily Gemini count the nightly jobs are held to, so the reserve is real.
	askAgent.SetRequestGate(&geminiRequestGate{state: geminiQuota, opts: geminiQuotaOpts, forAsks: true})
	// The window's read routes draw on the same store the daemon writes, on the compiler's live activity buffer — the one /buffer already serves — for what is on screen this second, and on the tracker's own active-window read for what has focus right now: the buffer only updates on the tracker's sampling interval, so a hotkey pressed between samples would otherwise name a window the user has already left.
	ipcServer := ipc.New(askAgent, store, func() []tracker.Activity {
		if compiler == nil {
			return nil
		}
		return compiler.GetCurrentBuffer()
	}, func(context.Context) (tracker.Activity, bool) {
		// The tracker's own read has no cancellable form, so a context here can only bound how long the caller waits on it (see ipc.readFocused), not interrupt the call itself.
		a, err := trackerImpl.GetActiveWindow()
		if err != nil || a == nil || (a.App == "Unknown" && a.Title == "Unknown") {
			return tracker.Activity{}, false
		}
		return *a, true
	})
	// A proactive moment goes to Ora's own card in the hover window when a window is there to show it, and falls back to the desktop's notifications only when none has been listening for a minute. Returning false is what makes that fallback happen, so a window that has just gone away does not swallow the notice.
	proactive.SetNoticeSender(func(n proactive.Notice) bool {
		if !ipcServer.Subscribed(time.Minute) {
			return false
		}
		ipcServer.Notice(ipc.Notice{Title: n.Title, Body: n.Body, Place: n.Place, ID: n.ID, Kind: n.Kind, Action: n.Action, Until: n.Until})
		return true
	})

	// point_at rings through the same overlay path POST /overlay uses, so the extension has one thing to listen to. One agent answers every ask, so the ring itself says nothing about which question drew it; the server names the ask running at that moment (see ipc.Server.DrawingAsk) and the overlay event goes out under that id, so a client watching /events can tie the ring to the question.
	askAgent.Point = func(x, y, w, h int, label string) {
		ipcServer.Ring(ipcServer.DrawingAsk(), x, y, w, h, label)
	}
	askAgent.Draw = ipcServer.Draw
	// show_marks marks through the same overlay path, one rect per observed item, labelled with the item's own number so the marks line up with what observe_screen just listed.
	askAgent.Marks = func(items []act.Item) {
		rects := make([]ipc.OverlayRect, len(items))
		for i, it := range items {
			rects[i] = ipc.OverlayRect{X: it.X, Y: it.Y, W: it.W, H: it.H, Label: strconv.Itoa(it.N)}
		}
		ipcServer.Marks(ipcServer.DrawingAsk(), rects)
	}
	// press_key, click_at and scroll_at drive the keyboard and pointer through the desktop portal, which asks the user to allow remote control the first time its session opens. The session opens on the first tool call that needs it, not here, so nobody sees that dialog until a task actually has to press a key or click a point the accessibility tree cannot reach; the grant is then restored from a token in the data directory, so the dialog is asked once rather than on every restart.
	askAgent.UsePortalInput(config.DataDir())
	// The bundled GNOME Shell extension, when the user has installed it and logged in again, raises another application's window on request; switch_window asks it first and falls back to the shell's own search through the portal keyboard when it is not there.
	var windowRaiser *window.Raiser
	if raiser, err := window.New(); err != nil {
		slog.Warn("window raiser unavailable, switch_window will use the keyboard path", "error", err)
	} else {
		windowRaiser = raiser
		askAgent.UseWindowRaiser(raiser)
	}
	// Codex answers asks the window routes to it by calling the ChatGPT backend directly with the user's own login, running the same tools through the same gate as the Gemini text path.
	ipcServer.AddBrain("codex", agent.CodexBrain{Agent: askAgent})
	// Claude answers through the Claude Code command line on the user's own subscription, with Ora's tools offered to it over MCP, so working on the screen does not depend on Codex's smaller monthly allowance.
	ipcServer.AddBrain("claude", agent.ClaudeBrain{Agent: askAgent})
	// Gemini is the default asker's own first choice, so naming it routes through that same path; without this line GET /brains offers Gemini while POST /ask refuses the name with a 400, which is what the window's picker hit on 2026-09-05.
	ipcServer.AddBrain("gemini", askAgent)

	// A long computer-use goal runs as a job in the daemon rather than inside one HTTP request (see internal/actjob): it plans, takes one checked step at a time, and can be stopped, paused, answered and resumed. Its rounds go to a plain prompt-in, text-out brain, never through an ask — an ask would run a second tool loop inside the job's own — and its steps go through the ask's own gated tool path, so the tool gate and the stop line have one copy.
	// The default is the daemon's own configured brain, metered and tallied like every other call it makes; the CLI logins are offered by name so the same goal can be run on each and the cost compared. No API-key path is ever picked by default.
	actModels := map[string]actjob.Model{"default": actjob.FromPromptFunc(brainProviderName(appConfig.Brain), mainBrain)}
	for _, provider := range []string{config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI} {
		actModels[provider] = actjob.FromPromptFunc(provider, brain.FromConfig(config.BrainConfig{Provider: provider}, apiKey))
	}
	actRunner := actjob.New(store, askAgent, actModels, "default", ipc.ActEmitter(ipcServer))
	actJobs := ipc.NewActJobs(actRunner)
	// A job the last daemon left in flight is never picked up by itself: resuming one moves things on the user's screen, so it waits to be asked for by name.
	if unfinished, err := store.UnfinishedActJobs(ctx); err == nil {
		for _, job := range unfinished {
			slog.Info("act job left unfinished; POST /act/{id}/resume picks it up", "job", job.ID, "goal", job.Goal, "state", job.State)
		}
	}

	// The unattended jobs were wired with a late-bound hand-over brain before the ask agent existed; publishing it here is what makes their 429 and 503 fallbacks live. It hands on again from Codex to Claude when Codex's own allowance is spent, so one spent subscription does not lose the day's summaries and minutes.
	publishCodexFallback(brain.FromAsker(agent.CodexThenClaude{Agent: askAgent}))

	// A routine asks through the same tool-calling path the window's /ask uses, so "when Priya replies about the venue" can look at the screen and the store rather than answer from a bare prompt.
	scheduler.SetRoutineAsk(func(ctx context.Context, q string) (string, error) {
		tr, err := askAgent.AskText(ctx, q)
		return tr.Answer, err
	})

	// local http for IPC between the tui and daemon
	mux := http.NewServeMux()

	// heartbeat — deliberately unauthenticated: root.go's pre-spawn liveness probe polls this before it can assume the token file even exists yet, and the build identity it returns reveals nothing sensitive.
	mux.HandleFunc("/ping", pingHandler)

	// data sharing endpoint, get latest tracking data
	mux.HandleFunc("/buffer", auth(func(w http.ResponseWriter, r *http.Request) {
		if compiler == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		buf := compiler.GetCurrentBuffer()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buf)
	}))

	// The tray asks the window it started to open or to show its hover; the instruction travels on the event stream the window already reads.
	mux.HandleFunc("/window", auth(ipcServer.Window))

	// pause/resume tracking
	mux.HandleFunc("/pause", auth(func(w http.ResponseWriter, r *http.Request) {
		daemon.Pause()
		slog.Info("tracking paused via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("paused"))
	}))

	mux.HandleFunc("/resume", auth(func(w http.ResponseWriter, r *http.Request) {
		daemon.Resume()
		slog.Info("tracking resumed via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("resumed"))
	}))

	mux.HandleFunc("/status", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paused := daemon.IsPaused()
		json.NewEncoder(w).Encode(map[string]bool{"paused": paused})
	}))

	// /vector/* let the client process reach the daemon's vector index over IPC instead of opening chromem itself — two processes opening the same chromem dir risks torn reads/corruption (see vecIndex's own doc comment above). All three return 503 with no body if the daemon has no vector index wired (no API key, or init failed) — the client's httpVectorIndex adapter treats any non-200 as an error, which HybridSearch already degrades gracefully from (see internal/db/hybrid.go's resilience handling).
	mux.HandleFunc("/vector/search", auth(func(w http.ResponseWriter, r *http.Request) {
		if vecIndex == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Embedding []float32         `json:"embedding"`
			N         int               `json:"n"`
			Where     map[string]string `json:"where"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		results, err := vecIndex.Search(r.Context(), req.Embedding, req.N, req.Where)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))

	mux.HandleFunc("/vector/add", auth(func(w http.ResponseWriter, r *http.Request) {
		if vecIndex == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			ID        string            `json:"id"`
			Content   string            `json:"content"`
			Embedding []float32         `json:"embedding"`
			Metadata  map[string]string `json:"metadata"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		if err := vecIndex.Add(r.Context(), req.ID, req.Content, req.Embedding, req.Metadata); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	mux.HandleFunc("/vector/delete", auth(func(w http.ResponseWriter, r *http.Request) {
		if vecIndex == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		if err := vecIndex.Delete(r.Context(), req.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// /embed lets the client reach the daemon's embedding engine instead of running one of its own. Only the daemon may own the llama-server child (one process, one port), so this is the client's only route to a local vector. 503 with no body when the daemon is on the Gemini path or has no embedder at all, which the client's httpEmbedder reports as an error and HybridSearch degrades from.
	mux.HandleFunc("/embed", auth(func(w http.ResponseWriter, r *http.Request) {
		if embedEngine == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Task string `json:"task"`
			Text string `json:"text"`
		}
		if !ipc.DecodeJSON(w, r, &req) {
			return
		}
		vec, err := embedEngine.Embed(r.Context(), embed.TaskType(req.Task), req.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	}))

	// /ask and /events let the desktop window pose a question about what's on screen and stream the answer as it comes together — see internal/ipc for the route bodies.
	mux.HandleFunc("/ask", auth(ipcServer.Ask))
	mux.HandleFunc("/events", auth(ipcServer.Events))

	// A long computer-use goal: POST /act starts one and returns its id, GET /act/{id} is its whole record, and the four control routes stop it, hold it, carry it on and answer the one question a stuck job asks. Progress rides the same /events stream as an ask, tagged type "act" with the job's id.
	mux.HandleFunc("POST /act", auth(actJobs.Start))
	mux.HandleFunc("GET /act/{id}", auth(actJobs.Get))
	mux.HandleFunc("POST /act/{id}/stop", auth(actJobs.Stop))
	mux.HandleFunc("POST /act/{id}/pause", auth(actJobs.Pause))
	mux.HandleFunc("POST /act/{id}/resume", auth(actJobs.Resume))
	mux.HandleFunc("POST /act/{id}/answer", auth(actJobs.Answer))

	// Hold-to-talk dictation: /dictate/start opens the microphone, /dictate/stop transcribes what was said with the same local whisper.cpp build the meeting recorder uses and hands the text back for the window to put in its input. Route bodies are in internal/ipc/dictate.go.
	dictation := ipc.NewDictation(ipcServer)
	mux.HandleFunc("/dictate/start", auth(dictation.Start))
	mux.HandleFunc("/dictate/stop", auth(dictation.Stop))

	// The window's read-only screens: what is on screen now, what is outstanding, what happened today, the meetings, a memory search and the people. Route bodies are in internal/ipc/reads.go.
	mux.HandleFunc("/context", auth(ipcServer.Context))
	mux.HandleFunc("/matters", auth(ipcServer.Matters))
	mux.HandleFunc("/today", auth(ipcServer.Today))
	mux.HandleFunc("/meetings", auth(ipcServer.Meetings))
	mux.HandleFunc("/meetings/live", auth(ipc.MeetingLive(meetingRecorder)))
	mux.HandleFunc("/memory/search", auth(ipcServer.MemorySearch))
	mux.HandleFunc("/people", auth(ipcServer.People))

	// The window's voice: the daemon runs the same Gemini Live loop the terminal client does, and what the session hears, says and calls rides the /events stream above. Route bodies are in internal/ipc/voice.go.
	voiceSession := ipc.NewVoice(ipcServer, store, apiKey)
	mux.HandleFunc("/voice/start", auth(voiceSession.Start))
	mux.HandleFunc("/voice/stop", auth(voiceSession.Stop))
	mux.HandleFunc("/voice/status", auth(voiceSession.Status))

	// The window's own record: the conversations it keeps and the turns inside them, the one list of work (action items plus the tasks the user typed in), the day pages, and which brains this machine is signed in to. Route bodies are in internal/ipc/conversations.go, tasks.go, days.go and brains.go.
	mux.HandleFunc("/conversations", auth(ipcServer.Conversations))
	mux.HandleFunc("/conversations/{id}", auth(ipcServer.Conversation)) // GET reads it, DELETE removes it
	mux.HandleFunc("/conversations/{id}/title", auth(ipcServer.ConversationTitle))
	mux.HandleFunc("/tasks", auth(ipcServer.Tasks))
	mux.HandleFunc("/tasks/{id}/done", auth(ipcServer.TaskDone))
	mux.HandleFunc("/tasks/{id}", auth(ipcServer.TaskOwner)) // PATCH corrects whose task it is, by hand
	// The window's own Done/1h/Evening/Tomorrow buttons on a live notice's rail line, answered through the same Act the desktop notification's own buttons call.
	mux.HandleFunc("POST /notices/{kind}/{id}/action", auth(ipc.NoticeAction(scheduler.Act)))

	mux.HandleFunc("/days", auth(ipcServer.Days))
	mux.HandleFunc("/days/{date}", auth(ipcServer.Day))
	mux.HandleFunc("/routines", auth(ipcServer.Routines))
	mux.HandleFunc("/routines/{id}", auth(ipcServer.RoutineDelete))
	mux.HandleFunc("/routines/{id}/run", auth(ipcServer.RoutineRun))
	brainLimits := brainLimitsFrom(brainUsage, geminiQuota, &appConfig, geminiQuotaOpts)
	mux.HandleFunc("/brains", auth(ipc.Brains(&appConfig, config.SaveConfig, brainLimits)))
	mux.HandleFunc("/overlay", auth(ipcServer.Overlay))
	mux.HandleFunc("/settings", auth(ipc.Settings(config.DataDir(), &appConfig, config.SaveConfig, appConfig.Meetings.OfferEnabled() || appConfig.Meetings.AutoRecord, daemon.IsPaused, startTime)))
	mux.HandleFunc("/usage", auth(ipc.Usage(store, appConfig.DailyTokenBudgetFor, brainLimits)))

	server := &http.Server{
		Handler: mux,
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("daemon ipc server failed", "error", err)
		}
	}()

	// The desktop window runs as the daemon's child, so the login entry starts one thing and stopping the daemon takes the window with it. It is started here, last, because the window reads the IPC token file once at startup: started any earlier it would read the token of the daemon that just exited, and every request it made would be refused for the life of the window.
	runWindow(ctx, appConfig.Window)

	stop = func() {
		// Every step here gets a bound, because shutdown runs them one after another and a step that never finishes keeps the process alive holding port 6942, which is the one thing that stops the next daemon from starting.
		// A recording in progress is closed first, before anything it depends on goes away. Nothing did this until a daemon restart on 2026-09-01 abandoned a meeting fourteen minutes in.
		// Five seconds: this stops the audio capture, closes mic.wav and system.wav, and calls the tray's state observer, which emits over D-Bus. All of it is local and takes milliseconds; the bound is there for a wedged session bus, not for the work.
		closeWithin("closing the running meeting recording", 5*time.Second, func() {
			if meetingRecorder != nil {
				if _, err := meetingRecorder.StopForShutdown(); err != nil {
					slog.Warn("could not close the running meeting recording on shutdown", "error", err)
				}
			}
		})
		// The event streams are ended first: Shutdown waits for open handlers but never cancels their requests, so a daemon with the window connected would otherwise hold its port for the whole timeout and the next daemon could not bind.
		ipcServer.CloseStreams()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		// The embedding server is this process's child and must never outlive it — a stranded llama-server holds ~600 MB and the port the next daemon needs.
		// Ten seconds: Close interrupts the child and waits up to five for it to exit before sending SIGKILL, then waits for the reap. That is its own five plus room for the kill, so this bound only fires if the child is unkillable.
		closeWithin("stopping the embedding server", 10*time.Second, func() {
			textEngine.Close()
			if embedEngine != nil {
				embedEngine.Close()
			}
		})
		// Five seconds: database/sql's Close waits for every connection in use to come back, and the daemon's background sweeps — vector reconciliation, note consolidation, episodic compaction — hold one for the length of their query. Five is long enough for a statement to finish and short enough that a sweep caught mid-flight does not keep the port bound.
		closeWithin("closing the store", 5*time.Second, func() { store.Close() })
		// The window raiser's session-bus connection is this process's too, so it is released here rather than left to process exit. Two seconds: closing a D-Bus connection is local and takes microseconds; the bound is there for a wedged bus, not for the work.
		closeWithin("closing the window raiser", 2*time.Second, func() {
			if windowRaiser != nil {
				if err := windowRaiser.Close(); err != nil {
					slog.Warn("could not close the window raiser's bus connection", "error", err)
				}
			}
		})
	}

	return stop, daemon, nil
}

// brainLimitsFrom is the allowance lookup GET /brains and GET /usage draw their bars from. Input: the store the Codex and Claude readings land in, the Gemini daily request counter, the live config (for the Gemini model those requests are metered under, and for whether the Claude read is turned on) and the configured ceilings. Output: a lookup taking a brain id and returning that brain's windows — Gemini's computed on the spot from the counter, Claude's read from its usage endpoint at most every ten minutes and only while someone is looking at the picker (skipped entirely when config.OraConfig.ClaudeUsageFromLogin is off), Codex's whatever its last response's headers said, and nothing at all for Grok and Ollama, which expose no allowance to read.
func brainLimitsFrom(usage *brain.UsageStore, quota *brain.QuotaState, cfg *config.OraConfig, opts brain.QuotaOptions) ipc.BrainLimits {
	return func(ctx context.Context, id string) (brain.UsageSnapshot, bool) {
		switch id {
		case "gemini":
			model, ok := brain.GeminiModelFor(cfg.Brain)
			if !ok {
				model = config.TextModel
			}
			return brain.GeminiDaily(quota, model, opts, time.Now())
		case "claude":
			if cfg.ClaudeUsageFromLoginEnabled() {
				agent.RefreshClaudeUsage(ctx)
			}
		}
		return usage.Get(id)
	}
}
