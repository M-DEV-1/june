package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"ora/internal/obs"
	"os"
	"os/signal"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

// rootCmd is the base command, run when ora is invoked with no subcommand.
var rootCmd = &cobra.Command{
	Use:   "ora",
	Short: "Ora is a native OS companion for all everyday needs.",
	Long:  `Ora is an ambient AI agent designed for power users. It operates as a thin, standalone Go binary that silently tracks your digital workspace activity locally. By maintaining a private memory of your day, Ora provides instant, context-aware assistance.`,
	Run: func(cmd *cobra.Command, args []string) {
		isDaemon, _ := cmd.Flags().GetBool("daemon")
		autostart, _ := cmd.Flags().GetString("autostart")
		workdir, _ := cmd.Flags().GetString("workdir")
		runRoot(isDaemon, autostart, workdir)
	},
}

// Execute runs the root command. Called once by main.main().
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().Bool("daemon", false, "Run as background daemon")
	rootCmd.PersistentFlags().String("autostart", "", "Turn start-on-login on or off, persist it to the config, and exit (on|off)")
	rootCmd.PersistentFlags().String("workdir", "", "Change to this directory before doing anything else — the login autostart entry passes it, because ORA resolves ora-db relative to the working directory and a session manager launches from an arbitrary one")
}

// runRoot is the root command's behaviour: with no flags it starts the TUI against a get-or-create daemon, --daemon runs the background daemon itself, and --autostart flips start-on-login and returns.
func runRoot(isDaemon bool, autostart, workdir string) {
	// Must happen before anything reads a relative path (.env, ora-db).
	if workdir != "" {
		if err := os.Chdir(workdir); err != nil {
			fmt.Fprintf(os.Stderr, "fatal: could not change to %s: %v\n", workdir, err)
			return
		}
	}

	if autostart != "" {
		if err := applyAutostart(autostart); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		return
	}

	if err := godotenv.Load(); err != nil {
		slog.Info("No .env file found, read from sys env")
	}
	secureEnvFile(".env")
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

	// A stale daemon (still running an old build after a rebuild) doesn't fail pingDaemon — it's alive and answers just fine, it's just running old code. Only checked once a daemon is actually reachable, and never blocks startup on its own failure.
	var buildMismatch string
	if daemonStatus == "connected" || daemonStatus == "started" {
		buildMismatch = checkDaemonBuildMismatch(daemonPingClient, "http://127.0.0.1:"+DaemonPort+"/ping")
		if buildMismatch != "" {
			slog.Warn(buildMismatch)
		}
	}

	// start tui
	if err := runClient(ctx, shutdownObs, daemonStatus, buildMismatch); err != nil {
		slog.Error("Client crashed", "error", err)
	}
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

// checkDaemonBuildMismatch GETs url (the daemon's /ping) and compares its build identity against this process's own buildIdentity. A daemon and a freshly-launched client always read the same executable path, so the only way they'd disagree is a daemon process that's been running since before the file on disk was last overwritten — i.e. a rebuild happened and the daemon is still running the old code. Returns "" (no warning) on any failure or an empty/matching body — this is a diagnostic, never a reason to block startup.
func checkDaemonBuildMismatch(client *http.Client, url string) string {
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	daemonID := string(body)
	if daemonID == "" || daemonID == buildIdentity {
		return ""
	}
	return "daemon is running an older build — quit it from the tray or `pkill ora`, then relaunch"
}
