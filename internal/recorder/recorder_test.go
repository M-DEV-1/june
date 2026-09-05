package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/db"
	"ora/internal/memory"
)

type fakeCapture struct{ stopped bool }

func (f *fakeCapture) Stop() { f.stopped = true }

type fakeStore struct {
	episodes []db.Episode
	personal []db.PersonalEntry
	// personalWrites records every SetPersonalContext call, subject to content, so a test can see what the meeting updater decided to write.
	personalWrites map[string]string
	notes          []string
	kinds          []string
	updates        map[int64]string
	actions        []memory.ActionItem
	closeSweeps    int
	// getNotesCalls counts the reads of every note in the store, which is the expensive call the backfill makes.
	getNotesCalls int
	mu            sync.Mutex
}

func (s *fakeStore) EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error) {
	return s.episodes, nil
}

func (s *fakeStore) PersonalContext(ctx context.Context) ([]db.PersonalEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.personal, nil
}

func (s *fakeStore) SetPersonalContext(ctx context.Context, subject, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.personalWrites == nil {
		s.personalWrites = map[string]string{}
	}
	s.personalWrites[subject] = content
	return nil
}

func (s *fakeStore) LogNote(ctx context.Context, content, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, content)
	s.kinds = append(s.kinds, kind)
	return int64(len(s.notes)), nil
}

func (s *fakeStore) UpdateNote(ctx context.Context, id int64, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updates == nil {
		s.updates = make(map[int64]string)
	}
	if id < 1 || int(id) > len(s.notes) {
		return fmt.Errorf("no note with id %d", id)
	}
	s.updates[id] = content
	s.notes[id-1] = content
	return nil
}

// GetNotes returns every filed note, newest first, matching the order db.Store's real GetNotes promises — which is what lets lastMatchingMeetingNote take the first match as the most recent one.
func (s *fakeStore) GetNotes(ctx context.Context) ([]db.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getNotesCalls++
	out := make([]db.Note, 0, len(s.notes))
	for i := len(s.notes) - 1; i >= 0; i-- {
		out = append(out, db.Note{ID: int64(i + 1), Content: s.notes[i], Kind: s.kinds[i]})
	}
	return out, nil
}

// logged returns the contents filed under kind, so a test can look at just the person notes or just the minutes.
func (s *fakeStore) logged(kind string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i, k := range s.kinds {
		if k == kind {
			out = append(out, s.notes[i])
		}
	}
	return out
}

// newTestRecorder builds a Recorder whose capture, whisper, Gemini and notification calls are all replaced by fakes, and whose data lives in a temp dir.
func newTestRecorder(t *testing.T, store *fakeStore) (*Recorder, *fakeCapture, *[]string) {
	t.Helper()
	cap := &fakeCapture{}
	var notes []string
	r := New(context.Background(), t.TempDir(), store, "")
	// The sweep New starts runs concurrently and would otherwise race the seams set below, and pick up recordings the test itself makes. Its data dir is empty, so this returns at once.
	<-r.swept
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		mic.Write(make([]byte, 3200))
		system.Write(make([]byte, 3200))
		now := time.Now()
		return cap, now, now.Add(500 * time.Millisecond), nil
	}
	r.whisper = func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
		if speaker == speakerMe {
			return []Segment{{Start: 0, End: time.Second, Speaker: speakerMe, Text: "shall we ship friday"}}, nil
		}
		return []Segment{{Start: 2 * time.Second, End: 3 * time.Second, Speaker: speakerCall, Text: "friday works"}}, nil
	}
	r.findWhisper = func(string) (string, error) { return "/fake/whisper", nil }
	// One seam serves both one-shot Gemini calls the pipeline makes: the minutes themselves, and the pass that asks whether the meeting changed personal context.
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[]}`, nil
		}
		if !strings.Contains(prompt, "friday works") {
			t.Errorf("minutes prompt is missing the transcript:\n%s", prompt)
		}
		return "# Minutes\n\n- ship friday", nil
	}
	r.notify = func(title, body string) { notes = append(notes, title+": "+body) }
	return r, cap, &notes
}

// Recording is a toggle: one at a time, and stopping requires an active one.
func TestRecorder_StartStopToggle(t *testing.T) {
	r, cap, _ := newTestRecorder(t, &fakeStore{})
	if r.Active() {
		t.Fatal("a fresh recorder must not be active")
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !r.Active() {
		t.Fatal("recorder must be active after Start")
	}
	if err := r.Start(); err == nil {
		t.Error("a second Start must fail rather than clobber the running recording")
	}
	s, err := r.stop()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !cap.stopped {
		t.Error("Stop must stop the capture streams")
	}
	if r.Active() {
		t.Error("recorder must not be active after Stop")
	}
	if _, err := r.stop(); err == nil {
		t.Error("stopping with nothing recording must fail")
	}
	for _, name := range []string{"mic.wav", "system.wav"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// After a recording stops, the pipeline transcribes both streams into one interleaved transcript, writes minutes, stores a note, and only then deletes the audio.
func TestRecorder_Pipeline(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{
		{CreatedAt: time.Now(), App: "zoom", Title: "Weekly sync"},
	}}
	r, _, notes := newTestRecorder(t, store)
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	sess, err := r.stop()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := r.process(context.Background(), sess); err != nil {
		t.Fatalf("process: %v", err)
	}
	dir := sess.dir

	transcript, err := os.ReadFile(filepath.Join(dir, "transcript.md"))
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	want := "[00:00:00] [me] shall we ship friday\n\n[00:00:02] [call] friday works\n"
	if !strings.Contains(string(transcript), want) {
		t.Errorf("transcript should interleave both sides chronologically, got:\n%s", transcript)
	}

	minutes, err := os.ReadFile(filepath.Join(dir, "minutes.md"))
	if err != nil {
		t.Fatalf("minutes: %v", err)
	}
	if !strings.Contains(string(minutes), "ship friday") {
		t.Errorf("minutes = %q", minutes)
	}

	if len(store.notes) != 1 || !strings.Contains(store.notes[0], "ship friday") {
		t.Errorf("expected the minutes stored as one note, got %v", store.notes)
	}
	if len(store.kinds) != 1 || store.kinds[0] != noteKind {
		t.Errorf("note kind = %v, want %q", store.kinds, noteKind)
	}

	for _, name := range []string{"mic.wav", "system.wav"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should be kept while keepAudio is on: %v", name, err)
		}
	}
	if len(*notes) == 0 {
		t.Error("the user should be notified when the minutes are ready")
	}
}

// The desktop timeline the tracker recorded during the meeting goes into the prompt, so the model knows which app and which window the call happened in.
func TestRecorder_PromptCarriesDesktopContext(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{
		{CreatedAt: time.Now(), App: "Brave Browser", Title: "Meet – weekly sync", ScreenText: "participating in a video call | Vikram Goel (Presenting) | Alex Rivera"},
	}}
	r, _, _ := newTestRecorder(t, store)
	var got string
	r.minutes = func(ctx context.Context, prompt string) (string, error) { got = prompt; return "ok", nil }
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	sess, _ := r.stop()
	if err := r.process(context.Background(), sess); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !strings.Contains(got, "Meet – weekly sync") {
		t.Errorf("prompt is missing the desktop timeline:\n%s", got)
	}
	// The window text is what carries participant names, so it has to reach the model, not just the window title.
	if !strings.Contains(got, "Vikram Goel (Presenting)") {
		t.Errorf("prompt is missing the on-screen participant names:\n%s", got)
	}
}

// Whisper can exit 0 and still yield nothing usable — a silent meeting, or a build whose output lines parseSegments does not recognise. That is not a success: the WAVs are still the only copy of the meeting, so they must survive, and nothing may be filed as minutes.
func TestRecorder_KeepsAudioWhenTranscriptionYieldsNothing(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	r.whisper = func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
		return nil, nil
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	sess, _ := r.stop()
	if err := r.process(context.Background(), sess); err == nil {
		t.Fatal("process must report that the transcription produced no speech")
	}
	for _, name := range []string{"mic.wav", "system.wav"} {
		if _, err := os.Stat(filepath.Join(sess.dir, name)); err != nil {
			t.Errorf("%s must survive a transcription that found no speech: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sess.dir, noSpeechMarker)); err != nil {
		t.Errorf("a marker naming the problem must be left in the recording dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sess.dir, "transcript.md")); !os.IsNotExist(err) {
		t.Error("an empty transcript.md must not be written, it would look like a finished recording")
	}
	if len(store.notes) != 0 {
		t.Errorf("nothing should be filed as minutes, got %v", store.notes)
	}
}

// Quitting the tray or crashing mid-recording leaves WAVs with no transcript beside them. The next time the recorder is built, those directories get picked up and processed.
func TestRecorder_ResumesOrphanedRecordings(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	<-r.swept // the sweep New starts has finished, so the fields below are ours alone
	// The sweep defers whisper work on battery, and this test's outcome must not depend on whether the machine running it happens to be plugged in.
	r.onAC = func() bool { return true }

	orphan := filepath.Join(r.dataDir, "recordings", "2026-08-27T09-30-00")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mic.wav", "system.wav"} {
		if err := os.WriteFile(filepath.Join(orphan, name), make([]byte, 3200), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A directory that already has both its transcript and its minutes is finished and must be left alone.
	done := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-26T09-30-00"), map[string]string{
		"mic.wav":       string(make([]byte, 3200)),
		"transcript.md": "[00:00:00] [me] already done\n",
		"minutes.md":    "# Meeting minutes\n",
	})

	// A recording that is running right now looks exactly like an abandoned one on disk, so the sweep must leave it alone.
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	live := r.live.dir

	r.pickup(context.Background())

	if _, err := os.Stat(filepath.Join(live, "transcript.md")); !os.IsNotExist(err) {
		t.Error("the sweep transcribed the recording that is still running")
	}
	if _, err := r.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if _, err := os.ReadFile(filepath.Join(orphan, "transcript.md")); err != nil {
		t.Errorf("the orphaned recording should have been transcribed: %v", err)
	}
	if len(store.notes) != 1 {
		t.Errorf("the orphaned recording should have filed exactly one set of minutes, got %v", store.notes)
	}
	if _, err := os.Stat(filepath.Join(done, "mic.wav")); err != nil {
		t.Errorf("an already-transcribed recording must not be reprocessed: %v", err)
	}
}

// notifications is a race-free sink for r.notify, since the silence warning fires from a timer goroutine.
type notifications struct {
	mu   sync.Mutex
	sent []string
}

func (n *notifications) add(title, body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, title+": "+body)
}

func (n *notifications) contains(s string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, got := range n.sent {
		if strings.Contains(strings.ToLower(got), s) {
			return true
		}
	}
	return false
}

// A meeting playing to a sink Ora is not recording writes an unbroken run of zero samples, which looks exactly like a working recording until the transcript comes back empty hours later. The user has to be told while the call is still running.
func TestRecorder_WarnsWhenSystemAudioStaysSilent(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	var got notifications
	r.notify = got.add
	r.silenceAfter = 20 * time.Millisecond
	// The default fake capture writes zeroes, which is exactly the wrong-sink symptom.
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.stop()

	deadline := time.Now().Add(2 * time.Second)
	for !got.contains("audio") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !got.contains("audio") {
		t.Errorf("expected a warning that the meeting audio is not reaching the recorder, got %v", got.sent)
	}
}

// loudSamples is a block of speech-level 16-bit samples, well clear of the noise floor.
func loudSamples() []byte {
	b := make([]byte, 3200)
	for i := 1; i < len(b); i += 2 {
		b[i] = 0x10 // 4096 once the low byte is added: speech level
	}
	return b
}

// Sound arriving throughout means the right sink is being recorded, and a warning then would be noise in the middle of a call.
func TestRecorder_NoSilenceWarningWhileSystemAudioKeepsArriving(t *testing.T) {
	r, cap, _ := newTestRecorder(t, &fakeStore{})
	var got notifications
	r.notify = got.add
	r.silenceAfter = 30 * time.Millisecond
	stop, finished := make(chan struct{}), make(chan struct{})
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		go func() {
			defer close(finished)
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
					mic.Write(loudSamples())
					system.Write(loudSamples())
				}
			}
		}()
		now := time.Now()
		return cap, now, now, nil
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The writer has to be off the files before stop closes them, which the real capture guarantees and this fake must too.
	defer func() {
		close(stop)
		<-finished
		r.stop()
	}()

	time.Sleep(300 * time.Millisecond)
	if got.contains("audio") {
		t.Errorf("a recording that is capturing sound must not warn, got %v", got.sent)
	}
}

// The output device can change halfway through a call — earbuds connecting, or dying and the audio hopping back to the speakers — and the recording then goes quiet from that moment. The watchdog has to catch it whenever it happens, not only in the first half-minute.
func TestRecorder_WarnsWhenSystemAudioStopsPartWayThrough(t *testing.T) {
	r, cap, _ := newTestRecorder(t, &fakeStore{})
	var got notifications
	r.notify = got.add
	r.silenceAfter = 30 * time.Millisecond
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		// Sound at the start, so the first-thirty-seconds check would have been satisfied, then nothing.
		mic.Write(loudSamples())
		system.Write(loudSamples())
		now := time.Now()
		return cap, now, now, nil
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.stop()

	deadline := time.Now().Add(2 * time.Second)
	for !got.contains("audio") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !got.contains("audio") {
		t.Errorf("a recording whose system audio stopped mid-meeting must warn, got %v", got.sent)
	}
}

// Transcribing on battery is what the 16-minute meeting was doing when it came back empty, and it drains a laptop fast either way. A recording made on battery is kept whole and left for later instead.
func TestRecorder_DefersTranscriptionUntilMainsPower(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	<-r.swept
	var got notifications
	r.notify = got.add

	onAC := false
	r.onAC = func() bool { return onAC }

	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	dir, err := r.StopAndProcess(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "transcript.md")); !os.IsNotExist(err) {
		t.Error("a recording made on battery must not be transcribed yet")
	}
	if _, err := os.Stat(filepath.Join(dir, noSpeechMarker)); !os.IsNotExist(err) {
		t.Error("a deferred recording must not be marked as having no speech, that would stop it ever being picked up")
	}
	if !got.contains("plug") {
		t.Errorf("the user must be told the transcript is waiting on mains power, got %v", got.sent)
	}
	for _, name := range []string{"mic.wav", "system.wav"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Plugged in, the periodic pickup finishes the job.
	onAC = true
	r.pickup(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "transcript.md")); err != nil {
		t.Errorf("once on mains power the deferred recording must be transcribed: %v", err)
	}
	if len(store.notes) != 1 {
		t.Errorf("the deferred recording should have filed exactly one set of minutes, got %v", store.notes)
	}
}

// The pickup must never transcribe the recording that is running right now: its WAVs are still being written, and on disk it looks exactly like an abandoned one.
func TestRecorder_PickupSkipsTheLiveRecording(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	<-r.swept
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	live := r.live.dir

	r.pickup(context.Background())

	if _, err := os.Stat(filepath.Join(live, "transcript.md")); !os.IsNotExist(err) {
		t.Error("the pickup transcribed the recording that is still running")
	}
	if _, err := r.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// captureLogs redirects the default slog logger into a buffer for the rest of the test, at Debug level so a slog.Debug call shows up too.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prior) })
	return &buf
}

// writeRecording lays out a recording directory on disk with the given files, which is how the sweep's cases are set up.
func writeRecording(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A whisper run on a long meeting can still be going the next time the sweep ticks, and the old code logged "finishing an unfinished meeting recording" for that directory anyway, every tick, though process() below was always going to find it already claimed and do nothing. The log line must wait until the sweep is actually about to hand the directory to process().
func TestRecorder_PickupDoesNotLogUntilItActuallyStartsWork(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	<-r.swept
	dir := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T11-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [me] shall we ship friday\n[00:00:02] [call] friday works\n",
	})

	logs := captureLogs(t)
	if !r.claim(dir) {
		t.Fatal("expected to be free to claim the directory before the sweep runs")
	}
	r.pickup(context.Background())
	if strings.Contains(logs.String(), "finishing an unfinished meeting recording") {
		t.Errorf("the sweep logged a directory it could not actually process:\n%s", logs.String())
	}

	r.release(dir)
	r.pickup(context.Background())
	if !strings.Contains(logs.String(), "finishing an unfinished meeting recording") {
		t.Errorf("the sweep should log once it actually starts on the directory:\n%s", logs.String())
	}
}

// A recording just ended on battery defers transcription until mains power, and the sweep must honour the same rule for anything it finds still needing whisper — otherwise the very next tick transcribes what StopAndProcess just deferred.
func TestRecorder_SweepDefersWhisperTranscriptionOnBattery(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	<-r.swept
	r.onAC = func() bool { return false }
	r.whisper = func(context.Context, string, string, string, string, time.Duration) ([]Segment, error) {
		t.Error("whisper ran for a recording found on battery")
		return nil, errors.New("whisper must not run on battery")
	}
	orphan := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T09-30-00"), map[string]string{
		"mic.wav":    string(make([]byte, 3200)),
		"system.wav": string(make([]byte, 3200)),
	})

	r.pickup(context.Background())

	if _, err := os.Stat(filepath.Join(orphan, "transcript.md")); !os.IsNotExist(err) {
		t.Error("the sweep transcribed an orphaned recording while on battery")
	}
}

// Regenerating minutes from a transcript that is already on disk never runs whisper, so it costs nothing extra on battery and must not wait for the same deferral that a fresh transcription does.
func TestRecorder_SweepRegeneratesMinutesOnBatteryEvenThoughTranscriptionWaits(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	<-r.swept
	r.onAC = func() bool { return false }
	dir := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T11-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [me] shall we ship friday\n[00:00:02] [call] friday works\n",
	})

	r.pickup(context.Background())

	if _, err := os.Stat(filepath.Join(dir, "minutes.md")); err != nil {
		t.Errorf("regenerating minutes from an existing transcript should not wait for mains power: %v", err)
	}
}

// A blank reply from the brain is not a summary of anything. Writing it as minutes.md would mark the meeting done, so the next sweep would never look at it again and the meeting would be lost. It must be treated like any other failed summarisation.
func TestRecorder_EmptyMinutesIsTreatedAsAFailure(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[]}`, nil
		}
		return "   \n", nil
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	sess, _ := r.stop()

	if err := r.process(context.Background(), sess); err == nil {
		t.Fatal("process must report an empty brain reply as a failure")
	}
	if _, err := os.Stat(filepath.Join(sess.dir, "minutes.md")); !os.IsNotExist(err) {
		t.Error("an empty brain reply must never be written as minutes.md")
	}
	if !exists(filepath.Join(sess.dir, failedMarker)) {
		t.Error("an empty brain reply must leave a failure marker so the sweep retries it later")
	}
}

// A summariser that refuses — a rate limit, a bad key, a prompt too big — used to leave the recording in exactly the shape the sweep looks for, so every tick spent another API call on the same failure. The failure is recorded next to the recording and the sweep leaves it alone until the marker is an hour old.
func TestRecorder_FailedProcessingWaitsBeforeBeingRetried(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	<-r.swept
	calls, failing := 0, true
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[]}`, nil
		}
		calls++
		if failing {
			return "", errors.New("429 from the summariser")
		}
		return "# Minutes\n\n- ship friday", nil
	}
	dir := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T11-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [me] shall we ship friday\n",
	})
	marker := filepath.Join(dir, failedMarker)

	r.pickup(context.Background())
	if calls != 1 {
		t.Fatalf("the sweep should have tried to summarise the recording once, it made %d calls", calls)
	}
	if !exists(marker) {
		t.Fatal("a recording whose summary failed left no failure marker behind")
	}

	r.pickup(context.Background())
	if calls != 1 {
		t.Errorf("the failed recording was summarised again on the next tick, %d calls in total", calls)
	}

	// Age the marker past the backoff window, which is what the passage of an hour does in production.
	old := time.Now().Add(-2 * failureRetryAfter)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	failing = false
	r.pickup(context.Background())
	if calls != 2 {
		t.Errorf("an hour-old failure should have been retried, %d calls in total", calls)
	}
	if exists(marker) {
		t.Error("the failure marker survived a run that succeeded")
	}
}

// A recording that never got its streams open leaves stub WAVs the sweep would read as an abandoned recording and try to transcribe on every tick.
func TestRecorder_StartCleansUpAfterItselfWhenCaptureFails(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	r.capture = func(io.Writer, io.Writer) (capturer, time.Time, time.Time, error) {
		return nil, time.Time{}, time.Time{}, errors.New("no sound server")
	}
	if err := r.Start(); err == nil {
		t.Fatal("Start reported success though capture could not be opened")
	}
	entries, err := os.ReadDir(filepath.Join(r.dataDir, "recordings"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed Start left %d recording directories behind", len(entries))
	}
}

// Summarising a recording twice must leave one meeting note holding the newest minutes. Regeneration used to file a second note, so memory ended up with several drafts of the same meeting and no way to tell which one was current.
func TestRecorder_RegenerationReplacesTheNoteItAlreadyFiled(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	dir := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T11-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [me] shall we ship friday\n[00:00:02] [call] friday works\n",
	})
	text := "# Minutes\n\n- first pass"
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[]}`, nil
		}
		return text, nil
	}

	now := time.Now()
	for _, pass := range []string{"# Minutes\n\n- first pass", "# Minutes\n\n- second pass"} {
		text = pass
		if err := r.process(context.Background(), &session{dir: dir, startedAt: now, stoppedAt: now, fromTranscript: true}); err != nil {
			t.Fatalf("summarising %s: %v", pass, err)
		}
	}

	got := store.logged(noteKind)
	if len(got) != 1 {
		t.Fatalf("regenerating minutes should leave one meeting note, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "second pass") {
		t.Errorf("the meeting note still holds the superseded minutes: %s", got[0])
	}
	if b, err := os.ReadFile(filepath.Join(dir, noteIDFile)); err != nil || strings.TrimSpace(string(b)) != "1" {
		t.Errorf("the recording should remember the note it was filed under, read %q err %v", b, err)
	}
}

// Deleting minutes.md is how the user asks for the minutes again, and a crash between writing the transcript and writing the minutes leaves the same shape on disk. Either way the sweep summarises the transcript that is already there and never runs whisper again.
func TestRecorder_RegeneratesMinutesFromAnExistingTranscript(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	<-r.swept
	r.whisper = func(context.Context, string, string, string, string, time.Duration) ([]Segment, error) {
		t.Error("whisper ran for a recording that already had its transcript")
		return nil, errors.New("whisper must not run")
	}
	r.findWhisper = func(string) (string, error) {
		t.Error("the whisper binary was looked up for a recording that already had its transcript")
		return "", errors.New("whisper must not be needed")
	}
	var sawPersonalPass bool
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			sawPersonalPass = true
			return `{"updates":[{"subject":"priya-shah","content":"Priya Shah was in the call."}]}`, nil
		}
		if !strings.Contains(prompt, "friday works") {
			t.Errorf("the regenerated minutes prompt is missing the existing transcript:\n%s", prompt)
		}
		return "# Meeting minutes\n\n## Attendees\n", nil
	}

	// note-id.txt is what says this meeting was filed once already, which is what deleting minutes.md asks Ora to do again.
	regen := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T11-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [me] shall we ship friday\n[00:00:02] [call] friday works\n",
		"note-id.txt":   "1",
		"mic.wav":       "",
		"system.wav":    "",
	})
	// A directory with both its transcript and its minutes is finished, and must be left exactly as it is.
	whole := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-26T11-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [me] already done\n",
		"minutes.md":    "# Meeting minutes\n(the original)\n",
	})

	r.pickup(context.Background())

	if _, err := os.ReadFile(filepath.Join(regen, "minutes.md")); err != nil {
		t.Errorf("the transcript with no minutes should have been summarised: %v", err)
	}
	if got := store.logged(noteKind); len(got) != 1 {
		t.Errorf("regenerating minutes should file exactly one note, got %v", got)
	}
	// The meeting already had its one chance to teach Ora something durable about a person when it was first summarised. Regenerating minutes from the same transcript must not run the updater again and re-propose the same writes.
	if sawPersonalPass {
		t.Error("regenerating minutes ran the personal context updater again")
	}
	if len(store.personalWrites) != 0 {
		t.Errorf("regenerating minutes wrote to personal context, writes: %v", store.personalWrites)
	}
	if got, _ := os.ReadFile(filepath.Join(whole, "minutes.md")); !strings.Contains(string(got), "the original") {
		t.Errorf("a finished recording was summarised again, its minutes now read: %s", got)
	}
}

// A recording the sweep finds has to be dated from what is on disk, and the meeting's window is what decides which screens the summary is written from. How much audio was recorded says how long the meeting ran; the file's timestamp does not, because anything that touches the file afterwards moves it.
func TestPickupSession_DatesTheMeetingFromHowMuchAudioThereIs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-08-27T10-00-00")
	// Ten minutes of 16 kHz mono 16-bit samples, in a file last written three hours after the meeting ended.
	writeRecording(t, dir, map[string]string{"mic.wav": string(make([]byte, wavHeaderSize+10*60*sampleRate*2))})
	touched := time.Date(2026, 8, 27, 13, 0, 0, 0, time.Local)
	if err := os.Chtimes(filepath.Join(dir, "mic.wav"), touched, touched); err != nil {
		t.Fatal(err)
	}

	s := pickupSession(dir)

	if want := time.Date(2026, 8, 27, 10, 0, 0, 0, time.Local); !s.startedAt.Equal(want) {
		t.Errorf("started at %s, want %s", s.startedAt, want)
	}
	if want := time.Date(2026, 8, 27, 10, 10, 0, 0, time.Local); !s.stoppedAt.Equal(want) {
		t.Errorf("stopped at %s, want %s — the meeting's length should come from the audio, not the file's timestamp", s.stoppedAt, want)
	}
}

// With the audio gone there is nothing to measure, so the last write to the transcript is the best guess left at when the meeting ended.
func TestPickupSession_FallsBackToTheTranscriptWhenTheAudioIsGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-08-27T10-00-00")
	writeRecording(t, dir, map[string]string{"transcript.md": "[00:00:00] [me] hello\n"})
	written := time.Date(2026, 8, 27, 10, 25, 0, 0, time.Local)
	if err := os.Chtimes(filepath.Join(dir, "transcript.md"), written, written); err != nil {
		t.Fatal(err)
	}

	if s := pickupSession(dir); !s.stoppedAt.Equal(written) {
		t.Errorf("stopped at %s, want the transcript's timestamp %s", s.stoppedAt, written)
	}
}

// The claim set covers the regeneration path too: a directory something else is already processing must not be summarised underneath it.
func TestRecorder_RegenerationRespectsTheClaimSet(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	<-r.swept
	dir := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-08-27T12-00-00"), map[string]string{
		"transcript.md": "[00:00:00] [call] friday works\n",
	})
	if !r.claim(dir) {
		t.Fatal("the directory should have been free to claim")
	}

	r.pickup(context.Background())

	if _, err := os.Stat(filepath.Join(dir, "minutes.md")); !os.IsNotExist(err) {
		t.Error("the sweep summarised a directory that was already claimed")
	}
}

// The live-recording guard sits ahead of both sweep cases, so a directory that is being recorded into right now is skipped whatever else it holds.
func TestRecorder_RegenerationSkipsTheLiveRecording(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	<-r.swept
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	live := r.live.dir
	if err := os.WriteFile(filepath.Join(live, "transcript.md"), []byte("[00:00:00] [call] friday works\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.pickup(context.Background())

	if _, err := os.Stat(filepath.Join(live, "minutes.md")); !os.IsNotExist(err) {
		t.Error("the sweep summarised the recording that is still running")
	}
	if _, err := r.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// OnACPower reads the kernel's power supply class, so a battery-only machine and a plugged-in one are told apart from the files themselves.
func TestOnACPower(t *testing.T) {
	write := func(t *testing.T, supplies map[string][2]string) string {
		root := t.TempDir()
		for name, kv := range supplies {
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "type"), []byte(kv[0]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if kv[1] != "" {
				if err := os.WriteFile(filepath.Join(dir, "online"), []byte(kv[1]+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return root
	}
	cases := []struct {
		name     string
		supplies map[string][2]string
		want     bool
	}{
		{name: "charger plugged in", supplies: map[string][2]string{"AC": {"Mains", "1"}, "BAT0": {"Battery", ""}}, want: true},
		{name: "running on battery", supplies: map[string][2]string{"AC": {"Mains", "0"}, "BAT0": {"Battery", ""}}, want: false},
		{name: "no mains supply at all, so the machine cannot be on battery", supplies: map[string][2]string{"BAT0": {"Battery", ""}}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := powerSupplyRoot
			powerSupplyRoot = write(t, c.supplies)
			defer func() { powerSupplyRoot = old }()
			if got := OnACPower(); got != c.want {
				t.Errorf("OnACPower() = %v, want %v", got, c.want)
			}
		})
	}
}

// A machine that does not export the power supply class at all — a desktop, or Windows — must count as plugged in, or nothing would ever be transcribed there.
func TestOnACPower_UnknownCountsAsMains(t *testing.T) {
	old := powerSupplyRoot
	powerSupplyRoot = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { powerSupplyRoot = old }()
	if !OnACPower() {
		t.Error("a machine that cannot report its power source must be treated as on mains")
	}
}

// A failed transcription must not destroy the recording — the WAVs are the only copy of what was said.
func TestRecorder_KeepsAudioWhenTranscriptionFails(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	r.whisper = func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
		return nil, errors.New("whisper exploded")
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	sess, _ := r.stop()
	if err := r.process(context.Background(), sess); err == nil {
		t.Fatal("process should report the transcription failure")
	}
	for _, name := range []string{"mic.wav", "system.wav"} {
		if _, err := os.Stat(filepath.Join(sess.dir, name)); err != nil {
			t.Errorf("%s must survive a failed transcription: %v", name, err)
		}
	}
}

// Nothing on a screen identifies whose computer it is: a repository page names its committers, a document names its author. A real meeting was filed with the recorder called "Deepak" because a GitHub commit list said "deepak-acmee". Personal context is the one store that says who the user is with certainty, so it is what names the [me] speaker — and nothing Ora merely inferred goes near this prompt.
func TestBuildPrompt_CarriesPersonalContextAboutTheUser(t *testing.T) {
	store := &fakeStore{personal: []db.PersonalEntry{
		{Subject: "identity", Content: "The user is Alex Rivera — goes by Alex; git handle M-DEV-1."},
	}}
	r, _, _ := newTestRecorder(t, store)
	prompt := r.buildPrompt(context.Background(), "[00:00:00] [me] hello", time.Now(), time.Now())
	if !strings.Contains(prompt, "Alex Rivera") {
		t.Errorf("the minutes prompt must carry the personal context entry that names the user:\n%s", prompt)
	}
	if !strings.Contains(prompt, "About the person recording") {
		t.Errorf("the personal context entries lost their heading:\n%s", prompt)
	}
}

// TestBuildPrompt_NoUserBlockWithoutPersonalContext keeps an empty store from putting a heading in the prompt with nothing under it.
func TestBuildPrompt_NoUserBlockWithoutPersonalContext(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	prompt := r.buildPrompt(context.Background(), "[00:00:00] [me] hello", time.Now(), time.Now())
	if strings.Contains(prompt, "About the person recording —") {
		t.Errorf("empty personal context still produced a block:\n%s", prompt)
	}
}

// The two sides of a call are independent files, and transcribing them one after the other doubles the wall time of every meeting for no reason. They have to run at the same time.
// The test is a barrier: each stream announces itself and then waits for the other, so a pipeline that runs them in sequence can never get past the first one.
func TestTranscriptFor_TranscribesBothStreamsAtTheSameTime(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	arrived := make(chan struct{}, 2)
	both := make(chan struct{})
	var once sync.Once
	r.whisper = func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
		arrived <- struct{}{}
		if len(arrived) == 2 {
			once.Do(func() { close(both) })
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("%s waited for the other stream to start and it never did", speaker)
		}
		return []Segment{{Start: 0, End: time.Second, Speaker: speaker, Text: "spoken by " + speaker}}, nil
	}

	dir := t.TempDir()
	s := &session{dir: dir, startedAt: time.Now(), stoppedAt: time.Now()}
	transcript, err := r.transcriptFor(context.Background(), s)
	if err != nil {
		t.Fatalf("transcriptFor: %v", err)
	}
	for _, want := range []string{"spoken by me", "spoken by call"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript is missing %q:\n%s", want, transcript)
		}
	}
}

// A failure on either stream still has to come back as an error naming which side failed, now that the two run concurrently.
func TestTranscriptFor_ReportsWhichStreamFailed(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	r.whisper = func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
		if speaker == speakerCall {
			return nil, errors.New("the model file is corrupt")
		}
		return []Segment{{Start: 0, End: time.Second, Speaker: speakerMe, Text: "hello"}}, nil
	}
	s := &session{dir: t.TempDir(), startedAt: time.Now(), stoppedAt: time.Now()}
	_, err := r.transcriptFor(context.Background(), s)
	if err == nil {
		t.Fatal("a failed stream must be an error")
	}
	if !strings.Contains(err.Error(), "system audio") || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error %q does not say which stream failed and why", err)
	}
}

// Two whisper runs sharing one machine must each take about half the threads, or the concurrency buys nothing and the two runs simply fight over the same cores.
func TestTranscribeThreads(t *testing.T) {
	got := transcribeThreads()
	if got < 1 {
		t.Fatalf("transcribeThreads() = %d, want at least 1", got)
	}
	if max := runtime.NumCPU()/2 + 1; got > max {
		t.Errorf("transcribeThreads() = %d, want no more than %d so two concurrent runs do not oversubscribe %d cores", got, max, runtime.NumCPU())
	}
}

// The thread count is a property of the machine, not of the code, so it has to be overridable without a rebuild.
func TestTranscribeThreads_HonoursTheOverride(t *testing.T) {
	t.Setenv("ORA_TRANSCRIBE_THREADS", "6")
	if got := transcribeThreads(); got != 6 {
		t.Errorf("transcribeThreads() = %d, want the configured 6", got)
	}
	t.Setenv("ORA_TRANSCRIBE_THREADS", "not a number")
	if got := transcribeThreads(); got < 1 {
		t.Errorf("transcribeThreads() = %d with junk configured, want the default", got)
	}
}

// AddActionItems records what the recorder lifted out of a meeting's minutes, so a test can see the action items filing them produced.
func (s *fakeStore) AddActionItems(ctx context.Context, items []memory.ActionItem) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, items...)
	return len(items), nil
}

// CloseDoneActionItems records that the recorder asked for the evidence sweep after filing a meeting, and closes nothing.
func (s *fakeStore) CloseDoneActionItems(ctx context.Context, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeSweeps++
	return 0, nil
}

// Filing a meeting's minutes also lifts its action items out into their own tracked rows, so the things somebody agreed to do outlive the three-day window the minutes themselves are read in. Every item is kept, the other people's included: whose an item is comes from its owner read against who the user is, so an "Vikram" bullet is something he is waiting for rather than something the store never heard.
func TestFileMinutes_LiftsActionItems(t *testing.T) {
	store := &fakeStore{}
	r := New(context.Background(), t.TempDir(), store, "FAKE_API_KEY")
	raised := time.Date(2026, 8, 28, 21, 36, 0, 0, time.UTC)

	r.fileMinutes(context.Background(), t.TempDir(), `# Meeting minutes

**md x mf tool — Google Meet, Fri 28 Aug 2026, 21:36–23:08 IST**

## Action items
- **Vikram** — carry PR #13 through CI and merge.
- **Me** — compare these minutes against his own agent's output.
`, raised, raised.Add(90*time.Minute))

	if len(store.actions) != 2 {
		t.Fatalf("want both action items lifted, got %d: %+v", len(store.actions), store.actions)
	}
	mine := store.actions[1]
	if store.actions[0].Owner != "Vikram" {
		t.Errorf("the other person's item was dropped instead of kept to wait on: %+v", store.actions)
	}
	if mine.Owner != "Me" || mine.Text != "compare these minutes against his own agent's output." {
		t.Errorf("lifted item = %+v", mine)
	}
	if mine.Source != "md x mf tool" || !mine.Raised.Equal(raised) {
		t.Errorf("provenance = %q / %v", mine.Source, mine.Raised)
	}
	if mine.Status != memory.StatusOpen {
		t.Errorf("lifted item is not open: %q", mine.Status)
	}
	if store.closeSweeps != 1 {
		t.Errorf("close sweeps after filing a meeting = %d, want 1", store.closeSweeps)
	}
	// The minutes themselves must still be filed unchanged: they are the record of what was said and nothing may edit them.
	if len(store.notes) != 1 || !strings.Contains(store.notes[0], "## Action items") {
		t.Errorf("the minutes were not filed intact: %+v", store.notes)
	}
}

// A meeting with one item the user owes, one owed by somebody else and one nobody was named for files all three with their owners intact. Filing is not the place whose-is-it gets decided: the store answers that on every read, from the owner against who the user is, so the user's own list and the list of what he is waiting on both come out of the same rows.
func TestFileMinutes_LiftsEveryItemWithItsOwner(t *testing.T) {
	store := &fakeStore{}
	r := New(context.Background(), t.TempDir(), store, "FAKE_API_KEY")

	r.fileMinutes(context.Background(), t.TempDir(), `# Meeting minutes

## Action items
- **Me** — send the deck by Friday.
- **Sandeep** — add battery optimisation to the app.
- **Owner unclear** — trial attaching walkthrough videos to PRs.
`, time.Now(), time.Now())

	if len(store.actions) != 3 {
		t.Fatalf("want 3 action items lifted, got %d: %+v", len(store.actions), store.actions)
	}
	want := []string{memory.MeOwner, "Sandeep", memory.UnknownOwner}
	for i, a := range store.actions {
		if a.Owner != want[i] {
			t.Errorf("item %d owner = %q, want %q", i, a.Owner, want[i])
		}
	}
}

// Minutes with no action items file normally and lift nothing.
func TestFileMinutes_NoActionItems(t *testing.T) {
	store := &fakeStore{}
	r := New(context.Background(), t.TempDir(), store, "FAKE_API_KEY")

	r.fileMinutes(context.Background(), t.TempDir(), "# Meeting minutes\n\n## Key points\n- nothing was agreed.\n", time.Now(), time.Now())

	if len(store.actions) != 0 {
		t.Errorf("lifted %d items from minutes with no action section", len(store.actions))
	}
	if len(store.notes) != 1 {
		t.Errorf("the minutes were not filed: %+v", store.notes)
	}
}

// fileMinutes appends the recording's wall-clock start and stop to the note it files, in RFC3339, since that is the only place the meeting's actual duration is known — the model writing the minutes is never told to report it.
func TestFileMinutes_RecordsStartAndStop(t *testing.T) {
	store := &fakeStore{}
	r := New(context.Background(), t.TempDir(), store, "FAKE_API_KEY")
	started := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	stopped := started.Add(41 * time.Minute)

	r.fileMinutes(context.Background(), t.TempDir(), "# Standup\n\n## Key points\n- shipped it.\n", started, stopped)

	if len(store.notes) != 1 {
		t.Fatalf("want 1 note filed, got %d", len(store.notes))
	}
	want := "<!--ora:duration start=2026-09-04T10:00:00Z stop=2026-09-04T10:41:00Z-->"
	if !strings.Contains(store.notes[0], want) {
		t.Errorf("filed note missing duration marker %q, got %q", want, store.notes[0])
	}
	if !strings.Contains(store.notes[0], "## Key points\n- shipped it.") {
		t.Errorf("filed note lost the minutes text: %q", store.notes[0])
	}
}

// Correcting an already-filed meeting's minutes (the noteIDFile path) must also correct its duration marker, not leave the first run's stale start/stop behind.
func TestFileMinutes_CorrectingANoteUpdatesTheDurationMarker(t *testing.T) {
	store := &fakeStore{}
	r := New(context.Background(), t.TempDir(), store, "FAKE_API_KEY")
	dir := t.TempDir()
	first := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	r.fileMinutes(context.Background(), dir, "# Standup\n", first, first.Add(10*time.Minute))
	second := time.Date(2026, 9, 4, 9, 5, 0, 0, time.UTC)
	r.fileMinutes(context.Background(), dir, "# Standup, corrected\n", second, second.Add(20*time.Minute))

	if len(store.notes) != 1 {
		t.Fatalf("correcting the same recording must not file a second note, got %d", len(store.notes))
	}
	want := "<!--ora:duration start=2026-09-04T09:05:00Z stop=2026-09-04T09:25:00Z-->"
	if !strings.Contains(store.notes[0], want) {
		t.Errorf("corrected note missing updated duration marker %q, got %q", want, store.notes[0])
	}
}

// A meeting filed before the duration marker existed has a note with nothing after its minutes text. The startup sweep must correct that note in place from the one thing on disk that still says how long the meeting ran: the size of the mic.wav next to it, read as wall-clock seconds from the directory's own timestamp.
func TestBackfillDurations_FillsInANoteFiledBeforeTheMarkerExisted(t *testing.T) {
	store := &fakeStore{notes: []string{"# Standup\n\n## Key points\n- shipped it.\n"}, kinds: []string{noteKind}}
	dataDir := t.TempDir()
	r := New(context.Background(), dataDir, store, "")
	<-r.swept

	started := time.Date(2026, 8, 20, 9, 0, 0, 0, time.Local)
	dir := filepath.Join(dataDir, "recordings", started.Format(dirTimeLayout))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note-id.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "minutes.md"), []byte(store.notes[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.md"), []byte("[me] shipped it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 5 seconds of 16kHz mono 16-bit silence after the header, matching audioDuration's own arithmetic.
	wav := make([]byte, wavHeaderSize+5*2*sampleRate)
	if err := os.WriteFile(filepath.Join(dir, "mic.wav"), wav, 0o644); err != nil {
		t.Fatal(err)
	}

	r.backfillOnce(context.Background())

	want := meetingDurationLine(started, started.Add(5*time.Second))
	if !strings.Contains(store.notes[0], want) {
		t.Errorf("backfilled note = %q, want it to contain %q", store.notes[0], want)
	}
}

// A recording whose mic.wav is gone (deleted, or made before keepAudio existed) has nothing left on disk to say how long it ran, so the sweep must leave its note alone rather than write a wrong or zero-length marker that looks like real data.
func TestBackfillDurations_LeavesADurationAloneWhenTheAudioIsGone(t *testing.T) {
	store := &fakeStore{notes: []string{"# Standup\n"}, kinds: []string{noteKind}}
	dataDir := t.TempDir()
	r := New(context.Background(), dataDir, store, "")
	<-r.swept

	dir := filepath.Join(dataDir, "recordings", "2026-08-20T09-00-00")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "note-id.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "minutes.md"), []byte(store.notes[0]), 0o644)
	os.WriteFile(filepath.Join(dir, "transcript.md"), []byte("[me] shipped it\n"), 0o644)
	// No mic.wav at all.

	r.backfillOnce(context.Background())

	if strings.Contains(store.notes[0], meetingDurationPrefix) {
		t.Errorf("backfilled a duration with no audio to measure it from: %q", store.notes[0])
	}
}

// DistinctTitles serves the fake's own episode titles, which is enough for prep to judge which words recur.
func (f *fakeStore) DistinctTitles(ctx context.Context, limit int) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, e := range f.episodes {
		if e.Title != "" && !seen[e.Title] {
			seen[e.Title] = true
			out = append(out, e.Title)
		}
	}
	return out, nil
}

// Killing the daemon mid-meeting on 2026-09-01 left a recording with no transcript, because nothing on the shutdown path stopped it. The audio survived only because the next start sweeps for unfinished recordings. Closing the files on the way out is what makes that a fallback rather than the mechanism.
func TestStopForShutdown_ClosesTheRecordingWithoutTranscribing(t *testing.T) {
	r, cap, _ := newTestRecorder(t, &fakeStore{})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}

	dir, err := r.StopForShutdown()

	if err != nil {
		t.Fatalf("StopForShutdown: %v", err)
	}
	if dir == "" {
		t.Error("want the directory of the recording that was closed")
	}
	if r.Active() {
		t.Error("a recording is still running after shutdown stopped it")
	}
	if !cap.stopped {
		t.Error("the capture was not stopped, so the audio device stays held")
	}
}

// Shutting down with nothing recording is the normal case and must not be an error the daemon logs on every exit.
func TestStopForShutdown_QuietWhenNothingIsRecording(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})

	dir, err := r.StopForShutdown()

	if err != nil || dir != "" {
		t.Errorf("StopForShutdown = %q, %v; want no directory and no error", dir, err)
	}
}

// In a standup the user talks for minutes while everyone else is muted, and Teams then sends exact zeros on the call side. On 2026-09-02 14:34 that fired the "audio isn't reaching the recorder" warning in the middle of the user's own update. Sound on either stream means the recording is alive.
func TestRecorder_NoSilenceWarningWhileMicKeepsArriving(t *testing.T) {
	r, cap, _ := newTestRecorder(t, &fakeStore{})
	var got notifications
	r.notify = got.add
	r.silenceAfter = 30 * time.Millisecond
	stop, finished := make(chan struct{}), make(chan struct{})
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		go func() {
			defer close(finished)
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
					mic.Write(loudSamples())
					system.Write(make([]byte, 3200))
				}
			}
		}()
		now := time.Now()
		return cap, now, now, nil
	}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		close(stop)
		<-finished
		r.stop()
	}()

	time.Sleep(300 * time.Millisecond)
	if got.contains("audio") {
		t.Errorf("a recording whose microphone is still capturing speech must not warn, got %v", got.sent)
	}
}

// TestRecorder_GivesUpOnARecordingThatKeepsFailing pins the retry cap. The recording of 2026-09-05T00-53-59 failed its first summary at 01:18 and was retried once an hour for the rest of the day — sixteen more identical 404s, each one holding the GPU at a moment a dictation might start. After maxProcessAttempts the sweep stops, and what happened is filed as a meeting note so the user sees it in the meetings list instead of only in the log.
func TestRecorder_GivesUpOnARecordingThatKeepsFailing(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	<-r.swept
	calls := 0
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[]}`, nil
		}
		calls++
		return "", errors.New("404 models/gpt-5.5 is not found for API version v1beta")
	}
	dir := writeRecording(t, filepath.Join(r.dataDir, "recordings", "2026-09-05T00-53-59"), map[string]string{
		"transcript.md": "[00:00:00] [me] shall we ship friday\n",
	})
	marker := filepath.Join(dir, failedMarker)

	// Twice as many sweeps as the cap allows attempts, each one an hour after the last as far as the backoff can tell.
	for i := 0; i < 2*maxProcessAttempts; i++ {
		r.pickup(context.Background())
		if exists(marker) {
			old := time.Now().Add(-2 * failureRetryAfter)
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	if calls != maxProcessAttempts {
		t.Errorf("the summariser was called %d times, want the cap of %d", calls, maxProcessAttempts)
	}
	store.mu.Lock()
	notes, kinds := append([]string(nil), store.notes...), append([]string(nil), store.kinds...)
	store.mu.Unlock()
	if len(notes) != 1 {
		t.Fatalf("giving up filed %d notes, want exactly one so the meeting shows in the list", len(notes))
	}
	if kinds[0] != noteKind {
		t.Errorf("the note was filed as kind %q, want %q so GET /meetings picks it up", kinds[0], noteKind)
	}
	if !strings.Contains(notes[0], "gpt-5.5") {
		t.Errorf("the note does not say why the recording failed: %q", notes[0])
	}
}

// With nothing recording, LiveSnapshot has nothing to report.
func TestRecorder_LiveSnapshot_NoneRunning(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	if _, ok := r.LiveSnapshot(context.Background()); ok {
		t.Error("LiveSnapshot should report false when no meeting is being recorded")
	}
}

// While a meeting is running, LiveSnapshot reads the window and participants off the same screen text prep.go uses, and reports that nothing has been transcribed yet: whisper only ever runs once, after the recording stops, so there is no transcript to show mid-call.
func TestRecorder_LiveSnapshot_WhileRecording(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{
		{CreatedAt: time.Now(), Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: hello"},
	}}
	r, _, _ := newTestRecorder(t, store)
	before := time.Now()
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.stop()

	snap, ok := r.LiveSnapshot(context.Background())
	if !ok {
		t.Fatal("LiveSnapshot should report true while a meeting is being recorded")
	}
	if snap.StartedAt.Before(before) || snap.StartedAt.After(time.Now()) {
		t.Errorf("StartedAt = %v, want between %v and now", snap.StartedAt, before)
	}
	if snap.Window != "Meet - abc-defg-hij - Brave" {
		t.Errorf("Window = %q, want the meeting window's title", snap.Window)
	}
	if len(snap.Participants) != 1 || snap.Participants[0] != "Priya Shah" {
		t.Errorf("Participants = %v, want [Priya Shah]", snap.Participants)
	}
	if len(snap.SegmentsSoFar) != 0 {
		t.Errorf("SegmentsSoFar = %v, want empty: transcription is one pass at stop, not incremental", snap.SegmentsSoFar)
	}
	if !snap.TranscribedThrough.IsZero() {
		t.Errorf("TranscribedThrough = %v, want zero: nothing has been transcribed yet", snap.TranscribedThrough)
	}
	if snap.Note == "" {
		t.Error("Note should explain why SegmentsSoFar is empty")
	}
}

// A recording directory is named to the second, so two recordings that start inside one second want the same name. os.MkdirAll is happy with a directory that already exists and newWAV truncates what it opens, so the second recording used to overwrite the first meeting's audio while that meeting was still waiting to be transcribed — and both sessions then carried the same directory, so the second was never summarised either.
func TestMakeRecordingDir_NeverHandsOutADirectoryThatIsAlreadyARecording(t *testing.T) {
	root := t.TempDir()

	first, err := makeRecordingDir(root)
	if err != nil {
		t.Fatalf("makeRecordingDir: %v", err)
	}
	audio := filepath.Join(first, "mic.wav")
	if err := os.WriteFile(audio, make([]byte, 100000), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := makeRecordingDir(root)
	if err != nil {
		t.Fatalf("makeRecordingDir: %v", err)
	}
	if second == first {
		t.Fatalf("both recordings got %s, so the second truncates the first meeting's audio", first)
	}
	if info, err := os.Stat(audio); err != nil || info.Size() != 100000 {
		t.Fatalf("the first recording's mic.wav is %v (%v), want its 100000 bytes untouched", info, err)
	}
}

// Start has to go through that, not around it: this is the case where the user stops one meeting from the tray and the next call starts inside the same second.
func TestRecorder_StartNeverOpensInsideARecordingThatIsAlreadyThere(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	taken := filepath.Join(r.dataDir, "recordings", time.Now().Format(dirTimeLayout))
	if err := os.MkdirAll(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(taken, "mic.wav")
	if err := os.WriteFile(audio, make([]byte, 100000), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.stop()

	if dir := r.liveDir(); dir == taken {
		t.Errorf("the new recording opened its WAVs inside %s, which already holds a recording", taken)
	}
	if info, err := os.Stat(audio); err != nil || info.Size() != 100000 {
		t.Fatalf("the earlier recording's mic.wav is %v (%v), want its 100000 bytes untouched", info, err)
	}
}

// Whisper succeeded and the minutes call then failed, so the sweep retries the recording from the transcript already on disk. That retry is the first run that ever reaches minutes, and therefore the only chance this meeting has to say anything about the people in it. Gating the personal-context pass on where the transcript came from skipped it for good.
func TestProcess_UpdatesPersonalContextOnTheRetryThatFirstFilesTheMinutes(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[{"subject":"priya-shah","content":"Priya Shah is a colleague at Acme."}]}`, nil
		}
		return "# Minutes\n\n- ship friday", nil
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "transcript.md"), []byte("[00:00:00] [me] shall we ship friday\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &session{dir: dir, startedAt: time.Now(), stoppedAt: time.Now(), fromTranscript: true}

	if err := r.process(context.Background(), s); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got := store.personalWrites["priya-shah"]; got == "" {
		t.Error("a meeting whose first attempt failed never updated personal context, so what it taught Ora is lost for good")
	}
}

// The same pass must not run twice for one meeting. A recording that has already been filed has its note id on disk, which is the evidence that a run got as far as minutes — deleting minutes.md to ask for a better summary re-summarises the meeting without re-proposing the same personal-context writes.
func TestProcess_DoesNotUpdatePersonalContextForAMeetingAlreadyFiled(t *testing.T) {
	store := &fakeStore{notes: []string{"# Minutes"}, kinds: []string{noteKind}}
	r, _, _ := newTestRecorder(t, store)
	asked := 0
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			asked++
			return `{"updates":[]}`, nil
		}
		return "# Minutes\n\n- ship friday", nil
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "transcript.md"), []byte("[00:00:00] [me] shall we ship friday\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, noteIDFile), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &session{dir: dir, startedAt: time.Now(), stoppedAt: time.Now(), fromTranscript: true}

	if err := r.process(context.Background(), s); err != nil {
		t.Fatalf("process: %v", err)
	}
	if asked != 0 {
		t.Errorf("the personal-context pass ran %d times for a meeting already filed, re-proposing what it wrote the first time", asked)
	}
}

// The retry loop lives as long as the daemon, and has to end with it: time.Tick's ticker is never stopped and never checks whether anyone is still listening, so the loop kept a recording being transcribed after the daemon had been told to quit.
func TestRetryDeferred_EndsWithTheContextItWasGiven(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.retryDeferred(ctx)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retryDeferred is still running after its context was cancelled")
	}
}

// The duration backfill corrects notes filed before the duration marker existed, which is a one-time job. It was reading every note in the store on every sweep — that is every two minutes, for the life of the daemon, to find nothing.
func TestPickup_DoesNotReadEveryNoteInTheStoreOnEverySweep(t *testing.T) {
	store := &fakeStore{notes: []string{"# Standup\n"}, kinds: []string{noteKind}}
	r, _, _ := newTestRecorder(t, store)

	dir := filepath.Join(r.dataDir, "recordings", "2026-08-20T09-00-00")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note-id.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "minutes.md"), []byte("# Standup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.md"), []byte("[me] shipped it\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	store.getNotesCalls = 0
	store.mu.Unlock()

	r.pickup(context.Background())

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.getNotesCalls != 0 {
		t.Errorf("the sweep read every note in the store %d times, want none: the backfill runs once at startup", store.getNotesCalls)
	}
}
