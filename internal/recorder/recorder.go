// Package recorder records a meeting from the two sides of the machine's audio — the microphone and the default sink's monitor — then transcribes both locally with whisper and turns the interleaved transcript into meeting minutes.
package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ora/internal/audio"
	"ora/internal/config"
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

// failedMarker is the file left in a recording directory whose processing failed, holding the error and when it happened. Without it the recording keeps the exact shape the sweep looks for, and every tick spends another summariser call on the same failure.
const failedMarker = "failed.txt"

// failureRetryAfter is how old the failure marker must be before the sweep tries that recording again. A rate limit or a dead API key clears on the scale of an hour, not of the two minutes between sweeps.
const failureRetryAfter = time.Hour

// noteIDFile is the file in a recording directory holding the id of the note its minutes were filed under, so summarising the same recording again corrects that note instead of filing a second copy of the meeting.
const noteIDFile = "note-id.txt"

// keepAudio preserves mic.wav and system.wav after a successful transcription instead of deleting them.
const keepAudio = true

// dirTimeLayout is how a recording directory is named, and therefore how its start time is read back when the sweep picks up an unfinished recording.
const dirTimeLayout = "2006-01-02T15-04-05"

// Store is the slice of *db.Store the recorder needs: the desktop timeline captured while the meeting ran, the personal context that says who the [me] speaker is (and which a finished meeting can add a person to), and somewhere to file the minutes.
type Store interface {
	EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error)
	PersonalContext(ctx context.Context) ([]db.PersonalEntry, error)
	SetPersonalContext(ctx context.Context, subject, content string) error
	LogNote(ctx context.Context, content, kind string) (int64, error)
	UpdateNote(ctx context.Context, id int64, content string) error
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
	done                 chan struct{} // closed when the recording stops, which retires the silence watchdog

	// fromTranscript marks a recording whose transcript.md is already on disk and whose minutes are the only thing missing. Processing it re-summarises that transcript and never runs whisper.
	fromTranscript bool
}

// defaultSilenceAfter is how long the system stream may stay quiet before the user is told. Long enough that a natural pause in a call, or a meeting still on its "waiting for the host" screen, does not trigger it.
const defaultSilenceAfter = 45 * time.Second

// silenceFloor is the sample magnitude below which a stream counts as silent. A stream recorded from a sink nothing plays into is exact zeros; a real room floor with nobody speaking still sits above this.
const silenceFloor = 64

// silenceWatch passes samples through to the real writer while remembering when sound last came through. A meeting playing to a sink Ora is not recording writes an unbroken run of zeros, which is indistinguishable from a working recording until the transcript comes back empty.
type silenceWatch struct {
	w    io.Writer
	last atomic.Int64 // unix nanoseconds of the last sample above the noise floor
}

func newSilenceWatch(w io.Writer) *silenceWatch {
	s := &silenceWatch{w: w}
	s.last.Store(time.Now().UnixNano())
	return s
}

func (s *silenceWatch) Write(p []byte) (int, error) {
	if hasSound(p) {
		s.last.Store(time.Now().UnixNano())
	}
	return s.w.Write(p)
}

// quietFor returns how long it has been since sound last came through.
func (s *silenceWatch) quietFor() time.Duration {
	return time.Since(time.Unix(0, s.last.Load()))
}

// watchSilence warns once, at any point in the recording, if the system stream goes quiet for longer than window. The first-thirty-seconds case is a meeting playing to the wrong sink from the start; the mid-call case is the output device changing under the recording — earbuds connecting, or dying and the audio hopping back to the speakers — which without this is silently lost for the rest of the meeting.
// ponytail: the warning tells the user to fix it by hand. Re-running the active-sink detection and re-attaching the monitor stream to the new sink mid-recording would fix it without them, and is the named follow-up; it needs a second record stream opened onto the same WAV writer while the first is torn down, which is more surgery than this pass.
func (r *Recorder) watchSilence(w *silenceWatch, window time.Duration, done <-chan struct{}) {
	tick := time.NewTicker(window / 3)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			if w.quietFor() < window {
				continue
			}
			slog.Warn("the system audio stream has gone silent, so the meeting is playing to a sink Ora is not recording", "quiet for", w.quietFor().Round(time.Second))
			r.notify("Meeting audio isn't reaching the recorder", "Nothing is coming through from the system audio — did the output device change? Check it, then restart the recording.")
			return
		}
	}
}

// hasSound reports whether any little-endian 16-bit sample in p is louder than silenceFloor.
func hasSound(p []byte) bool {
	for i := 0; i+1 < len(p); i += 2 {
		v := int16(binary.LittleEndian.Uint16(p[i:]))
		if v > silenceFloor || v < -silenceFloor {
			return true
		}
	}
	return false
}

// Recorder owns at most one meeting recording at a time. Start and StopAndProcess are what the tray calls; everything after the stop runs in the background.
type Recorder struct {
	dataDir string
	store   Store
	apiKey  string

	mu   sync.Mutex
	live *session

	// inFlight holds the recording directories being processed right now, so the stop path and the retry loop can never transcribe the same recording twice.
	inFlight map[string]bool

	// swept is closed once the startup sweep for unfinished recordings has finished. Only tests wait on it.
	swept chan struct{}

	// silenceAfter is how long the system stream may stay silent before the user is warned. Tests shorten it.
	silenceAfter time.Duration

	// retryEvery is how often a deferred recording is checked on. Tests do not rely on it, driving pickup directly instead.
	retryEvery time.Duration

	// Seams, all set by New and replaced in tests: opening the sound streams, running whisper, finding the whisper binary, calling Gemini, posting a desktop notification, and asking whether the machine is plugged in.
	capture     func(mic, system io.Writer) (capturer, time.Time, time.Time, error)
	whisper     func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error)
	findWhisper func(dataDir string) (string, error)
	minutes     func(ctx context.Context, prompt string) (string, error)
	notify      func(title, body string)
	onAC        func() bool
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
	// The engine is opt-in through the config file rather than switched on by model files merely being present: parakeet is far faster than whisper but speaks no Hindi, and a Hindi-English meeting must not silently start coming back as gibberish because someone downloaded a model.
	switch engine := config.LoadConfig().Transcribe.Engine; engine {
	case config.EngineParakeet:
		slog.Info("transcribing meetings with parakeet instead of whisper")
		r.whisper = transcribeParakeetWAV
		r.findWhisper = parakeetBinary
	case config.EngineWhisperCPP:
		// Same model and same flags as the whisperfile, so only where the binary is found changes; transcribeWAV notices the model beside it and adds the two flags whisper.cpp needs.
		slog.Info("transcribing meetings with a whisper.cpp build instead of the whisperfile")
		r.findWhisper = whisperCPPBinary
	}
	r.minutes = r.defaultBrain
	r.notify = notifySend
	r.onAC = onACPower
	r.silenceAfter = defaultSilenceAfter
	r.retryEvery = defaultRetryEvery
	r.swept = make(chan struct{})
	go func() {
		defer close(r.swept)
		r.pickup(context.Background())
	}()
	go r.retryDeferred(context.Background())
	return r
}

// defaultRetryEvery is how often the recorder looks for a recording it deferred, which is short enough that plugging in gets the transcript within a few minutes and long enough to cost nothing.
const defaultRetryEvery = 2 * time.Minute

// pickup finishes every recording that still needs it: one abandoned by a crash or by quitting the tray mid-recording, one whose transcription was deferred because the machine was on battery, and one that has its transcript but no minutes — which is how the user asks for the minutes again, by deleting minutes.md.
// It is best-effort — a directory that fails is logged and the next one still gets its turn — and it skips the recording that is running right now, whose WAVs are still being written and which on disk is indistinguishable from an abandoned one.
func (r *Recorder) pickup(ctx context.Context) {
	root := filepath.Join(r.dataDir, "recordings")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("could not scan for unfinished meeting recordings", "dir", root, "error", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s, ok := unfinished(dir)
		// liveDir is read per directory rather than once up front: a recording starting while this loop runs holds the lock from before it creates its directory until after it publishes it, so by the time this call returns the answer for that directory is settled.
		if !ok || dir == r.liveDir() {
			continue
		}
		slog.Info("finishing an unfinished meeting recording", "dir", dir, "from the existing transcript", s.fromTranscript)
		if err := r.process(ctx, s); err != nil {
			slog.Error("could not finish an unfinished meeting recording", "dir", dir, "error", err)
		}
	}
}

// liveDir returns the directory of the recording running right now, or "" when nothing is recording.
func (r *Recorder) liveDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		return ""
	}
	return r.live.dir
}

// claim reserves a recording directory for processing, reporting false if something else already has it. release gives it back.
func (r *Recorder) claim(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[dir] {
		return false
	}
	if r.inFlight == nil {
		r.inFlight = make(map[string]bool)
	}
	r.inFlight[dir] = true
	return true
}

func (r *Recorder) release(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, dir)
}

// retryDeferred runs pickup every retryEvery for as long as the process lives, so a recording deferred to save the battery is transcribed within a few minutes of the charger going in.
func (r *Recorder) retryDeferred(ctx context.Context) {
	for range time.Tick(r.retryEvery) {
		if r.onAC() {
			r.pickup(ctx)
		}
	}
}

// powerSupplyRoot is where Linux exposes the machine's power supplies. Tests point it elsewhere.
var powerSupplyRoot = "/sys/class/power_supply"

// onACPower reports whether the machine is on mains power, by reading the kernel's power supply class: a supply whose type is "Mains" and whose online flag is 1 is the charger, plugged in.
// A machine that reports no mains supply at all — a desktop, or any system that does not export this, Windows included — counts as on mains, so transcription is never deferred forever somewhere it cannot be asked.
func onACPower() bool {
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

// pickupSession dates a recording found on disk, since the original session's clocks died with the process that made it. The start comes from the directory's name, and the length from how much audio is in mic.wav — the file's own timestamp is not the end of the meeting, because anything that touches the file afterwards moves it, and the window is what decides which screens the summary is written from. With the audio gone, the last write to the transcript is the best guess left.
func pickupSession(dir string) *session {
	started, startErr := time.ParseInLocation(dirTimeLayout, filepath.Base(dir), time.Local)
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

// Active reports whether a recording is running, which is what the tray menu label keys off.
func (r *Recorder) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live != nil
}

// Start opens both audio streams and begins writing mic.wav and system.wav under a fresh timestamped directory.
func (r *Recorder) Start() error {
	if err := r.open(); err != nil {
		return err
	}
	// Notifying happens outside the lock, as it does on the stop path: notify shells out to notify-send, and holding the recorder's lock across a process spawn stalls anything asking whether a recording is running.
	r.notify("Recording meeting", "Ora is recording. Stop it from the tray when the call ends.")
	return nil
}

// open makes the recording directory, opens both audio streams and publishes the live session. Input: none. Output: an error if any of that fails, with the half-made recording directory removed again.
func (r *Recorder) open() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live != nil {
		return errors.New("a meeting recording is already running")
	}

	dir := filepath.Join(r.dataDir, "recordings", time.Now().Format("2006-01-02T15-04-05"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create recording dir: %w", err)
	}
	// A recording that never got its streams open leaves a directory of stub WAVs, which on disk is indistinguishable from a recording a crash abandoned — so the sweep would pick it up and try to transcribe silence on every tick.
	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				slog.Warn("could not remove the directory of a recording that failed to start", "dir", dir, "error", rmErr)
			}
		}
	}()

	mic, err := newWAV(filepath.Join(dir, "mic.wav"))
	if err != nil {
		return fmt.Errorf("create mic.wav: %w", err)
	}
	sys, err := newWAV(filepath.Join(dir, "system.wav"))
	if err != nil {
		mic.Close()
		return fmt.Errorf("create system.wav: %w", err)
	}

	watch := newSilenceWatch(sys)
	cap, micStart, sysStart, err := r.capture(mic, watch)
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
		done:      make(chan struct{}),
	}
	go r.watchSilence(watch, r.silenceAfter, r.live.done)
	return nil
}

// StopAndProcess halts capture and runs transcription and summarising in the background, notifying the user at each stage. Returns the recording directory.
func (r *Recorder) StopAndProcess(ctx context.Context) (string, error) {
	s, err := r.stop()
	if err != nil {
		return "", err
	}
	// Whisper pins every core for minutes. On battery that empties the laptop and the CPU is throttled while it runs, so the recording is left exactly as it is — no marker, nothing to say it is finished — and the retry loop picks it up once the charger is back in.
	if !r.onAC() {
		slog.Info("deferring meeting transcription until the machine is on mains power", "dir", s.dir)
		r.notify("Recording saved", "Ora will transcribe it once you plug in.")
		return s.dir, nil
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

	if s.done != nil {
		close(s.done)
	}
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

// process turns a recording into minutes: it gets the transcript — by running whisper over both streams, or by reading the one already on disk — then writes minutes.md, files the minutes as a note and updates personal context.
func (r *Recorder) process(ctx context.Context, s *session) (err error) {
	// Two paths can reach the same recording — the stop that made it and the retry loop looking for deferred ones — and transcribing it twice would race two whisper runs onto the same files.
	if !r.claim(s.dir) {
		return nil
	}
	defer r.release(s.dir)
	// How this attempt ended is recorded next to the recording, because on disk a recording that failed to summarise looks exactly like one that was never summarised at all.
	defer func() { markOutcome(s.dir, err) }()

	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()

	transcript, err := r.transcriptFor(ctx, s)
	if err != nil {
		return err
	}

	text, err := r.minutes(ctx, r.buildPrompt(ctx, transcript, s.startedAt, s.stoppedAt))
	if err != nil {
		return fmt.Errorf("summarise meeting: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "minutes.md"), []byte(text), 0o644); err != nil {
		return fmt.Errorf("write minutes: %w", err)
	}
	r.fileMinutes(ctx, s.dir, text)

	// The meeting may have taught Ora something durable about a person the user works with. This is the only path that writes personal context without the user saying it outright, so the model is held to a strict bar (see personalUpdateInstruction) and every write it makes is logged.
	r.updatePersonalContext(ctx, text, s.startedAt, s.stoppedAt)

	r.notify("Meeting summary ready", filepath.Join(s.dir, "minutes.md"))
	return nil
}

// markOutcome records how an attempt at processing a recording ended: a success clears any failure marker, a failure writes one holding the error and the time, which keeps the sweep off this recording for failureRetryAfter. Input: the recording directory and the error the attempt returned, nil on success. Output: none — failing to write the marker is logged, since it only costs a wasted retry.
func markOutcome(dir string, err error) {
	path := filepath.Join(dir, failedMarker)
	if err == nil {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("could not clear the failure marker of a recording that has now been processed", "dir", dir, "error", rmErr)
		}
		return
	}
	// A recording with no speech in it already has its own marker saying so, and unfinished() stops on that one first.
	if exists(filepath.Join(dir, noSpeechMarker)) {
		return
	}
	note := fmt.Sprintf("Processing this recording failed at %s:\n\n%v\n\nOra will try again in about an hour. Delete this file to have it try again straight away.\n", time.Now().Format(time.RFC3339), err)
	if wErr := os.WriteFile(path, []byte(note), 0o644); wErr != nil {
		slog.Warn("could not record why processing a recording failed", "dir", dir, "error", wErr)
	}
}

// fileMinutes puts a recording's minutes into memory. If the recording was filed before, the note it was filed under is corrected in place; otherwise the minutes are filed as a new note and its id written to noteIDFile so the next run corrects this one. Input: the recording directory and the minutes text. Output: none — a memory that refuses the minutes is logged and shrugged off, because minutes.md on disk is the copy that matters.
func (r *Recorder) fileMinutes(ctx context.Context, dir, text string) {
	path := filepath.Join(dir, noteIDFile)
	if b, err := os.ReadFile(path); err == nil {
		if id, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			// UpdateNote reports a missing row rather than silently doing nothing, so a note the user has since deleted falls through to being filed afresh.
			if uerr := r.store.UpdateNote(ctx, id, text); uerr == nil {
				return
			} else {
				slog.Warn("could not correct the note this recording was filed under, filing fresh minutes instead", "dir", dir, "note_id", id, "error", uerr)
			}
		}
	}
	id, err := r.store.LogNote(ctx, text, noteKind)
	if err != nil {
		slog.Warn("could not file meeting minutes as a note", "error", err)
		return
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(id, 10)), 0o644); err != nil {
		slog.Warn("could not record which note the meeting minutes were filed under", "dir", dir, "error", err)
	}
}

// transcriptFor returns the meeting's transcript. A recording the sweep found with its transcript already written just has it read back off disk, which is what makes deleting minutes.md a request for fresh minutes; anything else is transcribed with whisper and the result written to transcript.md.
func (r *Recorder) transcriptFor(ctx context.Context, s *session) (string, error) {
	if s.fromTranscript {
		b, err := os.ReadFile(filepath.Join(s.dir, "transcript.md"))
		if err != nil {
			return "", fmt.Errorf("read the existing transcript: %w", err)
		}
		return string(b), nil
	}

	bin, err := r.findWhisper(r.dataDir)
	if err != nil {
		return "", err
	}

	// Whisper is primed with the words Ora already watched go past on screen during the meeting, which is what gets the domain's own acronyms and the participants' names spelled right instead of guessed at phonetically.
	prompt := r.primingPrompt(ctx, s.startedAt, s.stoppedAt)
	if prompt != "" {
		slog.Info("priming whisper with the meeting's screen context", "dir", s.dir, "prompt", prompt)
	}

	// The two streams are separate files that share nothing, so they are transcribed at the same time rather than one after the other — which halves the wall time of every meeting. transcribeThreads gives each run half the machine so the two are not simply fighting over the same cores.
	var wg sync.WaitGroup
	var mine, theirs []Segment
	var micErr, sysErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		mine, micErr = r.whisper(ctx, bin, filepath.Join(s.dir, "mic.wav"), speakerMe, prompt, s.micOffset)
	}()
	go func() {
		defer wg.Done()
		theirs, sysErr = r.whisper(ctx, bin, filepath.Join(s.dir, "system.wav"), speakerCall, prompt, s.sysOffset)
	}()
	wg.Wait()
	if micErr != nil {
		return "", fmt.Errorf("transcribe mic: %w", micErr)
	}
	if sysErr != nil {
		return "", fmt.Errorf("transcribe system audio: %w", sysErr)
	}

	// Whisper exiting 0 with nothing to show for it is not a success: the meeting may have been silent, or this whisper build may print segments in a shape parseSegments does not recognise. Either way the WAVs are still the only copy of the meeting, so they stay put and the marker records why.
	segs := append(mine, theirs...)
	if len(segs) == 0 {
		note := fmt.Sprintf("Transcription ran without error but found no speech in this recording, so the audio has been kept instead of deleted. Written %s.\n", time.Now().Format(time.RFC3339))
		if err := os.WriteFile(filepath.Join(s.dir, noSpeechMarker), []byte(note), 0o644); err != nil {
			slog.Warn("could not write the no-speech marker", "dir", s.dir, "error", err)
		}
		return "", fmt.Errorf("transcription found no speech; the audio is kept in %s", s.dir)
	}

	transcript := renderTranscript(segs)
	if err := os.WriteFile(filepath.Join(s.dir, "transcript.md"), []byte(transcript), 0o644); err != nil {
		return "", fmt.Errorf("write transcript: %w", err)
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
	return transcript, nil
}

// notifySend posts a desktop notification through notify-send, which GNOME provides. Failure is logged and ignored: a missing notification must never take down a finished recording.
func notifySend(title, body string) {
	if err := exec.Command("notify-send", "-a", "Ora", "-i", "audio-input-microphone", title, body).Run(); err != nil {
		slog.Debug("notify-send failed", "title", title, "error", err)
	}
}
