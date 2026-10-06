package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/memory"
	"june/internal/util"
)

type fakeCapture struct{ stopped bool }

func (f *fakeCapture) Stop() { f.stopped = true }

type fakeStore struct {
	// conversations and turns record what prep filed: the titles of the conversations it opened and the text of the turns it added.
	conversations []string
	turns         []string
	episodes      []db.Episode
	personal      []db.PersonalEntry
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
	r.notifyAt = func(title, body, place, id string) { notes = append(notes, title+": "+body+" @"+place+"/"+id) }
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
	// The notice opens the meeting it announces, under the id the minutes were filed as, rather than naming a file path.
	if !slices.ContainsFunc(*notes, func(n string) bool { return strings.HasSuffix(n, "@meetings/1") }) {
		t.Errorf("the user should be notified when the minutes are ready, by a notice that opens the meeting; got %v", *notes)
	}
}

// A failed transcription, and whisper exiting 0 but yielding nothing usable (a silent meeting, or a build whose output lines parseSegments does not recognise), are both not a success: the WAVs are still the only copy of the meeting, so they must survive, and nothing may be filed as minutes. The "yields nothing" case additionally leaves a marker naming the problem and must not write an empty transcript.md, which would look like a finished recording.
func TestRecorder_KeepsAudioWhenTranscriptionFailsOrYieldsNothing(t *testing.T) {
	cases := []struct {
		name       string
		whisper    func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error)
		wantMarker bool
	}{
		{
			name: "yields nothing usable",
			whisper: func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
				return nil, nil
			},
			wantMarker: true,
		},
		{
			// The shape of a call that went to voicemail: the microphone side is whisper's stock credit lines invented over silence, and the call side is the carrier's recorded greeting. Nobody spoke.
			name: "hears only a voicemail greeting and whisper's invented credits",
			whisper: func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
				if speaker == speakerMe {
					return []Segment{
						{Start: 0, End: 3 * time.Second, Speaker: speakerMe, Text: "Copyright © 2019 Example Media Ltd. All Rights Reserved."},
						{Start: 3 * time.Second, End: 9 * time.Second, Speaker: speakerMe, Text: "No part of this recording may be reproduced without Example Media Ltd.'s express consent."},
						{Start: 9 * time.Second, End: 12 * time.Second, Speaker: speakerMe, Text: "Thank you for watching! Please subscribe to my channel for more videos."},
					}, nil
				}
				return []Segment{
					{Start: 0, End: 6 * time.Second, Speaker: speakerCall, Text: "The person you are calling is currently unavailable, please leave a message after the tone."},
					{Start: 13 * time.Second, End: 15 * time.Second, Speaker: speakerCall, Text: "When you have finished, please hang up."},
				}, nil
			},
			wantMarker: true,
		},
		{
			name: "fails outright",
			whisper: func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
				return nil, errors.New("whisper exploded")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{}
			r, _, _ := newTestRecorder(t, store)
			r.whisper = c.whisper
			if err := r.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			sess, _ := r.stop()
			if err := r.process(context.Background(), sess); err == nil {
				t.Fatal("process must report the transcription problem")
			}
			for _, name := range []string{"mic.wav", "system.wav"} {
				if _, err := os.Stat(filepath.Join(sess.dir, name)); err != nil {
					t.Errorf("%s must survive a transcription problem: %v", name, err)
				}
			}
			if len(store.notes) != 0 {
				t.Errorf("nothing should be filed as minutes, got %v", store.notes)
			}
			if c.wantMarker {
				if _, err := os.Stat(filepath.Join(sess.dir, noSpeechMarker)); err != nil {
					t.Errorf("a marker naming the problem must be left in the recording dir: %v", err)
				}
				if _, err := os.Stat(filepath.Join(sess.dir, "transcript.md")); !os.IsNotExist(err) {
					t.Error("an empty transcript.md must not be written, it would look like a finished recording")
				}
			}
		})
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

// addAt records a notice that names where it opens, as "title: body @place/id", and like add when it names nowhere.
func (n *notifications) addAt(title, body, place, id string) {
	if place == "" {
		n.add(title, body)
		return
	}
	n.add(title, body+" @"+place+"/"+id)
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

// A meeting playing to a sink June is not recording writes an unbroken run of zero samples, which looks exactly like a working recording until the transcript comes back empty hours later. The user has to be told while the call is still running.
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

// Sound on either side means the right sink is being recorded, and a warning then is noise in the middle of a call: in the real 2026-09-02 case the user was talking through a standup while everyone else on the call was muted and the call side sent exact zeros.
func TestRecorder_NoSilenceWarningWhileSoundKeepsArriving(t *testing.T) {
	r, cap, _ := newTestRecorder(t, &fakeStore{})
	var got notifications
	r.notify = got.add
	// The window is wide enough that a busy CI runner pausing the writer goroutine does not read as a dead stream; the 30 ms it used to be failed on both Linux and Windows runners. The sleep below spans three windows, so a watchdog that listened to the call side alone would fire.
	r.silenceAfter = 250 * time.Millisecond
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
	// The writer has to be off the files before stop closes them, which the real capture guarantees and this fake must too.
	defer func() {
		close(stop)
		<-finished
		r.stop()
	}()

	time.Sleep(750 * time.Millisecond)
	if got.contains("audio") {
		t.Errorf("a recording that is capturing sound must not warn, got %v", got.sent)
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
	if !util.Exists(filepath.Join(sess.dir, failedMarker)) {
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
	if !util.Exists(marker) {
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
	if util.Exists(marker) {
		t.Error("the failure marker survived a run that succeeded")
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
			return `{"updates":[{"subject":"vexil-quorin","content":"Vexil Quorin was in the call."}]}`, nil
		}
		if !strings.Contains(prompt, "friday works") {
			t.Errorf("the regenerated minutes prompt is missing the existing transcript:\n%s", prompt)
		}
		return "# Meeting minutes\n\n## Attendees\n", nil
	}

	// note-id.txt is what says this meeting was filed once already, which is what deleting minutes.md asks June to do again.
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
	// The meeting already had its one chance to teach June something durable about a person when it was first summarised. Regenerating minutes from the same transcript must not run the updater again and re-propose the same writes.
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

// OnACPower reads the kernel's power supply class, so a battery-only machine and a plugged-in one are told apart from the files themselves.
// OnACPower reads the kernel's power supply class, so a battery-only machine and a plugged-in one are told apart from the files themselves, no mains supply at all cannot be battery, and a machine that exports no power supply class whatsoever — a desktop, or Windows — must count as plugged in, or nothing would ever be transcribed there.
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
		missing  bool // read a directory that does not exist at all
		want     bool
	}{
		{name: "charger plugged in", supplies: map[string][2]string{"AC": {"Mains", "1"}, "BAT0": {"Battery", ""}}, want: true},
		{name: "running on battery", supplies: map[string][2]string{"AC": {"Mains", "0"}, "BAT0": {"Battery", ""}}, want: false},
		{name: "no mains supply at all, so the machine cannot be on battery", supplies: map[string][2]string{"BAT0": {"Battery", ""}}, want: true},
		{name: "no power supply class exported at all counts as mains", missing: true, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "does-not-exist")
			if !c.missing {
				root = write(t, c.supplies)
			}
			if got := onACPowerAt(root); got != c.want {
				t.Errorf("OnACPower() = %v, want %v", got, c.want)
			}
		})
	}
}

// whisper -l auto writes a Hindi or Malayalam call in its own script, and the transcript on disk has to be in Latin letters whatever the language.
func TestTranscriptFor_WritesIndicSpeechInLatinLetters(t *testing.T) {
	r, _, _ := newTestRecorder(t, &fakeStore{})
	r.whisper = func(ctx context.Context, bin, path, speaker, prompt string, offset time.Duration) ([]Segment, error) {
		if speaker == speakerMe {
			return []Segment{{Start: 0, End: time.Second, Speaker: speakerMe, Text: "मैं कल रिपोर्ट भेज दूंगा, टेस्ट करके देखना है"}}, nil
		}
		return []Segment{{Start: 3 * time.Second, End: 4 * time.Second, Speaker: speakerCall, Text: "ok, ഞാൻ നാളെ അത് ചെയ്യാം"}}, nil
	}
	s := &session{dir: t.TempDir(), startedAt: time.Now(), stoppedAt: time.Now()}
	if _, err := r.transcriptFor(context.Background(), s); err != nil {
		t.Fatalf("transcriptFor: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "transcript.md"))
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	want := "[00:00:00] [me] main kal riport bhej doonga, test karke dekhna hai\n\n[00:00:03] [call] ok, njaan naale athu cheyyam\n"
	if string(b) != want {
		t.Errorf("transcript.md =\n%s\nwant\n%s", b, want)
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

// Filing a meeting's minutes also lifts its action items out into their own tracked rows, so the things somebody agreed to do outlive the three-day window the minutes themselves are read in. Every item is kept, the other people's included and each with its owner intact — whose an item is comes from its owner read against who the user is, so an "Ravix" bullet is something he is waiting for rather than something the store never heard. Minutes with no action items file normally and lift nothing.
func TestFileMinutes_ActionItems(t *testing.T) {
	raised := time.Date(2026, 8, 28, 21, 36, 0, 0, time.UTC)
	cases := []struct {
		name    string
		minutes string
		raised  time.Time
		check   func(t *testing.T, store *fakeStore)
	}{
		{
			name: "lifts every item, keeping other people's to wait on",
			minutes: `# Meeting minutes

**vq x zb tool — Google Meet, Fri 28 Aug 2026, 21:36–23:08 IST**

## Action items
- **Ravix** — carry PR #13 through CI and merge.
- **Me** — compare these minutes against his own agent's output.
`,
			raised: raised,
			check: func(t *testing.T, store *fakeStore) {
				if len(store.actions) != 2 {
					t.Fatalf("want both action items lifted, got %d: %+v", len(store.actions), store.actions)
				}
				mine := store.actions[1]
				if store.actions[0].Owner != "Ravix" {
					t.Errorf("the other person's item was dropped instead of kept to wait on: %+v", store.actions)
				}
				if mine.Owner != "Me" || mine.Text != "compare these minutes against his own agent's output." {
					t.Errorf("lifted item = %+v", mine)
				}
				if mine.Source != "vq x zb tool" || !mine.Raised.Equal(raised) {
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
			},
		},
		{
			// Filing is not the place whose-is-it gets decided: the store answers that on every read, from the owner against who the user is, so the user's own list and the list of what he is waiting on both come out of the same rows.
			name: "every item keeps its owner, including nobody named",
			minutes: `# Meeting minutes

## Action items
- **Me** — send the deck by Friday.
- **Jandreth** — add battery optimisation to the app.
- **Owner unclear** — trial attaching walkthrough videos to PRs.
`,
			check: func(t *testing.T, store *fakeStore) {
				if len(store.actions) != 3 {
					t.Fatalf("want 3 action items lifted, got %d: %+v", len(store.actions), store.actions)
				}
				want := []string{memory.MeOwner, "Jandreth", memory.UnknownOwner}
				for i, a := range store.actions {
					if a.Owner != want[i] {
						t.Errorf("item %d owner = %q, want %q", i, a.Owner, want[i])
					}
				}
			},
		},
		{
			name:    "no action items lifts nothing and still files",
			minutes: "# Meeting minutes\n\n## Key points\n- nothing was agreed.\n",
			check: func(t *testing.T, store *fakeStore) {
				if len(store.actions) != 0 {
					t.Errorf("lifted %d items from minutes with no action section", len(store.actions))
				}
				if len(store.notes) != 1 {
					t.Errorf("the minutes were not filed: %+v", store.notes)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{}
			r := New(context.Background(), t.TempDir(), store, "FAKE_API_KEY")
			raised := c.raised
			if raised.IsZero() {
				raised = time.Now()
			}
			r.fileMinutes(context.Background(), t.TempDir(), c.minutes, raised, raised.Add(90*time.Minute))
			c.check(t, store)
		})
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

// Killing the daemon mid-meeting on 2026-09-01 left a recording with no transcript, because nothing on the shutdown path stopped it. The audio survived only because the next start sweeps for unfinished recordings. Closing the files on the way out is what makes that a fallback rather than the mechanism — and shutting down with nothing recording is the normal case and must not be an error the daemon logs on every exit.
func TestStopForShutdown(t *testing.T) {
	t.Run("closes the recording without transcribing", func(t *testing.T) {
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
	})

	t.Run("quiet when nothing is recording", func(t *testing.T) {
		r, _, _ := newTestRecorder(t, &fakeStore{})

		dir, err := r.StopForShutdown()

		if err != nil || dir != "" {
			t.Errorf("StopForShutdown = %q, %v; want no directory and no error", dir, err)
		}
	})
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
		if util.Exists(marker) {
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

// A recording directory is named to the second, so two recordings that start inside one second want the same name. os.MkdirAll is happy with a directory that already exists and newWAV truncates what it opens, so the second recording used to overwrite the first meeting's audio while that meeting was still waiting to be transcribed — and both sessions then carried the same directory, so the second was never summarised either.
// Start has to go through that, not around it: this is the case where the user stops one meeting from the tray and the next call starts inside the same second.
func TestRecorder_StartNeverOpensInsideARecordingThatIsAlreadyThere(t *testing.T) {
	// makeRecordingDir itself must never hand out a directory that is already a recording: two recordings that start inside one second want the same name, and os.MkdirAll is happy with a directory that already exists while newWAV truncates what it opens, so the second recording used to overwrite the first meeting's audio while that meeting was still waiting to be transcribed.
	root := t.TempDir()
	first, err := makeRecordingDir(root)
	if err != nil {
		t.Fatalf("makeRecordingDir: %v", err)
	}
	firstAudio := filepath.Join(first, "mic.wav")
	if err := os.WriteFile(firstAudio, make([]byte, 100000), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := makeRecordingDir(root)
	if err != nil {
		t.Fatalf("makeRecordingDir: %v", err)
	}
	if second == first {
		t.Fatalf("both recordings got %s, so the second truncates the first meeting's audio", first)
	}
	if info, err := os.Stat(firstAudio); err != nil || info.Size() != 100000 {
		t.Fatalf("the first recording's mic.wav is %v (%v), want its 100000 bytes untouched", info, err)
	}

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
			return `{"updates":[{"subject":"vexil-quorin","content":"Vexil Quorin is a colleague at Acme."}]}`, nil
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
	if got := store.personalWrites["vexil-quorin"]; got == "" {
		t.Error("a meeting whose first attempt failed never updated personal context, so what it taught June is lost for good")
	}
}

func (f *fakeStore) CreateConversation(ctx context.Context, title, brain string) (int64, error) {
	f.conversations = append(f.conversations, title)
	return int64(len(f.conversations)), nil
}

func (f *fakeStore) AddTurn(ctx context.Context, conversationID int64, role, text, kind string, evidence json.RawMessage, tools []string) (int64, error) {
	f.turns = append(f.turns, text)
	return int64(len(f.turns)), nil
}

// The model often opens its reply with a line of its own ("I'll write the minutes for this standup.") before the heading it was asked for. That line is not part of the minutes, and because the meeting's name is read off the first line that is not a heading, it became the name of the meeting in the window: two of the meetings on 16 September 2026 were listed as "I'll write the minutes for this platform sprint standup meeting."
func TestProcess_DropsThePreambleTheModelWritesBeforeTheHeading(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[]}`, nil
		}
		return "I'll write the minutes for this platform sprint standup meeting.\n\n# Daily Platform Sprint Standup\n**Daily Platform Sprint Standup — Wed 16 Sep 2026, 14:30–14:53**\n\n## Your part\n- ship friday\n", nil
	}
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

	minutes, err := os.ReadFile(filepath.Join(sess.dir, "minutes.md"))
	if err != nil {
		t.Fatalf("minutes: %v", err)
	}
	if !strings.HasPrefix(string(minutes), "# Daily Platform Sprint Standup") {
		t.Errorf("minutes.md should start at the heading, got:\n%s", minutes)
	}
	filed := store.logged(noteKind)
	if len(filed) != 1 {
		t.Fatalf("expected one filed note, got %d", len(filed))
	}
	if got := memory.MinutesLabel(filed[0]); got != "Daily Platform Sprint Standup" {
		t.Errorf("the meeting is named %q, want %q", got, "Daily Platform Sprint Standup")
	}
}
