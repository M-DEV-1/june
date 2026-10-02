package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"june/internal/agent"
	"june/internal/audio"
	"june/internal/db"
)

// voiceFakeMic is a Microphone that hands out a channel nobody writes to and records that it was closed, so a test can assert Stop frees the real device without opening one. Its amplitude is settable under a mutex (sync/atomic has no Float64) so the level-ticker tests can drive what CurrentAmplitude reports without touching real hardware.
type voiceFakeMic struct {
	ch       chan []byte
	captured atomic.Bool
	closed   atomic.Bool
	ampMu    sync.Mutex
	amp      float64
}

func (m *voiceFakeMic) StartCapture(ctx context.Context) (<-chan []byte, error) {
	m.captured.Store(true)
	return m.ch, nil
}
func (m *voiceFakeMic) CurrentAmplitude() float64 {
	m.ampMu.Lock()
	defer m.ampMu.Unlock()
	return m.amp
}
func (m *voiceFakeMic) setAmplitude(v float64) {
	m.ampMu.Lock()
	m.amp = v
	m.ampMu.Unlock()
}
func (m *voiceFakeMic) Close() error { m.closed.Store(true); return nil }

// voiceFakeSpeaker is a Speaker that discards audio and records that it was closed. Its amplitude is settable the same way voiceFakeMic's is.
type voiceFakeSpeaker struct {
	closed atomic.Bool
	ampMu  sync.Mutex
	amp    float64
}

func (s *voiceFakeSpeaker) Play(pcm []byte) error { return nil }
func (s *voiceFakeSpeaker) Flush()                {}
func (s *voiceFakeSpeaker) CurrentAmplitude() float64 {
	s.ampMu.Lock()
	defer s.ampMu.Unlock()
	return s.amp
}
func (s *voiceFakeSpeaker) setAmplitude(v float64) {
	s.ampMu.Lock()
	s.amp = v
	s.ampMu.Unlock()
}
func (s *voiceFakeSpeaker) Close() error { s.closed.Store(true); return nil }

// voiceFakeRunner is a voiceRunner that never dials anything: Run blocks until its context is cancelled, and the test drives the session's observable behaviour by writing to the same channels a real agent writes to.
type voiceFakeRunner struct {
	text    chan agent.ResponseChunk
	tools   chan agent.ToolActivity
	running chan struct{}
	ended   chan struct{}

	// usage is what the fake reports for the turn that just finished, standing in for what a real live session accumulated. Guarded because the test sets it while the session's own goroutine reads it.
	usageMu sync.Mutex
	usage   agent.TokenUsage
}

func newVoiceFakeRunner() *voiceFakeRunner {
	return &voiceFakeRunner{
		text:    make(chan agent.ResponseChunk, 16),
		tools:   make(chan agent.ToolActivity, 16),
		running: make(chan struct{}, 1),
		ended:   make(chan struct{}, 1),
	}
}

func (f *voiceFakeRunner) Run(ctx context.Context, mic <-chan []byte) error {
	f.running <- struct{}{}
	<-ctx.Done()
	f.ended <- struct{}{}
	return ctx.Err()
}
func (f *voiceFakeRunner) Text() <-chan agent.ResponseChunk { return f.text }
func (f *voiceFakeRunner) Tools() <-chan agent.ToolActivity { return f.tools }
func (f *voiceFakeRunner) Usage() agent.TokenUsage {
	f.usageMu.Lock()
	defer f.usageMu.Unlock()
	return f.usage
}

// voiceFailingRunner is a voiceRunner whose Run always fails immediately, standing in for a session that can never connect — a missing or expired API key, say — so dial's backoff-and-give-up behaviour can be tested without waiting out a real Gemini dial on every attempt.
type voiceFailingRunner struct {
	err   error
	calls atomic.Int64
	// goAways is how many runs end on the server's GoAway before err takes over.
	goAways int64
}

func (f *voiceFailingRunner) Run(ctx context.Context, mic <-chan []byte) error {
	if f.calls.Add(1) <= f.goAways {
		return agent.ErrGoAway
	}
	return f.err
}
func (f *voiceFailingRunner) Text() <-chan agent.ResponseChunk { return make(chan agent.ResponseChunk) }
func (f *voiceFailingRunner) Tools() <-chan agent.ToolActivity { return make(chan agent.ToolActivity) }
func (f *voiceFailingRunner) Usage() agent.TokenUsage          { return agent.TokenUsage{} }

// newVoiceServer wires a Server with only the routes a voice test needs — /events plus the three voice routes, under the same names cmd/daemon.go gives them — and a VoiceSession whose audio and agent are the fakes above.
func newVoiceServer(t *testing.T) (*httptest.Server, *VoiceSession, *voiceFakeMic, *voiceFakeSpeaker, *voiceFakeRunner) {
	t.Helper()
	return newVoiceServerStore(t, nil)
}

// newVoiceServerStore is newVoiceServer with a store behind the session, for the tests that read what a finished turn filed. Input: the store, or nil for the sessions that write nothing. Output: the same server, session, audio fakes and runner newVoiceServer returns.
func newVoiceServerStore(t *testing.T, store *db.Store) (*httptest.Server, *VoiceSession, *voiceFakeMic, *voiceFakeSpeaker, *voiceFakeRunner) {
	t.Helper()
	mic := &voiceFakeMic{ch: make(chan []byte)}
	spk := &voiceFakeSpeaker{}
	run := newVoiceFakeRunner()

	s := New(nil, store, nil, nil)
	v := NewVoice(s, store, "", nil)
	v.open = func() (audio.Microphone, audio.Speaker, voiceRunner, error) {
		return mic, spk, run, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.Events)
	mux.HandleFunc("/voice/start", v.Start)
	mux.HandleFunc("/voice/stop", v.Stop)
	mux.HandleFunc("/voice/status", v.Status)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, v, mic, spk, run
}

func voicePost(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// voiceStatus reads GET /voice/status and returns the decoded body.
func voiceStatus(t *testing.T, srv *httptest.Server) (active bool, id, state string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/voice/status")
	if err != nil {
		t.Fatalf("GET /voice/status: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Active bool   `json:"active"`
		ID     string `json:"id"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return body.Active, body.ID, body.State
}

// waitState polls /voice/status until it reports want, so a test never races the goroutine that reads the agent's channels.
func waitState(t *testing.T, srv *httptest.Server, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, state := voiceStatus(t, srv); state == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _, state := voiceStatus(t, srv)
	t.Fatalf("state = %q, want %q", state, want)
}

func TestVoiceStart_ReturnsIDAndGoesActive(t *testing.T) {
	srv, _, mic, _, run := newVoiceServer(t)

	resp := voicePost(t, srv, "/voice/start")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if body.ID == "" {
		t.Fatalf("expected a non-empty session id")
	}

	select {
	case <-run.running:
	case <-time.After(2 * time.Second):
		t.Fatalf("the session never started running")
	}
	if !mic.captured.Load() {
		t.Fatalf("expected the mic to be capturing")
	}

	active, id, state := voiceStatus(t, srv)
	if !active || id != body.ID || state != "listening" {
		t.Fatalf("status = %v/%q/%q, want true/%q/listening", active, id, state, body.ID)
	}
}

func TestVoiceStop_EndsTheRunAndFreesTheAudio(t *testing.T) {
	srv, _, mic, spk, run := newVoiceServer(t)

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running

	stop := voicePost(t, srv, "/voice/stop")
	defer stop.Body.Close()
	if stop.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want %d", stop.StatusCode, http.StatusOK)
	}

	select {
	case <-run.ended:
	case <-time.After(2 * time.Second):
		t.Fatalf("the run never ended")
	}
	if !mic.closed.Load() || !spk.closed.Load() {
		t.Fatalf("mic closed = %v, speaker closed = %v, want both true", mic.closed.Load(), spk.closed.Load())
	}

	active, id, state := voiceStatus(t, srv)
	if active || id != "" || state != "idle" {
		t.Fatalf("status after stop = %v/%q/%q, want false//idle", active, id, state)
	}

	// A second start is allowed once the first has stopped.
	again := voicePost(t, srv, "/voice/start")
	defer again.Body.Close()
	if again.StatusCode != http.StatusAccepted {
		t.Fatalf("restart status = %d, want %d", again.StatusCode, http.StatusAccepted)
	}
}

func TestVoiceEvents_HeardSaidAndStateReachASubscriber(t *testing.T) {
	srv, _, _, _, run := newVoiceServer(t)

	events, closeSSE := readSSE(t, srv)
	defer closeSSE()

	start := voicePost(t, srv, "/voice/start")
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(start.Body).Decode(&body)
	start.Body.Close()
	<-run.running

	run.text <- agent.ResponseChunk{Text: "what was I doing", Sender: agent.SenderYou}
	run.text <- agent.ResponseChunk{Text: "the API doc"}

	want := []struct{ typ, text string }{
		{"state", "listening"},
		{"heard", "what was I doing"},
		{"state", "speaking"},
		{"said", "the API doc"},
	}
	for _, w := range want {
		ev := mustEvent(t, events)
		// Level readings run on their own 50 ms clock beside these events, so one can land anywhere in the sequence.
		for ev.Type == "level" {
			ev = mustEvent(t, events)
		}
		if ev.Type != w.typ || ev.Text != w.text {
			t.Fatalf("event = %q/%q, want %q/%q", ev.Type, ev.Text, w.typ, w.text)
		}
		if ev.ID != body.ID {
			t.Fatalf("event id = %q, want %q", ev.ID, body.ID)
		}
	}
}

// TestVoiceHeard_StopEndsTheSession covers the spoken way out: the window can be hidden while a session runs, so "stop" has to end it from the daemon's side rather than from a key the hidden window never sees.
func TestVoiceHeard_StopEndsTheSession(t *testing.T) {
	for _, said := range []string{"stop", "Stop.", "June, stop!", "June STOP"} {
		t.Run(said, func(t *testing.T) {
			srv, _, mic, spk, run := newVoiceServer(t)

			start := voicePost(t, srv, "/voice/start")
			start.Body.Close()
			<-run.running

			run.text <- agent.ResponseChunk{Text: said, Sender: agent.SenderYou}

			select {
			case <-run.ended:
			case <-time.After(2 * time.Second):
				t.Fatalf("the run never ended")
			}
			waitState(t, srv, "idle")
			active, id, _ := voiceStatus(t, srv)
			if active || id != "" {
				t.Fatalf("status after %q = %v/%q, want false/empty", said, active, id)
			}
			if !mic.closed.Load() || !spk.closed.Load() {
				t.Fatalf("mic closed = %v, speaker closed = %v, want both true", mic.closed.Load(), spk.closed.Load())
			}
		})
	}
}

// TestVoiceDial_GivesUpAfterConsecutiveFailures covers the other half of the same defect: a run that fails every single time, such as a missing or expired API key, must not be redialed forever. Once it has failed voiceMaxConsecutiveFailures times in a row, dial must stop retrying and tear the session down itself — releasing the microphone and the speaker and going idle — since nobody is going to call stop on a session they don't know is stuck.
func TestVoiceDial_GivesUpAfterConsecutiveFailures(t *testing.T) {
	origDelay, origMax, origAttempts := voiceReconnectDelay, voiceMaxReconnectDelay, voiceMaxConsecutiveFailures
	voiceReconnectDelay = time.Millisecond
	voiceMaxReconnectDelay = 2 * time.Millisecond
	voiceMaxConsecutiveFailures = 3
	t.Cleanup(func() {
		voiceReconnectDelay, voiceMaxReconnectDelay, voiceMaxConsecutiveFailures = origDelay, origMax, origAttempts
	})

	mic := &voiceFakeMic{ch: make(chan []byte)}
	spk := &voiceFakeSpeaker{}
	run := &voiceFailingRunner{err: errors.New("dial gemini: 401 Unauthorized")}

	s := New(nil, nil, nil, nil)
	v := NewVoice(s, nil, "", nil)
	v.open = func() (audio.Microphone, audio.Speaker, voiceRunner, error) {
		return mic, spk, run, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/voice/start", v.Start)
	mux.HandleFunc("/voice/status", v.Status)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()

	waitState(t, srv, "idle")

	if n := run.calls.Load(); n != int64(voiceMaxConsecutiveFailures) {
		t.Fatalf("Run was called %d times before giving up, want %d", n, voiceMaxConsecutiveFailures)
	}
	if !mic.closed.Load() || !spk.closed.Load() {
		t.Fatalf("mic closed = %v, speaker closed = %v, want both true once the session gives up", mic.closed.Load(), spk.closed.Load())
	}
	active, id, _ := voiceStatus(t, srv)
	if active || id != "" {
		t.Fatalf("status after giving up = %v/%q, want inactive with no id", active, id)
	}
}

// TestStart_DoesNotHoldTheLockAcrossOpeningHardware checks that Start releases the session mutex before it dials the microphone and the speaker, so GET /voice/status and POST /voice/stop keep answering while that hardware call is in flight — the same shape Dictation.Start already uses. Held across it, a slow (or wedged) device open blocks every other voice route for as long as the open takes.
func TestStart_DoesNotHoldTheLockAcrossOpeningHardware(t *testing.T) {
	srv, v, mic, spk, run := newVoiceServer(t)
	proceed := make(chan struct{})
	v.open = func() (audio.Microphone, audio.Speaker, voiceRunner, error) {
		<-proceed
		return mic, spk, run, nil
	}

	startErr := make(chan error, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/voice/start", "application/json", nil)
		if err != nil {
			startErr <- err
			return
		}
		resp.Body.Close()
		startErr <- nil
	}()

	// Give Start a moment to reach the (currently blocked) hardware open before checking that status still answers.
	time.Sleep(50 * time.Millisecond)

	statusErr := make(chan error, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/voice/status")
		if err == nil {
			resp.Body.Close()
		}
		statusErr <- err
	}()

	select {
	case err := <-statusErr:
		if err != nil {
			t.Fatalf("GET /voice/status: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GET /voice/status blocked while /voice/start was still opening the microphone — Start must not hold the session lock across the hardware open")
	}

	close(proceed)
	if err := <-startErr; err != nil {
		t.Fatalf("POST /voice/start: %v", err)
	}
}

// levelDetail decodes a "level" event's Detail JSON, so a test can assert on the mic/speaker numbers rather than the raw string.
type levelDetail struct {
	Mic     float64 `json:"mic"`
	Speaker float64 `json:"speaker"`
}

// waitLevelEvent reads events off ch, skipping every type but "level", until one arrives or the wait times out. A running session interleaves "state" events with "level" ones, so a test wanting the latter cannot just take the next event off the stream.
func waitLevelEvent(t *testing.T, ch <-chan Event) (Event, levelDetail) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event stream closed before a level event arrived")
			}
			if ev.Type != "level" {
				continue
			}
			var d levelDetail
			if err := json.Unmarshal([]byte(ev.Detail), &d); err != nil {
				t.Fatalf("level detail is not JSON: %v (%q)", err, ev.Detail)
			}
			return ev, d
		case <-deadline:
			t.Fatalf("timed out waiting for a level event")
		}
	}
}

// TestVoiceLevels_EmitsMicAndSpeakerAmplitude covers the shape the window's waveform needs: while a session runs, the mic's and the speaker's amplitude reach the stream as a "level" event carrying both, stamped with the session's own id like every other voice event.
func TestVoiceLevels_EmitsMicAndSpeakerAmplitude(t *testing.T) {
	srv, _, mic, spk, run := newVoiceServer(t)
	events, closeSSE := readSSE(t, srv)
	defer closeSSE()

	mic.setAmplitude(0.6)
	spk.setAmplitude(0.3)

	start := voicePost(t, srv, "/voice/start")
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(start.Body).Decode(&body)
	start.Body.Close()
	<-run.running

	ev, d := waitLevelEvent(t, events)
	if ev.ID != body.ID {
		t.Fatalf("level event id = %q, want %q", ev.ID, body.ID)
	}
	if math.Abs(d.Mic-0.6) > 1e-9 || math.Abs(d.Speaker-0.3) > 1e-9 {
		t.Fatalf("level = %+v, want mic=0.6 speaker=0.3", d)
	}
}

// A run that ended on the server's GoAway is not a failure: the session was closed on purpose so it could be resumed before the server's deadline (measured 2026-09-09, three GoAways in 25 minutes). It is redialed at once and does not count toward giving up, so a long conversation is never abandoned for having lasted.
func TestVoiceDial_AGoAwayRedialsAtOnceAndIsNotAFailure(t *testing.T) {
	origDelay, origMax, origAttempts := voiceReconnectDelay, voiceMaxReconnectDelay, voiceMaxConsecutiveFailures
	voiceReconnectDelay = 200 * time.Millisecond
	voiceMaxReconnectDelay = 400 * time.Millisecond
	voiceMaxConsecutiveFailures = 2
	t.Cleanup(func() {
		voiceReconnectDelay, voiceMaxReconnectDelay, voiceMaxConsecutiveFailures = origDelay, origMax, origAttempts
	})

	mic := &voiceFakeMic{ch: make(chan []byte)}
	spk := &voiceFakeSpeaker{}
	run := &voiceFailingRunner{goAways: 3, err: errors.New("dial gemini: 401 Unauthorized")}

	s := New(nil, nil, nil, nil)
	v := NewVoice(s, nil, "", nil)
	v.open = func() (audio.Microphone, audio.Speaker, voiceRunner, error) { return mic, spk, run, nil }

	mux := http.NewServeMux()
	mux.HandleFunc("/voice/start", v.Start)
	mux.HandleFunc("/voice/status", v.Status)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	started := time.Now()
	voicePost(t, srv, "/voice/start").Body.Close()
	waitState(t, srv, "idle")

	if n := run.calls.Load(); n != 5 {
		t.Fatalf("Run was called %d times, want 5: three GoAway redials that count for nothing, then two failures", n)
	}
	// Three GoAway redials at the ordinary delay would alone take 600ms plus backoff; at once they take almost nothing.
	if took := time.Since(started); took > 700*time.Millisecond {
		t.Errorf("the run took %v, want the GoAway redials not to wait out the backoff", took)
	}
}
