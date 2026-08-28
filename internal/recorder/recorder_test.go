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
	// The sweep New starts runs concurrently and would otherwise race the seams set below, and pick up recordings the test itself makes. Its data dir is empty, so this returns at once.
	<-r.swept
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
		{CreatedAt: time.Now(), App: "Brave Browser", Title: "Meet – weekly sync", ScreenText: "participating in a video call | Arjun Goel (Presenting) | Mahadevan KS"},
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
	if !strings.Contains(got, "Arjun Goel (Presenting)") {
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

// onACPower reads the kernel's power supply class, so a battery-only machine and a plugged-in one are told apart from the files themselves.
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
			if got := onACPower(); got != c.want {
				t.Errorf("onACPower() = %v, want %v", got, c.want)
			}
		})
	}
}

// A machine that does not export the power supply class at all — a desktop, or Windows — must count as plugged in, or nothing would ever be transcribed there.
func TestOnACPower_UnknownCountsAsMains(t *testing.T) {
	old := powerSupplyRoot
	powerSupplyRoot = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { powerSupplyRoot = old }()
	if !onACPower() {
		t.Error("a machine that cannot report its power source must be treated as on mains")
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
