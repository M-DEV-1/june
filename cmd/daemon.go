package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"june/internal/act"
	"june/internal/actjob"
	"june/internal/agent"
	"june/internal/brain"
	"june/internal/config"
	"june/internal/db"
	"june/internal/dream"
	"june/internal/embed"
	"june/internal/ipc"
	"june/internal/ipctoken"
	"june/internal/memory"
	"june/internal/proactive"
	"june/internal/recorder"
	"june/internal/study"
	"june/internal/tally"
	"june/internal/tracker"
	"june/internal/vector"
	"june/internal/window"
)

// meetingRecorder is the tray's handle on the meeting recorder. startDaemonServices assigns it once the store exists, before registerSNI runs; it stays nil if the daemon never got that far, and every read of it is nil-safe.
var meetingRecorder *recorder.Recorder

// DaemonPort is the loopback port the daemon binds and every client in this package dials. Overridable via JUNE_PORT so a second daemon (a test, a dry run) can run beside the live one without fighting it for the port. Defaults to 6942.
var DaemonPort = func() string {
	if p := os.Getenv("JUNE_PORT"); p != "" {
		return p
	}
	return "6942"
}()

// pickPlacedWindow chooses which of the shell's windows the tracker.UseWindowPlacer wiring should answer for a node's window. A title alone can miss: Teams' title carries a live memory count that changes between the listing that read it and the click that verifies it, so the exact string the tracker asks for by then names nothing. The pid it also carries names the right process regardless of what its title has become, so it is tried first: the exact title among that pid's windows, then the pid's focused window, then its only window. The exact title across every window, the placer's old behaviour, is what is left when the pid itself is 0 or matches nothing. Input: the open windows the shell extension reports, the pid the accessibility bus gave for the node's connection, and the title the accessibility bus read for its window. Output: the chosen window and true, or false when nothing matches.
func pickPlacedWindow(windows []window.Window, pid uint32, title string) (window.Window, bool) {
	var byPid []window.Window
	if pid != 0 {
		for _, w := range windows {
			if w.Pid == pid {
				byPid = append(byPid, w)
			}
		}
	}
	for _, w := range byPid {
		if w.Title == title {
			return w, true
		}
	}
	for _, w := range byPid {
		if w.Focused {
			return w, true
		}
	}
	if len(byPid) == 1 {
		return byPid[0], true
	}
	for _, w := range windows {
		if w.Title == title {
			return w, true
		}
	}
	return window.Window{}, false
}

// pingHandler answers with this process's build identity — the client compares it against its own to detect a daemon that's been running since before the most recent rebuild (see checkDaemonBuildMismatch in root.go). Extracted as a named function so it's testable in isolation from the rest of the daemon's mux.
func pingHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buildIdentity))
}

// maxDeriveStateNotes bounds how many relevance-ranked notes feed the 5-minute working-state derive, instead of the full notes table.
// concealSettle is how long the daemon waits after telling the hover to take itself off the screen, before the picture is taken. The instruction reaches the window over the event stream it is already reading, and the compositor needs one frame to redraw without it; a tenth of a second is about six frames.
const concealSettle = 100 * time.Millisecond

const maxDeriveStateNotes = 10

// maxDeriveStateEpisodes bounds how many tracked episodes the change gate reads to build its app-and-window-title signature. Two hundred: this store logged at most 60 episodes in its busiest hour on 2026-09-04, so it covers well over the ten-minute window the gate compares across, and the signature is a set so reading extra rows only costs the query.
const maxDeriveStateEpisodes = 200

func runDaemon(ctx context.Context, shutdownObs func(context.Context) error) error {
	slog.Info("Starting June Daemon...")

	// port binding instance lock to prevent double spawning
	listener, err := net.Listen("tcp", "127.0.0.1:"+DaemonPort)
	if err != nil {
		// A bind failure means another daemon already holds the port, and this one must say so on stderr and exit non-zero. Returning nil made the process exit 0, so systemd called a restart a success and the /ping that followed was answered by the old daemon still running the old build.
		fmt.Fprintf(os.Stderr, "june: port %s is already in use, so another daemon is still running: %v\n", DaemonPort, err)
		slog.Error("failed to bind daemon port", "port", DaemonPort, "error", err)
		return fmt.Errorf("bind daemon port %s: %w", DaemonPort, err)
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
	// The Live model the voice session dials is a choice rather than a constant: 3.1 answers in about two seconds with one tone, 2.5 takes five to eight and carries affective dialog and proactive audio. A name the config gets wrong falls back to the default rather than dialling a model that does not exist.
	if appConfig.LiveModel != "" && !config.SetVoiceModel(appConfig.LiveModel) {
		slog.Warn("unknown live voice model in config, using the default", "model", appConfig.LiveModel, "using", config.VoiceModel())
	}

	// The config file is the switch for start-on-login: make the on-disk login entry agree with it on every daemon start, so a config edited by hand (or an entry left behind by an older build) is corrected here rather than drifting.
	reconcileAutostart(appConfig.Autostart)

	apiKey := os.Getenv("GEMINI_API_KEY")

	// geminiQuota is the free-tier daily request ceiling shared by every Gemini-routed brain in this function — the summarizer's own direct calls below, plus evening close/morning brief and dream further down — persisted under the data dir so it survives a daemon restart; see internal/brain/quota.go. None of these are the interactive ask (that path lives in internal/agent and calls Gemini directly, bypassing this package), so everything here is metered as background, not asks.
	geminiQuota := brain.NewQuotaState(config.DataDir())
	geminiQuotaOpts := brain.DefaultQuotaOptions()

	// brainUsage keeps what each provider says about the user's own allowance, so the brain picker can draw a bar per brain. Codex fills it from the rate-limit headers of every response June already makes; Claude is read from its OAuth usage endpoint when the picker is opened; Gemini's is computed from geminiQuota below. It persists under the data dir beside brain_quota.json, so the bars are there the moment the window opens after a restart.
	brainUsage := brain.NewUsageStore(config.DataDir())
	agent.SetUsageRecorder(brainUsage)

	// Every Exa or Tavily call web_search makes files its own row on the same token ledger a model call does, so the cost view shows a search counting against the account's quota. See defaultWebSearch and recordSearchUse in internal/agent/websearch.go.
	agent.SetSearchUsageRecorder(func(u db.TokenUse) {
		storeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := store.AddTokenUse(storeCtx, u); err != nil {
			slog.Warn("could not file a web search token use", "provider", u.Provider, "error", err)
		}
	})

	meetingRecorder = recorder.New(ctx, config.DataDir(), store, apiKey)

	// June watches the microphone rather than the meeting apps: a call is the one thing that always takes it, and watching it needs no list of which applications count as a meeting.
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

	// embedsFree says the embedder is the local llama-server rather than a metered API. Declared out here because it is set inside the block below and read by the reconciliation sweeps further down, which size their budget by it.
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
	}

	// The text server yields to a whisper decode and does not climb back on until it is done. The embedding server stays: measured on this card, it holds 662 MiB of 4096 against whisper-medium's ~2.2 GB, so the two fit together with room over, and evicting it only cost every embed that arrived during a decode — hybrid search fell back to lexical and the reconcile sweep dropped its work. The text server is the one that does not fit: 1851 MiB, which with whisper on the card leaves nothing for the embedder.
	// A decode that still runs out of memory is redone on the CPU by RunWhisper, so keeping the embedder resident costs a slow transcription at worst, never a lost one.
	recorder.SetGPUReleaser(func() bool {
		return textEngine.StopIfIdle()
	})
	textEngine.SetGPUGate(recorder.GPUBusy)

	if embedEngine != nil {
		// 10000 = the deck's agreed pruning cap for the vector index.
		index, err := vector.NewChromemIndex(filepath.Join(config.DataDir(), "vectors"), config.LocalEmbedDim, 10000)
		if err != nil {
			slog.Warn("failed to init vector index, hybrid search degrades to lexical-only", "error", err)
		} else {
			store.SetEmbedder(&embedderAdapter{inner: embedEngine})
			// The local engine costs CPU rather than API calls, which is what lets reconciliation backfill episodes of any age instead of only the last ten days.
			store.SetEmbedsAreFree(true)
			embedsFree = true
			store.SetVectorSimilarityFloor(float32(appConfig.Embed.Floor()))
			// The floor the act run reference block scores past screen questions against, on the same embedder's scale as the one above.
			store.SetActRunSimilarityFloor(appConfig.Embed.ActRunFloor())
			store.SetVectorIndex(&vectorIndexAdapter{inner: index})

			// Startup sweep: heals a store carried over from before targeted vector deletes existed (orphaned notes/summaries/thinned episodes) and backfills anything wired in later that never got a vector. Async — a sweep of a large dirty store can spend real time on embeds and must not delay the rest of startup.
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
	daemon := tracker.NewDaemon(trackerImpl, 2*time.Second, time.Duration(appConfig.Tracker.DwellTime)*time.Millisecond, appConfig.Tracker.Blocklist, eventChan)

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

	// hourly safety-net flush: catches long idle sessions where no new activities fire.
	// Metered, because ForceFlush has no minimum-buffer gate: two minutes of captured activity is enough to pay for one attribution call, plus a ReconcileNotes call when the attribution names an identity.
	go everyMetered(ctx, store, time.Hour, "safety-net-flush", func() {
		if compiler != nil {
			compiler.ForceFlush(ctx)
		}
	})

	// roll up fine-grained summaries older than 7 days into daily digests every 12 h.
	// skipped when no API key is available (summarizer == nil).
	if summarizer != nil {
		compactor := memory.NewCompactor(summarizer, store)
		// Metered: one Digest call per day-group older than seven days that has not been rolled up yet, so a restart that finds a backlog pays for it again.
		go everyMetered(ctx, store, 12*time.Hour, "episodic-compaction", func() {
			if err := compactor.Compact(ctx, 7*24*time.Hour); err != nil {
				slog.Error("episodic compaction failed", "error", err)
			} else {
				slog.Info("episodic compaction complete")
			}
		})

		// consolidate the notes table every 6 h: merge near-duplicates and drop transient task detail that leaked in as "facts."
		noteCompactor := db.NewNoteCompactor(summarizer, store)
		// Metered: Compact calls the model whenever there are at least twenty fact notes, which on a real store is always, so every restart bought one call and a full ReconcileVectors sweep behind it.
		go everyMetered(ctx, store, 6*time.Hour, "note-consolidation", func() {
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
		// The ten-minute floor belongs to the store, not to this process: an empty gate let the first tick of every restart through however recently the state had been derived, so a day of june-restart cycles bought a derive call each. The working state's own updated_at is when the last derive succeeded, so the loop starts from there and the first tick inside the floor records it in the gate instead of calling the model.
		var lastDerive time.Time
		if at, err := store.MemoryAsOf(ctx, "working_state"); err != nil {
			slog.Warn("could not read when the working state was last derived, the first tick will derive", "error", err)
		} else {
			lastDerive = at
		}
		go every(ctx, 5*time.Minute, "working-state-derive", func() {
			now := time.Now()
			// The window starts at the last derive, or one interval back on the first tick, so the signature describes the episodes this derive would actually be covering.
			since := lastDerive
			if since.IsZero() {
				since = now.Add(-memory.StateInterval)
			}
			// Both gate reads are checked rather than ignored: a failing read used to yield an empty signature and a zero count, which match the cached values, so the gate said no and the working state went stale with nothing logged.
			episodes, err := store.EpisodesInWindow(ctx, since, now, maxDeriveStateEpisodes)
			if err != nil {
				slog.Error("working-state derive skipped: could not read the recent episodes", "error", err)
				return
			}
			windows := make([]string, 0, len(episodes))
			for _, e := range episodes {
				windows = append(windows, e.App+"|"+e.Title)
			}
			signature := memory.EpisodeSignature(windows)
			// A running total rather than a count since the last derive, so the gate can measure how many arrived between one derive and the next.
			summaryCount, err := store.CountSummariesSince(ctx, time.Time{})
			if err != nil {
				slog.Error("working-state derive skipped: could not count the summaries", "error", err)
				return
			}
			// The store says a derive already ran inside the floor, so this tick records that moment and this tick's material in the gate and spends nothing. From the next tick on the gate has its own history and decides on its own.
			if !lastDerive.IsZero() && now.Sub(lastDerive) < memory.StateInterval {
				gate.Derived(lastDerive, signature, summaryCount)
				return
			}
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

	// proactive seams: the evening close writes June's diary for the day and the morning brief meets the first activity after the configured hour. One goroutine, per-minute condition checks, everything best-effort.
	// These duties are unattended, so they run on the background model like every other one: config.TextModel's free tier allows 20 requests a day against DefaultBackgroundModel's 500, and dreaming alone ticks every five minutes all night. No Job* name exists for the proactive duties yet, and BackgroundModel degrades a name it does not know to DefaultBackgroundModel, which is the model wanted here — naming it "proactive" means a later entry in background_models pins it without another change here.
	// askAgent answers /ask questions the same way the CLI's text-eval path does: no mic/speaker (text only), the daemon's own store as the ContextReader, no compiler (buffer context isn't needed here).
	askAgent := agent.NewAgent(nil, nil, store, nil, apiKey)
	// The window's asks and the live voice session spend the interactive share of the same daily Gemini count the nightly jobs are held to, so the reserve is real.
	askAgent.SetRequestGate(&geminiRequestGate{state: geminiQuota, opts: geminiQuotaOpts, forAsks: true})
	// meteredJobBrain builds the brain one unattended duty answers through: the router's providers in order, cfg's own provider first, each on the background model config.BackgroundBrainConfig picks for job and metered against the shared Gemini quota. Codex answers through the ask agent's own ChatGPT login, which is the only way it can answer a prompt. Input: the duty's brain config and its background-model job name. Output: the routed, tallied brain.
	codexAsker := agent.CodexBrain{Agent: askAgent}
	meteredJobBrain := func(cfg config.BrainConfig, job string) brain.Brain {
		build := func(provider string) brain.Brain {
			c := cfg
			if provider != cfg.Provider {
				// A model and a binary pinned for the configured provider belong to that provider's namespace; handing a Gemini model name to the Claude CLI names nothing it has. The provider being handed on to uses its own default instead.
				c.Model, c.Binary = "", ""
			}
			c.Provider = provider
			// The tally is inside the routing rather than around it, so a duty that was handed on is counted against the provider that actually answered it and not against the one that refused.
			return tally.Wrap(brainProviderName(c), brain.Metered(config.BackgroundBrainConfig(c, job), apiKey, geminiQuota, false, geminiQuotaOpts, codexAsker), store)
		}
		// cfg.Provider is asked first: a background_brains entry pins a duty to a plan on purpose, and the router's global order must not answer it somewhere the user did not choose.
		return brain.RoutedFor(cfg.Provider, build)
	}
	mainBrain := meteredJobBrain(appConfig.Brain, "proactive")
	// Every memory duty the config names a provider for is pointed at it here; a duty named nowhere stays on the Gemini API. StripFence is applied because three of these duties parse the answer as JSON and a CLI login wraps JSON in a markdown fence where the SDK could simply be told to answer in JSON, and this is the one place that knows a CLI is involved.
	// JobScreenSight is not offered: it sends a screenshot rather than a prompt, and a text seam cannot carry an image.
	if summarizer != nil {
		if textEngine != nil {
			// This is what takes the five-minute working-state job off the user's free-tier daily request allowance. A derive the local model cannot answer — the card is held by a whisper decode, or the server would not start — goes through the router instead of being lost. A background_brains entry for the same job overrides both, because naming a provider in the config is the more deliberate act.
			summarizer.SetJobBackend(config.JobWorkingState, memory.TextBackend(fallThrough(textEngine.Generate, meteredJobBrain(appConfig.Brain, config.JobWorkingState))))
		}
		for job, cfg := range appConfig.BackgroundBrains {
			if job == config.JobScreenSight {
				slog.Warn("background_brains names screen_sight, which sends an image and cannot run on a text brain; leaving it on Gemini")
				continue
			}
			jobBrain := meteredJobBrain(cfg, job)
			summarizer.SetJobBackend(job, func(ctx context.Context, prompt string) (string, error) {
				text, err := jobBrain(ctx, prompt)
				return brain.StripFence(text), err
			})
			slog.Info("background duty routed to its own brain", "job", job, "provider", brainProviderName(cfg))
		}
	}
	// Meeting minutes previously built their own unmetered brain per meeting; pinning to the meeting-minutes job's own model (as defaultBrain did unmetered) and metering it against the same shared quota means an unattended write-up spends the day's allowance in the same place it always spent it, just counted now.
	// Minutes go through the router like every other duty. They used to be one brain with a Codex-only hand-over behind them, which caught nothing once Codex itself was at its monthly limit: on 2026-09-15 a broken Antigravity login lost every write-up while Grok and Claude sat signed in on the same machine.
	meetingRecorder.SetBrain(meteredJobBrain(appConfig.Brain, config.JobMeetingMinutes))
	scheduler := proactive.New(store, mainBrain, proactive.NotifySend, appConfig.Proactive)
	// The evening close makes two brain calls, so its deadline is sized from the limit this machine's config puts on one of them rather than from a fixed number.
	scheduler.SetBrainTimeout(appConfig.Brain.Timeout())
	// One-click answers to the morning brief's question about an item that has gone quiet. The notification blocks until it is answered, so the scheduler asks from its own goroutine.
	scheduler.SetAsk(proactive.NotifySendAsk)
	// Every other notice is posted straight to the desktop's notification service, carrying the buttons a macOS reminder carries: Open in June, Done, In an hour, This evening, Tomorrow. Presses arrive back over the session bus, so nothing blocks waiting for one, and a machine with no session bus falls back to notify-send.
	scheduler.SetNotifier(proactive.NewNotifier(ctx))
	// "Open in June", and a click on the notification body itself, do what the tray's own Open June item does.
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
		ipctoken.Attach(req, ipctoken.DefaultPath)
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
		replays, traces := weeklyStudyMaterial(config.DataDir())
		if len(replays) == 0 && len(traces) == 0 {
			slog.Info("weekly distillation study skipped: no replays and no dream traces to read")
			return nil
		}
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
	// The dream brain gets the dream job's own background model and the same routing the configured-dream-brain branch below gets, since a night of stages left on config.TextModel spends the next day's 20 requests before the morning brief runs.
	dreamer := dream.New(store, meteredJobBrain(appConfig.Brain, config.JobDream), dream.Probes{
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
		// On the Gemini API the night runs on the model config.BackgroundModel names for the dream job, and a provider that is spent or signed out hands the stage on to the next one the router offers.
		dreamer.SetBrain(meteredJobBrain(appConfig.Dream.Brain, config.JobDream))
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
		// The night's traces and replay artifacts are aged on the same tick: nothing else pruned them, and the Sunday study globs the whole directory every week.
		removed, err := ageDreamArtifacts(config.DataDir(), dreamArtifactRetention)
		if err != nil {
			slog.Error("dream artifact aging failed", "error", err)
		} else {
			slog.Info("dream artifact aging complete", "removed_files", removed)
		}
	})

	var ingestEpisode func(context.Context, tracker.Activity)
	if compiler != nil {
		ingestEpisode = compiler.Ingest
	}
	go drainEpisodes(ctx, eventChan, store.WriteEpisode, ingestEpisode)

	// IPC token: every handler below except /ping requires it (see requireIPCToken) — without this, any local process (or, since browsers can reach 127.0.0.1, any webpage) could read the store, ask questions as the user, or toggle tracking. Regenerated on every startup so a leftover/stale process's copy stops working.
	ipcToken, err := ipctoken.Generate(ipctoken.DefaultPath)
	if err != nil {
		slog.Error("failed to generate IPC auth token, daemon IPC will be unreachable", "error", err)
	}
	// Every authenticated request comes from a live client such as the desktop window, so the auth wrapper doubles as the presence signal that pins the embedding server in memory and warms it. The daemon has no other notion of a client session, and adding one just for this would be more machinery than a timestamp.
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return requireIPCToken(ipcToken, func(w http.ResponseWriter, r *http.Request) {
			if embedEngine != nil {
				embedEngine.MarkClientPresence(ctx)
			}
			h(w, r)
		})
	}

	// The window's read routes draw on the same store the daemon writes, on the compiler's live activity buffer for what is on screen this second, and on the tracker's own active-window read for what has focus right now: the buffer only updates on the tracker's sampling interval, so a hotkey pressed between samples would otherwise name a window the user has already left.
	// The brain picked in Settings answers first; the router otherwise ranks by cost and left the pick last.
	agent.SetPreferredProvider(appConfig.Brain.Provider)
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
	// June's own hover is drawn over whatever the user was looking at, so a picture of the screen taken while it is up has June's card sitting in the middle of the thing the question was about. The window takes itself off the screen for the moment the picture is taken and puts itself back exactly as it was — a window already hidden stays hidden, so this costs nothing when the hover is not up.
	// ponytail: a fixed settle wait rather than an acknowledgement from the window. The instruction reaches it over the event stream in a millisecond or two and the compositor needs a frame to redraw; if that ever proves too short the window should answer that it is hidden and this should wait for that instead.
	tracker.SetScreenGuard(func() func() {
		if !ipcServer.Subscribed(time.Minute) {
			return nil
		}
		// Every capture pays the settle wait, not only the ones taken while June's window is in front: during a computer-use job the target application holds the focus while the hover stays up over it with its live steps, and a guard keyed on focus left the hover in every picture the job took (found 2026-09-10). A window already hidden ignores the instruction, so the cost when the hover is down is the wait alone.
		ipcServer.Tell("conceal")
		time.Sleep(concealSettle)
		return func() { ipcServer.Tell("reveal") }
	})
	// The faces both windows show for what the daemon is doing on its own: a meeting being captured, and the nightly run.
	meetingRecorder.SetOnStateChange(func() {
		ipcServer.Announce("recording", onOff(meetingRecorder.Active()))
	})
	dreamer.OnNight = func(running bool) { ipcServer.Announce("dreaming", onOff(running)) }

	// A proactive moment goes to June's own card in the hover window when a window is there to show it, and falls back to the desktop's notifications only when none has been listening for a minute. Returning false is what makes that fallback happen, so a window that has just gone away does not swallow the notice.
	proactive.SetNoticeSender(func(n proactive.Notice) bool {
		if !ipcServer.Subscribed(time.Minute) {
			return false
		}
		// A notice that carries its own answers (the stale-task question) hands them to the window as buttons; the field-by-field copy keeps the two packages from importing each other.
		buttons := make([]ipc.NoticeButton, len(n.Actions))
		for i, a := range n.Actions {
			buttons[i] = ipc.NoticeButton{Key: a.Key, Label: a.Label}
		}
		ipcServer.Notice(ipc.Notice{Title: n.Title, Body: n.Body, Place: n.Place, ID: n.ID, Kind: n.Kind, Action: n.Action, Until: n.Until, Actions: buttons, Expires: n.Expires})
		return true
	})

	// A routine the user runs from the window's own Run button answers 202 and delivers its result as a notice, the same way the scheduler's tick on that routine does — through the scheduler's own say, so it falls back to a desktop notification when no window is listening.
	ipcServer.SetSay(func(n ipc.Notice) {
		scheduler.Say(proactive.Notice{Title: n.Title, Body: n.Body, Place: n.Place, ID: n.ID, Kind: n.Kind, Action: n.Action, Until: n.Until})
	})

	// point_at rings through the same overlay path POST /overlay uses, so the extension has one thing to listen to. One agent answers every ask, so the ring itself says nothing about which question drew it; the server names the ask running at that moment (see ipc.Server.DrawingAsk) and the overlay event goes out under that id, so a client watching /events can tie the ring to the question.
	askAgent.Point = func(x, y, w, h int, label string) error {
		return ipcServer.Ring(ipcServer.DrawingAsk(), x, y, w, h, label)
	}
	askAgent.Draw = ipcServer.Draw
	// A press through the real pointer is shown first as the overlay's pointer flying to the same point, under the same ask id as a ring would be.
	askAgent.Tap = func(x, y int, label string) error {
		return ipcServer.Tap(ipcServer.DrawingAsk(), x, y, label)
	}
	// show_marks marks through the same overlay path, one rect per observed item, labelled with the item's own number so the marks line up with what observe_screen just listed.
	askAgent.Marks = func(items []act.Item) error {
		rects := make([]ipc.OverlayRect, len(items))
		for i, it := range items {
			rects[i] = ipc.OverlayRect{X: it.X, Y: it.Y, W: it.W, H: it.H, Label: strconv.Itoa(it.N)}
		}
		return ipcServer.Marks(ipcServer.DrawingAsk(), rects)
	}
	// press_key, click_at and scroll_at drive the keyboard and pointer through the desktop portal, which asks the user to allow remote control the first time its session opens. The session opens on the first tool call that needs it, not here, so nobody sees that dialog until a task actually has to press a key or click a point the accessibility tree cannot reach; the grant is then restored from a token in the data directory, so the dialog is asked once rather than on every restart.
	askAgent.UsePortalInput(config.DataDir())
	// The bundled GNOME Shell extension, when the user has installed it and logged in again, raises another application's window on request; switch_window asks it first and falls back to the shell's own search through the portal keyboard when it is not there.
	// dbus.ConnectSessionBus has no timeout of its own and this runs while the IPC port is bound but nothing is accepting on it yet, so a wedged session bus used to hang startup past the client's ten-second readiness poll and print "daemon spawn failed: timed out" for a daemon that was merely stuck here. The dial is off the startup path entirely now, and lateRaiser is what the agent and the shutdown hold in the meantime, so nothing waits on it and nothing is assigned behind their backs when it finishes.
	windowRaiser := &lateRaiser{}
	askAgent.UseWindowRaiser(windowRaiser)
	// The same extension is the only thing on a Wayland desk that knows where a window is, so the tracker's listings are placed from its frame rectangles; until it answers, or on a desk without it, the tracker falls back to guessing from window size.
	// The same list says which window the compositor has in front, which is the window observe_screen reads: the accessibility bus's own idea of focus drifts between two windows of one application.
	tracker.UseFocusReader(func(ctx context.Context) (pid uint32, title string, ok bool) {
		windows, err := windowRaiser.List(ctx)
		if err != nil {
			return 0, "", false
		}
		for _, win := range windows {
			if win.Focused {
				return win.Pid, win.Title, true
			}
		}
		return 0, "", false
	})
	tracker.UseWindowPlacer(func(ctx context.Context, pid uint32, title string) (x, y, w, h int, ok bool) {
		windows, err := windowRaiser.List(ctx)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		win, ok := pickPlacedWindow(windows, pid, title)
		if !ok || win.W <= 0 || win.H <= 0 {
			return 0, 0, 0, 0, false
		}
		return win.X, win.Y, win.W, win.H, true
	})
	go func() {
		raiser, err := window.New()
		if err != nil {
			slog.Warn("window raiser unavailable, switch_window will use the keyboard path", "error", err)
			return
		}
		windowRaiser.publish(raiser)
	}()
	// Codex answers asks the window routes to it by calling the ChatGPT backend directly with the user's own login, running the same tools through the same gate as the Gemini text path.
	ipcServer.AddBrain("codex", agent.CodexBrain{Agent: askAgent})
	// Claude answers through the Claude Code command line on the user's own subscription, with June's tools offered to it over MCP, so working on the screen does not depend on Codex's smaller monthly allowance.
	ipcServer.AddBrain("claude", agent.ClaudeBrain{Agent: askAgent})
	// Antigravity answers through the agy command line on the user's own Google plan, with June's tools offered to it over MCP the same way Claude gets them. The brain id is "antigravity" because that is the id GET /brains publishes and the window's picker posts back; the CLI it runs is called agy.
	ipcServer.AddBrain("antigravity", agent.AgyBrain{Agent: askAgent})
	// Gemini is the default asker's own first choice, so naming it routes through that same path; without this line GET /brains offers Gemini while POST /ask refuses the name with a 400, which is what the window's picker hit on 2026-09-05.
	ipcServer.AddBrain("gemini", askAgent)

	// A long computer-use goal runs as a job in the daemon rather than inside one HTTP request (see internal/actjob): it plans, takes one checked step at a time, and can be stopped, paused, answered and resumed. Its rounds go to a plain prompt-in, text-out brain, never through an ask — an ask would run a second tool loop inside the job's own — and its steps go through the ask's own gated tool path, so the tool gate and the stop line have one copy.
	// The default is the daemon's own configured brain, metered and tallied like every other call it makes; the CLI logins are offered by name so the same goal can be run on each and the cost compared. No API-key path is ever picked by default.
	// The default job brain is the configured chain with the Claude CLI behind it, so a spent Codex allowance moves a job to Claude the way an ask already moves.
	// mainBrain is routed, so a spent or signed-out brain is handed on from inside it. The Claude tail stays on top of that, because a job the user is watching hands over on any failure at all and not only on the two the router acts on — see fallThrough, and the three jobs that died in a row on 2026-09-08.
	claudeJobBrain := brain.FromConfig(config.BrainConfig{Provider: config.BrainClaudeCLI}, apiKey)
	actModels := map[string]actjob.Model{"default": actjob.FromPromptFunc(brainProviderName(appConfig.Brain), fallThrough(mainBrain, claudeJobBrain))}
	for _, provider := range []string{config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI} {
		actModels[provider] = actjob.FromPromptFunc(provider, brain.FromConfig(config.BrainConfig{Provider: provider}, apiKey))
	}
	// askAgent is the job runner's executor, and it also carries the past-run reference block a job plans against: the runner picks that up by asserting actjob.Referencer off the executor it was given, so nothing here passes it explicitly. Asserted at compile time because a silent failure of that assertion looks exactly like a job with no history to plan against, which is what it did before 2026-09-12.
	var _ actjob.Referencer = askAgent
	actRunner := actjob.New(store, askAgent, actModels, "default", ipc.ActEmitter(ipcServer))
	actJobs := ipc.NewActJobs(actRunner)
	// A job the last daemon left in flight is never picked up by itself: resuming one moves things on the user's screen, so it waits to be asked for by name.
	if unfinished, err := store.UnfinishedActJobs(ctx); err == nil {
		for _, job := range unfinished {
			slog.Info("act job left unfinished; POST /act/{id}/resume picks it up", "job", job.ID, "goal", job.Goal, "state", job.State)
		}
	}

	// The summarizer's own direct Gemini calls were wired with a late-bound hand-over brain before this point; publishing it here is what makes their 429 and 503 fallback live. It hands on again from Codex to Claude when Codex's own allowance is spent.
	publishCodexFallback(brain.FromAsker(agent.CodexThenClaude{Agent: askAgent}))

	// A routine asks through the same tool-calling path the window's /ask uses, so "when Vexil replies about the venue" can look at the screen and the store rather than answer from a bare prompt.
	scheduler.SetRoutineAsk(func(ctx context.Context, q string) (string, error) {
		tr, err := askAgent.AskText(ctx, q)
		return tr.Answer, err
	})

	// local http for IPC between the daemon and its clients (window, tray, the june command)
	mux := http.NewServeMux()

	// One accessor over the config the request goroutines share, so POST /settings writing ClaudeUsageFromLogin and the /brains and /usage handlers reading it are not touching the same struct from several goroutines at once.
	liveConfig := ipc.NewLiveConfig(&appConfig, config.SaveConfig)
	brainLimits := brainLimitsFrom(brainUsage, geminiQuota, liveConfig, geminiQuotaOpts)

	registerDaemonRoutes(mux, routeDependencies{
		auth:            auth,
		daemon:          daemon,
		ipcServer:       ipcServer,
		actJobs:         actJobs,
		store:           store,
		apiKey:          apiKey,
		meetingRecorder: meetingRecorder,
		scheduler:       scheduler,
		liveConfig:      liveConfig,
		brainLimits:     brainLimits,
		appConfig:       appConfig,
		startTime:       startTime,
	})

	server := &http.Server{
		// Every CORS preflight is answered in front of the routes; see withPreflight for why it cannot be a route of its own.
		Handler: withPreflight(mux, auth),
		// A header-read bound, and only that: WriteTimeout stays zero because /events holds its response open for as long as a window is listening. Ten seconds is long enough for any local caller to finish a request line and short enough that a process opening connections and never finishing one does not hold them.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("daemon ipc server failed", "error", err)
		}
	}()

	// Names the tray mark as the desktop window's dock icon (GNOME otherwise shows a generic gear); never fatal. It runs here, after Serve, rather than on the way up: it writes a .desktop file and execs gtk-update-icon-cache, and doing that between binding the port and accepting on it meant a slow exec held every client's readiness poll on a port that was bound but answering nothing.
	if err := installDesktopEntry(); err != nil {
		slog.Warn("failed to install desktop entry", "error", err)
	}

	// The desktop window runs as the daemon's child, so the login entry starts one thing and stopping the daemon takes the window with it. It is started here, last, because the window reads the IPC token file once at startup: started any earlier it would read the token of the daemon that just exited, and every request it made would be refused for the life of the window.
	runWindow(ctx, appConfig.Window)

	stop = func() {
		// Every step here gets a bound, because shutdown runs them one after another and a step that never finishes keeps the process alive holding port 6942, which is the one thing that stops the next daemon from starting.
		// A recording in progress is closed first, before anything it depends on goes away. Nothing did this until a daemon restart on 2026-09-01 abandoned a meeting fourteen minutes in.
		// Five seconds: this stops the audio capture, closes mic.wav and system.wav, and calls the tray's state observer, which emits over D-Bus. All of it is local and takes milliseconds; the bound is there for a wedged session bus, not for the work.
		within("closing the running meeting recording", 5*time.Second, func() {
			if meetingRecorder != nil {
				if _, err := meetingRecorder.StopForShutdown(); err != nil {
					slog.Warn("could not close the running meeting recording on shutdown", "error", err)
				}
			}
		})
		// The compiler's activity buffer is written out next, before the store it writes into is closed. It is otherwise flushed only on the hourly tick, so a logout or an june-restart lost up to an hour of activity — the same loss the recorder above was given a shutdown step for on 2026-09-01.
		// The context is not the root one: SIGTERM has already cancelled the root context by the time stop() runs, and a flush started with it would fail on its first query. It carries the same deadline as the bound, so a flush that runs out of time stops at its next call rather than being left behind writing into a store this function is about to close.
		within("flushing the activity buffer", shutdownFlushBound, func() {
			if compiler != nil {
				flushCtx, cancelFlush := context.WithTimeout(context.Background(), shutdownFlushBound)
				defer cancelFlush()
				compiler.ForceFlush(flushCtx)
			}
		})
		// The event streams are ended first: Shutdown waits for open handlers but never cancels their requests, so a daemon with the window connected would otherwise hold its port for the whole timeout and the next daemon could not bind.
		ipcServer.CloseStreams()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		// The embedding server is this process's child and must never outlive it — a stranded llama-server holds ~600 MB and the port the next daemon needs.
		// Ten seconds: Close interrupts the child and waits up to five for it to exit before sending SIGKILL, then waits for the reap. That is its own five plus room for the kill, so this bound only fires if the child is unkillable.
		within("stopping the embedding server", 10*time.Second, func() {
			textEngine.Close()
			if embedEngine != nil {
				embedEngine.Close()
			}
		})
		// Five seconds: database/sql's Close waits for every connection in use to come back, and the daemon's background sweeps — vector reconciliation, note consolidation, episodic compaction — hold one for the length of their query. Five is long enough for a statement to finish and short enough that a sweep caught mid-flight does not keep the port bound.
		// The Antigravity process an ask keeps alive is this daemon's child too; left running it would hold June's tool server and answer nobody. Three seconds: the kill is immediate and the reap runs in the background.
		within("stopping the agy session", 3*time.Second, askAgent.CloseAgySession)
		within("closing the store", 5*time.Second, func() { store.Close() })
		// The window raiser's session-bus connection is this process's too, so it is released here rather than left to process exit. Two seconds: closing a D-Bus connection is local and takes microseconds; the bound is there for a wedged bus, not for the work.
		within("closing the window raiser", 2*time.Second, func() {
			if err := windowRaiser.close(); err != nil {
				slog.Warn("could not close the window raiser's bus connection", "error", err)
			}
		})
	}

	return stop, daemon, nil
}

// onOff is "on" for true and "off" for false, the two words an announce event carries.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
