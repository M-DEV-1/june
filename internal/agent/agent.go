package agent

import (
	"context"
	"ora/internal/audio"
	"ora/internal/db"
	"ora/internal/memory"
	"sync"
	"sync/atomic"
	"time"
)

// atomic is well, really cool
// lighter lock-free primitives but only work per-variable and mutex protects blocks but is heavier

// brain interface. notes ops live here too so the TUI can save
// user-stated facts via the agent handle.
type ContextReader interface {
	GetImplicitContext(ctx context.Context) ([]string, error)
	SearchMemory(ctx context.Context, query string) ([]db.MemoryHit, error)
	RankedEpisodes(ctx context.Context, focus string, limit int) ([]db.MemoryHit, error)
	RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error)
	LogNote(ctx context.Context, content, kind string) (int64, error)
	GetNotes(ctx context.Context) ([]db.Note, error)
	DeleteNote(ctx context.Context, id int64) error
	EpisodesInWindow(ctx context.Context, since, until time.Time, limit int) ([]db.Episode, error)
	RecallSubject(ctx context.Context, subject string, limit int) ([]string, error)
}

type ToolRequest struct {
	Command    string
	ResultChan chan<- string
}

type Agent struct {
	mic              audio.Microphone
	speaker          audio.Speaker
	brain            ContextReader
	compiler         *memory.Compiler
	apiKey           string
	model            atomic.Value
	isMuted          atomic.Bool
	writeMu          sync.Mutex  // protects websocket writes
	TextChan         chan string // this is for tui text input
	TextResponseChan chan string // results for tui text resp
	ErrorChan        chan error  // websocket connection crashes
	ToolApprovalChan chan ToolRequest
	AllowedCmds      sync.Map // session allowlist for shell commands
}

// initializer and orchestrates all hardware (2) and memory (1) moduels
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
	}
	return a
}
