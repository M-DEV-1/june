package cmd

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"june/internal/recorder"
)

// The tray menu on every platform offers the same items, in this order, with the same words: a status line, Open June, the pause item, the meeting item, and Quit June. The pause item is a submenu of the window's own three pauses while June watches, Resume watching while it is paused, and Finish setting up June… while it waits for setup. The helpers here are the parts both trays share, so the wording cannot drift between them.

// setupWaits reports whether observation is waiting for first-run setup, as it is now: setup is not done, and June's window, where setup runs, is turned on. With "window": false in june-config.json there is nothing to finish setup in, so the user's own Resume is what turns observation on. startDaemonServices points it at the live config before the tray starts; until then it says no, which only a tray that never comes up could read.
var setupWaits = func() bool { return false }

// pauseChoice is one way to pause from the tray: the label and how long, 0 for until the user resumes.
type pauseChoice struct {
	label   string
	minutes int
}

// pauseChoices are the pauses the tray offers, the same three, worded the same, as the window's own pause menu (PAUSES in app/src/next/pause.tsx).
var pauseChoices = []pauseChoice{
	{"Pause for 15 minutes", 15},
	{"Pause for 1 hour", 60},
	{"Pause until I resume", 0},
}

// The pause item's three faces.
const (
	pauseMenuLabel   = "Pause watching"
	resumeLabel      = "Resume watching"
	finishSetupLabel = "Finish setting up June…"
)

// trayMode is which face the pause item shows.
type trayMode int

const (
	modeWatching trayMode = iota
	modePaused
	modeSetup
)

// pauseMode is the pause item's face now. Input: whether observation is paused. While observation waits for setup the item opens setup rather than resuming: nothing is observed until setup is finished, which is what turns observation on, and POST /resume refuses until then.
func pauseMode(paused bool) trayMode {
	switch {
	case setupWaits():
		return modeSetup
	case paused:
		return modePaused
	}
	return modeWatching
}

// trayStatus is the tray's status line, in the window's words. Input: whether observation is paused now.
func trayStatus(paused bool) string {
	switch pauseMode(paused) {
	case modeSetup:
		return "Waiting for setup"
	case modeWatching:
		return "Watching"
	}
	until := pauses.pausedUntil()
	if until.IsZero() {
		return "Paused"
	}
	return "Paused, back in " + timeLeft(time.Until(until))
}

// timeLeft says how long a pause has left, rounded up to the minute: "5 min", "1 hr 20 min", "2 days 3 hr". A time of day would need the user's locale to read right, a 24-hour "15:04" being wrong beside the 12-hour clock Windows shows in the US, and a weekday is ambiguous for a pause of most of a week; a count reads the same everywhere. The trays redraw every second, so it counts down.
func timeLeft(d time.Duration) string {
	mins := int((d + time.Minute - 1) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	// Past a day the minutes are not shown, so they are rounded up into the hours rather than dropped.
	if mins > 24*60 {
		mins = (mins + 59) / 60 * 60
	}
	days, hours, minutes := mins/(24*60), mins/60%24, mins%60
	switch {
	case days > 0:
		s := strconv.Itoa(days) + " days"
		if days == 1 {
			s = "1 day"
		}
		if hours > 0 {
			s += " " + strconv.Itoa(hours) + " hr"
		}
		return s
	case hours > 0:
		s := strconv.Itoa(hours) + " hr"
		if minutes > 0 {
			s += " " + strconv.Itoa(minutes) + " min"
		}
		return s
	}
	return strconv.Itoa(minutes) + " min"
}

// pauseFromTray pauses observation the way POST /pause does, ending by itself after minutes, or never when minutes is 0. The tray runs in the daemon, so it uses the same pause control directly.
func pauseFromTray(minutes int) {
	var until time.Time
	if minutes > 0 {
		until = time.Now().Add(time.Duration(minutes) * time.Minute)
	}
	pauses.pause(until)
	slog.Info("tracking paused from the tray", "minutes", minutes)
}

// resumeFromTray does what the pause item says while paused or waiting on setup (see pauseMode): it opens setup in the window while setup waits, and resumes observation otherwise. It may wait on the daemon, so it runs on a goroutine of its own.
func resumeFromTray() {
	if setupWaits() {
		authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
		return
	}
	pauses.resume()
	slog.Info("tracking resumed from the tray")
}

// trayWanted reports whether the daemon should put up its tray icon: always, unless JUNE_NO_TRAY is set. A second daemon run beside the real one on its own JUNE_PORT and JUNE_DATA_DIR — a test, a CI job — would otherwise add a second June icon indistinguishable from the first, whose menu drives the wrong daemon.
func trayWanted() bool { return os.Getenv("JUNE_NO_TRAY") == "" }

// meetingLabel is the recording menu item's text — a toggle, so the label always names the action the click performs.
func meetingLabel(recording bool) string {
	if recording {
		return "Stop recording"
	}
	return "Record a meeting"
}

// toggleMeeting starts or stops the meeting recording. Input: the daemon's own context, handed to a stop so the transcription it starts ends with the daemon, and the recorder, which may be nil before the daemon wires one up. Stopping hands off to background transcription and summarising, so neither branch blocks the tray's click handler.
func toggleMeeting(ctx context.Context, rec *recorder.Recorder) {
	if rec == nil {
		slog.Warn("meeting recorder is not wired up, ignoring tray click")
		return
	}
	if rec.Active() {
		if _, err := rec.StopAndProcess(ctx); err != nil {
			slog.Error("failed to stop meeting recording", "error", err)
		}
		return
	}
	if err := rec.Start(); err != nil {
		slog.Error("failed to start meeting recording", "error", err)
	}
}
