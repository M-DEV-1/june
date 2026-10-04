package cmd

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"june/internal/recorder"
)

// The tray menu on every platform offers the same items, in this order: a status line, Open June, Pause or Resume Observation, Start or Stop meeting recording, and Quit June. The helpers here are the parts both trays share.

// setupWaits reports whether observation is waiting for first-run setup, as it is now: setup is not done, and June's window, where setup runs, is turned on. With "window": false in june-config.json there is nothing to finish setup in, so the user's own Resume is what turns observation on. startDaemonServices points it at the live config before the tray starts; until then it says no, which only a tray that never comes up could read.
var setupWaits = func() bool { return false }

// pauseLabels is the tray's status line and its pause item. Input: whether observation is paused now. Output: the two labels.
// While observation waits for setup the item opens setup rather than resuming: nothing is observed until setup is finished, which is what turns observation on, and POST /resume refuses until then.
func pauseLabels(paused bool) (status, item string) {
	switch {
	case setupWaits():
		return "Waiting for setup", "Finish setting up June…"
	case !paused:
		return "Observing", "Pause Observation"
	}
	until := pauses.pausedUntil()
	if until.IsZero() {
		return "Paused", "Resume Observation"
	}
	return "Paused, resumes in " + timeLeft(time.Until(until)), "Resume Observation"
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

// clickPauseItem does what the pause item says (see pauseLabels). Input: whether observation is paused now. It waits on the daemon, so it runs on a goroutine of its own.
func clickPauseItem(paused bool) {
	base := "http://127.0.0.1:" + DaemonPort
	switch {
	case setupWaits():
		authedDaemonGet(base + "/window?action=open")
	case paused:
		authedDaemonPost(base + "/resume")
	default:
		authedDaemonPost(base + "/pause")
	}
}

// trayWanted reports whether the daemon should put up its tray icon: always, unless JUNE_NO_TRAY is set. A second daemon run beside the real one on its own JUNE_PORT and JUNE_DATA_DIR — a test, a CI job — would otherwise add a second June icon indistinguishable from the first, whose menu drives the wrong daemon.
func trayWanted() bool { return os.Getenv("JUNE_NO_TRAY") == "" }

// meetingLabel is the recording menu item's text — a toggle, so the label always names the action the click performs.
func meetingLabel(recording bool) string {
	if recording {
		return "Stop meeting recording"
	}
	return "Start meeting recording"
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
