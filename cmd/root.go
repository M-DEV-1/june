package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"ora/internal/obs"
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

		isDaemon, _ := cmd.Flags().GetBool("daemon")

		if isDaemon {
			if err := runDaemon(ctx, shutdownObs); err != nil {
				slog.Error("daemon crashed", "error", err)
			}
			return
		}

		// get-or-create daemon.
		// using short per-request timeout without it http.Get hangs if the TCP port is bound but nobody has called Accept yet
		var daemonStatus string
		if pingDaemon() {
			slog.Info("connected to existing daemon")
			daemonStatus = "connected"
		} else {
			slog.Info("daemon not found, spawning background process")
			if spawnErr := spawnHiddenDaemon(); spawnErr != nil {
				slog.Error("failed to spawn background daemon", "error", spawnErr)
				daemonStatus = "daemon spawn failed: " + spawnErr.Error()
			} else {
				// 300ms timeout, i.e. no blocking
				// poll until daemon is ready, max 10s, 100ms sleep b/w attempts
				deadline := time.Now().Add(10 * time.Second)
				daemonStatus = "daemon spawn failed: timed out"
				for time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
					if pingDaemon() {
						daemonStatus = "started"
						break
					}
				}
			}
		}

		// start tui
		if err := runClient(ctx, shutdownObs, daemonStatus); err != nil {
			slog.Error("Client crashed", "error", err)
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
	rootCmd.PersistentFlags().Bool("daemon", false, "Run as background daemon")
}

// pingDaemon sends a single /ping with a short timeout.
// Returns true only if the daemon responds 200 OK.
var daemonPingClient = &http.Client{Timeout: 300 * time.Millisecond}

func pingDaemon() bool {
	resp, err := daemonPingClient.Get("http://127.0.0.1:" + DaemonPort + "/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
