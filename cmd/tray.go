package cmd

import (
	"context"
	"log/slog"

	"june/internal/recorder"
)

// The tray menu on every platform offers the same items, in this order: a status line, Open June, Pause or Resume Observation, Start or Stop meeting recording, and Quit June. The helpers here are the parts both trays share.

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
