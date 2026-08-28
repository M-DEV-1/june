// Package recorder records a meeting from the two sides of the machine's audio — the microphone and the default sink's monitor — then transcribes both locally with whisper and turns the interleaved transcript into meeting minutes.
package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"ora/internal/audio"
	"ora/internal/db"
)

// noteKind is the notes.kind written for a meeting, so minutes are distinguishable from the memory compiler's facts.
const noteKind = "meeting"

// transcribeTimeout bounds one whisper run. small.en runs several times faster than real time on CPU, so two hours covers a very long meeting with room to spare.
const transcribeTimeout = 2 * time.Hour

// episodeLimit caps how much of the desktop timeline goes into the summary prompt. The tracker writes an episode every couple of seconds, so a long meeting produces far more than a prompt needs.
const episodeLimit = 200

// noSpeechMarker is the file left in a recording directory whose transcription ran fine but produced no speech at all. It tells the user why the audio is still there, and it stops the startup sweep from transcribing that directory again on every daemon start.
const noSpeechMarker = "no-speech.txt"

// keepAudio preserves mic.wav and system.wav after a successful transcription instead of deleting them.
const keepAudio = true

// dirTimeLayout is how a recording directory is named, and therefore how its start time is read back when the sweep picks up an unfinished recording.
const dirTimeLayout = "2006-01-02T15-04-05"

// Store is the read-mostly slice of *db.Store the recorder needs: the desktop timeline captured while the meeting ran, and somewhere to file the minutes.
type Store interface {
	EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error)
	LogNote(ctx context.Context, content, kind string) (int64, error)
}

// capturer is the running-capture half of audio.MeetingCapture, kept as an interface so tests can drive the pipeline without a sound server.
type capturer interface{ Stop() }

// session is one recording: where it lives, when it ran, and how far each stream's clock is offset from the recording's own zero.
type session struct {
	dir                  string
	startedAt, stoppedAt time.Time
	cap                  capturer
	mic, sys             *wavWriter
	micOffset, sysOffset time.Duration
}

// Recorder owns at most one meeting recording at a time. Start and StopAndProcess are what the tray calls; everything after the stop runs in the background.
type Recorder struct {
	dataDir string
	store   Store
	apiKey  string

	mu   sync.Mutex
	live *session

	// swept is closed once the startup sweep for unfinished recordings has finished. Only tests wait on it.
	swept chan struct{}

	// Seams, all set by New and replaced in tests: opening the sound streams, running whisper, finding the whisper binary, calling Gemini, and posting a desktop notification.
	capture     func(mic, system io.Writer) (capturer, time.Time, time.Time, error)
	whisper     func(ctx context.Context, bin, path, speaker string, offset time.Duration) ([]Segment, error)
	findWhisper func(dataDir string) (string, error)
	minutes     func(ctx context.Context, prompt string) (string, error)
	notify      func(title, body string)
}

// New returns a Recorder that writes under dataDir/recordings, reads desktop context from and files minutes into store, and summarises with the Gemini API key apiKey.
func New(dataDir string, store Store, apiKey string) *Recorder {
	r := &Recorder{dataDir: dataDir, store: store, apiKey: apiKey}
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		c, err := audio.StartMeetingCapture(mic, system)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		return c, c.MicStart, c.SystemStart, nil
	}
	r.whisper = transcribeWAV
	r.findWhisper = whisperBinary
	r.minutes = r.geminiMinutes
	r.notify = notifySend
	r.swept = make(chan struct{})
	started := time.Now()
	go func() {
		defer close(r.swept)
		r.resumeOrphans(context.Background(), started)
	}()
	return r
}

// resumeOrphans finishes recordings that were never processed: a crash, or quitting the tray mid-recording, leaves WAVs with no transcript.md beside them and nothing else ever picks them up.
// It runs once per Recorder, in the background, and is best-effort — a directory that fails is logged and the next one still gets its turn.
// Only recordings that began before `before` are considered, so a recording this process starts while the sweep is still running is never mistaken for an abandoned one and transcribed out from under itself.
func (r *Recorder) resumeOrphans(ctx context.Context, before time.Time) {
	root := filepath.Join(r.dataDir, "recordings")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("could not scan for unfinished meeting recordings", "dir", root, "error", err)
		}
		return
	}
	// A directory is named to the second, so a recording started in the same second as the cutoff cannot be told apart from one started just after it. Truncating makes the ambiguous case skip: the worst outcome is that an abandoned recording waits for the next daemon start, against transcribing a live one out from under itself.
	cutoff := before.Truncate(time.Second)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s, ok := unfinished(dir)
		if !ok || !s.startedAt.Before(cutoff) {
			continue
		}
		slog.Info("resuming an unfinished meeting recording", "dir", dir)
		if err := r.process(ctx, s); err != nil {
			slog.Error("could not finish an unfinished meeting recording", "dir", dir, "error", err)
		}
	}
}

// unfinished reports whether dir holds a recording that still needs processing — both WAVs present, no transcript.md and no no-speech marker — and returns the session to run it as. The start time comes from the directory name and the stop time from the audio's last write, since the original session's clocks died with the process.
func unfinished(dir string) (*session, bool) {
	for _, name := range []string{"transcript.md", noSpeechMarker} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return nil, false
		}
	}
	mic, err := os.Stat(filepath.Join(dir, "mic.wav"))
	if err != nil {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(dir, "system.wav")); err != nil {
		return nil, false
	}
	started, err := time.ParseInLocation(dirTimeLayout, filepath.Base(dir), time.Local)
	if err != nil {
		started = mic.ModTime()
	}
	return &session{dir: dir, startedAt: started, stoppedAt: mic.ModTime()}, true
}

// Active reports whether a recording is running, which is what the tray menu label keys off.
func (r *Recorder) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live != nil
}

// Start opens both audio streams and begins writing mic.wav and system.wav under a fresh timestamped directory.
func (r *Recorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live != nil {
		return errors.New("a meeting recording is already running")
	}

	dir := filepath.Join(r.dataDir, "recordings", time.Now().Format("2006-01-02T15-04-05"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create recording dir: %w", err)
	}

	mic, err := newWAV(filepath.Join(dir, "mic.wav"))
	if err != nil {
		return fmt.Errorf("create mic.wav: %w", err)
	}
	sys, err := newWAV(filepath.Join(dir, "system.wav"))
	if err != nil {
		mic.Close()
		return fmt.Errorf("create system.wav: %w", err)
	}

	cap, micStart, sysStart, err := r.capture(mic, sys)
	if err != nil {
		mic.Close()
		sys.Close()
		return fmt.Errorf("start capture: %w", err)
	}

	// The earlier of the two stream starts is the recording's zero, so segments from the two transcripts land on one clock.
	zero := micStart
	if sysStart.Before(zero) {
		zero = sysStart
	}
	r.live = &session{
		dir:       dir,
		startedAt: zero,
		cap:       cap,
		mic:       mic,
		sys:       sys,
		micOffset: micStart.Sub(zero),
		sysOffset: sysStart.Sub(zero),
	}
	r.notify("Recording meeting", "Ora is recording. Stop it from the tray when the call ends.")
	return nil
}

// StopAndProcess halts capture and runs transcription and summarising in the background, notifying the user at each stage. Returns the recording directory.
func (r *Recorder) StopAndProcess(ctx context.Context) (string, error) {
	s, err := r.stop()
	if err != nil {
		return "", err
	}
	r.notify("Transcribing meeting", "Ora is transcribing the recording in the background.")
	go func() {
		if err := r.process(ctx, s); err != nil {
			slog.Error("meeting post-processing failed", "dir", s.dir, "error", err)
			r.notify("Meeting transcription failed", err.Error())
		}
	}()
	return s.dir, nil
}

// stop halts capture and closes both WAV files, returning the finished session.
func (r *Recorder) stop() (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.live
	if s == nil {
		return nil, errors.New("no meeting recording is running")
	}
	r.live = nil

	s.cap.Stop()
	s.stoppedAt = time.Now()
	if err := s.mic.Close(); err != nil {
		slog.Warn("closing mic.wav", "error", err)
	}
	if err := s.sys.Close(); err != nil {
		slog.Warn("closing system.wav", "error", err)
	}
	return s, nil
}

// process transcribes both streams, writes transcript.md and minutes.md, files the minutes as a note, and deletes the audio once both transcriptions have succeeded.
func (r *Recorder) process(ctx context.Context, s *session) error {
	bin, err := r.findWhisper(r.dataDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()

	mine, err := r.whisper(ctx, bin, filepath.Join(s.dir, "mic.wav"), speakerMe, s.micOffset)
	if err != nil {
		return fmt.Errorf("transcribe mic: %w", err)
	}
	theirs, err := r.whisper(ctx, bin, filepath.Join(s.dir, "system.wav"), speakerCall, s.sysOffset)
	if err != nil {
		return fmt.Errorf("transcribe system audio: %w", err)
	}

	// Whisper exiting 0 with nothing to show for it is not a success: the meeting may have been silent, or this whisper build may print segments in a shape parseSegments does not recognise. Either way the WAVs are still the only copy of the meeting, so they stay put and the marker records why.
	segs := append(mine, theirs...)
	if len(segs) == 0 {
		note := fmt.Sprintf("Transcription ran without error but found no speech in this recording, so the audio has been kept instead of deleted. Written %s.\n", time.Now().Format(time.RFC3339))
		if err := os.WriteFile(filepath.Join(s.dir, noSpeechMarker), []byte(note), 0o644); err != nil {
			slog.Warn("could not write the no-speech marker", "dir", s.dir, "error", err)
		}
		return fmt.Errorf("transcription found no speech; the audio is kept in %s", s.dir)
	}

	transcript := renderTranscript(segs)
	if err := os.WriteFile(filepath.Join(s.dir, "transcript.md"), []byte(transcript), 0o644); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}

	// Both transcriptions landed and the transcript is on disk, so the audio is no longer the only copy of the meeting.
	// ponytail: keepAudio stays true for the alpha phase so bad transcripts can be diagnosed and re-run from source; flip to false (or make it config) once transcription is trusted, since a two-stream hour is ~230 MB.
	if !keepAudio {
		for _, name := range []string{"mic.wav", "system.wav"} {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
				slog.Warn("could not delete recording audio", "file", name, "error", err)
			}
		}
	}

	text, err := r.minutes(ctx, r.buildPrompt(ctx, transcript, s.startedAt, s.stoppedAt))
	if err != nil {
		return fmt.Errorf("summarise meeting: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "minutes.md"), []byte(text), 0o644); err != nil {
		return fmt.Errorf("write minutes: %w", err)
	}
	if _, err := r.store.LogNote(ctx, text, noteKind); err != nil {
		slog.Warn("could not file meeting minutes as a note", "error", err)
	}

	r.notify("Meeting summary ready", filepath.Join(s.dir, "minutes.md"))
	return nil
}

// notifySend posts a desktop notification through notify-send, which GNOME provides. Failure is logged and ignored: a missing notification must never take down a finished recording.
func notifySend(title, body string) {
	if err := exec.Command("notify-send", "-a", "Ora", "-i", "audio-input-microphone", title, body).Run(); err != nil {
		slog.Debug("notify-send failed", "title", title, "error", err)
	}
}
