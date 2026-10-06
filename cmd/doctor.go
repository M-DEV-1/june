package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"

	"june/internal/agent"
	"june/internal/components"
	"june/internal/config"
	"june/internal/ipc"
	"june/internal/ipctoken"
	"june/internal/recorder"
	"june/internal/util"
	"june/internal/window"
)

// doctorCheck is one line of the readiness report: what was checked, whether it is ready, what was found, and what to do when it is not.
type doctorCheck struct {
	Name, Detail, Fix string
	OK                bool
	// Required marks a check June cannot run without — the session bus, a brain, the desktop window and its runtime, the daemon — as against one whose failure turns a single feature off, such as meeting transcription or computer use. It decides doctor's exit status (see doctorExit).
	Required bool
}

// doctorCmd is "june doctor": one report over everything computer use stands on and every piece June runs locally, with the first blocker and its fix named at the end, so a desk where clicks do not land or a meeting that never transcribes can be read in one go instead of from the log.
// Its exit status tells "June cannot run here" (1) from "June runs, with a feature off" (2), so an installer or a script can act on the answer without parsing the text. Exiting 1 for any failed check made every first install fail on its last line: a GNOME desk cannot load the extension until the next login, and whisper and the diarizer are installed separately, if at all.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Report whether this desk is ready for June to see and drive the screen, and which local pieces are missing",
	Long:  "Report whether this desk is ready for June to see and drive the screen, and which local pieces are missing.\n\nExit status: 0 when everything is in place; 1 when June cannot run here (no session bus, no brain that can answer, no desktop window or its runtime, or no daemon answering as this build); 2 when June runs but a feature is off, such as meeting transcription or computer use.",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Each line is printed as soon as its check has an answer, so a slow check shows as the line that has not appeared yet rather than as a terminal with nothing on it.
		checks := runDoctor(ctx, func(c doctorCheck) { fmt.Print(doctorLine(c)) })
		fmt.Print(doctorVerdict(checks))
		if show, _ := cmd.Flags().GetBool("windows"); show {
			// A bound of its own, since the daemon's /brains alone may have spent the first one (see doctorBrainsTimeout).
			listCtx, cancelList := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelList()
			fmt.Print(windowReport(listCtx))
		}
		exitCode = doctorExit(checks)
	},
}

func init() {
	doctorCmd.Flags().Bool("windows", false, "Also list every open window as the shell extension reports it: focus, class, title and frame")
	rootCmd.AddCommand(doctorCmd)
}

// windowReport lists the open windows as the extension reports them, one per line, which is the view every click is placed against. Output: the lines, or one line saying why there are none.
func windowReport(ctx context.Context) string {
	r, err := window.New()
	if err != nil {
		return "\nwindows: " + err.Error() + "\n"
	}
	defer r.Close()
	windows, err := r.List(ctx)
	if err != nil {
		return "\nwindows: " + err.Error() + "\n"
	}
	var b strings.Builder
	b.WriteString("\nwindows:\n")
	for _, w := range windows {
		mark := "  "
		if w.Focused {
			mark = "* "
		}
		fmt.Fprintf(&b, "%s%-18s %4d,%-4d %4dx%-4d %s\n", mark, w.WmClass, w.X, w.Y, w.W, w.H, w.Title)
	}
	return b.String()
}

// runDoctor runs every check against the live desk. Input: a context bounding the bus calls, and emit, called with each check as soon as it is known, in report order. Output: the checks, in the order the report prints them. The desk checks (the session bus, accessibility, screenshot, pointer consent) are about a Linux desktop and run only there; window frames, the brain, the desktop window, the local pieces and the daemon are checked everywhere. Windows reports window frames through Win32 itself, with no extension to install.
func runDoctor(ctx context.Context, emit func(doctorCheck)) []doctorCheck {
	var out []doctorCheck
	add := func(c doctorCheck) {
		out = append(out, c)
		emit(c)
	}
	need := func(c doctorCheck) {
		c.Required = true
		add(c)
	}
	if runtime.GOOS == "linux" {
		desk, ok := deskChecks(ctx)
		for _, c := range desk {
			add(c)
		}
		if !ok {
			return out
		}
	} else {
		add(windowFramesCheck(ctx))
	}
	cfg := config.LoadConfig()
	// The daemon is asked first, because the brain check reads the daemon's own /brains whenever one answers; its line still comes last, since everything above is driven by it.
	daemon, daemonUp := daemonCheck()
	home, _ := os.UserHomeDir()
	need(brainCheck(home, os.Getenv("GEMINI_API_KEY"), daemonUp, cfg))
	need(desktopWindowCheck(cfg.Window))
	for _, c := range localPieceChecks(config.DataDir(), os.Getenv("XDG_RUNTIME_DIR"), cfg.Embed) {
		add(c)
	}
	// Only a running June knows whether its shortcut works; with none running the daemon line below already says what to do.
	if daemonUp {
		add(shortcutCheck(daemonHotkey(doctorPingTimeout)))
	}
	need(daemon)
	return out
}

// shortcutCheck reports whether June has a shortcut that shows its hover, as the running June says, the same way on Windows and Linux. Input: what daemonHotkey answered. Output: the check, which fails, turning only the shortcut off, when another app holds the shortcut or this desktop does not let June set one.
func shortcutCheck(hotkey, status, note string, err error) doctorCheck {
	const fix = "Open June → Settings and choose a shortcut"
	switch {
	case err != nil:
		// The reason is left out: it names the address and Go's own error words, and doctor keeps no log to put it in, so a slog line would land in the same terminal.
		return doctorCheck{Name: "shortcut", Detail: "June didn't answer about its shortcut", Fix: "run june doctor again"}
	case status == ipc.HotkeyOK:
		return doctorCheck{Name: "shortcut", Detail: hotkey + " shows June", OK: true}
	case status == ipc.HotkeyPending:
		return doctorCheck{Name: "shortcut", Detail: "June is still setting up " + hotkey, OK: true}
	case status == ipc.HotkeyUnsupported && note == ipc.WindowOffNote:
		return doctorCheck{Name: "shortcut", Detail: note, Fix: `turn June's window back on: set "window" to true in ` + config.ConfigPath() + ", then restart June"}
	case status == ipc.HotkeyUnsupported:
		return doctorCheck{Name: "shortcut", Detail: note, Fix: "add the shortcut in your desktop's keyboard settings"}
	case note == "":
		note = "June has no shortcut"
	}
	return doctorCheck{Name: "shortcut", Detail: note, Fix: fix}
}

// doctorPingTimeout bounds doctor's /ping, which a healthy daemon answers in milliseconds.
const doctorPingTimeout = 2 * time.Second

// doctorBrainsTimeout bounds doctor's read of the daemon's /brains. It answers from caches in about 150 ms, but once every ten minutes it first re-reads the Claude, Grok and Codex logins over the network, one after another and up to 25 seconds between them; on 2026-10-03 one such read outlasted a five-second bound and doctor fell back to the login files, which is the answer that misses a refused login.
const doctorBrainsTimeout = 15 * time.Second

// daemonCheck asks the daemon's /ping whether it is up, on a client with a deadline of its own. On the default client, a port that accepted the connection and never answered — a wedged daemon, or another program on June's port — hung doctor for ever before it had printed a single line. The connection is tried first on its own, so nothing listening and something listening but silent are told apart. Output: the check, and true when June itself answered, which is when the brain check can read the daemon's /brains.
func daemonCheck() (doctorCheck, bool) {
	addr := "127.0.0.1:" + DaemonPort
	held := func(what string) doctorCheck {
		_, who, isJune, known := describePortHolder(DaemonPort)
		fix := "if it is a stuck June, " + stopDaemonHint + "; otherwise close it. Then run june"
		switch {
		case known && isJune:
			fix = stopDaemonHint + ", then run june"
		case known:
			fix = "close " + who + ", then run june; JUNE_PORT starts June on another port, but the desktop window only talks to 6942"
		}
		if who == "" {
			who = "a stuck June daemon or another program"
		}
		return doctorCheck{Name: "daemon", Detail: "port " + DaemonPort + " " + what + "; it is held by " + who, Fix: fix}
	}
	conn, err := net.DialTimeout("tcp", addr, doctorPingTimeout)
	if err != nil {
		// A free loopback port refuses at once, on Windows as on Linux (measured on 2026-10-03: half a millisecond). A dial that times out instead met a listener that is not accepting, such as a wedged daemon whose listen backlog is full, which on Linux drops the handshake rather than refusing it, so that port is held and not free.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return held(fmt.Sprintf("did not accept a connection within %s", doctorPingTimeout)), false
		}
		return doctorCheck{Name: "daemon", Detail: "nothing is listening on port " + DaemonPort, Fix: "run june"}, false
	}
	conn.Close()
	resp, err := (&http.Client{Timeout: doctorPingTimeout}).Get("http://" + addr + "/ping")
	if err != nil {
		return held("accepts connections but does not answer /ping"), false
	}
	defer resp.Body.Close()
	id, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if pid := quittingPid(id); resp.StatusCode == http.StatusServiceUnavailable && pid > 0 {
		return doctorCheck{Name: "daemon", Detail: fmt.Sprintf("June (pid %d) is quitting and still holds port %s while it finishes", pid, DaemonPort), Fix: "wait for it to exit, then run june"}, false
	}
	if resp.StatusCode != http.StatusOK {
		return held("answers /ping with " + resp.Status + ", which June never does"), false
	}
	// Another account's June answers /ping like this account's would, then refuses everything else, the brain check's /brains included.
	if account, program, other := portHeldByOtherAccount(DaemonPort); other {
		if account == "" {
			account = "another account signed in to this computer"
		}
		if program != "" && !juneProgram(program) {
			return doctorCheck{Name: "daemon", Detail: "port " + DaemonPort + " is held by " + program + ", a program running for " + account + " that is not June, so this account cannot start its own", Fix: "switch to that account and close " + program + ", or sign it out; then run june"}, false
		}
		return doctorCheck{Name: "daemon", Detail: "the June answering on port " + DaemonPort + " is running for " + account + ", so this account cannot use it or start its own", Fix: "switch to that account and quit June from its tray icon, or sign it out; then run june"}, false
	}
	// Answering is not the whole question: after an upgrade the old daemon keeps answering, running the old code, until it is restarted.
	if len(id) > 0 && !sameBuild(string(id)) {
		return doctorCheck{Name: "daemon", Detail: "answering on port " + DaemonPort + ", but it is running a different build from this june", Fix: stopDaemonHint + ", then run june"}, true
	}
	return doctorCheck{Name: "daemon", Detail: "answering on port " + DaemonPort, OK: true}, true
}

// desktopWindowCheck reports whether the desktop window `june` shows is installed, looked for exactly where `june` looks (see windowBinary). Without it `june` exits 1, and doctor used to say everything was in place on a machine where that happened every time. Input: the config's window switch. Output: the check, which passes with the window turned off, since `june` then shows none.
func desktopWindowCheck(want bool) doctorCheck {
	if !want {
		return doctorCheck{Name: "desktop window", Detail: `turned off: "window" is false in ` + config.ConfigPath(), OK: true}
	}
	path, tried, err := windowBinary()
	if err != nil {
		return doctorCheck{Name: "desktop window", Detail: "not installed (looked at: " + strings.Join(tried, ", ") + "); june exits without it", Fix: "set JUNE_WINDOW to the window program, or build it in app/"}
	}
	return doctorCheck{Name: "desktop window", Detail: "at " + path, OK: true}
}

// deskChecks checks what computer use needs from a Linux desktop. Input: a context bounding the bus calls. Output: the checks, and false when there is no session bus, in which case nothing after it can be checked either.
func deskChecks(ctx context.Context) ([]doctorCheck, bool) {
	var out []doctorCheck
	conn, err := dbus.SessionBus()
	if err != nil {
		return append(out, doctorCheck{Name: "session bus", Detail: err.Error(), Fix: "run from a graphical login", Required: true}), false
	}
	// Accessibility: the bus must exist and applications must be told to build their trees, or observe_screen lists nothing.
	var enabled dbus.Variant
	if err := conn.Object("org.a11y.Bus", "/org/a11y/bus").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.a11y.Status", "IsEnabled").Store(&enabled); err != nil {
		out = append(out, doctorCheck{Name: "accessibility bus", Detail: err.Error(), Fix: "the daemon sets org.a11y.Status.IsEnabled at start; start it once"})
	} else if on, _ := enabled.Value().(bool); !on {
		out = append(out, doctorCheck{Name: "accessibility bus", Detail: "reachable, IsEnabled is false", Fix: "start the daemon once; it turns IsEnabled on"})
	} else {
		out = append(out, doctorCheck{Name: "accessibility bus", Detail: "reachable, IsEnabled is true", OK: true})
	}
	// Screenshot: the shell's own screenshot call is what look and the press check use.
	var owned bool
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, "org.gnome.Shell").Store(&owned); err != nil || !owned {
		out = append(out, doctorCheck{Name: "screenshot", Detail: "org.gnome.Shell is not on the bus", Fix: "looks fall back to the portal, which asks for consent"})
	} else {
		out = append(out, doctorCheck{Name: "screenshot", Detail: "gnome-shell screenshot available", OK: true})
	}
	out = append(out, windowFramesCheck(ctx))
	// Pointer: a saved restore token means the RemoteDesktop consent was given once and the session opens silently. What it does not say is how many monitors that grant covers, so the desk's live monitor count is read separately and handed alongside it.
	out = append(out, pointerCheck(filepath.Join(config.DataDir(), "portal-input-token"), monitorCount(ctx, conn)))
	return out, true
}

// windowFramesCheck reports whether open windows can be listed with their frames, which every click is placed against. On Wayland the only source of window positions is the bundled GNOME extension, and only a build that reports frames places clicks. Input: a context bounding the call. Output: the check.
func windowFramesCheck(ctx context.Context) doctorCheck {
	r, err := window.New()
	if err != nil {
		return doctorCheck{Name: "window frames", Detail: err.Error(), Fix: "install the GNOME extension (packaging/gnome-extension) and log in again"}
	}
	defer r.Close()
	windows, err := r.List(ctx)
	switch {
	case err != nil:
		return doctorCheck{Name: "window frames", Detail: "extension not answering: " + err.Error(), Fix: "enable the june@june.local extension and log in again"}
	case !anyFrame(windows):
		return doctorCheck{Name: "window frames", Detail: fmt.Sprintf("%d windows listed, none with a frame", len(windows)), Fix: "the installed extension predates window frames: log out and in to load the new one"}
	default:
		return doctorCheck{Name: "window frames", Detail: fmt.Sprintf("%d windows listed with frames", len(windows)), OK: true}
	}
}

// pointerCheck reports whether pointer and keyboard consent is saved, and, when it is, the desk's live monitor count next to the exact file a re-grant needs deleted. A restore token saved from an older, narrower grant keeps restoring that same grant forever — the portal never asks again on its own — so a click on a monitor the grant does not cover fails deep inside the coordinate mapping. Input: the saved token's path, and the desk's monitor count (-1 when it could not be read). Output: the check.
func pointerCheck(tokenPath string, monitors int) doctorCheck {
	if _, err := os.Stat(tokenPath); err != nil {
		return doctorCheck{Name: "pointer and keyboard", Detail: "no saved consent", Fix: "the first press will show the portal's consent dialog once; allow it"}
	}
	if monitors < 0 {
		return doctorCheck{Name: "pointer and keyboard", Detail: "consent saved; this desk's monitor count could not be read", OK: true}
	}
	return doctorCheck{
		Name:   "pointer and keyboard",
		Detail: fmt.Sprintf("consent saved; this desk has %d monitor(s). A grant saved with fewer keeps restoring that narrower one; if a click on another monitor fails, delete %s and the next press will ask again", monitors, tokenPath),
		OK:     true,
	}
}

// monitorCount asks GNOME's own display config for how many monitors this desk has right now, the same count a fresh RemoteDesktop consent would be granted across. Input: a context bounding the call, and the session bus doctor already opened. Output: the monitor count, or -1 when Mutter's DisplayConfig is not reachable (not GNOME, or no monitors attached).
func monitorCount(ctx context.Context, conn *dbus.Conn) int {
	var serial uint32
	var monitors, logical []interface{}
	var props map[string]dbus.Variant
	err := conn.Object("org.gnome.Mutter.DisplayConfig", "/org/gnome/Mutter/DisplayConfig").
		CallWithContext(ctx, "org.gnome.Mutter.DisplayConfig.GetCurrentState", 0).
		Store(&serial, &monitors, &logical, &props)
	if err != nil {
		return -1
	}
	return len(monitors)
}

// localFeatureFix is how a missing local piece is put right: the window's Local features section downloads, checks and configures each one, so nobody has to place files or edit the config by hand.
const localFeatureFix = "Open June → Settings → Local features → Set up"

// localPieceChecks reports every piece June runs locally and a clean machine may not have: whisper-cli and its model, the Silero voice-activity model, the sherpa-onnx diarizer and its models, the embedding llama-server and its GGUF, and what recording and call detection need from the desktop (see platformChecks). Each line names the exact path it was looked for at, whether June installed what is there or it was put there by hand, and the feature that is off without it.
// Input: the data directory, $XDG_RUNTIME_DIR, and the embed block of the config. Output: one check per piece, in that order.
func localPieceChecks(dataDir, runtimeDir string, embed config.EmbedConfig) []doctorCheck {
	states := components.Status(dataDir)
	how := func(feature string) string {
		switch states[feature] {
		case "managed":
			return " (installed by June)"
		case "external":
			return " (installed by hand)"
		}
		return ""
	}
	var out []doctorCheck
	if bin, err := recorder.WhisperCPPBinary(dataDir); err != nil {
		out = append(out, doctorCheck{Name: "meeting transcription", Detail: err.Error() + "; meeting transcription and voice typing are off", Fix: localFeatureFix + " under Voice typing & meeting transcripts"})
	} else {
		out = append(out, doctorCheck{Name: "meeting transcription", Detail: "whisper-cli at " + bin + " with " + recorder.WhisperModelName() + how("transcribe"), OK: true})
	}
	if vad := recorder.WhisperVADModel(dataDir); !util.Exists(vad) {
		out = append(out, doctorCheck{Name: "voice activity model", Detail: "no Silero model at " + vad + "; voice activity detection is off, and a long, mostly quiet meeting transcribes badly without it", Fix: localFeatureFix + " under Voice typing & meeting transcripts"})
	} else {
		out = append(out, doctorCheck{Name: "voice activity model", Detail: "installed at " + vad + how("transcribe"), OK: true})
	}
	if bin, err := recorder.SherpaBinary(dataDir); err != nil {
		out = append(out, doctorCheck{Name: "speaker diarization", Detail: err.Error() + "; splitting the call into speakers is off", Fix: localFeatureFix + " under Who said what"})
	} else {
		out = append(out, doctorCheck{Name: "speaker diarization", Detail: "diarizer at " + bin + how("speakers"), OK: true})
	}
	embedded := embedCheck(embed)
	if embedded.OK {
		embedded.Detail += how("memory")
	}
	out = append(out, embedded)
	out = append(out, platformChecks(runtimeDir)...)
	return out
}

// embedCheck reports whether the local embedding server and its model are where the config says. The daemon starts no embedder unless both paths are set, and then search matches words only. Input: the config's embed block. Output: the check, naming the config file or the missing path.
func embedCheck(embed config.EmbedConfig) doctorCheck {
	const off = "; local memory search is off and search matches words only"
	if !embed.LocalEnabled() {
		return doctorCheck{Name: "local memory search", Detail: "embed.llama_server and embed.model_path are not both set in " + config.ConfigPath() + off, Fix: localFeatureFix + " under Smarter memory search"}
	}
	for _, p := range []string{embed.LlamaServer, embed.ModelPath} {
		if !util.Exists(p) {
			return doctorCheck{Name: "local memory search", Detail: "nothing at " + p + ", named in " + config.ConfigPath() + off, Fix: localFeatureFix + " under Smarter memory search, or correct the path in " + config.ConfigPath()}
		}
	}
	return doctorCheck{Name: "local memory search", Detail: "llama-server at " + embed.LlamaServer + ", model at " + embed.ModelPath, OK: true}
}

// brainRow is the part of one GET /brains row doctor reads.
type brainRow struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SignedIn   bool   `json:"signed_in"`
	LimitsNote string `json:"limits_note"`
}

// claudeUsageOffNote is the limits_note internal/ipc's brainList puts on the Claude row when reading its usage from the login is turned off in Settings, before a refusal's own note can replace it.
const claudeUsageOffNote = "turned off in Settings"

// askBrainIDs are the GET /brains rows a question can be routed to (internal/agent's router cards that take asks). Grok answers unattended duties only and Ollama has no backend at all, so a machine with only those cannot answer a single question.
var askBrainIDs = map[string]bool{"antigravity": true, "gemini": true, "codex": true, "claude": true}

// brainCheck reports whether June has anything to think with. Everything else doctor checks is about the desk — the buses, the screen, the pointer — and a machine can pass all of it and still not answer a single question, which is exactly what a clean install does before a key or a login is in place. Input: the home directory the CLI login files live under, the Gemini API key as the environment gives it, whether the daemon answered, and the config. Output: the check, naming every brain that can answer and every one whose login is refused, or saying how to give it one.
// A login file on disk is not a working login: an expired Codex login leaves auth.json where it was, and doctor called it a brain while every ask to it was refused — and it never looked for agy at all, the brain this machine was actually answering with. So with the daemon up the answer is the daemon's own GET /brains, whose signed_in already turns off a login the provider refused, and the names are the ones the brain picker shows. Only with no daemon does doctor fall back to the files and PATH, and it says that is all it read.
func brainCheck(home, apiKey string, daemonUp bool, cfg config.JuneConfig) doctorCheck {
	var rows []brainRow
	source := "; read from the running daemon"
	if daemonUp {
		var err error
		if rows, err = daemonBrains(); err != nil {
			source = "; the daemon's /brains could not be read (" + err.Error() + "), so this is only what the login files and PATH say, not whether each login still works"
		}
	} else {
		source = "; no daemon is answering, so this is only what the login files and PATH say, not whether each login still works"
	}
	fromDaemon := rows != nil
	if !fromDaemon {
		rows = localBrains(home, apiKey)
	}
	setUp := brainsSetUp(home, apiKey)
	var usable, refused []string
	for _, r := range rows {
		if !askBrainIDs[r.ID] {
			continue
		}
		switch {
		case r.SignedIn:
			usable = append(usable, r.Name)
		case setUp[r.ID]:
			// Only a brain that is set up here is named as one that cannot answer; one never installed or signed in to is not news. The daemon's note says why when it knows. On the Claude row with its usage read turned off, the note is claudeUsageOffNote, which is about the row's usage bars and not its login — unless a refusal the provider recorded has since overwritten it with the real reason, which is kept.
			why := r.LimitsNote
			if fromDaemon && (why == "" || (r.ID == "claude" && why == claudeUsageOffNote)) {
				why = "set up here, but the running daemon cannot use it"
			}
			refused = append(refused, r.Name+" cannot answer ("+why+")")
		}
	}
	notes := ""
	if len(refused) > 0 {
		notes = "; " + strings.Join(refused, "; ")
	}
	if len(usable) == 0 {
		// The local text model runs the memory duties and nothing else, so it is named here only so a machine that has one is not left wondering why it does not count.
		if cfg.LocalText.Enabled(cfg) {
			notes += "; the local model in local_text runs the memory duties but cannot answer a question"
		}
		return doctorCheck{
			Name:   "brain",
			Detail: "no brain can answer a question" + notes + source,
			Fix:    "install Antigravity's agy and sign in to it, or run claude auth login, or codex login, or put GEMINI_API_KEY in " + filepath.Join(config.DataDir(), "env"),
		}
	}
	return doctorCheck{Name: "brain", Detail: strings.Join(usable, ", ") + " can answer" + notes + source, OK: true}
}

// daemonBrains reads the running daemon's GET /brains. Output: its rows, or an error when it did not answer 200 with a list in time.
func daemonBrains() ([]brainRow, error) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+DaemonPort+"/brains", nil)
	if err != nil {
		return nil, err
	}
	ipctoken.Attach(req, ipctoken.DefaultPath())
	resp, err := (&http.Client{Timeout: doctorBrainsTimeout}).Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, fmt.Errorf("it did not answer within %s", doctorBrainsTimeout)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		// /ping needs no token and /brains does, so this is the token file and not the daemon: missing, or written by a daemon on another data directory.
		return nil, errors.New("it answered " + resp.Status + ": the token in " + ipctoken.DefaultPath() + " is not the running daemon's")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("it answered " + resp.Status)
	}
	var body struct {
		Brains []brainRow `json:"brains"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Brains) == 0 {
		return nil, errors.New("it listed no brains")
	}
	return body.Brains, nil
}

// brainsSetUp is what this machine has in place for each brain a question can go to, whether or not it still works: agy on PATH, a Gemini key, and the Codex and Claude login files. Input: the home directory and the Gemini API key. Output: true by GET /brains id.
func brainsSetUp(home, apiKey string) map[string]bool {
	_, agyErr := exec.LookPath("agy")
	return map[string]bool{
		"antigravity": agyErr == nil,
		"gemini":      strings.TrimSpace(apiKey) != "",
		"codex":       util.Exists(agent.CodexAuthPath(home)),
		"claude":      util.Exists(agent.ClaudeCredentialsPath(home)),
	}
}

// localBrains is what the login files and PATH say about each brain a question can go to, the same signals GET /brains starts from before any provider has had the chance to refuse a login. Input: the home directory and the Gemini API key. Output: one row each, named as GET /brains names them.
func localBrains(home, apiKey string) []brainRow {
	set := brainsSetUp(home, apiKey)
	claude := brainRow{ID: "claude", Name: "Claude", SignedIn: agent.ClaudeSignedIn(home)}
	if set["claude"] && !claude.SignedIn {
		// A dead Claude OAuth session keeps its file with both tokens emptied, which is the one refused login the files alone can show; these are the words the daemon's own row uses for it.
		claude.LimitsNote = "the Claude login has expired: run claude in a terminal to sign in again"
	}
	return []brainRow{
		{ID: "antigravity", Name: "Antigravity", SignedIn: set["antigravity"]},
		{ID: "gemini", Name: "Gemini", SignedIn: set["gemini"]},
		{ID: "codex", Name: "Codex", SignedIn: set["codex"]},
		claude,
	}
}

// anyFrame reports whether at least one listed window carries a frame rectangle.
func anyFrame(windows []window.Window) bool {
	for _, w := range windows {
		if w.W > 0 && w.H > 0 {
			return true
		}
	}
	return false
}

// doctorLine renders one check as its line of the report. Input: the check. Output: the line, newline included.
func doctorLine(c doctorCheck) string {
	mark := "ok  "
	if !c.OK {
		mark = "FAIL"
	}
	return fmt.Sprintf("%s  %-22s %s\n", mark, c.Name, c.Detail)
}

// doctorVerdict renders the readiness line that follows the checks: every check June cannot run without that failed, then every feature that is off, and the fix for the first of them. Input: the checks. Output: the text.
func doctorVerdict(checks []doctorCheck) string {
	blockers, off := failedChecks(checks)
	switch {
	case len(blockers) > 0:
		also := ""
		if len(off) > 0 {
			also = "also off: " + checkNames(off) + "\n"
		}
		return "\nnot ready: " + checkNames(blockers) + "\n" + also + "next: " + blockers[0].Fix + "\n"
	case len(off) > 0:
		return "\nready, with these off: " + checkNames(off) + "\nnext: " + off[0].Fix + "\n"
	}
	return "\nready: everything June needs is in place\n"
}

// doctorExit is doctor's exit status. Input: the checks. Output: 1 when a check June cannot run without failed, 2 when only checks that turn a feature off did, and 0 when everything passed.
func doctorExit(checks []doctorCheck) int {
	blockers, off := failedChecks(checks)
	switch {
	case len(blockers) > 0:
		return 1
	case len(off) > 0:
		return 2
	}
	return 0
}

// failedChecks splits the checks that failed into those June cannot run without and those that turn a feature off, each in report order.
func failedChecks(checks []doctorCheck) (blockers, off []doctorCheck) {
	for _, c := range checks {
		switch {
		case c.OK:
		case c.Required:
			blockers = append(blockers, c)
		default:
			off = append(off, c)
		}
	}
	return blockers, off
}

// checkNames is the checks' names as one comma-separated list.
func checkNames(checks []doctorCheck) string {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}
