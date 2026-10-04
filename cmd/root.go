package cmd

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
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
	// Every command, doctor included, starts here: the working directory is settled first, because the checkout's .env is read relative to it — the login entry passes --workdir for exactly that — and then the env files are read, once. They used to be read in Execute and again in runRoot after the chdir, which printed the "no .env file" line twice on every start.
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if workdir, _ := cmd.Flags().GetString("workdir"); workdir != "" {
			if err := os.Chdir(workdir); err != nil {
				reportFailure(fmt.Sprintf("June could not start: it could not change to %s: %v", workdir, err))
				// Nothing with a deferred cleanup has started yet, so exiting here skips nothing; returning used to carry on and exit 0, which a login entry or a supervisor reads as a start that worked.
				os.Exit(1)
			}
		}
		loadEnvFiles()
	},
	Run: func(cmd *cobra.Command, args []string) {
		var opts rootOptions
		opts.daemon, _ = cmd.Flags().GetBool("daemon")
		opts.autostart, _ = cmd.Flags().GetString("autostart")
		opts.updateCheck, _ = cmd.Flags().GetString("update-check")
		opts.quit, _ = cmd.Flags().GetBool("quit")
		opts.waitPid, _ = cmd.Flags().GetInt("wait-pid")
		opts.open, _ = cmd.Flags().GetBool("open")
		runRoot(opts)
	},
}

// rootOptions are the root command's flags.
type rootOptions struct {
	daemon      bool
	autostart   string
	updateCheck string
	quit        bool
	waitPid     int
	open        bool
}

// exitCode is what the process exits with once the command has returned and its deferred cleanup — the telemetry shutdown above all — has run. Written by runRoot or doctor on the one goroutine cobra calls them from, read by Execute after that call has come back.
var exitCode int

// loadEnvFiles reads the two files a key may live in, in precedence order: the repo checkout's own .env for a run started from there, then a fixed file under the data directory for every other way june is launched. godotenv never overwrites a variable that is already set, so the first one to carry a key wins and the real environment still beats both.
// It runs for every command, not just the daemon. june doctor used to read only the environment the shell handed over, so a key in the file the first-run panel tells the user to write was invisible to it, and doctor reported no brain on a machine that had one.
// Without the data-directory file the key had exactly one source and it was relative to the process's working directory: start june from anywhere but the checkout and GEMINI_API_KEY was empty, which is voice failing completely with nothing in the log to say why. The first-run panel has been telling the user to put the key in that file all along, and nothing read it.
// The daemons this process starts are handed the environment as it was before the files were read (util.StartupEnviron), so they read the files afresh instead of inheriting what this one read.
func loadEnvFiles() {
	cwdEnv := godotenv.Load()
	dataEnv := godotenv.Load(filepath.Join(config.DataDir(), "env"))
	if cwdEnv != nil && dataEnv != nil {
		slog.Info("no .env file found, reading the environment as it is", "looked_in", []string{".env", filepath.Join(config.DataDir(), "env")})
	}
	secureEnvFile(".env")
	secureEnvFile(filepath.Join(config.DataDir(), "env"))
}

// reportFailure says why june could not start or show June. Input: the sentence for the user. Output: none.
// It goes to stderr for a terminal or a script, to june.log, and in front of the user when nothing would show stderr (see showFailure): the Start menu runs junew.exe, which has no console, and a click on June that failed used to do nothing at all and leave no trace anywhere, since these lines never reached the log either; a click in a Linux app grid sends stderr to a journal nobody reads.
func reportFailure(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	slog.Error("june could not do what it was started for", "reason", msg)
	if stderrUnseen() {
		showFailure(msg)
	}
}

// Execute runs the root command. Called once by main.main().
func Execute() {
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
	rootCmd.Flags().String("update-check", "", "Turn the daily check for a new version of June on or off, persist it to the config, and exit (on|off)")
	rootCmd.Flags().Bool("quit", false, "Ask the running daemon to shut down and wait for it to exit; exits 0 once no daemon is running, 1 if one still is")
	rootCmd.Flags().Int("wait-pid", 0, "With --daemon: first wait for this process (the daemon being replaced) to exit and free the port")
	rootCmd.Flags().Bool("open", false, "With --daemon: show the desktop window as soon as it is up")
	// --version prints the release this build was stamped with, "dev" when none; the installer and a bug report read it from here.
	rootCmd.Version = config.Version
}

// runRoot is the root command's behaviour: with no flags it gets-or-creates a daemon and then shows June's desktop window; --daemon runs the background daemon itself (see runDaemonLifecycle for --wait-pid and --open); --autostart flips start-on-login and returns; --update-check flips the daily release check and returns; --quit stops a running daemon. --workdir and the env files are handled before it, in rootCmd's PersistentPreRun.
// Every failure sets exitCode before returning: a bad --autostart value or a telemetry init failure used to print its error and exit 0, so a script or an installer could not tell it had failed.
func runRoot(opts rootOptions) {
	if opts.autostart != "" {
		if err := applyAutostart(opts.autostart); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			exitCode = 1
		}
		return
	}
	if opts.updateCheck != "" {
		if err := applyUpdateCheck(opts.updateCheck); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			exitCode = 1
		}
		return
	}
	if opts.quit {
		exitCode = quitDaemon()
		return
	}

	// The wait comes before the log is opened: the daemon being replaced still has june.log open and may roll it aside, and Windows refuses to rename a file another process holds.
	var waited error
	if opts.daemon && opts.waitPid > 0 {
		waited = awaitPredecessor(opts.waitPid, predecessorWait)
	}

	// global context that listens for sigint
	// SIGTERM as well as SIGINT: kill, a logout and a system shutdown all send SIGTERM, and catching only SIGINT meant every one of those killed the process outright with no cleanup — abandoning a meeting recording mid-call.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// initialize otel
	shutdownObs, err := obs.InitTelemetry(ctx, false)
	if err != nil {
		reportFailure(fmt.Sprintf("June could not start: it could not open its log in %s (%v).", config.DataDir(), err))
		exitCode = 1
		return
	}
	defer shutdownObs(ctx)

	if opts.daemon {
		if opts.waitPid > 0 {
			switch {
			case errors.Is(waited, errAlreadyServed):
				slog.Info("not starting: another June daemon took over while the restart was under way", "replaced", opts.waitPid)
				// The window the restart meant to bring back is that June's to show now.
				if takeOpenMarker() {
					authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
				}
				return
			case waited != nil:
				slog.Warn("starting anyway, though the daemon being replaced has not finished", "error", waited)
			default:
				slog.Info("the daemon being replaced has exited", "pid", opts.waitPid)
			}
		}
		// The code is recorded rather than exited on, so the deferred telemetry shutdown and signal-context cancel below still run. os.Exit here skipped both, which was harmless only while the sole error this could return was the port bind, before anything had been traced.
		exitCode = runDaemonLifecycle(ctx, shutdownObs, opts.open)
		return
	}

	// get-or-create daemon.
	// using short per-request timeout without it http.Get hangs if the TCP port is bound but nobody has called Accept yet
	freshDaemon := false
	if pingDaemon() {
		slog.Info("connected to existing daemon")
	} else if pid := quittingDaemon(); pid > 0 {
		exitCode = startAfterQuit(pid)
		return
	} else {
		if !awaitDaemon() {
			exitCode = 1
			return
		}
		// Whether this process spawned it or found one still starting, the daemon has only just come up and its window with it, so the show instruction is retried the same way.
		freshDaemon = true
	}

	// A stale daemon (still running an old build after a rebuild) doesn't fail pingDaemon — it's alive and answers just fine, it's just running old code. Never blocks startup on its own failure.
	// Another account's June is another install, so its build says nothing about this one, and the advice to stop it cannot reach it; showWindow says what is actually wrong.
	if mismatch := checkDaemonBuildMismatch(daemonPingClient, "http://127.0.0.1:"+DaemonPort+"/ping"); mismatch != "" {
		if _, _, other := portHeldByOtherAccount(DaemonPort); !other {
			slog.Warn(mismatch)
			fmt.Fprintln(os.Stderr, mismatch)
		}
	}

	exitCode = showWindow(freshDaemon)
}

// freshDaemonOpenAttempts is how many times showWindow retries the show instruction when this process just spawned the daemon itself.
const freshDaemonOpenAttempts = 5

// openRetryInterval is the pause between those retries. A var, not a const, so a test can shrink it instead of actually waiting out four real pauses.
var openRetryInterval = 400 * time.Millisecond

// showWindow asks the running daemon to show June's desktop window and prints one line saying what happened. Input: freshDaemon is true when this same process just spawned the daemon (as opposed to finding one already running) — its window child, if any, was only just started and needs a moment to launch and subscribe to the daemon's event stream before it can act on the show instruction. Output: the process exit code, 1 when no window binary is installed or the daemon does not take the instruction, and 0 otherwise, including when the config has the window turned off.
func showWindow(freshDaemon bool) int {
	appConfig := config.LoadConfig()
	if !appConfig.Window {
		fmt.Println("June is running without its desktop window, because \"window\" is false in june-config.json.")
		return 0
	}
	path, tried, err := windowBinary()
	if err != nil {
		reportFailure(fmt.Sprintf("June's desktop window is not installed (looked at: %s). Set JUNE_WINDOW=/path/to/it, or build it in app/.", strings.Join(tried, ", ")))
		return 1
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
		// The daemon answered /ping a moment ago, so an instruction it does not take is a daemon gone wedged or one this june holds the wrong token for. Repeating it would only add two seconds a try before the same answer, and the line below would then say June is running when nothing will show.
		if err := authedDaemonRequest(http.MethodGet, "http://127.0.0.1:"+DaemonPort+"/window?action=open"); err != nil {
			// It answered /ping and then began to quit, and has no window left to show.
			if pid := quittingDaemon(); pid > 0 {
				return startAfterQuit(pid)
			}
			// It answers /ping as June but refuses this account's token because it is another account's June, which the advice below, to stop it from here, cannot reach.
			if message, other := otherAccountProblem(DaemonPort, func() bool { return true }); other {
				reportFailure(message)
				return 1
			}
			reportFailure(fmt.Sprintf("June's daemon answers /ping but did not take the instruction to show its window (%v). Its side of it is in %s; if it stays like this, %s, then run june again.", err, filepath.Join(config.DataDir(), "june.log"), stopDaemonHint))
			return 1
		}
	}

	hotkey := formatHotkey(fetchWindowHotkey())
	if hotkey == "" {
		hotkey = "your June shortcut"
	}
	fmt.Printf("June is running (window: %s). It starts hidden — showing it now; if it doesn't appear, press %s.\n", path, hotkey)
	return 0
}

// fetchWindowHotkey asks the daemon's own /settings for the GNOME accelerator that shows the window (see internal/ipc.windowHotkey). Output: the raw accelerator, e.g. "<Control><Alt>space", or "" on any failure — this only ever feeds a hint line, never something startup can block or fail on.
func fetchWindowHotkey() string {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+DaemonPort+"/settings", nil)
	if err != nil {
		return ""
	}
	ipctoken.Attach(req, ipctoken.DefaultPath())
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

// daemonWait is how long the client waits for a daemon to answer /ping, one it spawned or one already holding the port while it starts.
const daemonWait = 10 * time.Second

// daemonColdStart is how long after it started a June holding the port is still waited for, when that is longer than daemonWait (see awaitDaemon). A June older than this that has not answered is stuck.
const daemonColdStart = time.Minute

// awaitDaemon gets a daemon answering on DaemonPort after the first ping found none: it spawns one when the port is free, and otherwise waits for whatever holds the port to answer. Output: true once a daemon answers /ping; false once it has printed the one line saying why none will.
// A port merely in use is not refused up front, because a daemon still starting holds the port before it answers /ping, and the wait is what finds it. But no second daemon is spawned onto a held port: it could only fail to bind and exit with its reason on a stderr nobody reads, and this process then waited ten seconds and said the daemon it had started did not answer, when the port was held by something else all along. The port is tried again on every round, so a daemon that is shutting down and lets go of it is replaced.
func awaitDaemon() bool {
	spawned, looked, extended := false, false, false
	holder, holderIsJune := "", false
	began := time.Now()
	deadline := began.Add(daemonWait)
	for {
		if !spawned {
			probe, err := net.Listen("tcp", "127.0.0.1:"+DaemonPort)
			switch {
			case err == nil:
				probe.Close()
				slog.Info("daemon not found, spawning background process")
				if err := spawnHiddenDaemon(); err != nil {
					reportFailure(fmt.Sprintf("June could not start its daemon: %v", err))
					return false
				}
				spawned = true
				// A port let go of late in the wait still gives the daemon spawned onto it the whole wait to come up.
				deadline = time.Now().Add(daemonWait)
			case !portInUse(err):
				// A port the daemon cannot bind, such as one inside a range Hyper-V or WSL reserved, would make a spawned daemon exit at once; trying the bind here says what is actually wrong.
				reportFailure("June could not start its daemon: " + bindFailure(DaemonPort, err))
				return false
			case !looked:
				looked = true
				// Another account's program will never take this account's requests, June or not, so there is nothing to wait for.
				if message, other := otherAccountProblem(DaemonPort, nil); other {
					reportFailure(message)
					return false
				}
				var known bool
				_, holder, holderIsJune, known = describePortHolder(DaemonPort)
				// A program that is plainly not June will never answer as June, so there is nothing to wait ten seconds for.
				if known && !holderIsJune {
					reportFailure(fmt.Sprintf("June could not start its daemon: port %s is held by %s, which is not June. Close it and run june again; JUNE_PORT starts June on another port, but the desktop window only talks to 6942.", DaemonPort, holder))
					return false
				}
			}
		}
		if !time.Now().Before(deadline) {
			// A June started moments ago holds the port while it opens its store and starts its services, before it answers, and at a cold sign-in that can take longer than daemonWait, for the June the sign-in entry started as for one this process spawned; a click on June then called it stuck. It is given until it has been running daemonColdStart, once.
			if !extended {
				extended = true
				if later, ok := youngJuneDeadline(); ok && later.After(deadline) {
					slog.Info("the June holding the port started moments ago; waiting for it to finish starting", "until", later)
					deadline = later
					continue
				}
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
		if pingDaemon() {
			return true
		}
	}
	waited := int(time.Since(began).Round(time.Second).Seconds())
	if spawned {
		reportFailure(fmt.Sprintf("June started its daemon but it did not answer within %d seconds; why is in %s.", waited, filepath.Join(config.DataDir(), "june.log")))
		return false
	}
	switch {
	case holderIsJune:
		reportFailure(fmt.Sprintf("June could not start its daemon: port %s is held by %s, a June daemon that has not answered for %d seconds; %s, then run june again.", DaemonPort, holder, waited, stopDaemonHint))
	case holder != "":
		reportFailure(fmt.Sprintf("June could not start its daemon: port %s is held by %s, which has not answered as June for %d seconds. If it is a stuck June, %s; otherwise close it. Then run june again.", DaemonPort, holder, waited, stopDaemonHint))
	default:
		reportFailure(fmt.Sprintf("June could not start its daemon: port %s is held by a program that has not answered as June for %d seconds, a stuck June daemon or another program. If it is June, %s; otherwise close it. Then run june again.", DaemonPort, waited, stopDaemonHint))
	}
	return false
}

// youngJuneDeadline is when to stop waiting for the June holding the port, if it started so recently that it may still be starting (see daemonColdStart). Output: the moment it has been running that long, and false when the holder is not June or when it started cannot be told.
func youngJuneDeadline() (time.Time, bool) {
	pid, _, isJune, _ := describePortHolder(DaemonPort)
	if !isJune {
		return time.Time{}, false
	}
	started, ok := processStarted(pid)
	if !ok {
		return time.Time{}, false
	}
	return started.Add(daemonColdStart), true
}

// describePortHolder names whoever is listening on port, for a message about a port June cannot have (see portHolder). Output: its pid, 0 when none was found; who, as "node.exe (pid 1234)" or "pid 1234" when the program would not give its name; whether that program is June, judged by its file name; and whether a name was found at all, without which nothing can be said either way.
func describePortHolder(port string) (pid uint32, who string, isJune, known bool) {
	pid, name, ok := portHolder(port)
	if !ok {
		return 0, "", false, false
	}
	if name == "" {
		return pid, fmt.Sprintf("pid %d", pid), false, false
	}
	return pid, fmt.Sprintf("%s (pid %d)", name, pid), juneProgram(name), true
}

// juneProgram reports whether a program's file name is June's: june, june.exe, junew.exe and the like.
func juneProgram(name string) bool {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
	return strings.HasPrefix(base, "june")
}

// otherAccountProblem is what to tell the user when port is held by a program running for another account signed in to this computer (see portHeldByOtherAccount). Input: the port, and what says whether that program has answered /ping as June, nil for not, which is asked only when the program's name cannot be read. Output: the message, and false when the port is not another account's.
func otherAccountProblem(port string, answeredAsJune func() bool) (string, bool) {
	account, program, other := portHeldByOtherAccount(port)
	if !other {
		return "", false
	}
	// A name that is read is the answer; a program that is not called June but answers /ping is not trusted to be June.
	isJune := juneProgram(program) || program == "" && answeredAsJune != nil && answeredAsJune()
	return otherAccountMessage(port, account, program, isJune), true
}

// otherAccountMessage says that June's port is held by another account signed in to this computer, and what to do about it. Input: the port; that account's name, "" when the system would not say; the program's file name, "" when it cannot be read; and whether that program is known to be June. Output: the sentences for the user.
// Every June listens on the one port, which every account on the computer shares, so the first June to start has it and another account's June cannot; the window only talks to that port. The usual advice, to quit June from the tray or end it from a terminal, cannot reach a program running for someone else.
func otherAccountMessage(port, account, program string, isJune bool) string {
	who, where, them := "another account signed in to this computer", "that account", "that account"
	if account != "" {
		who, where, them = account+", who is also signed in to this computer", account+"'s account", account
	}
	switch {
	case isJune:
		return fmt.Sprintf("June is already running for %s. For now only one person on a computer can use June at a time, because every June listens on the same port (%s). To use June here, switch to %s and quit June from its tray icon (or sign %s out), then open June again.", who, port, where, them)
	case program != "":
		return fmt.Sprintf("June could not start: port %s, which every June listens on, is held by %s, a program running for %s. It is not June, and only that account can close it. To use June here, switch to %s and close %s (or sign %s out), then open June again.", port, program, who, where, program, them)
	}
	return fmt.Sprintf("June could not start: port %s, which every June listens on, is held by a program running for %s, most likely their June. To use June here, switch to %s and quit June from its tray icon (or sign %s out), then open June again.", port, who, where, them)
}

// checkDaemonBuildMismatch GETs url (the daemon's /ping) and compares its build identity against this process's own (see sameBuild). They disagree when the daemon has been running since before the file it was started from was last overwritten — i.e. a rebuild happened and the daemon is still running the old code — or when it was started from another build altogether. Which of the two is newer is not known, so the warning says "different", not "older". Returns "" (no warning) on any failure or an empty/matching body — this is a diagnostic, never a reason to block startup.
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
	if daemonID == "" || sameBuild(daemonID) {
		return ""
	}
	return "the running daemon is a different build from this june — " + stopDaemonHint + ", then relaunch"
}

// sameBuild reports whether a daemon whose /ping answered daemonID runs the build this process would start. Input: the daemon's identity. Output: true when it is this executable as it stands on disk, or the other program of the Windows pair beside it, unchanged since the daemon started and built from the same source as this one.
// The Windows package ships one program twice: june.exe, with a console, which the user types, and junew.exe, without one, which the login entry runs (see autostart_windows.go). They are two files with their own sizes and times, so a daemon started at login never matched the june.exe asking it, and every `june` warned of an older build that was not.
func sameBuild(daemonID string) bool {
	if daemonID == buildIdentity {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	twin := buildTwin(exe)
	if twin == "" || daemonID != fileIdentity(twin) {
		return false
	}
	// The twin's identity matching says the daemon runs what is on disk now; the build info says that is the same source as this program, so a june.exe rebuilt alone from another commit beside an old junew.exe still warns. Two builds of the same uncommitted tree share the build info too, but the release script always builds the pair together, so only a pair built by hand can fall in that gap.
	theirs, err := buildinfo.ReadFile(twin)
	if err != nil {
		return false
	}
	ours, ok := debug.ReadBuildInfo()
	return ok && sourceKey(ours) == sourceKey(theirs)
}

// buildTwin is the path of the other program of the Windows pair beside exe: junew.exe for june.exe, and june.exe for junew.exe. Output: "" for any other name, and everywhere but Windows, where the daemon and the command are always the one file.
func buildTwin(exe string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir, name := filepath.Split(exe)
	switch strings.ToLower(name) {
	case "june.exe":
		return filepath.Join(dir, "junew.exe")
	case "junew.exe":
		return filepath.Join(dir, "june.exe")
	}
	return ""
}

// sourceKey is what two builds share when they were built from the same source by the same toolchain: the Go version, the module version, and the version-control revision, commit time and dirty flag. The linker flags are left out, since they are exactly what makes junew.exe windowless.
func sourceKey(info *debug.BuildInfo) string {
	key := info.GoVersion + " " + info.Main.Version
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision", "vcs.time", "vcs.modified":
			key += " " + s.Key + "=" + s.Value
		}
	}
	return key
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

// buildIdentity is computed once, here at package init, from the currently-running executable's own bytes (see fileIdentity) — not lazily on every /ping call. A daemon process that's been running since before a rebuild must keep reporting the OLD binary's identity even after the file on disk has been overwritten with a new build; recomputing per-request would just report whatever's CURRENTLY on disk, indistinguishable from a fresh build and defeating the whole point (see cmd/root.go's checkDaemonBuildMismatch).
var buildIdentity = computeBuildIdentity()

// computeBuildIdentity resolves the path to the currently-running executable and fingerprints it. No build tooling changes (no injected version/ldflags) — just what's already on disk.
func computeBuildIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	return fileIdentity(exe)
}

// fileIdentity fingerprints path by its size and a CRC-32C of its bytes, "unknown" if it cannot be read. Extracted from computeBuildIdentity for testability — os.Executable() itself isn't something a test can point at a fixture file.
// The bytes rather than the modification time, because the question is whether two files are the same build: a byte-identical copy of june.exe made by a plain cp, an installer or a download gets a new time, and the size-and-time identity called the daemon it was identical to "an older build". CRC-32C rather than SHA-256 because every june start pays for it and nothing here is adversarial: measured on 2026-10-03, a 38 MB june.exe took about 12 ms from the file cache, where a program just started always is, against SHA-256's 43, and about 60 ms read cold.
func fileIdentity(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sum := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	n, err := io.Copy(sum, f)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d-%08x", n, sum.Sum32())
}
