package agent

import (
	"context"
	"ora/internal/audio"
	"ora/internal/db"
	"ora/internal/memory"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genai"
)

// atomic is well, really cool
// lighter lock-free primitives but only work per-variable and mutex protects blocks but is heavier

// brain interface. Notes ops live here too so the TUI can save user-stated facts via the agent handle.
type ContextReader interface {
	GetImplicitContext(ctx context.Context) ([]string, error)
	SearchMemory(ctx context.Context, query string) ([]db.MemoryHit, error)
	RankedEpisodes(ctx context.Context, focus string, limit int) ([]db.MemoryHit, error)
	RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error)
	LogNote(ctx context.Context, content, kind string) (int64, error)
	GetNotes(ctx context.Context) ([]db.Note, error)
	// UpdateNote/DeleteNote back update_note/delete_note (tools.go) — the model's only way to fix or remove a wrong note, using the id query_memory's "[note#N]" format gives it.
	UpdateNote(ctx context.Context, id int64, content string) error
	DeleteNote(ctx context.Context, id int64) error
	EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error)
	RecallSubject(ctx context.Context, subject string, limit int) ([]string, error)
	// HybridSearch fuses lexical (FTS5) and vector search via reciprocal rank fusion, optionally filtered to domainFilter ("work" | "personal" | "" for none). Backs query_memory's "domain" param.
	HybridSearch(ctx context.Context, query, domainFilter string, limit int) ([]db.MemoryHit, error)
	// SaveFold/UnconsumedFolds/ConsumeFold back branch()'s dead-session fallback (subtask.go, connect.go): a fold that missed its live session is staged here and surfaced once at the next handshake instead of dropped. Separate from notes — see db.Fold.
	SaveFold(ctx context.Context, task, result string) (int64, error)
	UnconsumedFolds(ctx context.Context) ([]db.Fold, error)
	ConsumeFold(ctx context.Context, id int64) error
}

type ToolRequest struct {
	Command    string
	ResultChan chan<- string
}

// ToolPhase is where a tool call is in its started->finished lifecycle.
type ToolPhase int

const (
	ToolStarted ToolPhase = iota
	ToolFinished
)

// ToolActivity is one event in a tool call's lifecycle (Started or Finished), correlated by ID (the model's fc.ID). Emitted for every tool call, not just HITL shell_exec — unlike ToolApprovalChan, which only fires for shell_exec approvals.
type ToolActivity struct {
	ID          string // fc.ID — correlates the Started/Finished pair
	Name        string // raw tool name, e.g. "query_memory"
	ArgsSummary string // pre-formatted primary-arg literal, e.g. `"Riddler puzzles"` — computed here (tools.go) so the UI never needs per-tool arg-shape knowledge
	Phase       ToolPhase
	Started     time.Time // set on both events (echoed on Finished) so a consumer can compute duration without tracking it itself

	// ResultSummary/Err are only meaningful on ToolFinished.
	ResultSummary string
	Err           bool
}

type Agent struct {
	mic              audio.Microphone
	speaker          audio.Speaker
	brain            ContextReader
	compiler         *memory.Compiler
	apiKey           string
	model            atomic.Value
	voice            atomic.Value
	isMuted          atomic.Bool
	writeMu          sync.Mutex  // protects websocket writes
	TextChan         chan string // this is for tui text input
	TextResponseChan chan string // results for tui text resp
	ErrorChan        chan error  // websocket connection crashes
	ToolApprovalChan chan ToolRequest
	// ToolActivityChan carries a Started/Finished pair for every tool call, so a UI can show live "tool running" status. Buffered 20 to absorb a burst of concurrent tool calls in one turn; sends are non-blocking like every other Agent channel — a missed event is cosmetic, never a correctness issue.
	ToolActivityChan chan ToolActivity
	AllowedCmds      sync.Map // session allowlist for shell commands
	// ReconnectChan lets callers (e.g. /voice) force the live session to drop and re-dial, so a config change fixed at handshake (like TTS voice) applies immediately. Buffered 1, non-blocking send — a redundant trigger is a no-op.
	ReconnectChan chan struct{}
	// resumeHandle stores the latest Gemini Live session-resumption handle (from LiveServerSessionResumptionUpdate), so a reconnect can resume via SessionResumptionConfig instead of cold-starting. atomic.Value, same pattern as model/voice above.
	resumeHandle atomic.Value

	// subtaskClientOnce/subtaskClient/subtaskClientErr back branch()'s side-call engine (subtask.go): a second, independent *genai.Client, lazily built once and reused for the process lifetime — same pattern as memory.GeminiSummarizer.
	subtaskClientOnce sync.Once
	subtaskClient     *genai.Client
	subtaskClientErr  error
	// subtaskModelFactory returns the model behind branch's side-call loop — defaultSubtaskModel in production, overridable in tests to inject a fake (same seam as liveSession in connect.go).
	subtaskModelFactory func() (subtaskModel, error)
	// branchCalls counts branch() invocations in the current live session — reset at the top of each Connect() call. maxBranchesPerSession (see subtask.go) bounds it.
	branchCalls atomic.Int32
}

// setResumeHandle stores the latest session-resumption handle reported by the server.
// Empty string is a valid value to store (e.g. before any handle has been received yet), and getResumeHandle treats an unset/empty handle the same way: "no resumption available, cold-start on next connect."
func (a *Agent) setResumeHandle(handle string) {
	a.resumeHandle.Store(handle)
}

// getResumeHandle returns the last stored session-resumption handle, or "" if none has been captured yet.
func (a *Agent) getResumeHandle() string {
	val := a.resumeHandle.Load()
	if val == nil {
		return ""
	}
	return val.(string)
}

func (a *Agent) GetMic() audio.Microphone {
	return a.mic
}

func (a *Agent) GetSpeaker() audio.Speaker {
	return a.speaker
}

func (a *Agent) GetBrain() ContextReader {
	return a.brain
}

func (a *Agent) SetMute(muted bool) {
	a.isMuted.Store(muted)
}

func (a *Agent) ToggleMute() bool {
	current := a.isMuted.Load()
	a.isMuted.Store(!current)
	return !current
}

func (a *Agent) IsMuted() bool {
	return a.isMuted.Load()
}

func (a *Agent) SetModel(name string) {
	a.model.Store(name)
}

func (a *Agent) GetModel() string {
	val := a.model.Load()
	if val == nil {
		return ""
	}
	return val.(string)
}

// SetVoice sets the Gemini Live prebuilt voice name to use on the next (re)connect.
// Does not itself trigger a reconnect — call TriggerReconnect (or let the next natural reconnect pick it up).
func (a *Agent) SetVoice(name string) {
	a.voice.Store(name)
}

// GetVoice returns the currently configured voice name, or "" if unset.
func (a *Agent) GetVoice() string {
	val := a.voice.Load()
	if val == nil {
		return ""
	}
	return val.(string)
}

// TriggerReconnect asks the running Connect session to drop and re-dial, so a voice change applies immediately rather than on the next natural reconnect.
// Non-blocking: if a reconnect is already pending, this is a no-op.
func (a *Agent) TriggerReconnect() {
	select {
	case a.ReconnectChan <- struct{}{}:
	default:
	}
}

func NewAgent(mic audio.Microphone, speaker audio.Speaker, brain ContextReader, compiler *memory.Compiler, apiKey string) *Agent {
	a := &Agent{
		mic:              mic,
		speaker:          speaker,
		brain:            brain,
		compiler:         compiler,
		apiKey:           apiKey,
		TextChan:         make(chan string, 100),
		TextResponseChan: make(chan string, 100),
		ErrorChan:        make(chan error, 10),
		ToolApprovalChan: make(chan ToolRequest, 1),
		ToolActivityChan: make(chan ToolActivity, 20),
		ReconnectChan:    make(chan struct{}, 1),
	}
	a.subtaskModelFactory = a.defaultSubtaskModel
	return a
}
