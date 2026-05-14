package cmd

import (
	"context"
	"fmt"
	"ora/internal/agent"
	"ora/internal/audio"
	"ora/internal/db"
	"ora/internal/obs"
	"ora/internal/tracker"
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
		// take user input for API key
		if err := godotenv.Load(); err != nil {
			fmt.Println("No .env file found, read from sys env")
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

		// initialize db
		store, err := db.New("ora-db/db")
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not initialize db: %v\n", err)
			return
		}
		defer store.Close()

		// initialize tracker and daemon
		eye, err := tracker.New()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not initialize tracker: %v\n", err)
			return
		}

		eventChan := make(chan tracker.Activity, 100)
		daemon := tracker.NewDaemon(eye, 2*time.Second, eventChan)
		// TODO: allow 2s, 5s both in TUI

		// starts daemon in the background!!!
		go daemon.Start(ctx)

		go func() {
			for ev := range eventChan {
				store.LogActivity(ctx, ev.App, ev.Title)
			}
		}()

		// initialize voice and audio
		mic, err := audio.NewMic()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not initialize audio pipeline mic: %v\n", err)
			return
		}
		defer mic.Close()

		speaker, err := audio.NewSpeaker()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not initialize audio pipeline speaker: %v\n", err)
			return
		}
		defer speaker.Close()

		apiKey := os.Getenv("GEMINI_API_KEY")

		// inject dependencies
		orchestrator := agent.NewAgent(mic, speaker, store, apiKey)

		// start voice loop
		fmt.Println("Starting Ora. Press Ctrl+C to stop.")
		if err := orchestrator.Connect(ctx); err != nil {
			fmt.Printf("Agent Crashed: %v\n", err)
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
