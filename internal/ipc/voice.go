package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ora/internal/act"
	"ora/internal/agent"
	"ora/internal/audio"
	"ora/internal/config"
	"ora/internal/db"
)

// voiceRunner is the part of a live *agent.Agent a voice session drives: the blocking session loop, the two channels the agent already writes everything it hears, says and calls to, and what the turn that just finished cost. Narrowed to an interface so the tests run the whole session against a fake instead of dialing Gemini and opening the user's microphone.
type voiceRunner interface {
	// Run holds one live session open until it fails or its context is cancelled, reading microphone audio from mic.
	Run(ctx context.Context, mic <-chan []byte) error
	// Text carries the user's transcribed speech (Sender "you"), Ora's own transcribed speech (no sender), her thoughts, and the end-of-turn marker.
	Text() <-chan agent.ResponseChunk
	// Tools carries a started/finished pair for every tool call the model makes.
	Tools() <-chan agent.ToolActivity
	// Usage is what the turn that just finished cost in tokens, read at the end-of-turn marker on Text and before the next turn finishes.
	Usage() agent.TokenUsage
}

// liveAgent adapts a real *agent.Agent to voiceRunner. The agent exposes its channels as fields rather than methods, so this is the whole adaptation.
type liveAgent struct{ a *agent.Agent }

func (l liveAgent) Run(ctx context.Context, mic <-chan []byte) error { return l.a.Connect(ctx, mic) }
func (l liveAgent) Text() <-chan agent.ResponseChunk                 { return l.a.TextResponseChan }
func (l liveAgent) Tools() <-chan agent.ToolActivity                 { return l.a.ToolActivityChan }
func (l liveAgent) Usage() agent.TokenUsage                          { return l.a.VoiceUsage() }

// voiceReconnectDelay is how long a dropped live session waits before its first redial. Each further failure in the same run of retries doubles it (see voiceBackoff), up to voiceMaxReconnectDelay, so a stretch of bad luck is retried patiently rather than hammered. A variable only so a test can shrink it.
var voiceReconnectDelay = 2 * time.Second

// voiceMaxReconnectDelay caps the backoff voiceBackoff computes, so a session that has been failing for a while still tries again every so often rather than the wait growing without bound.
var voiceMaxReconnectDelay = 30 * time.Second

// voiceMaxConsecutiveFailures is how many times in a row dial will redial a run that keeps failing before it gives up on the session rather than trying again. Past this many failures the cause is almost certainly not transient — a missing or expired API key, say — and retrying for ever would just hold the microphone open and fill the log with the same error. A variable only so a test can shrink it.
var voiceMaxConsecutiveFailures = 8

// voiceStopTimeout bounds how long POST /voice/stop waits for the session goroutine to return before closing the audio devices anyway, so a wedged session cannot hang the request.
var voiceStopTimeout = 5 * time.Second

// levelTickInterval is how often a running voice session samples the microphone's and the speaker's amplitude for the window's waveform — the same 50ms the terminal UI already redraws its own two Waveforms at (see internal/ui/ui.go's tickMsg handling).
const levelTickInterval = 50 * time.Millisecond

// levelChangeThreshold is how far a reading has to move from the last one broadcast, on either channel, before levels sends again. A session sitting in silence reads the same near-zero amplitude tick after tick, so without this it would still cost a "level" event twenty times a second for nothing on screen.
const levelChangeThreshold = 0.005

// levels samples mic's and speaker's amplitude every levelTickInterval and broadcasts a "level" event carrying both as JSON in Detail — {"mic":0.0-1.0,"speaker":0.0-1.0} — so the window can drive the same waveform the terminal UI draws from the same two CurrentAmplitude() calls (see internal/ui/waveform.go, internal/audio/capture_linux.go and player_linux.go). Input: the session's context (levels returns once it is cancelled, so the ticker starts and stops with the session itself), the session's id to stamp the event with, and the mic and speaker the session opened. Output: none. A reading within levelChangeThreshold of the last one sent on both channels is skipped, so a silent session emits nothing.
func (v *VoiceSession) levels(ctx context.Context, id string, mic audio.Microphone, speaker audio.Speaker) {
	ticker := time.NewTicker(levelTickInterval)
	defer ticker.Stop()
	lastMic, lastSpeaker := -1.0, -1.0 // unreachable by a real amplitude, so the first sample always sends even if it happens to be silence.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m, s := mic.CurrentAmplitude(), speaker.CurrentAmplitude()
			if math.Abs(m-lastMic) < levelChangeThreshold && math.Abs(s-lastSpeaker) < levelChangeThreshold {
				continue
			}
			lastMic, lastSpeaker = m, s
			detail, _ := json.Marshal(map[string]float64{"mic": m, "speaker": s})
			v.hub.broadcast(Event{ID: id, Type: levelEventType, Detail: string(detail), Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
		}
	}
}

// VoiceSession runs the Gemini Live voice loop inside the daemon, so the desktop window gets the same conversation the terminal client has instead of needing its own audio stack. At most one session exists at a time. Everything it hears, says and calls is broadcast on the same hub /events already serves, tagged with the session's id, so the window reads voice off the stream it is already reading.
type VoiceSession struct {
	hub *hub
	// store is the token ledger a finished turn is filed in, so the voice model shows up on the usage screen next to the typed one. nil files nothing, for a daemon or a test running without a store.
	store *db.Store
	// open builds the microphone, the speaker and the agent for one session. A field rather than a call so the tests can substitute fakes for the user's real hardware.
	open func() (audio.Microphone, audio.Speaker, voiceRunner, error)

	mu      sync.Mutex
	seq     int
	id      string
	state   string
	cancel  context.CancelFunc
	done    chan struct{}
	mic     audio.Microphone
	speaker audio.Speaker
}

// NewVoice builds the daemon's voice session. Input: the server whose hub the session broadcasts on and whose live activity buffer the handshake reads, the store the agent uses as its memory and the session files each turn's token count in, and the Gemini API key. Output: the session, idle; register its Start, Stop and Status methods on the mux (see cmd/daemon.go for the route names).
func NewVoice(s *Server, store *db.Store, apiKey string) *VoiceSession {
	v := &VoiceSession{hub: s.hub, store: store, state: "idle"}
	v.open = func() (audio.Microphone, audio.Speaker, voiceRunner, error) {
		mic, err := audio.NewMic()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("microphone: %w", err)
		}
		speaker, err := audio.NewSpeaker()
		if err != nil {
			mic.Close()
			return nil, nil, nil, fmt.Errorf("speaker: %w", err)
		}
		return mic, speaker, liveAgent{newVoiceAgent(s, mic, speaker, store, apiKey)}, nil
	}
	return v
}

// newVoiceAgent builds the live agent one voice session drives. Input: the server the session broadcasts on, the session's microphone and speaker, the store the agent uses as its memory, and the Gemini API key. Output: the agent, configured with the live model and the user's chosen voice and wired to the screen. Extracted from NewVoice so the wiring is testable without opening the user's real microphone.
func newVoiceAgent(s *Server, mic audio.Microphone, speaker audio.Speaker, store *db.Store, apiKey string) *agent.Agent {
	a := agent.NewAgent(mic, speaker, store, nil, apiKey)
	a.SetModel(config.VoiceModel())
	a.SetVoice(config.LoadConfig().Voice)
	// The handshake's "[working]" context comes from the same live activity buffer the window's read routes draw on, which the daemon already wired into the server.
	if s.screen != nil {
		a.SetBufferProvider(s.screen)
	}
	// point_at rings through the same overlay path POST /overlay uses, so the extension has one thing to listen to whether the ring came from a typed ask or from speech. A spoken ring belongs to no /ask, so it is stamped with the non-ask id rather than with an id that names a question the user never typed.
	a.Point = func(x, y, w, h int, label string) error { return s.Ring(overlayNoAsk, x, y, w, h, label) }
	// draw goes through the same overlay path as point_at and show_marks. Server.Draw stamps it with whichever ask is running, which for a spoken session is none, so it lands under the same non-ask id the other two use.
	a.Draw = s.Draw
	// A keyboard and a pointer, through the same desktop portal the typed ask has always had. Without one press_key, click_at and scroll_at refused every call with "this session cannot reach the keyboard or the pointer", so a spoken request to scroll down a page ended with Ora asking the user to scroll it themselves — which is what happened on 2026-09-07 against a blog the user asked it to read. The portal asks for consent once and remembers it, so this does not put a dialog in front of the user on every session.
	a.UsePortalInput(config.DataDir())
	// show_marks marks through the same overlay path, one rect per observed item, labelled with the item's own number so the marks line up with what observe_screen just listed.
	a.Marks = func(items []act.Item) error {
		rects := make([]OverlayRect, len(items))
		for i, it := range items {
			rects[i] = OverlayRect{X: it.X, Y: it.Y, W: it.W, H: it.H, Label: strconv.Itoa(it.N)}
		}
		return s.Marks(overlayNoAsk, rects)
	}
	return a
}

// emit broadcasts one voice event under the session's id. Evidence and Actions are sent empty rather than nil so the JSON matches every other event on the stream.
func (v *VoiceSession) emit(id, typ, text string) {
	v.hub.broadcast(Event{ID: id, Type: typ, Text: text, Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
}

// setState records the session's new state and broadcasts it, but only when it actually changed and only while id is still the running session — Ora's speech arrives word by word, so without this every chunk would repeat "speaking".
func (v *VoiceSession) setState(id, state string) {
	v.mu.Lock()
	if v.id != id || v.state == state {
		v.mu.Unlock()
		return
	}
	v.state = state
	v.mu.Unlock()
	v.emit(id, "state", state)
}

// Start handles POST /voice/start. Input: no body. Output: 405 for any method but POST — a GET would otherwise open the microphone; 202 with JSON {"id": string} once the microphone, the speaker and the live session are up, or 409 when a session is already running (or was stopped while this call was still opening its hardware), or 500 when the audio devices cannot be opened. The session then runs in the background and everything it hears and says arrives on /events under that id.
//
// The lock is held only to claim the id and to install the opened hardware, never across v.open or mic.StartCapture themselves — those reach real devices and can take a while, and Status and Stop must keep answering while they do, the same shape Dictation.Start already uses. The claim is a reservation under v.id with state "starting": Status sees a session already turning on, a second Start sees one already running, and a Stop landing during the reservation clears v.id, which the check after opening the hardware reads back to know its own start was cancelled out from under it.
func (v *VoiceSession) Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v.mu.Lock()
	if v.id != "" {
		v.mu.Unlock()
		http.Error(w, "a voice session is already running", http.StatusConflict)
		return
	}
	v.seq++
	id := fmt.Sprintf("voice-%d", v.seq)
	v.id, v.state = id, "starting"
	v.mu.Unlock()

	mic, speaker, run, err := v.open()
	if err != nil {
		v.clearReservation(id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	// A spoken session belongs to no conversation, so its calls are filed under conversation 0 — until now it recorded nothing at all, which is why a voice turn that used look and draw looked, from the store, like a turn that used no tools.
	ctx = agent.WithToolRecorder(ctx, toolRecorder(v.store, "voice", 0))
	// Capture starts once and outlives every reconnect below, the same way the terminal client does it.
	micChan, err := mic.StartCapture(ctx)
	if err != nil {
		cancel()
		mic.Close()
		speaker.Close()
		v.clearReservation(id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	done := make(chan struct{})
	v.mu.Lock()
	if v.id != id {
		// A Stop landed while the hardware was opening, or already claimed the reservation for itself; whatever was just opened is never wired up, so it is released here rather than left running with nothing watching it.
		v.mu.Unlock()
		cancel()
		mic.Close()
		speaker.Close()
		http.Error(w, "voice session was stopped while starting", http.StatusConflict)
		return
	}
	v.state, v.cancel, v.done, v.mic, v.speaker = "listening", cancel, done, mic, speaker
	v.mu.Unlock()

	v.emit(id, "state", "listening")
	go v.watch(ctx, id, run)
	go v.dial(ctx, id, run, micChan, done)
	go v.levels(ctx, id, mic, speaker)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

// clearReservation releases the id Start reserved for a session whose hardware never came up, but only while that reservation is still standing — a concurrent Stop may already have cleared it, in which case there is nothing left for this to undo.
func (v *VoiceSession) clearReservation(id string) {
	v.mu.Lock()
	if v.id == id {
		v.id, v.state = "", "idle"
	}
	v.mu.Unlock()
}

// voiceBackoff returns how long dial should wait before its next redial, given the delay it waited last time. It doubles, capped at voiceMaxReconnectDelay. Input: the previous delay (voiceReconnectDelay for the first retry after a failure). Output: the delay to wait before the next one.
func voiceBackoff(prev time.Duration) time.Duration {
	next := prev * 2
	if next > voiceMaxReconnectDelay {
		next = voiceMaxReconnectDelay
	}
	return next
}

// dial holds the live session open, redialing when it drops, until the session's context is cancelled or the run has failed voiceMaxConsecutiveFailures times in a row. Closing done tells Stop the run is over. A run that keeps failing — a missing or expired API key holds the microphone open on nothing, say — is retried with a growing backoff rather than every voiceReconnectDelay for ever, and once the failures run out dial tears the session down itself so the microphone is not left open with nobody watching.
func (v *VoiceSession) dial(ctx context.Context, id string, run voiceRunner, micChan <-chan []byte, done chan struct{}) {
	defer close(done)
	// delay and failures are only touched once a run has actually failed, so a session whose run never fails never reads voiceReconnectDelay, voiceMaxReconnectDelay or voiceMaxConsecutiveFailures at all — the same as before backoff and giving-up existed.
	var delay time.Duration
	failures := 0
	for {
		err := run.Run(ctx, micChan)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			return
		}
		// A run that ended on the server's GoAway closed itself so it could be resumed before the server's deadline: it is redialed at once and counts for nothing, since a conversation that has merely lasted long is not failing.
		if errors.Is(err, agent.ErrGoAway) {
			slog.Info("live session ended on GoAway, resuming", "id", id)
			continue
		}
		failures++
		if failures >= voiceMaxConsecutiveFailures {
			slog.Error("daemon voice session failed too many times in a row, giving up — check that the Gemini API key (GEMINI_API_KEY) is set and valid", "id", id, "error", err, "attempts", failures)
			v.giveUp(id)
			return
		}
		if delay == 0 {
			delay = voiceReconnectDelay
		} else {
			delay = voiceBackoff(delay)
		}
		slog.Error("daemon voice session lost, reconnecting", "id", id, "error", err, "in", delay, "attempt", failures)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
}

// giveUp tears a session down from inside dial itself once retrying it has stopped being worth it: it releases the microphone and the speaker and broadcasts idle, the same as Stop, but does not wait on done — dial is the very goroutine that closes done, on its way out right after this returns, so waiting on it here would deadlock. It does still call cancel, same as Stop/end: without it the session's ctx is never cancelled, and watch's goroutine — parked on ctx.Done() and the agent's channels — leaks forever instead of returning once dial gives up. Input: the id of the session dial was running. Output: nothing. A stop landing at the same moment is safe either way: whichever of the two clears v.id first does the teardown, and the other finds nothing left of this session to do.
func (v *VoiceSession) giveUp(id string) {
	v.mu.Lock()
	if v.id != id {
		v.mu.Unlock()
		return
	}
	cancel, mic, speaker := v.cancel, v.mic, v.speaker
	v.id, v.state, v.cancel, v.done, v.mic, v.speaker = "", "idle", nil, nil, nil, nil
	v.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if mic != nil {
		mic.Close()
	}
	if speaker != nil {
		speaker.Close()
	}
	v.emit(id, "state", "idle")
}

// watch turns the agent's own channels into events on the hub for as long as the session runs: what the user said becomes "heard", what Ora said becomes "said", each tool call becomes "tool", and the session's state changes become "state".
func (v *VoiceSession) watch(ctx context.Context, id string, run voiceRunner) {
	for {
		select {
		case <-ctx.Done():
			return
		case chunk := <-run.Text():
			switch {
			case chunk.TurnBoundary:
				v.setState(id, "listening")
				// The end-of-turn marker is the one thing the live session sends per turn, so it is where the turn's token count is read and filed.
				v.recordTurnUsage(run.Usage())
			case chunk.Sender == agent.SenderYou:
				v.emit(id, "heard", chunk.Text)
				if isStopPhrase(chunk.Text) {
					// end waits for the run to return, and the run may be blocked writing the next chunk to the channel this loop reads, so it cannot be waited on from here.
					go v.end()
				}
			case chunk.Sender == agent.SenderSystem:
				// Connection notices and "[ora stopped]" are the terminal transcript's business, not the window's.
			case chunk.IsThought:
				v.setState(id, "thinking")
			case chunk.Text != "":
				// The first transcribed word of a reply is the closest thing to the first sound the agent reports outside its logs, so it is what marks the session as speaking.
				v.setState(id, "speaking")
				v.emit(id, "said", chunk.Text)
			}
		case tool := <-run.Tools():
			if tool.Phase == agent.ToolStarted {
				v.setState(id, "thinking")
				v.emit(id, "tool", tool.Name)
			}
		}
	}
}

// recordTurnUsage files what one finished voice turn cost in tokens, so the user can see what the voice model is costing them rather than reading zero however long they talk. Input: the turn's usage as the live session accumulated it. The model is the configured voice model, the channel is "voice", and the provider is the one the usage names, falling back to Gemini since a Live session is a Gemini call whatever it counted. The write runs on its own bounded context and a failure is logged rather than returned — recording usage must never disturb the session, the same as it never fails an ask (see recordTokenUse in ipc.go).
// A turn that reported nothing at all is not filed. Zeroes used to be written on the argument that the user was charged for a turn whose count never arrived, but the ledger says otherwise: on 2026-09-07 the Recent calls table alternated a real count with a zero, turn after turn, because a boundary arrives for every turn while the usage is snapshotted and reset on only some of them. So a zero row is the same turn counted twice, once with its tokens and once without, and half the ledger was a duplicate of the other half.
func (v *VoiceSession) recordTurnUsage(usage agent.TokenUsage) {
	if v.store == nil || emptyUsage(usage) {
		return
	}
	provider := usage.Provider
	if provider == "" {
		provider = agent.ProviderGemini
	}
	storeCtx, storeCancel := context.WithTimeout(context.Background(), storeTokenUseTimeout)
	defer storeCancel()
	if _, err := v.store.AddTokenUse(storeCtx, db.TokenUse{
		Provider:     provider,
		Model:        config.VoiceModel(),
		Channel:      "voice",
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
		CachedTokens: usage.CachedInputTokens,
		Rounds:       usage.Rounds,
	}); err != nil {
		slog.Error("voice: could not record the token use", "error", err)
	}
}

// stopWords are the whole utterances that end a session when the user says one. The window is often hidden while a session runs, so a key press is not always available and the spoken command has to work from the daemon's side.
var stopWords = map[string]bool{"stop": true, "ora stop": true, "ora, stop": true}

// isStopPhrase reports whether what the user just said is the spoken command to end the session. Input: one transcribed utterance. Output: true for "stop" and "Ora, stop" in any case, with surrounding space and trailing punctuation ignored; false for a sentence that merely contains the word.
func isStopPhrase(text string) bool {
	return stopWords[strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!?…"))]
}

// end tears one session down: it cancels the run, waits for it to return, releases the microphone and the speaker, and broadcasts the idle state so every window — including one that is hidden and never asked for this — sees the session finish. Input: nothing. Output: nothing; ending when nothing is running does nothing.
func (v *VoiceSession) end() {
	v.mu.Lock()
	id, cancel, done, mic, speaker := v.id, v.cancel, v.done, v.mic, v.speaker
	v.id, v.state, v.cancel, v.done, v.mic, v.speaker = "", "idle", nil, nil, nil, nil
	v.mu.Unlock()

	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(voiceStopTimeout):
			slog.Warn("daemon voice session did not end in time, closing its audio anyway", "id", id)
		}
	}
	// The speaker and the microphone are the user's own devices: they are released whether or not the session shut down cleanly.
	if mic != nil {
		mic.Close()
	}
	if speaker != nil {
		speaker.Close()
	}
	if id != "" {
		v.emit(id, "state", "idle")
	}
}

// Stop handles POST /voice/stop. Input: no body. Output: 405 for any method but POST; 200 with JSON {"active": false} once the live session has ended and the microphone and speaker are closed. Stopping when nothing is running is not an error.
func (v *VoiceSession) Stop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v.end()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"active": false})
}

// Status handles GET /voice/status. Output: JSON {"active": bool, "id": string, "state": string}, where state is "idle" when nothing is running and otherwise "listening", "speaking" or "thinking".
func (v *VoiceSession) Status(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	active, id, state := v.id != "", v.id, v.state
	v.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"active": active, "id": id, "state": state})
}

// emptyUsage reports whether a turn's usage carries no count of any kind, which is what a boundary that arrived between snapshots looks like. Input: the usage. Output: true when every field the ledger stores is zero.
func emptyUsage(u agent.TokenUsage) bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 && u.CachedInputTokens == 0 && u.Rounds == 0
}
