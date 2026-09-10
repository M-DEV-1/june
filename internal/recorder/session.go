package recorder

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ora/internal/proactive"
)

// noSpeechMarker is the file left in a recording directory whose transcription ran fine but produced no speech at all. It tells the user why the audio is still there, and it stops the startup sweep from transcribing that directory again on every daemon start.
const noSpeechMarker = "no-speech.txt"

// failedMarker is the file left in a recording directory whose processing failed, holding the error and when it happened. Without it the recording keeps the exact shape the sweep looks for, and every tick spends another summariser call on the same failure.
const failedMarker = "failed.txt"

// failureRetryAfter is how old the failure marker must be before the sweep tries that recording again. A rate limit or a dead API key clears on the scale of an hour, not of the two minutes between sweeps.
const failureRetryAfter = time.Hour

// maxProcessAttempts is how many times a recording may fail before the sweep stops offering it. A summariser refusing for a reason that clears — a rate limit, a flat battery, a network — is back well inside six hours. One refusing for a reason that does not, such as a model name the API has never had, never comes back: the 2026-09-05T00-53-59 recording failed at 01:18 with a 404 and was retried hourly for the rest of the day, each retry running whisper again and taking the GPU out from under whatever else wanted it.
const maxProcessAttempts = 6

// failedAttemptsPrefix opens the first line of the failure marker, where the number of failed attempts is kept. It lives in the file rather than in memory because the sweep restarts with the daemon and the count has to survive that.
const failedAttemptsPrefix = "attempts: "

// failedAttempts returns how many times processing this recording has already failed. Input: the recording directory. Output: the count off the first line of its failure marker, and 0 when there is no marker or its first line does not carry one.
func failedAttempts(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, failedMarker))
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(string(b), "\n")
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(first, failedAttemptsPrefix)))
	if err != nil {
		return 0
	}
	return n
}

// noteIDFile is the file in a recording directory holding the id of the note its minutes were filed under, so summarising the same recording again corrects that note instead of filing a second copy of the meeting.
const noteIDFile = "note-id.txt"

// keepAudio preserves mic.wav and system.wav after a successful transcription instead of deleting them.
const keepAudio = true

// dirTimeLayout is how a recording directory is named, and therefore how its start time is read back when the sweep picks up an unfinished recording.
const dirTimeLayout = "2006-01-02T15-04-05"

// capturer is the running-capture half of audio.MeetingCapture, kept as an interface so tests can drive the pipeline without a sound server.
type capturer interface{ Stop() }

// session is one recording: where it lives, when it ran, and how far each stream's clock is offset from the recording's own zero.
type session struct {
	dir                  string
	startedAt, stoppedAt time.Time
	cap                  capturer
	mic, sys             *wavWriter
	micOffset, sysOffset time.Duration
	done                 chan struct{} // closed when the recording stops, which retires the silence watchdog

	// fromTranscript marks a recording whose transcript.md is already on disk and whose minutes are the only thing missing. Processing it re-summarises that transcript and never runs whisper.
	fromTranscript bool
}

// powerSupplyRoot is where Linux exposes the machine's power supplies. Tests point it elsewhere.
var powerSupplyRoot = "/sys/class/power_supply"

// OnACPower reports whether the machine is on mains power, by reading the kernel's power supply class: a supply whose type is "Mains" and whose online flag is 1 is the charger, plugged in. Exported because the overnight dreaming loop gates on the same fact.
// A machine that reports no mains supply at all — a desktop, or any system that does not export this, Windows included — counts as on mains, so transcription is never deferred forever somewhere it cannot be asked.
func OnACPower() bool {
	entries, err := os.ReadDir(powerSupplyRoot)
	if err != nil {
		return true
	}
	mains := false
	for _, e := range entries {
		dir := filepath.Join(powerSupplyRoot, e.Name())
		if readTrimmed(filepath.Join(dir, "type")) != "Mains" {
			continue
		}
		mains = true
		if readTrimmed(filepath.Join(dir, "online")) == "1" {
			return true
		}
	}
	return !mains
}

// readTrimmed returns the contents of a one-line sysfs file without its trailing newline, or "" if it cannot be read.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// unfinished reports whether dir holds a recording that still needs processing, and returns the session to run it as. There are two cases: both WAVs present with no transcript.md, which is a recording a crash or a battery deferral left mid-flight, and a transcript.md with no minutes.md, which is either a crash between those two writes or the user deleting minutes.md to ask for the summary again. A no-speech marker means the recording is done with either way.
func unfinished(dir string) (*session, bool) {
	if exists(filepath.Join(dir, noSpeechMarker)) {
		return nil, false
	}
	// A recording that has failed maxProcessAttempts times is not offered again: whatever is wrong with it is not the kind of thing another hour fixes, and the user has been told so in the meetings list. Deleting the marker is how they ask for another go.
	if failedAttempts(dir) >= maxProcessAttempts {
		return nil, false
	}
	// A recording whose last attempt failed is left alone until its marker is failureRetryAfter old, so a summariser that is refusing costs one call an hour instead of one every sweep. Deleting the marker is how the user asks for the retry now.
	if t := modTime(filepath.Join(dir, failedMarker)); !t.IsZero() && time.Since(t) < failureRetryAfter {
		return nil, false
	}
	if exists(filepath.Join(dir, "transcript.md")) {
		if exists(filepath.Join(dir, "minutes.md")) {
			return nil, false
		}
		s := pickupSession(dir)
		s.fromTranscript = true
		return s, true
	}
	if !exists(filepath.Join(dir, "mic.wav")) || !exists(filepath.Join(dir, "system.wav")) {
		return nil, false
	}
	return pickupSession(dir), true
}

// makeRecordingDir makes a fresh directory for a recording under root, named for the moment it starts. Input: the recordings root. Output: the directory it created, and an error if it could not create one.
// The name has one-second resolution, so two recordings starting inside one second ask for the same name. os.Mkdir refuses a directory that already exists, where os.MkdirAll accepts it and the WAVs opened inside then truncate the earlier meeting's audio; the suffix gives the second recording its own directory instead.
func makeRecordingDir(root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(root, time.Now().Format(dirTimeLayout))
	for n := 1; n <= maxSameSecondRecordings; n++ {
		dir := base
		if n > 1 {
			dir = fmt.Sprintf("%s-%d", base, n)
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s and its suffixes are all taken", base)
}

// maxSameSecondRecordings bounds the search for a free name, so a directory that cannot be created for some reason os.Mkdir reports as ErrExist can never spin. Nothing on this machine starts nine recordings in one second.
const maxSameSecondRecordings = 9

// dirStart reads the time a recording started off its directory's name. Input: the base name, which is the start time and, for a recording that began in the same second as another, a "-2" or "-3" suffix. Output: the start time, or an error when the name is not a recording directory's.
func dirStart(name string) (time.Time, error) {
	t, err := time.ParseInLocation(dirTimeLayout, name, time.Local)
	if err == nil {
		return t, nil
	}
	if i := strings.LastIndex(name, "-"); i > 0 {
		if t, suffixErr := time.ParseInLocation(dirTimeLayout, name[:i], time.Local); suffixErr == nil {
			return t, nil
		}
	}
	return t, err
}

// pickupSession dates a recording found on disk, since the original session's clocks died with the process that made it. The start comes from the directory's name, and the length from how much audio is in mic.wav — the file's own timestamp is not the end of the meeting, because anything that touches the file afterwards moves it, and the window is what decides which screens the summary is written from. With the audio gone, the last write to the transcript is the best guess left.
func pickupSession(dir string) *session {
	started, startErr := dirStart(filepath.Base(dir))
	stopped := modTime(filepath.Join(dir, "transcript.md"))
	if d := audioDuration(filepath.Join(dir, "mic.wav")); d > 0 && startErr == nil {
		stopped = started.Add(d)
	} else if t := modTime(filepath.Join(dir, "mic.wav")); !t.IsZero() {
		stopped = t
	}
	if startErr != nil {
		started = stopped
	}
	return &session{dir: dir, startedAt: started, stoppedAt: stopped}
}

// audioDuration returns how long the samples in a recorded WAV run for, from its size: capture is always 16 kHz mono 16-bit, so the byte count is the clock. It returns zero if the file is missing or holds nothing but a header.
func audioDuration(path string) time.Duration {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= wavHeaderSize {
		return 0
	}
	return time.Duration((info.Size()-wavHeaderSize)/2) * time.Second / sampleRate
}

// exists reports whether path is there at all.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// modTime returns when path was last written, or the zero time if it cannot be read.
func modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// markOutcome records how an attempt at processing a recording ended: a success clears any failure marker, a failure writes one holding the error, the time and how many attempts have now failed, which keeps the sweep off this recording for failureRetryAfter and off it for good once the count reaches maxProcessAttempts. Input: the recording directory and the error the attempt returned, nil on success. Output: how many attempts have now failed, 0 on success — failing to write the marker is logged, since it only costs a wasted retry.
func markOutcome(dir string, err error) int {
	path := filepath.Join(dir, failedMarker)
	if err == nil {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("could not clear the failure marker of a recording that has now been processed", "dir", dir, "error", rmErr)
		}
		return 0
	}
	// A recording with no speech in it already has its own marker saying so, and unfinished() stops on that one first.
	if exists(filepath.Join(dir, noSpeechMarker)) {
		return 0
	}
	attempts := failedAttempts(dir) + 1
	next := "Ora will try again in about an hour. Delete this file to have it try again straight away."
	if attempts >= maxProcessAttempts {
		next = fmt.Sprintf("Ora has stopped retrying after %d attempts. Delete this file to have it try once more.", attempts)
	}
	note := fmt.Sprintf("%s%d\n\nProcessing this recording failed at %s:\n\n%v\n\n%s\n", failedAttemptsPrefix, attempts, time.Now().Format(time.RFC3339), err, next)
	if wErr := os.WriteFile(path, []byte(note), 0o644); wErr != nil {
		slog.Warn("could not record why processing a recording failed", "dir", dir, "error", wErr)
	}
	return attempts
}

// meetingDurationPrefix opens the machine-readable line fileMinutes appends to a meeting note's stored content, after the minutes text. GET /meetings (internal/ipc/reads.go) parses it back out and strips it before the minutes ever reach the window, so the user never sees it.
const meetingDurationPrefix = "<!--ora:duration "

// meetingDurationLine renders the wall-clock start and stop of a recording as the machine-readable line fileMinutes appends to a note's content. This is the one place that knows how long a meeting actually ran — the model writing the minutes is never asked to compute or report it, so it cannot be trusted to get it right — and it survives a restart because it travels with the note rather than living only in the recorder's own memory.
func meetingDurationLine(startedAt, stoppedAt time.Time) string {
	return fmt.Sprintf("%sstart=%s stop=%s-->", meetingDurationPrefix, startedAt.UTC().Format(time.RFC3339), stoppedAt.UTC().Format(time.RFC3339))
}

// withMeetingDuration appends the machine-readable duration line to a meeting's minutes text, for storing in the note. minutes.md on disk and the text lifted for action items stay exactly what the model wrote; only the copy filed as a note carries this.
func withMeetingDuration(text string, startedAt, stoppedAt time.Time) string {
	return text + "\n\n" + meetingDurationLine(startedAt, stoppedAt) + "\n"
}

// notifySend posts a microphone-icon desktop notification; a long body gets a "Read in full" button, see proactive.Notify.
func notifySend(title, body string) {
	proactive.Notify("audio-input-microphone", title, body)
}

// notifySendAt posts a notice whose card opens the window at place and id, through the same path as notifySend.
func notifySendAt(title, body, place, id string) {
	proactive.NotifyAt("audio-input-microphone", title, body, place, id)
}
