package recorder

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/db"
)

type fakeCapture struct{ stopped bool }

func (f *fakeCapture) Stop() { f.stopped = true }

type fakeStore struct {
	episodes []db.Episode
	notes    []string
	kinds    []string
	mu       sync.Mutex
}

func (s *fakeStore) EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error) {
	return s.episodes, nil
}

func (s *fakeStore) LogNote(ctx context.Context, content, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, content)
	s.kinds = append(s.kinds, kind)
	return int64(len(s.notes)), nil
}

// newTestRecorder builds a Recorder whose capture, whisper, Gemini and notification calls are all replaced by fakes, and whose data lives in a temp dir.
func newTestRecorder(t *testing.T, store *fakeStore) (*Recorder, *fakeCapture, *[]string) {
	t.Helper()
	cap := &fakeCapture{}
	var notes []string
	r := New(t.TempDir(), store, "")
	r.capture = func(mic, system io.Writer) (capturer, time.Time, time.Time, error) {
		mic.Write(make([]byte, 3200))
		system.Write(make([]byte, 3200))
		now := time.Now()
		return cap, now, now.Add(500 * time.Millisecond), nil
	}
	r.whisper = func(ctx context.Context, bin, path, speaker string, offset time.Duration) ([]Segment, error) {
		if speaker == speakerMe {
			return []Segment{{Start: 0, End: time.Second, Speaker: speakerMe, Text: "shall we ship friday"}}, nil
		}
		return []Segment{{Start: 2 * time.Second, End: 3 * time.Second, Speaker: speakerCall, Text: "friday works"}}, nil
	}
	r.findWhisper = func(string) (string, error) { return "/fake/whisper", nil }
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
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
	want := "[00:00:00] [me] shall we ship friday\n[00:00:02] [call] friday works\n"
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
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted once both transcriptions succeeded", name)
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
	r.whisper = func(ctx context.Context, bin, path, speaker string, offset time.Duration) ([]Segment, error) {
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

	orphan := filepath.Join(r.dataDir, "recordings", "2026-08-27T09-30-00")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mic.wav", "system.wav"} {
		if err := os.WriteFile(filepath.Join(orphan, name), make([]byte, 3200), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A directory that already has its transcript is finished and must be left alone.
	done := filepath.Join(r.dataDir, "recordings", "2026-08-26T09-30-00")
	if err := os.MkdirAll(done, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(done, "mic.wav"), make([]byte, 3200), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(done, "transcript.md"), []byte("[00:00:00] [me] already done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A recording that is running right now looks exactly like an abandoned one on disk, so the sweep must leave anything started at or after its cutoff alone.
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	live, err := r.stop()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	r.resumeOrphans(context.Background(), time.Now())

	if _, err := os.Stat(filepath.Join(live.dir, "transcript.md")); !os.IsNotExist(err) {
		t.Error("the sweep transcribed a recording that started after its cutoff, which is how it would eat a live one")
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

// A failed transcription must not destroy the recording — the WAVs are the only copy of what was said.
func TestRecorder_KeepsAudioWhenTranscriptionFails(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	r.whisper = func(ctx context.Context, bin, path, speaker string, offset time.Duration) ([]Segment, error) {
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
