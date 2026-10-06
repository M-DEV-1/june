package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/config"
	"june/internal/ipc"
	"june/internal/ipctoken"
	"june/internal/lifecycle"
)

// June restarts itself rather than reloading in place: the Gemini key is read once at start and threaded into about ten constructors, and the embedding and local text engines are built there too, so a key saved in first-run setup or a model installed from the window only takes effect in a fresh process. A restart is therefore the ordinary shutdown — window, services, tray — with a replacement process started just before it, which waits for this one to exit.

// restartWanted is set once a restart is under way and no quit has overridden it. The shutdown reads it to keep the window up through its slow steps, and runDaemonLifecycle to pick the exit code.
var restartWanted atomic.Bool

// quitWanted is set by a quit, and a restart asked for after it is refused: the installer stops June with a quit, and a restart setup had put off until a download finished must not bring June back underneath it.
var quitWanted atomic.Bool

// shuttingDown is set when the daemon's shutdown begins, whatever began it: a quit, a restart, a logoff.
var shuttingDown atomic.Bool

// restartMu makes starting a restart and a quit one at a time, so a quit never runs between a replacement being started and its being recorded in replacement, which would leave that replacement waiting to take over from a June the user had quit.
var restartMu sync.Mutex

// replacement is the daemon started to take over from this one, waiting for this process to exit. It is kept rather than released so a quit that comes after the restart can stop it. Guarded by restartMu.
var replacement *os.Process

// restartDaemon starts a restart: the replacement first, then this daemon's shutdown. Input: whether the replacement shows the window. Output: nil once the restart is under way, or why it is not, with this daemon still running. Set by runDaemonLifecycle before the daemon starts, and nil in any process that is not the daemon.
var restartDaemon func(reopen bool) error

// errShuttingDown is a restart asked for while the daemon is already quitting or shutting down for another reason, such as a logout.
var errShuttingDown = errors.New("June is already shutting down")

// openWindowOnStart is whether this daemon shows its window as soon as the window is listening: set by --open, which a click on June passes, and by the marker a restart leaves for its replacement. Written by runRoot before the daemon starts and only read after.
var openWindowOnStart bool

// eventServer is the daemon's event stream, for a restart to tell the window it is coming; nil until startDaemonServices has made it.
var eventServer atomic.Pointer[ipc.Server]

// restartAsks counts the requests in flight that can restart June on the user's behalf, POST /restart and setup's own, and restartAskEnded is when the last one finished, in Unix nanoseconds. Together they tell a restart the user has just asked for, whose window should come back, from one setup put off until a download finished, which must leave a hidden window hidden rather than pop it up minutes later.
var (
	restartAsks     atomic.Int32
	restartAskEnded atomic.Int64
)

// restartAskGrace is how long after such a request has ended a restart still counts as asked for by it. The restart hook runs on a goroutine of its own, which can start just after the request that set it off has returned; a restart setup put off asks again only every five seconds, so it never falls inside this.
const restartAskGrace = 2 * time.Second

// predecessorWait is how long a replacement waits for the daemon it replaces to exit and let go of the port. The replacement is started before that daemon shuts down (see beginRestart), so this covers the whole shutdown: its steps are bounded one by one at just under two minutes in all, the activity flush alone at forty-five seconds since it makes a model call, and the wait leaves room over that for the process to exit. An idle daemon is gone in a second or two.
const predecessorWait = 150 * time.Second

// quitWait is how long `june --quit` waits for the daemon to be gone. The installer runs it before replacing the files and falls back to taskkill after this, so it is short: a daemon still flushing past it loses only the summary of its last buffer, as the raw activity is already in the store.
const quitWait = 10 * time.Second

// systemdRestartCode is what the daemon exits with to be restarted by a systemd unit that asked for that (see supervisedBySystemd): any non-zero code counts as a failure to Restart=on-failure, and 75 (EX_TEMPFAIL) says "try again" to anyone reading the journal.
const systemdRestartCode = 75

// windowListenWait bounds how long --open waits for the window to connect to the event stream. A fresh WebView2 profile on a machine that has just installed the runtime can take tens of seconds to bring up its first page.
const windowListenWait = time.Minute

// openMarkerName is the file in the data directory that tells the next daemon to show its window. A restart leaves it rather than passing --open, because the replacement is started before the shutdown and reads the marker only once this daemon has exited, so a window that asks during the shutdown can still be brought back; under systemd, which starts the replacement with the unit's own command line, it is the only way at all.
const openMarkerName = "open-window"

// pauseMarkerName is the file in the data directory that keeps observation paused across a restart, a quit and a reboot for as long as the user has it paused. Restarts are ordinary now — saving a key, installing a local feature, an update — and each one used to end a pause the user had chosen, without a word. It is empty for a pause until the user resumes, and holds the RFC 3339 time a pause for a while ends (see pauseControl).
const pauseMarkerName = "paused"

// lifecycleEventID is the id every "lifecycle" event carries.
const lifecycleEventID = "june"

// errAlreadyServed is awaitPredecessor finding another June answering on the port once the daemon it replaces has gone, such as one a click on June started while the restart was under way. That June serves, so the replacement is not needed.
var errAlreadyServed = errors.New("another June daemon is already answering on its port")

// runDaemonLifecycle runs the daemon until it is quit or asked to restart. Input: the root context, the telemetry shutdown handed on to runDaemon, and whether --open was given. Output: the process exit code.
func runDaemonLifecycle(ctx context.Context, shutdownObs func(context.Context) error, open bool) int {
	ctx, stopDaemon := context.WithCancel(ctx)
	defer stopDaemon()
	restartDaemon = func(reopen bool) error {
		if err := beginRestart(ctx, reopen); err != nil {
			return err
		}
		// Cancelling the root context is exactly what SIGTERM does, so a restart takes the shutdown path every logout already exercises.
		stopDaemon()
		return nil
	}
	lifecycle.SetHooks(func() {
		// beginRestart logs and announces its own failure, which the window that asked hears as an event.
		restartDaemon(restartAskedByUser())
	}, func() {
		beginQuit()
		stopDaemon()
	})
	// The marker is taken even when --open already shows the window, so one a restart left and nothing took up — its replacement would not start, or was killed — does not show the window unasked at the next login.
	marker := takeOpenMarker()
	openWindowOnStart = open || marker

	if err := runDaemon(ctx, shutdownObs); err != nil {
		// A bind failure is not a crash, and runDaemon has already said why on stderr and in the log; it used to be logged a second time as "daemon crashed", an error line for every second start beside a running daemon.
		var op *net.OpError
		if !errors.As(err, &op) || op.Op != "listen" {
			slog.Error("daemon stopped with an error", "error", err)
		}
		// Non-zero, so a supervisor calls the start a failure. The commonest cause is a second daemon finding the port held by the first, and exiting 0 there meant systemd and june-restart both reported a restart that never happened.
		return 1
	}
	if !restartWanted.Load() {
		return 0
	}
	if supervisedBySystemd() {
		slog.Info("restarting: exiting for systemd to start June again", "code", systemdRestartCode)
		return systemdRestartCode
	}
	slog.Info("restarting: the replacement daemon takes over once this one has exited")
	return 0
}

// beginRestart starts the replacement for a restart, unless one is already under way or the daemon is quitting or shutting down. Input: the daemon's context, and whether the replacement shows the window. Output: nil when the restart is under way, errShuttingDown, or why the replacement could not be started.
// The replacement is started before anything is torn down, and waits for this process to exit: one that cannot be started — its program held by antivirus, or mid-update — then calls the restart off with June still running, where starting it after the shutdown left no June at all.
func beginRestart(ctx context.Context, reopen bool) error {
	restartMu.Lock()
	defer restartMu.Unlock()
	switch {
	case restartWanted.Load():
		// Asked again while the restart is under way, by the window the user is looking at, so the replacement brings it back.
		if reopen {
			leaveOpenMarker()
		}
		return nil
	case quitWanted.Load() || ctx.Err() != nil:
		return errShuttingDown
	}
	if reopen {
		leaveOpenMarker()
	}
	if !supervisedBySystemd() {
		p, err := spawnReplacement(os.Getpid())
		if err != nil {
			if reopen {
				takeOpenMarker()
			}
			slog.Error("restart called off: the replacement daemon could not be started, so this one keeps running", "error", err)
			announce("restart_failed", map[string]string{"error": err.Error()}, true)
			return fmt.Errorf("the replacement daemon could not be started: %w", err)
		}
		replacement = p
	}
	restartWanted.Store(true)
	slog.Info("restarting", "reopen_window", reopen)
	announce("restarting", map[string]bool{"reopen": reopen}, false)
	return nil
}

// beginQuit records a quit. A quit asked for during a restart is the later wish, so it wins: the replacement waiting to take over is stopped, and the marker that would have shown its window is taken back.
func beginQuit() {
	restartMu.Lock()
	defer restartMu.Unlock()
	quitWanted.Store(true)
	if !restartWanted.Swap(false) {
		return
	}
	if replacement != nil {
		if err := replacement.Kill(); err != nil {
			slog.Warn("could not stop the replacement daemon a restart had started; it takes over once this one has quit", "pid", replacement.Pid, "error", err)
		}
		replacement = nil
	}
	takeOpenMarker()
	slog.Info("quit asked for during a restart, so June quits instead")
}

// goingAway reports whether this daemon is shutting down with no June to follow it: a quit, or a logoff. A restart is not, since its replacement follows, and it keeps its window up through the shutdown.
func goingAway() bool {
	return quitWanted.Load() || shuttingDown.Load() && !restartWanted.Load()
}

// windowRoute wraps /window for a daemon on its way out, and for one whose window is not listening yet. Input: ipc's own handler, and what keeps an open for a window that is not listening (see openWhenListening), nil for none. Output: the handler to register.
// A quitting daemon has closed its window but answers on its port for up to a minute or more while it finishes, so an instruction to show the window is answered 409 {"error":"quitting"} rather than taken and lost, and the client starts the June that follows instead (see startAfterQuit). During a restart the window is still up, saying "Restarting June…", but the one the user is left with is the replacement's, so an open asked for then leaves the marker that shows that one.
func windowRoute(h http.HandlerFunc, keepOpen func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if goingAway() {
			writeLifecycleJSON(w, http.StatusConflict, map[string]string{"error": "quitting", "message": "June is quitting."})
			return
		}
		if r.URL.Query().Get("action") == "open" {
			reopenAfterRestart()
			if keepOpen != nil {
				keepOpen()
			}
		}
		h(w, r)
	}
}

// openPending is set while an open asked for before the window was listening waits for it (see openWhenListening), so the client's repeated tries start one wait between them.
var openPending atomic.Bool

// openWhenListening keeps an open asked for while no window is listening on the event stream, and shows the window once one connects, within windowListenWait. Input: the daemon's context and its ipc server.
// The instruction itself is broadcast to whoever is listening, which then is nobody: a click on June at sign-in, while the June the sign-in entry started was still bringing its window up, found the daemon, sent its few tries over a second and a half into the void, and no window came. The window is started only once the daemon serves, and WebView2's first page at a cold sign-in takes longer than that.
func openWhenListening(ctx context.Context, server *ipc.Server) {
	if server.Subscribed(0) || !openPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer openPending.Store(false)
		// Setup is passed as done: this wait is for the one open asked for, and a June still waiting on setup already shows every window that connects (see startDaemonServices).
		openWhenWindowListens(ctx, server, true, func() bool { return true })
	}()
}

// reopenAfterRestart leaves the open marker when a restart is under way. Under restartMu, so a quit arriving at the same moment takes the marker back rather than running before it is written.
func reopenAfterRestart() {
	restartMu.Lock()
	defer restartMu.Unlock()
	if restartWanted.Load() {
		leaveOpenMarker()
	}
}

// restartAskedByUser reports whether a request the user made can have set off the restart starting now (see restartAsks).
func restartAskedByUser() bool {
	if restartAsks.Load() > 0 {
		return true
	}
	ended := restartAskEnded.Load()
	return ended != 0 && time.Since(time.Unix(0, ended)) < restartAskGrace
}

// countRestartAsk wraps a route that can restart June on the user's behalf, so a restart it sets off is known to be one the user is watching for. Input: the handler. Output: the wrapped handler.
func countRestartAsk(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		restartAsks.Add(1)
		defer func() {
			// The end is stored before the count drops, so a hook that finds the count at zero always finds this request's end.
			restartAskEnded.Store(time.Now().UnixNano())
			restartAsks.Add(-1)
		}()
		h(w, r)
	}
}

// announce puts a "lifecycle" event on the window's stream: "restarting" with {"reopen": bool}, saying whether the replacement shows its window, or "restart_failed" with {"error": string}. Nothing is sent before the stream exists.
func announce(text string, detail any, failed bool) {
	server := eventServer.Load()
	if server == nil {
		return
	}
	d, _ := json.Marshal(detail)
	server.Publish(ipc.Event{ID: lifecycleEventID, Type: "lifecycle", Text: text, Detail: string(d), Failed: failed})
}

// restartRefusals is the sentence POST /restart answers with for each word restartBlocker gives.
var restartRefusals = map[string]string{
	"recording":   "Restarting now would stop the meeting recording. Restart once the meeting is over.",
	"processing":  "June is still writing up the meeting. Restart in a few minutes, once the notes are ready.",
	"downloading": "Restarting now would stop the download. Restart once it finishes, or cancel it first.",
	"voice":       "Restarting now would end the voice chat. Restart once you're done talking.",
	"dictating":   "Restarting now would lose what you're dictating. Restart once it's typed in.",
	"acting":      "Restarting now would stop the task June is doing for you. Finish or stop it first, then restart.",
	"asking":      "Restarting now would cut off the answer June is writing. Restart once it's done.",
}

// lifecycleRoutes answers POST /quit and POST /restart. Input: what a restart now would cut short, as restartBlocker gives it. Output: the handlers keyed by ServeMux pattern, for registerRoutes to put behind the IPC token.
// Both answer 202 with this process's pid, so a caller can tell the daemon it asked from the one that replaces it, and `june --quit` can wait for that very process to be gone rather than only for the port. The shutdown lets the reply finish: the server is shut down only after the window, the recording's close, the activity flush and the local-feature stop, and its Shutdown waits for handlers in flight.
func lifecycleRoutes(blocker func() string) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /quit": func(w http.ResponseWriter, r *http.Request) {
			writeLifecycleJSON(w, http.StatusAccepted, map[string]any{"pid": os.Getpid()})
			slog.Info("quit requested over IPC")
			lifecycle.RequestQuit()
		},
		"POST /restart": func(w http.ResponseWriter, r *http.Request) {
			// A restart closes a recording, a meeting's write-up or a download, and the replacement does not take them up again, so it is refused while one runs; the window offers it again later. A quit is never refused this way, because the installer has to be able to stop June. Asked again while a restart is already under way it is not refused either, since all it does then is bring the window back.
			if code := blocker(); code != "" && !restartWanted.Load() {
				slog.Info("refused a restart that would cut something short", "busy", code)
				writeLifecycleJSON(w, http.StatusConflict, map[string]string{"error": code, "message": restartRefusals[code]})
				return
			}
			if restartDaemon == nil {
				writeLifecycleJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_daemon", "message": "This copy of June can't restart itself."})
				return
			}
			// The restart is started here rather than through lifecycle.RequestRestart, so a replacement that will not start is answered to the window that asked instead of only announced.
			if err := restartDaemon(true); err != nil {
				status, code := http.StatusInternalServerError, "restart_failed"
				if errors.Is(err, errShuttingDown) {
					status, code = http.StatusConflict, "shutting_down"
				}
				writeLifecycleJSON(w, status, map[string]string{"error": code, "message": "June couldn't restart: " + strings.TrimSuffix(err.Error(), ".") + "."})
				return
			}
			writeLifecycleJSON(w, http.StatusAccepted, map[string]any{"restarting": true, "pid": os.Getpid()})
			slog.Info("restart requested over IPC")
		},
	}
}

// writeLifecycleJSON writes v as a JSON body with status.
func writeLifecycleJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// registerRoutes puts every handler a package hands back on mux behind the IPC token. Input: the mux, the auth wrapper, and the handlers keyed by ServeMux pattern. A pattern the mux refuses — one that conflicts with a route already there — costs that route, logged, rather than the whole daemon: these maps come from packages that add routes on their own schedule, and HandleFunc panics on a conflict. The patterns go in sorted so the same conflict always costs the same route.
func registerRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, routes map[string]http.HandlerFunc) {
	for _, pattern := range slices.Sorted(maps.Keys(routes)) {
		func() {
			defer func() {
				if err := recover(); err != nil {
					slog.Error("an IPC route could not be registered and is not served", "pattern", pattern, "error", err)
				}
			}()
			mux.HandleFunc(pattern, auth(routes[pattern]))
		}()
	}
}

// openWhenWindowListens shows the window when it connects to the event stream: once when the start asked for it, and each time a window connects for as long as first-run setup is not done. Input: the daemon's context, its ipc server, whether the start asked for the window (--open or the restart's marker), and what says whether setup is done now. Output: none; it returns when the context ends, once setup is done and a window asked for has been shown, or with a log line when one asked for after setup has not connected within windowListenWait.
// The instruction is sent only after a subscriber is there, because one sent before is heard by nobody; the client's blind five tries over two seconds were too short for a window starting cold beside a daemon that had itself only just started.
// Until setup is done there is no deadline, and a window that connects again is shown again: the window starts hidden, so a first WebView2 start slower than any deadline, or a window the supervisor restarted after a crash, left a June waiting on setup with no way to reach a user who may never have seen its tray icon. A window the user hid keeps its streams, so it is not shown again behind their back.
func openWhenWindowListens(ctx context.Context, server *ipc.Server, asked bool, setupDone func() bool) {
	deadline := time.NewTimer(windowListenWait)
	defer deadline.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	listening := false
	for {
		connected := server.Subscribed(0)
		if connected && !listening && (asked || !setupDone()) {
			asked = false
			tellOpen(ctx, server)
		}
		listening = connected
		if !asked && setupDone() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			if setupDone() {
				slog.Warn("the desktop window did not connect in time to be shown; it starts hidden, so open it from the tray", "waited", windowListenWait)
				return
			}
		case <-poll.C:
		}
	}
}

// tellOpen sends the window the instruction to show itself. It is sent three times, half a second apart, because the window's page and its native layer each hold a stream of their own and the first to connect may not be the one that acts on it; a window already showing ignores the repeats. Input: the daemon's context and its ipc server.
func tellOpen(ctx context.Context, server *ipc.Server) {
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		server.Tell("open")
	}
}

// awaitPredecessor waits for the daemon this one replaces to exit and let go of the port. Input: the old daemon's pid and how long to wait. Output: nil once both have happened; errAlreadyServed when another June took the port meanwhile; otherwise an error saying which did not happen, after which the caller starts regardless and a port still held fails the bind with its usual message.
func awaitPredecessor(pid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	if !waitProcessExit(pid, bound) {
		return fmt.Errorf("the daemon being replaced (pid %d) was still running after %s", pid, bound)
	}
	for {
		probe, err := net.Listen("tcp", "127.0.0.1:"+DaemonPort)
		if err == nil {
			probe.Close()
			return nil
		}
		// Only another socket holding the port is worth waiting out; a port the system refuses fails the same way for ever.
		if !portInUse(err) || !time.Now().Before(deadline) {
			return fmt.Errorf("port %s is still not free: %w", DaemonPort, err)
		}
		// The daemon being replaced has exited, so whoever answers is a June started some other way, and waiting on would only end in a failed bind.
		if pingOnce() {
			return errAlreadyServed
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// quitDaemon is `june --quit`: it asks a running daemon to shut down and waits for it to be gone. Output: the exit code, 0 when no daemon is running by the end (including when none was to begin with), 1 when one still is.
// The installer runs this before it replaces June's files, and Windows refuses to overwrite a program that is running, so "gone" means the process has exited and not only that /ping stopped answering.
func quitDaemon() int {
	var pid int
	if pingDaemon() {
		var err error
		if pid, err = postQuit(); err != nil {
			// The installer quits this account's June before replacing its files. Another account's June runs from that account's own copy and is not this one to stop, and this account's June cannot be running while that one holds the port.
			if account, program, other := portHeldByOtherAccount(DaemonPort); other {
				if account == "" {
					account = "another account"
				}
				what := "the June on port " + DaemonPort
				if program != "" && !juneProgram(program) {
					what = program + ", which holds port " + DaemonPort + ","
				}
				fmt.Printf("June is not running for this account; %s is running for %s.\n", what, account)
				return 0
			}
			fmt.Fprintf(os.Stderr, "June's daemon answers /ping but did not take the instruction to quit (%v); %s.\n", err, stopDaemonHint)
			return 1
		}
	} else if pid = quittingDaemon(); pid == 0 {
		fmt.Println("June is not running.")
		return 0
	}
	// A daemon already quitting — from its tray, say — is waited for the same as one this asked: it still runs, and still holds its files, until it exits.
	deadline := time.Now().Add(quitWait)
	// The port goes first, when the server shuts down; the process follows once the store and the children are closed.
	gone := true
	for pingOnce() {
		if !time.Now().Before(deadline) {
			gone = false
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if gone && pid > 0 {
		gone = waitProcessExit(pid, time.Until(deadline))
	}
	if !gone {
		fmt.Fprintf(os.Stderr, "June was asked to quit but is still running after %d seconds; %s.\n", int(quitWait.Seconds()), stopDaemonHint)
		return 1
	}
	fmt.Println("June has quit.")
	return 0
}

// postQuit sends the authenticated POST /quit. Output: the pid the daemon answered with, 0 when it named none, or the reason the request failed.
func postQuit() (int, error) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+DaemonPort+"/quit", nil)
	if err != nil {
		return 0, err
	}
	ipctoken.Attach(req, ipctoken.DefaultPath())
	resp, err := daemonRequestClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return 0, errors.New("it answered " + resp.Status + ": the token in " + ipctoken.DefaultPath() + " is not the running daemon's")
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		// A daemon from before /quit existed; the installer's taskkill fallback is what stops that one.
		return 0, errors.New("it answered " + resp.Status + ", so it is an older June that cannot be asked to quit")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return 0, errors.New("it answered " + resp.Status)
	}
	var body struct {
		Pid int `json:"pid"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	return body.Pid, nil
}

// quittingDaemon asks /ping whether the daemon on the port is quitting (see pingHandler). Output: that daemon's pid when it says so, 0 otherwise.
func quittingDaemon() int {
	resp, err := daemonPingClient.Get("http://127.0.0.1:" + DaemonPort + "/ping")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return quittingPid(body)
}

// quittingPid reads the pid out of the body a quitting daemon answers /ping with. Output: the pid, 0 when the body is not that answer.
func quittingPid(body []byte) int {
	var answer struct {
		Quitting bool `json:"quitting"`
		Pid      int  `json:"pid"`
	}
	if json.Unmarshal(body, &answer) != nil || !answer.Quitting {
		return 0
	}
	return answer.Pid
}

// startAfterQuit is a click on June that finds the daemon quitting: it starts the June that follows, which waits for that one to exit and then shows its window, the way a restart's replacement does. Input: the quitting daemon's pid. Output: the exit code.
// Showing the quitting daemon's window cannot work, as it is already closed, and starting a daemon now would only fail to bind the port that daemon holds until it exits, which after a busy hour can be a minute or more.
func startAfterQuit(pid int) int {
	p, err := spawnReplacement(pid, "--open")
	if err != nil {
		reportFailure(fmt.Sprintf("June is still quitting and couldn't start again (%v). Try again in a minute.", err))
		return 1
	}
	slog.Info("the daemon is quitting; started the one that follows it", "quitting", pid, "pid", p.Pid)
	p.Release()
	fmt.Println("June is quitting. It will start again and show its window when it's done.")
	return 0
}

// pingOnce is one /ping with the client's short timeout, for a loop that does its own retrying. Output: true when a daemon answered 200.
func pingOnce() bool {
	resp, err := daemonPingClient.Get("http://127.0.0.1:" + DaemonPort + "/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// leaveOpenMarker writes the marker that tells the next daemon to show its window. Best effort: without it the window comes back hidden, which the tray can still open.
func leaveOpenMarker() {
	if err := os.WriteFile(filepath.Join(config.DataDir(), openMarkerName), nil, 0600); err != nil {
		slog.Warn("could not leave the marker that reopens the window after the restart", "error", err)
	}
}

// takeOpenMarker removes the marker leaveOpenMarker wrote. Output: true when there was one.
func takeOpenMarker() bool {
	return os.Remove(filepath.Join(config.DataDir(), openMarkerName)) == nil
}
