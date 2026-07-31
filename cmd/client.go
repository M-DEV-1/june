package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"ora/internal/agent"
	"ora/internal/audio"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/ui"
)

func runClient(ctx context.Context, shutdownObs func(context.Context) error, daemonStatus string) error {
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
		store, err = db.New("ora-db/db")
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

	orchestrator := agent.NewAgent(mic, speaker, store, nil, apiKey)
	orchestrator.SetModel(config.VoiceModel)

	appConfig := config.LoadConfig()
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
				case orchestrator.TextResponseChan <- "\n*[connection lost — reconnecting...]*\n":
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

	if err := ui.Run(orchestrator, daemonStatus); err != nil {
		return fmt.Errorf("UI Error: %w", err)
	}

	return nil
}
