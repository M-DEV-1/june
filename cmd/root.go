package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"june/internal/config"
	"june/internal/ipctoken"
	"june/internal/obs"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

// rootCmd is the base command, run when june is invoked with no subcommand.
var rootCmd = &cobra.Command{
	Use:   "june",
	Short: "June is a native OS companion for all everyday needs.",
	Long:  `June is an ambient AI agent designed for power users. It operates as a thin, standalone Go binary that silently tracks your digital workspace activity locally. By maintaining a private memory of your day, June provides instant, context-aware assistance.`,
	Run: func(cmd *cobra.Command, args []string) {
		isDaemon, _ := cmd.Flags().GetBool("daemon")
		autostart, _ := cmd.Flags().GetString("autostart")
		workdir, _ := cmd.Flags().GetString("workdir")
		forceTUI, _ := cmd.Flags().GetBool("tui")
		runRoot(isDaemon, autostart, workdir, forceTUI)
	},
}

// exitCode is what the process exits with once runRoot has returned and its deferred cleanup — the telemetry shutdown above all — has run. Written by runRoot on the one goroutine cobra calls it from, read by Execute after that call has come back.
var exitCode int

// Execute runs the root command. Called once by main.main().
// loadEnvFiles reads the two files a key may live in, in precedence order: the repo checkout's own .env for a run started from there, then a fixed file under the data directory for every other way june is launched. godotenv never overwrites a variable that is already set, so the first one to carry a key wins and the real environment still beats both.
// It runs for every command, not just the daemon. june doctor used to read only the environment the shell handed over, so a key in the file the first-run panel tells the user to write was invisible to it, and doctor reported no brain on a machine that had one.
func loadEnvFiles() {
	cwdEnv := godotenv.Load()
	dataEnv := godotenv.Load(filepath.Join(config.DataDir(), "env"))
	if cwdEnv != nil && dataEnv != nil {
		slog.Info("no .env file found, reading the environment as it is", "looked_in", []string{".env", filepath.Join(config.DataDir(), "env")})
	}
	secureEnvFile(".env")
	secureEnvFile(filepath.Join(config.DataDir(), "env"))
}

func Execute() {
	loadEnvFiles()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
	// os.Exit skips deferred functions, so the code runRoot chose is applied here, after its defers have run, rather than inside it.
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func init() {
	rootCmd.PersistentFlags().Bool("daemon", false, "Run as background daemon")
	rootCmd.PersistentFlags().String("autostart", "", "Turn start-on-login on or off, persist it to the config, and exit (on|off)")
	rootCmd.PersistentFlags().String("workdir", "", "Change to this directory before doing anything else — the login autostart entry passes it, because June loads .env relative to the working directory and a session manager launches from an arbitrary one")
	rootCmd.PersistentFlags().Bool("tui", false, "Force the terminal UI even when a desktop window is available")
}

// runRoot is the root command's behaviour: with no flags it gets-or-creates a daemon and then shows June's desktop window if one is built and wanted, falling back to the terminal UI otherwise; --tui forces the terminal UI regardless; --daemon runs the background daemon itself; --autostart flips start-on-login and returns.
func runRoot(isDaemon bool, autostart, workdir string, forceTUI bool) {
	// Must happen before anything reads a relative path (.env — every june-db/data path now resolves through config.DataDir(), independent of cwd).
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

	// Two places, in precedence order: the repo checkout's own .env for a run started from there, then a fixed file under the data directory for every other way june is launched. godotenv never overwrites a variable that is already set, so the first one to carry a key wins and the real environment still beats both.
	// Without the second, the key had exactly one source and it was relative to the process's working directory: start june from anywhere but the checkout and GEMINI_API_KEY was empty, which is voice failing completely with nothing in the log to say why. The first-run panel has been telling the user to put the key in this file all along, and nothing read it.
	loadEnvFiles()
	// global context that listens for sigint
	// SIGTERM as well as SIGINT: kill, a logout and a system shutdown all send SIGTERM, and catching only SIGINT meant every one of those killed the process outright with no cleanup — abandoning a meeting recording mid-call.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
			// Non-zero, so a supervisor calls the start a failure. The commonest cause is a second daemon finding the port held by the first, and exiting 0 there meant systemd and june-restart both reported a restart that never happened.
			// The code is recorded rather than exited on, so the deferred telemetry shutdown and signal-context cancel below still run. os.Exit here skipped both, which was harmless only while the sole error this could return was the port bind, before anything had been traced.
			slog.Error("daemon crashed", "error", err)
			exitCode = 1
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

	// A live daemon is what a desktop window needs — one that failed to spawn has nothing to show a window in front of, so the terminal UI is the only option left.
	if !forceTUI && (daemonStatus == "connected" || daemonStatus == "started") {
		if offerWindow(daemonStatus == "started") {
			return
		}
	}

	// start tui
	if err := runClient(ctx, shutdownObs, daemonStatus, buildMismatch); err != nil {
		slog.Error("Client crashed", "error", err)
	}
}

// freshDaemonOpenAttempts is how many times offerWindow retries the show instruction when this process just spawned the daemon itself.
const freshDaemonOpenAttempts = 5

// openRetryInterval is the pause between those retries. A var, not a const, so a test can shrink it instead of actually waiting out four real pauses.
var openRetryInterval = 400 * time.Millisecond

// offerWindow decides whether this invocation of `june` should show the desktop window instead of the terminal UI, and does so when it can. Input: freshDaemon is true when this same process just spawned the daemon (as opposed to finding one already running) — its window child, if any, was only just started and needs a moment to launch and subscribe to the daemon's event stream before it can act on the show instruction. Output: true when it took over startup and there is nothing left for the caller to do (it already printed a line explaining what happened); false when the caller should still open the terminal UI, because the config has the window turned off or no window is built.
func offerWindow(freshDaemon bool) bool {
	appConfig := config.LoadConfig()
	if !appConfig.Window {
		return false
	}
	path, tried, err := windowBinary()
	if err != nil {
		fmt.Printf("No desktop window binary found (looked at: %s). Set JUNE_WINDOW=/path/to/it, or build one in app/, and `june` will open it instead of the terminal UI.\n", strings.Join(tried, ", "))
		return false
	}

	attempts := 1
	if freshDaemon {
		// ponytail: a fixed retry budget standing in for a real "the window is listening" signal, which nothing here exposes over IPC yet — upgrade path is a server-side subscriber check the daemon could answer instead of this guess. Comfortably longer than a Tauri window normally takes to launch and open its event stream, and harmless to repeat since the daemon just rebroadcasts "open" to whoever is listening.
		attempts = freshDaemonOpenAttempts
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(openRetryInterval)
		}
		authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
	}

	hotkey := formatHotkey(fetchWindowHotkey())
	if hotkey == "" {
		hotkey = "your June shortcut"
	}
	fmt.Printf("June is running (window: %s). It starts hidden — showing it now; if it doesn't appear, press %s or run `june --tui` for the terminal UI instead.\n", path, hotkey)
	return true
}

// fetchWindowHotkey asks the daemon's own /settings for the GNOME accelerator that shows the window (see internal/ipc.windowHotkey). Output: the raw accelerator, e.g. "<Control><Alt>space", or "" on any failure — this only ever feeds a hint line, never something startup can block or fail on.
func fetchWindowHotkey() string {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+DaemonPort+"/settings", nil)
	if err != nil {
		return ""
	}
	ipctoken.Attach(req, ipctoken.DefaultPath)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Hotkey string `json:"hotkey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.Hotkey
}

// hotkeyModifier matches one "<Name>" modifier segment of a GNOME accelerator string.
var hotkeyModifier = regexp.MustCompile(`<([^>]+)>`)

// formatHotkey turns a GNOME accelerator like "<Control><Alt>space" into the plain "Ctrl+Alt+Space" a terminal hint can print. Input: the raw accelerator, or "". Output: the formatted string, or "" when there was nothing to format.
func formatHotkey(raw string) string {
	if raw == "" {
		return ""
	}
	var parts []string
	for _, m := range hotkeyModifier.FindAllStringSubmatch(raw, -1) {
		mod := m[1]
		if mod == "Control" {
			mod = "Ctrl"
		}
		parts = append(parts, mod)
	}
	if key := hotkeyModifier.ReplaceAllString(raw, ""); key != "" {
		parts = append(parts, strings.ToUpper(key[:1])+key[1:])
	}
	return strings.Join(parts, "+")
}

// pingDaemon sends a single /ping with a short timeout.
// Returns true only if the daemon responds 200 OK.
var daemonPingClient = &http.Client{Timeout: 300 * time.Millisecond}

// One miss is not an answer. A single 300ms GET is easily outlived by a GC pause or a busy log flush in a perfectly healthy daemon, and a false "not running" makes the caller spawn a second one, which then fails to bind the port and exits loudly — seven times in the log, every one against a daemon that was demonstrably alive and serving. Three tries costs at most 900ms on the genuine cold-start path, where a spawn is about to happen anyway.
func pingDaemon() bool {
	for i := 0; ; i++ {
		resp, err := daemonPingClient.Get("http://127.0.0.1:" + DaemonPort + "/ping")
		if err == nil {
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
		if i == 2 {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
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
	return "daemon is running an older build — quit it from the tray or `pkill june`, then relaunch"
}

// secureEnvFile restricts path (the .env file, which holds the Gemini API key) to 0600 if it exists — it commonly defaults to whatever umask created it (often 0644, world-readable on a multi-user machine). Best-effort and silent on a missing file (env vars set directly, the common case) or on Windows, where these POSIX bits don't apply.
func secureEnvFile(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.Chmod(path, 0600); err != nil {
		slog.Warn("failed to restrict .env file permissions", "error", err)
		return
	}
	slog.Info("restricted .env file permissions to 0600")
}

// buildIdentity is computed once, here at package init, from the currently-running executable's own size+mtime — not lazily on every /ping call. A daemon process that's been running since before a rebuild must keep reporting the OLD binary's identity even after the file on disk has been overwritten with a new build; recomputing per-request would just report whatever's CURRENTLY on disk, indistinguishable from a fresh build and defeating the whole point (see cmd/root.go's checkDaemonBuildMismatch).
var buildIdentity = computeBuildIdentity()

// computeBuildIdentity resolves the path to the currently-running executable and fingerprints it. No build tooling changes (no injected version/ldflags) — just what's already on disk.
func computeBuildIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	return fileIdentity(exe)
}

// fileIdentity stats path and combines its size and modification time into a stable identity string, "unknown" if the stat fails. Extracted from computeBuildIdentity for testability — os.Executable() itself isn't something a test can point at a fixture file.
func fileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}
