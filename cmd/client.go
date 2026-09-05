package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"ora/internal/agent"
	"ora/internal/audio"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/ui"
)

func runClient(ctx context.Context, shutdownObs func(context.Context) error, daemonStatus, buildMismatch string) error {
	slog.Info("Starting Ora Client...")

	// Parallel hardware & DB init
	var (
		store   *db.Store
		mic     audio.Microphone
		speaker audio.Speaker
	)

	initErrChan := make(chan error, 3)

	go func() {
		var err error
		store, err = db.New(filepath.Join(config.DataDir(), "db"))
		initErrChan <- err
	}()

	go func() {
		var err error
		mic, err = audio.NewMic()
		initErrChan <- err
	}()

	go func() {
		var err error
		speaker, err = audio.NewSpeaker()
		initErrChan <- err
	}()

	for i := 0; i < 3; i++ {
		if err := <-initErrChan; err != nil {
			return fmt.Errorf("fatal: startup component failure: %w", err)
		}
	}

	defer store.Close()
	defer mic.Close()
	defer speaker.Close()

	// Start mic capture once — survives session reconnects.
	micChan, err := mic.StartCapture(ctx)
	if err != nil {
		return fmt.Errorf("failed to start microphone: %w", err)
	}

	apiKey := os.Getenv("GEMINI_API_KEY")
	appConfig := config.LoadConfig()

	// Wire the client's own store for hybrid search: an embedder built locally (same recipe the daemon uses) plus a vector index reached over the daemon's /vector/* IPC instead of opening chromem directly — chromem must stay exclusive to the daemon process. Without this the client-side store (which is what the live agent's query_memory/RetrieveRelevant/GetImplicitContext actually run against) was lexical-only in production, and client-side note saves never got embedded at all. HybridSearch already degrades to lexical-only if the daemon is unreachable (see internal/db/hybrid.go).
	// The client never builds an embedder of its own: the daemon owns the llama-server child process and its port, so embeds go over the same IPC the vector index already uses. If the daemon has no embedding engine, /embed answers 503 and HybridSearch degrades to lexical-only.
	store.SetEmbedder(&embedderAdapter{inner: newHTTPEmbedder()})
	store.SetVectorIndex(newHTTPVectorIndex())
	// The client runs its own HybridSearch against the daemon's index, so it needs the same embedder-matched cosine floor the daemon uses.
	store.SetVectorSimilarityFloor(float32(appConfig.Embed.Floor()))
	// The act run reference block is looked up on this side too, and its floor is a cosine on the same embedder's scale.
	store.SetActRunSimilarityFloor(appConfig.Embed.ActRunFloor())

	orchestrator := agent.NewAgent(mic, speaker, store, nil, apiKey)
	// The client process has no in-process compiler (that only exists in the daemon), so the handshake's "[working]" current-activity context was always dead here — wire it over the daemon's /buffer IPC instead (F2).
	orchestrator.SetBufferProvider(newBufferProvider().Get)
	orchestrator.SetModel(config.VoiceModel)

	orchestrator.SetVoice(appConfig.Voice)

	// Reconnect loop: if the Gemini session drops (idle timeout, network blip, session limit), restart automatically.
	// Mic stays running throughout.
	go func() {
		for {
			if err := orchestrator.Connect(ctx, micChan); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("agent session lost, reconnecting in 2s", "error", err)
				select {
				case orchestrator.TextResponseChan <- agent.ResponseChunk{Text: "connection lost — reconnecting…", Sender: agent.SenderSystem}:
				default:
				}
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					return
				}
			} else {
				// Clean exit (ctx cancelled)
				return
			}
		}
	}()

	if err := ui.Run(orchestrator, daemonStatus, buildMismatch); err != nil {
		return fmt.Errorf("UI Error: %w", err)
	}

	return nil
}
