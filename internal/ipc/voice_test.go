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

	"ora/internal/act"
	"ora/internal/agent"
	"ora/internal/audio"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/db/dbtest"
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

// setUsage is what the test calls before sending a turn boundary, so the session reads these counts as that turn's cost.
func (f *voiceFakeRunner) setUsage(u agent.TokenUsage) {
	f.usageMu.Lock()
	f.usage = u
	f.usageMu.Unlock()
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
	v := NewVoice(s, store, "")
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

func TestVoiceStart_SecondStartConflicts(t *testing.T) {
	srv, _, _, _, _ := newVoiceServer(t)

	first := voicePost(t, srv, "/voice/start")
	first.Body.Close()

	second := voicePost(t, srv, "/voice/start")
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second start status = %d, want %d", second.StatusCode, http.StatusConflict)
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

func TestVoiceStatus_FollowsTheAgentsChannels(t *testing.T) {
	srv, _, _, _, run := newVoiceServer(t)

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running
	waitState(t, srv, "listening")

	run.tools <- agent.ToolActivity{Name: "query_memory", Phase: agent.ToolStarted}
	waitState(t, srv, "thinking")

	run.text <- agent.ResponseChunk{Text: "you were on the API doc"}
	waitState(t, srv, "speaking")

	run.text <- agent.ResponseChunk{TurnBoundary: true}
	waitState(t, srv, "listening")
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
	for _, said := range []string{"stop", "Stop.", "Ora, stop!", "ORA STOP"} {
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

// TestVoiceHeard_OrdinaryWordsKeepTheSessionRunning guards the stop match: "stop" has to be the whole utterance, so a sentence that merely contains the word is just something the user said.
func TestVoiceHeard_OrdinaryWordsKeepTheSessionRunning(t *testing.T) {
	srv, _, _, _, run := newVoiceServer(t)

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running

	run.text <- agent.ResponseChunk{Text: "stop the meeting at four", Sender: agent.SenderYou}
	run.text <- agent.ResponseChunk{Text: "sure", Sender: ""}
	waitState(t, srv, "speaking")

	if active, _, _ := voiceStatus(t, srv); !active {
		t.Fatalf("the session ended on a sentence that only contains the word stop")
	}
}

// TestVoiceHeard_StopBroadcastsIdle covers what the window reads: the daemon ending the session itself has to reach the event stream as an idle state, because that is the only way a window that never pressed anything learns the session is over.
func TestVoiceHeard_StopBroadcastsIdle(t *testing.T) {
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

	run.text <- agent.ResponseChunk{Text: "stop", Sender: agent.SenderYou}

	for _, want := range []struct{ typ, text string }{
		{"state", "listening"},
		{"heard", "stop"},
		{"state", "idle"},
	} {
		ev := mustEvent(t, events)
		if ev.Type != want.typ || ev.Text != want.text || ev.ID != body.ID {
			t.Fatalf("event = %q/%q/%q, want %q/%q/%q", ev.ID, ev.Type, ev.Text, body.ID, want.typ, want.text)
		}
	}
}

// TestVoiceStatus_ReportsIdleAfterEveryEnding checks the one field the overlay's word by the clock reads: /voice/status has to be accurate through the whole run, including after the session ends on its own.
func TestVoiceStatus_ReportsIdleAfterEveryEnding(t *testing.T) {
	srv, _, _, _, run := newVoiceServer(t)

	if _, _, state := voiceStatus(t, srv); state != "idle" {
		t.Fatalf("state before any session = %q, want idle", state)
	}

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running
	waitState(t, srv, "listening")

	run.text <- agent.ResponseChunk{IsThought: true, Text: "she means the venue"}
	waitState(t, srv, "thinking")

	run.text <- agent.ResponseChunk{Text: "nearly sorted"}
	waitState(t, srv, "speaking")

	run.text <- agent.ResponseChunk{TurnBoundary: true}
	waitState(t, srv, "listening")

	stop := voicePost(t, srv, "/voice/stop")
	stop.Body.Close()
	waitState(t, srv, "idle")
}

// TestNewVoiceAgent_WiresTheScreenDrawing covers what the voice session could not do before: the live agent it builds gets the same Point and Marks callbacks cmd/daemon.go gives the typed /ask agent, so point_at and show_marks draw on the screen instead of answering that this session cannot draw.
func TestNewVoiceAgent_WiresTheScreenDrawing(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	s := New(nil, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	a := newVoiceAgent(s, nil, nil, nil, "")
	// Draw was the one of the three this wiring missed: a real session on 2026-09-07 called draw with a sensible circle and got "this session cannot draw on the screen" back, because only the typed /ask agent had ever been given it.
	if a.Point == nil || a.Marks == nil || a.Draw == nil {
		t.Fatalf("voice agent has Point set = %v, Marks set = %v, Draw set = %v, want all three", a.Point != nil, a.Marks != nil, a.Draw != nil)
	}

	a.Point(10, 20, 30, 40, "here")
	ev, ring := waitOverlay(t, ch)
	if ev.Type != "overlay" {
		t.Errorf("event type = %q, want overlay", ev.Type)
	}
	if ring.Kind != "ring" || ring.Label != "here" || len(ring.Rects) != 1 || ring.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
		t.Errorf("overlay = %+v, want a ring labelled here around 10,20 30x40", ring)
	}

	a.Marks([]act.Item{{N: 1, X: 1, Y: 2, W: 3, H: 4}, {N: 2, X: 5, Y: 6, W: 7, H: 8}})
	ev, marks := waitOverlay(t, ch)
	if ev.Type != "overlay" {
		t.Errorf("event type = %q, want overlay", ev.Type)
	}
	want := []OverlayRect{{X: 1, Y: 2, W: 3, H: 4, Label: "1"}, {X: 5, Y: 6, W: 7, H: 8, Label: "2"}}
	if marks.Kind != "marks" || len(marks.Rects) != 2 || marks.Rects[0] != want[0] || marks.Rects[1] != want[1] {
		t.Errorf("overlay = %+v, want marks numbered 1 and 2 over the given rects", marks)
	}

	if err := a.Draw("g", "circle", nil, 590, 299, 100, 100, "there"); err != nil {
		t.Fatalf("Draw: %v", err)
	}
	ev, drawn := waitOverlay(t, ch)
	if ev.Type != "overlay" {
		t.Errorf("event type = %q, want overlay", ev.Type)
	}
	if drawn.Label != "there" || len(drawn.Rects) != 1 || drawn.Rects[0] != (OverlayRect{X: 590, Y: 299, W: 100, H: 100}) {
		t.Errorf("overlay = %+v, want a circle labelled there around 590,299 100x100", drawn)
	}
}

// TestVoiceBackoff_DoublesAndCaps covers the retry delay itself, in isolation from any timer: each redial after a failure must wait longer than the last, not the same fixed interval forever, and the growth must stop at voiceMaxReconnectDelay rather than climbing without bound.
func TestVoiceBackoff_DoublesAndCaps(t *testing.T) {
	orig := voiceMaxReconnectDelay
	voiceMaxReconnectDelay = 20 * time.Second
	t.Cleanup(func() { voiceMaxReconnectDelay = orig })

	d := voiceReconnectDelay
	for i, want := range []time.Duration{4 * time.Second, 8 * time.Second, 16 * time.Second, 20 * time.Second, 20 * time.Second} {
		d = voiceBackoff(d)
		if d != want {
			t.Fatalf("backoff #%d = %v, want %v", i, d, want)
		}
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
	v := NewVoice(s, nil, "")
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

// TestVoiceGiveUp_EndsTheWatchGoroutine checks the other half of giving up: giveUp must cancel the session's context, or watch — parked in a select on ctx.Done() and the agent's own channels, neither of which a runner that only ever fails ever closes — leaks forever instead of returning once dial gives up. Drives watch directly (rather than through Start/dial) so the assertion is a channel close, not a goroutine count that an httptest.Server's own long-lived connection goroutines would make noisy.
func TestVoiceGiveUp_EndsTheWatchGoroutine(t *testing.T) {
	s := New(nil, nil, nil, nil)
	v := NewVoice(s, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	run := newVoiceFakeRunner() // Text()/Tools() channels that nothing ever writes to or closes, the same as a run that has already returned would leave watch's select with nothing to read.

	v.mu.Lock()
	v.id, v.cancel, v.mic, v.speaker = "voice-1", cancel, &voiceFakeMic{ch: make(chan []byte)}, &voiceFakeSpeaker{}
	v.mu.Unlock()

	watchReturned := make(chan struct{})
	go func() {
		v.watch(ctx, "voice-1", run)
		close(watchReturned)
	}()

	v.giveUp("voice-1")

	select {
	case <-watchReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after giveUp — giveUp must cancel the session's context, or watch's goroutine leaks forever")
	}
}

// waitTokenUse polls the token ledger until it holds at least want rows, so a test never races the goroutine that files them. Input: the store and how many rows to wait for. Output: the newest rows, newest first.
func waitTokenUse(t *testing.T, store *db.Store, want int) []db.TokenUse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		uses, err := store.TokenUseRecent(context.Background(), 10)
		if err != nil {
			t.Fatalf("TokenUseRecent: %v", err)
		}
		if len(uses) >= want {
			return uses
		}
		if time.Now().After(deadline) {
			t.Fatalf("the token ledger holds %d rows, want %d", len(uses), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestVoiceTurn_RecordsWhatItCostInTokens covers the voice half of the usage screen. The live session counted every turn's tokens and then dropped them, so the voice model showed zero however long the user talked. A finished turn — the boundary the session already watches for — has to reach the ledger with the counts the session accumulated, the configured voice model, and the channel "voice" so the user can tell a spoken call from a typed one.
func TestVoiceTurn_RecordsWhatItCostInTokens(t *testing.T) {
	store := dbtest.Open(t)
	srv, _, _, _, run := newVoiceServerStore(t, store)

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running

	run.setUsage(agent.TokenUsage{Provider: agent.ProviderGemini, InputTokens: 1200, OutputTokens: 340, TotalTokens: 1540})
	run.text <- agent.ResponseChunk{TurnBoundary: true}

	u := waitTokenUse(t, store, 1)[0]
	if u.Channel != "voice" {
		t.Errorf("channel = %q, want voice", u.Channel)
	}
	if u.Provider != agent.ProviderGemini {
		t.Errorf("provider = %q, want %q", u.Provider, agent.ProviderGemini)
	}
	if u.Model != config.VoiceModel() {
		t.Errorf("model = %q, want the configured voice model %q", u.Model, config.VoiceModel())
	}
	if u.InputTokens != 1200 || u.OutputTokens != 340 || u.TotalTokens != 1540 {
		t.Errorf("counts = %d in, %d out, %d total, want 1200/340/1540", u.InputTokens, u.OutputTokens, u.TotalTokens)
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

// TestVoiceLevels_SkipsUnchangedReadings covers the cost guard: a session sitting in silence reads the same amplitude on every tick, and only the first reading may reach the stream — every later tick reporting the same numbers must be skipped, or a quiet session would still cost a "level" event twenty times a second.
func TestVoiceLevels_SkipsUnchangedReadings(t *testing.T) {
	srv, _, _, _, run := newVoiceServer(t)
	events, closeSSE := readSSE(t, srv)
	defer closeSSE()

	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running

	// The fakes default to amplitude 0 on both channels; left untouched for several ticks, only the first tick's reading is new.
	time.Sleep(5 * levelTickInterval)

	levels := 0
loop:
	for {
		select {
		case ev := <-events:
			if ev.Type == "level" {
				levels++
			}
		case <-time.After(75 * time.Millisecond):
			break loop
		}
	}
	if levels != 1 {
		t.Fatalf("got %d level events for an unchanged silent reading, want exactly 1 (the first sample)", levels)
	}
}

// TestVoiceLevels_StopsWhenSessionEnds covers the other half of the ticker's lifecycle: it must start with the session and also stop with it, or a session that has ended would keep sampling audio devices nobody is reading from any more (and, once Stop has closed them, would be reading from closed devices).
func TestVoiceLevels_StopsWhenSessionEnds(t *testing.T) {
	srv, _, mic, _, run := newVoiceServer(t)
	events, closeSSE := readSSE(t, srv)
	defer closeSSE()

	mic.setAmplitude(0.4)
	start := voicePost(t, srv, "/voice/start")
	start.Body.Close()
	<-run.running
	waitLevelEvent(t, events) // the first sample, so the ticker is confirmed running before it is stopped.

	stop := voicePost(t, srv, "/voice/stop")
	stop.Body.Close()
	<-run.ended

	// Changed after teardown: if the ticker somehow survived Stop, this would produce a fresh level event.
	mic.setAmplitude(0.9)
	for {
		select {
		case ev := <-events:
			if ev.Type == "level" {
				t.Fatalf("level event arrived after the session ended")
			}
		case <-time.After(3 * levelTickInterval):
			return
		}
	}
}

// TestVoiceStartAndStopRefuseAGet checks both mutating voice routes guard their method the way every other mutating route in this package does: a GET /voice/start would otherwise open the user's microphone and start a live session, which is the sort of thing a stray prefetch from the window's own origin does.
func TestVoiceStartAndStopRefuseAGet(t *testing.T) {
	srv, _, mic, _, _ := newVoiceServer(t)

	for _, path := range []string{"/voice/start", "/voice/stop"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, resp.StatusCode)
		}
	}
	if mic.captured.Load() {
		t.Errorf("a GET opened the microphone")
	}
}

// A live session sends an end-of-turn boundary for every turn but snapshots the token count on only some of them, so filing every boundary wrote one real row and one all-zero row per turn — half the usage ledger was a duplicate of the other half, and the Recent calls table on 2026-09-07 alternated a count with a zero all the way down.
func TestEmptyUsage_TellsACountedTurnFromAnUncountedBoundary(t *testing.T) {
	if !emptyUsage(agent.TokenUsage{Provider: agent.ProviderGemini}) {
		t.Error("a boundary carrying no counts reads as countable, so it would be filed as a zero row")
	}
	for name, u := range map[string]agent.TokenUsage{
		"input only":  {InputTokens: 19324},
		"output only": {OutputTokens: 259},
		"total only":  {TotalTokens: 19501},
		"cached only": {CachedInputTokens: 8192},
		"rounds only": {Rounds: 1},
	} {
		if emptyUsage(u) {
			t.Errorf("%s reads as empty, so a turn that did cost something would go unfiled", name)
		}
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
	v := NewVoice(s, nil, "")
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
