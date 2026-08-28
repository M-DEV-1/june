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
	"strings"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/embed"
	"ora/internal/ipctoken"
	"ora/internal/memory"
	"ora/internal/recorder"
	"ora/internal/tracker"
	"ora/internal/vector"
)

// meetingRecorder is the tray's handle on the meeting recorder. startDaemonServices assigns it once the store exists, before registerSNI runs; it stays nil if the daemon never got that far, and every read of it is nil-safe.
var meetingRecorder *recorder.Recorder

const DaemonPort = "6942"

// pingHandler answers with this process's build identity — the client compares it against its own to detect a daemon that's been running since before the most recent rebuild (see checkDaemonBuildMismatch in root.go). Extracted as a named function so it's testable in isolation from the rest of the daemon's mux.
func pingHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buildIdentity))
}

// maxDeriveStateNotes bounds how many relevance-ranked notes feed the 5-minute working-state derive, instead of the full notes table.
const maxDeriveStateNotes = 10

// reconcileEmbedCap bounds how many backfill embeds one ReconcileVectors sweep performs, to protect API quota on a large dirty store — the sweep runs again on the next trigger (startup / note consolidation) and picks up where it left off.
const reconcileEmbedCap = 200

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
	store, err := db.New(filepath.Join(config.DataDir(), "db"))
	if err != nil {
		slog.Error("failed to init db", "error", err)
		return nil, nil, err
	}

	trackerImpl, err := tracker.New()
	if err != nil {
		slog.Error("failed to init tracker", "error", err)
		store.Close()
		return nil, nil, err
	}

	appConfig := config.LoadConfig()

	// The config file is the switch for start-on-login: make the on-disk login entry agree with it on every daemon start, so a config edited by hand (or an entry left behind by an older build) is corrected here rather than drifting.
	reconcileAutostart(appConfig.Autostart)

	apiKey := os.Getenv("GEMINI_API_KEY")

	meetingRecorder = recorder.New(config.DataDir(), store, apiKey)

	summarizer, err := memory.NewGeminiSummarizer(apiKey)
	if err != nil {
		slog.Warn("failed to init summarizer, semantic memory disabled", "error", err)
	}
	compiler := memory.NewCompiler(summarizer, store)

	// vecIndex is nil unless the block below succeeds — declared here (not just inside the block) so the /vector/* IPC handlers further down can serve the client's hybrid search over the same index the daemon itself uses, instead of each opening chromem separately (two processes opening the same chromem dir risks torn reads/corruption).
	var vecIndex *vector.ChromemIndex

	// The embedding engine is the local llama-server child process, and only that: there is no API-backed embedder any more. It stays nil when no local embedder is configured, which is what the shutdown path and the /embed IPC handler key off, and means no semantic half at all — HybridSearch already falls back to lexical-only when Store has no embedder/vector index set.
	embedEngine := embed.NewEngine(appConfig.Embed)
	if embedEngine == nil {
		slog.Warn("no local embedder configured (embed.llama_server / embed.model_path), hybrid search degrades to lexical-only")
	}

	if embedEngine != nil {
		// 10000 = the deck's agreed pruning cap for the vector index.
		index, err := vector.NewChromemIndex(filepath.Join(config.DataDir(), "vectors"), config.LocalEmbedDim, 10000)
		if err != nil {
			slog.Warn("failed to init vector index, hybrid search degrades to lexical-only", "error", err)
		} else {
			vecIndex = index
			store.SetEmbedder(&embedderAdapter{inner: embedEngine})
			// The local engine costs CPU rather than API calls, which is what lets reconciliation backfill episodes of any age instead of only the last ten days.
			store.SetEmbedsAreFree(true)
			store.SetVectorSimilarityFloor(float32(appConfig.Embed.Floor()))
			store.SetVectorIndex(&vectorIndexAdapter{inner: vecIndex})

			// Startup sweep: heals a store carried over from before targeted vector deletes existed (orphaned notes/summaries/thinned episodes) and backfills anything wired in later (e.g. client-side note saves) that never got a vector. Async — a sweep of a large dirty store can spend real time on embeds and must not delay the rest of startup.
			go func() {
				report, err := store.ReconcileVectors(ctx, reconcileEmbedCap)
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
				if report, err := store.ReconcileVectors(ctx, reconcileEmbedCap); err != nil {
					slog.Error("post-consolidation vector reconciliation failed", "error", err)
				} else {
					slog.Info("post-consolidation vector reconciliation complete", "deleted", report.Deleted, "backfilled", report.Backfilled)
				}
			}
		})

		// recompute the working-state cache every 5 minutes from recent summaries + notes.
		// cost guard: skip the LLM call when no new summaries have arrived.
		var lastDerive time.Time
		go every(ctx, 5*time.Minute, "working-state-derive", func() {
			if !lastDerive.IsZero() {
				n, _ := store.CountSummariesSince(ctx, lastDerive)
				if n == 0 {
					return
				}
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
			lastDerive = time.Now()
		})
	}

	// age out old, low-importance episode text every 24 hours: clears screen_text (row kept, not deleted) for episodes older than keepRawFor whose importance is below importanceFloor.
	go every(ctx, 24*time.Hour, "episode-aging", func() {
		const (
			keepRawFor      = 10 * 24 * time.Hour
			importanceFloor = 0.3
			keepImagesFor   = 14 * 24 * time.Hour
		)
		n, err := store.AgeEpisodes(ctx, keepRawFor, importanceFloor)
		if err != nil {
			slog.Error("episode aging failed", "error", err)
		} else {
			slog.Info("episode aging complete", "aged_rows", n)
		}
		// JPEGs are the storage hog. Drop every vision thumbnail older than two weeks; keep the structured description.
		dropped, err := store.AgeEpisodeImages(ctx, keepImagesFor)
		if err != nil {
			slog.Error("episode image aging failed", "error", err)
		} else {
			slog.Info("episode image aging complete", "dropped_jpegs", dropped)
		}
	})

	// delete already-thinned, very old episode rows once a week, since AgeEpisodes above only ever empties screen_text and never deletes.
	// Only rows already thinned (screen_text already empty) are eligible. episodes_fts stays in sync via the episodes_ad AFTER DELETE trigger.
	go every(ctx, 7*24*time.Hour, "ancient-episode-prune", func() {
		const ancientAfter = 365 * 24 * time.Hour
		n, err := store.PruneAncientEpisodes(ctx, ancientAfter)
		if err != nil {
			slog.Error("ancient episode prune failed", "error", err)
		} else {
			slog.Info("ancient episode prune complete", "deleted_rows", n)
		}
	})

	go func() {
		for ev := range eventChan {
			if _, err := store.WriteEpisode(ctx, db.EpisodeWrite{
				App: ev.App, Title: ev.Title, ScreenText: ev.ScreenText,
				UserActivity: ev.UserActivity, VisibleText: ev.VisibleText, ImageJPEG: ev.ImageJPEG,
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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

	server := &http.Server{
		Handler: mux,
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("daemon ipc server failed", "error", err)
		}
	}()

	stop = func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		// The embedding server is this process's child and must never outlive it — a stranded llama-server holds ~600 MB and the port the next daemon needs.
		if embedEngine != nil {
			embedEngine.Close()
		}
		store.Close()
	}

	return stop, daemon, nil
}
