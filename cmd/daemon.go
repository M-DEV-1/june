package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/embed"
	"ora/internal/memory"
	"ora/internal/tracker"
	"ora/internal/vector"

	"google.golang.org/genai"
)

const DaemonPort = "6942"

// maxDeriveStateNotes bounds how many relevance-ranked notes feed the 5-minute working-state derive, instead of the full notes table.
const maxDeriveStateNotes = 10

// embedderAdapter adapts *embed.GeminiEmbedder's Embed (which takes embed.TaskType) to the plain-string task param db.Store.SetEmbedder expects.
// internal/db can't import internal/embed, so the adapter lives here instead.
type embedderAdapter struct {
	inner *embed.GeminiEmbedder
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

func (v *vectorIndexAdapter) Count() int { return v.inner.Count() }

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
	store, err := db.New("ora-db/db")
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
	apiKey := os.Getenv("GEMINI_API_KEY")

	summarizer, err := memory.NewGeminiSummarizer(apiKey)
	if err != nil {
		slog.Warn("failed to init summarizer, semantic memory disabled", "error", err)
	}
	compiler := memory.NewCompiler(summarizer, store)

	// No API key means no genai client, so skip wiring the semantic half of hybrid search entirely.
	// HybridSearch already falls back to lexical-only when Store has no embedder/vector index set.
	if apiKey != "" {
		embedClient, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:  apiKey,
			Backend: genai.BackendGeminiAPI,
		})
		if err != nil {
			slog.Warn("failed to init genai client for embeddings, hybrid search degrades to lexical-only", "error", err)
		} else {
			embedder := embed.NewGeminiEmbedder(embedClient.Models, config.EmbedModel, 3072)
			// 10000 = the deck's agreed pruning cap for the vector index.
			vecIndex, err := vector.NewChromemIndex("ora-db/vectors", "memory", 10000)
			if err != nil {
				slog.Warn("failed to init vector index, hybrid search degrades to lexical-only", "error", err)
			} else {
				store.SetEmbedder(&embedderAdapter{inner: embedder})
				store.SetVectorIndex(&vectorIndexAdapter{inner: vecIndex})
			}
		}
	}

	eventChan := make(chan tracker.Activity, 100)
	daemon := tracker.NewDaemon(trackerImpl, 2*time.Second, appConfig.Tracker.DwellTime*time.Millisecond, appConfig.Tracker.Blocklist, eventChan)

	// vision tier: when accessibility text is too thin (browsers, video, games), the tracker grabs a screenshot and asks the model to describe it.
	// Gated by cost guards inside the daemon (thinTextThreshold + minVisionInterval). Disabled when no model is available.
	if summarizer != nil {
		daemon.SetVisionFn(func(ctx context.Context, png []byte) string {
			return summarizer.DescribeScreen(ctx, png)
		})

		// prompt for screenshot permission up front (Linux portal) so the first real vision capture doesn't silently fail waiting on consent.
		// Blocks on the dialog, so run it off the startup path. No-op on other platforms.
		go tracker.WarmUpScreenshotPermission(ctx)
	}

	// tracker loop entry point
	go daemon.Start(ctx)

	// hourly safety-net flush: catches long idle sessions where no new activities fire
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if compiler != nil {
					compiler.ForceFlush(ctx)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// roll up fine-grained summaries older than 7 days into daily digests every 12 h.
	// skipped when no API key is available (summarizer == nil).
	if summarizer != nil {
		compactor := memory.NewCompactor(summarizer, store)
		go func() {
			t := time.NewTicker(12 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := compactor.Compact(ctx, 7*24*time.Hour); err != nil {
						slog.Error("episodic compaction failed", "error", err)
					} else {
						slog.Info("episodic compaction complete")
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		// consolidate the notes table every 6 h: merge near-duplicates and drop transient task detail that leaked in as "facts."
		noteCompactor := memory.NewNoteCompactor(summarizer, store)
		go func() {
			t := time.NewTicker(6 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := noteCompactor.Compact(ctx); err != nil {
						slog.Error("note consolidation failed", "error", err)
					} else {
						slog.Info("note consolidation complete")
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		// recompute the working-state cache every 5 minutes from recent summaries + notes.
		// cost guard: skip the LLM call when no new summaries have arrived.
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			var lastDerive time.Time
			for {
				select {
				case <-t.C:
					if !lastDerive.IsZero() {
						n, _ := store.CountSummariesSince(ctx, lastDerive)
						if n == 0 {
							continue
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
						continue
					}
					if state != "" {
						if err := store.SetWorkingState(ctx, state); err != nil {
							slog.Error("set working state failed", "error", err)
						}
					}
					lastDerive = time.Now()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// drop raw activity rows older than 72 h every 6 hours
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				n, err := store.CullRawActivities(ctx, 72*time.Hour)
				if err != nil {
					slog.Error("activity cull failed", "error", err)
				} else {
					slog.Info("activity cull complete", "deleted_rows", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// age out old, low-importance episode text every 24 hours: clears screen_text (row kept, not deleted) for episodes older than keepRawFor whose importance is below importanceFloor.
	// Separate from the activity cull above, which operates on activity nodes rather than episode rows.
	go func() {
		const (
			keepRawFor      = 10 * 24 * time.Hour
			importanceFloor = 0.3
		)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				n, err := store.AgeEpisodes(ctx, keepRawFor, importanceFloor)
				if err != nil {
					slog.Error("episode aging failed", "error", err)
				} else {
					slog.Info("episode aging complete", "aged_rows", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// delete already-thinned, very old episode rows once a week, since AgeEpisodes above only ever empties screen_text and never deletes.
	// Only rows already thinned (screen_text already empty) are eligible. episodes_fts stays in sync via the episodes_ad AFTER DELETE trigger.
	go func() {
		const ancientAfter = 365 * 24 * time.Hour
		t := time.NewTicker(7 * 24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				n, err := store.PruneAncientEpisodes(ctx, ancientAfter)
				if err != nil {
					slog.Error("ancient episode prune failed", "error", err)
				} else {
					slog.Info("ancient episode prune complete", "deleted_rows", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		for ev := range eventChan {
			store.LogActivity(ctx, ev.App, ev.Title)
			// episode substrate: append-only capture of the raw screen_text that LogActivity above throws away. Additive, doesn't replace LogActivity/Ingest.
			if _, err := store.LogEpisode(ctx, ev.App, ev.Title, ev.ScreenText); err != nil {
				slog.Error("log episode failed", "error", err)
			}
			if compiler != nil {
				compiler.Ingest(ctx, ev)
			}
		}
	}()

	// local http for IPC between the tui and daemon
	mux := http.NewServeMux()

	// heartbeat
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	// data sharing endpoint, get latest tracking data
	mux.HandleFunc("/buffer", func(w http.ResponseWriter, r *http.Request) {
		if compiler == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		buf := compiler.GetCurrentBuffer()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buf)
	})

	// pause/resume tracking
	mux.HandleFunc("/pause", func(w http.ResponseWriter, r *http.Request) {
		daemon.Pause()
		slog.Info("tracking paused via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("paused"))
	})

	mux.HandleFunc("/resume", func(w http.ResponseWriter, r *http.Request) {
		daemon.Resume()
		slog.Info("tracking resumed via IPC")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("resumed"))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paused := daemon.IsPaused()
		json.NewEncoder(w).Encode(map[string]bool{"paused": paused})
	})

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
		store.Close()
	}

	return stop, daemon, nil
}
