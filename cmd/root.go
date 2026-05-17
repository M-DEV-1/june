package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"ora/internal/agent"
	"ora/internal/audio"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
	"ora/internal/obs"
	"ora/internal/tracker"
	"ora/internal/ui"
	"os"
	"os/signal"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "ora",
	Short: "Ora is a native OS companion for all everyday needs.",
	Long:  `Ora is an ambient AI agent designed for power users. It operates as a thin, standalone Go binary that silently tracks your digital workspace activity locally. By maintaining a private memory of your day, Ora provides instant, context-aware assistance.`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	Run: func(cmd *cobra.Command, args []string) {

		// globally load the .env
		if err := godotenv.Load(); err != nil {
			slog.Info("No .env file found, read from sys env")
		}
		// global context that listens for sigint
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()

		// initialize otel
		shutdownObs, err := obs.InitTelemetry(ctx, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not initialize telemetry: %v\n", err)
			return
		}
		defer shutdownObs(ctx)

		// 🚀 Parallel Hardware & DB Initialization
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

		// wait for all 3 to finish
		for i := 0; i < 3; i++ {
			if err := <-initErrChan; err != nil {
				fmt.Fprintf(os.Stderr, "fatal: startup component failure: %v\n", err)
				return
			}
		}

		defer store.Close()
		defer mic.Close()
		defer speaker.Close()

		// initialize tracker and daemon
		eye, err := tracker.New()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not initialize tracker: %v\n", err)
			return
		}

		apiKey := os.Getenv("GEMINI_API_KEY")

		// initialize compiler
		summarizer, err := memory.NewGeminiSummarizer(apiKey)
		if err != nil {
			slog.Warn("failed to initialize summarizer, semantic memory disabled", "error", err)
		}
		compiler := memory.NewCompiler(summarizer, store)

		eventChan := make(chan tracker.Activity, 100)
		daemon := tracker.NewDaemon(eye, 2*time.Second, eventChan)

		// starts daemon in the background!!!
		go daemon.Start(ctx)

		go func() {
			for ev := range eventChan {
				// temp integration of compiler
				store.LogActivity(ctx, ev.App, ev.Title)
				if compiler != nil {
					compiler.Ingest(ctx, ev)
				}
			}
		}()

		// inject dependencies
		orchestrator := agent.NewAgent(mic, speaker, store, compiler, apiKey)
		orchestrator.SetModel(config.VoiceModel)

		// voice loop starts in bg
		go func() {
			if err := orchestrator.Connect(ctx); err != nil {
				// if tui is running, we might not want to print to stdout directly
				// but for now this is fine for debugging crashes
				slog.Error("agent connection crashed", "error", err)
			}
		}()

		// starts TUI
		if err := ui.Run(orchestrator); err != nil {
			fmt.Printf("UI Error: %v\n", err)
			return
		}

	},
	// root command is supposed to startup the CLI and other tools - db, memory companion, tui, et cetera.
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// Root flags can be added here
}
