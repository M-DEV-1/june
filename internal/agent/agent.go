package agent

import (
	"context"
	"ora/internal/act"
	"ora/internal/audio"
	"ora/internal/db"
	"ora/internal/memory"
	"ora/internal/tracker"
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
	RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error)
	LogNote(ctx context.Context, content, kind string) (int64, error)
	GetNotes(ctx context.Context) ([]db.Note, error)
	// NotesOfKindSince lists the notes of one kind filed at or after since, newest first, so a reader after the meeting minutes does not page the whole notes table through memory.
	NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]db.Note, error)
	// UpdateNote/DeleteNote back the "revise" tool's note path (tools.go) — the model's only way to fix or remove a wrong note, using the id query_memory's "[note#N]" format gives it.
	UpdateNote(ctx context.Context, id int64, content string) error
	DeleteNote(ctx context.Context, id int64) error
	// PersonalContext/SetPersonalContext/DeletePersonalContext back the personal_context tool (tools.go) and the block the system prompt opens with. Separate from notes on purpose: these are the things the user stated about themselves, edited in place by subject, and no inference path writes here.
	PersonalContext(ctx context.Context) ([]db.PersonalEntry, error)
	SetPersonalContext(ctx context.Context, subject, content string) error
	DeletePersonalContext(ctx context.Context, subject string) error
	// SetActionStatus backs the "revise" tool's state path (tools.go). An action item is a notes row whose content carries its own status, so a plain content edit could technically reach one — but it would overwrite that structure with prose and silently un-track the item, which is why correcting one goes through here instead. SetActionPriority is unused by any tool now that revise only exposes state — kept for the store's own callers.
	SetActionStatus(ctx context.Context, id int64, status string) error
	SetActionPriority(ctx context.Context, id int64, priority string) error
	// SetActionText backs the "revise" tool's content path for an action item: it re-renders the line around the corrected work text instead of writing over it, and reports db.ErrNotActionItem for an id that is an ordinary note, which is how revise knows to fall back to UpdateNote.
	SetActionText(ctx context.Context, id int64, text string) error
	// OpenActionItems backs the action_items tool (tools.go). "What do I owe?" is a question about a column, not about meaning: the rows say "[open/normal] Alex Rivera — check out develop-essentials-api", which shares no words with the question and sits nowhere near it in embedding space. Asked through query_memory it returned ten summaries about attending meetings and not one action item, so the structural query gets its own door.
	OpenActionItems(ctx context.Context) ([]memory.ActionItem, error)
	// EpisodesForThread backed the retired thread_evidence tool. No tool calls it now; left on the interface rather than rippling its removal into the store.
	EpisodesForThread(ctx context.Context, threadID int64, limit int) ([]db.Episode, error)
	// UpdateThreadState backs the "revise" tool's thread path (tools.go). Threads live in their own table with their own semantics — a subject plus a state summary — so a note edit cannot reach them, and a thread whose summary merged two unrelated things was unfixable until this existed.
	UpdateThreadState(ctx context.Context, id int64, state string) error
	ListEpisodes(ctx context.Context, q db.EpisodeQuery) ([]db.Episode, error)
	// SummaryTimeline backs recall's coverage tier: when a window holds more episodes than one tool result fits, the window's task summaries — bounded per day by construction — answer instead, so a busy stretch cannot scroll the rest of its own day out of the reply.
	SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error)
	RecallSubject(ctx context.Context, subject string, limit int) ([]string, error)
	// HybridSearchWindow fuses lexical (FTS5) and vector search via reciprocal rank fusion, optionally filtered to domainFilter ("work" | "personal" | "" for none) and to items whose timestamp falls in [since, until] (a zero bound is open on that side). The window is enforced store-side, before ranking's top-k, so a sparse window still yields its items. Backs query_memory's "domain" and "since"/"until" params.
	HybridSearchWindow(ctx context.Context, query, domainFilter string, since, until time.Time, limit int) ([]db.MemoryHit, error)
	// QueryStore backs the query_store tool (tools.go): one read-only SQL statement run over a separate read-only connection, rendered as a header line plus one line per row. For structural/aggregate questions query_memory's ranking can't answer — "which meetings today", "what hour do I usually stop" — where the model needs to read the schema and write the query itself, not have relevance-ranked rows guessed at it.
	QueryStore(ctx context.Context, query string, rowCap int) (string, error)
	// SaveFold/UnconsumedFolds/ConsumeFold back branch()'s dead-session fallback (subtask.go, connect.go): a fold that missed its live session is staged here and surfaced once at the next handshake instead of dropped. Separate from notes — see db.Fold.
	SaveFold(ctx context.Context, task, result string) (int64, error)
	UnconsumedFolds(ctx context.Context) ([]db.Fold, error)
	ConsumeFold(ctx context.Context, id int64) error
}

// ResponseChunk is one piece of text bound for the UI transcript: model output (ora), a finished voice utterance (you), or a system notice. Carries genai's own Part.Thought bit end to end so the UI never has to infer which kind of text it received.
type ResponseChunk struct {
	Text      string
	IsThought bool
	// Sender routes the chunk to the right transcript speaker: "" (zero value) is ora, or one of SenderYou/SenderSystem below.
	Sender string
	// TurnBoundary marks the end of one model turn (Text is empty on this chunk) — the UI closes the current ora message block on receipt so the next ora chunk starts a fresh one instead of merging into whatever came before.
	TurnBoundary bool
}

const (
	SenderYou    = "you"
	SenderSystem = "system"
)

// ToolRequest is a generic HITL approval request — not shell-specific, so tools beyond shell_exec (read_file on a sensitive path, read_clipboard) can gate through the same TUI approve/reject/edit flow instead of shipping their result to the model with zero user involvement.
type ToolRequest struct {
	// Description is shown in the approval prompt, e.g. "shell: ls -la" or "read file: ~/.ssh/id_rsa".
	Description string
	// Execute runs the approved action and returns its result — called by the TUI on "Allow once"/"Allow for session", never on reject.
	Execute    func() string
	ResultChan chan<- string
	// AllowKey, if non-empty, is what "Allow for session" stores into AllowedCmds so a repeat request for the same resource skips approval for the rest of the session. Callers check AllowedCmds themselves before sending a request; this only tells the TUI what to store on approval.
	AllowKey string
	// EditableCommand, if non-empty, is the raw command text "Suggest changes" pre-fills into the textarea for editing — only shell-backed requests support this; other tools have no command text to edit.
	EditableCommand string
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
	// evalWrites lets the eval entry points run write tools; see AllowEvalWrites.
	evalWrites bool
	mic        audio.Microphone
	speaker    audio.Speaker
	brain      ContextReader
	// gate, when set, is asked before every Gemini request an interactive ask or a live voice session makes, so those count against the same daily allowance as the nightly jobs; nil means unmetered.
	gate RequestGate
	// bufferProvider is the source of the handshake's "[working]" current-activity context — nil means skip that part of the handshake entirely. NewAgent seeds it from a non-nil compiler (in-process use); the client process (which never has a compiler) wires its own via SetBufferProvider — see cmd/client.go's IPC-backed provider.
	bufferProvider func() []tracker.Activity
	// observe reads the window in front for the observe_screen tool: tracker.Observe in production, a fake in tests.
	observe func(ctx context.Context) (app, title string, nodes []act.Node, err error)
	// Point draws a ring around a rectangle on the screen with a label beside it, for the point_at tool. The daemon sets it to the overlay hub; nil means this session cannot draw, and point_at says so. It answers an error when the drawing reached no window, which point_at hands back rather than claiming it ringed anything.
	Point func(x, y, w, h int, label string) error
	// Marks draws one numbered mark over each of the given items, for the show_marks tool. The daemon sets it to the overlay hub; nil means this session cannot draw, and show_marks says so. Its error is what Point's is.
	Marks func(items []act.Item) error
	// Draw draws an arrow, line, path, box or circle on the screen with a label beside it, for the draw tool. Points carries the path for arrow/line/path; x, y, w, h carry the rectangle to draw around or inscribe within for box/circle, and are ignored otherwise. The daemon sets it to the overlay hub's Draw method; nil means this session cannot draw, and draw says so. Returns an error naming what was wrong when shape is none of the five.
	Draw func(shape string, points [][2]int, x, y, w, h int, label string) error
	// lastTarget holds the newest ScreenTarget a point_at, click or draw(on) call recorded, so a later ask's bare "it" resolves against what was actually done rather than a fresh screen read. It is the one piece of screen memory that outlives an ask on purpose — see rememberTarget in tools.go. The list observe_screen produced, the answer it gave and the field a click focused all belong to one ask and live on its context instead; see askLookState and askState in tools.go.
	lastTarget atomic.Value
	// askScreen is the screen state a tool call driven directly rather than through an ask reads and writes, since only an ask attaches one to its context. See askState in tools.go.
	askScreen askLookState
	// capture takes the picture the look tool sends the model: tracker.CaptureFront in production, a fake in tests. nil means this session cannot see the screen, and look says so.
	// What one ask's looks leave behind — the newest picture, whether it has been handed to the model, how many it has taken and what they cost — lives on an askLookState carried on the ask's own context (see withAskLookState in tools.go), not here, so two asks running at once never share one screenshot.
	capture func(ctx context.Context) (tracker.Capture, error)
	// doAction and scrollTo act on the screen for the click and scroll_to tools (type_text goes through the input session, see UsePortalInput): the tracker's in production, fakes in tests.
	doAction func(ctx context.Context, ref string) (string, error)
	scrollTo func(ctx context.Context, ref string) error
	// input hands back the keyboard and pointer the press_key, click_at and scroll_at tools drive, and the one type_text falls back to, opening it on the first call that needs it — see UsePortalInput and onceInput in tools.go. nil means nothing wired one up and those tools say so.
	input func(ctx context.Context) (InputDevice, error)
	// raiser brings another application's window to the front through the bundled GNOME Shell extension, which switch_window tries before driving the shell's own search from the keyboard — see UseWindowRaiser and WindowRaiser in tools.go. nil means nothing wired one up, which is also what an uninstalled extension amounts to, and switch_window uses the keys instead.
	raiser WindowRaiser
	// verify re-reads a node's role, label and rectangle right before click acts on it or point_at rings it, since a toolkit can recycle an object path onto a different element after the page re-renders, and a page that scrolls under the list leaves every number pointing at the right element in the wrong place. Takes what observe_screen listed for the node; returns an error naming what changed, or nil when the node still matches. The tracker's in production, a fake in tests.
	verify func(ctx context.Context, ref, role, label string, x, y, w, h int) error
	// focused reports whether one element holds the keyboard right now, read from the accessibility focused state, so type_text and a focused key press check their stop line against the field that actually has the keyboard rather than against the one the last click acted on. The tracker's in production, a fake in tests; nil means nothing wired one up and the remembered click stands on its own.
	focused func(ctx context.Context, ref string) (bool, error)
	// extents reads where one element is on the screen right now, so point_at and the guarded-click confirmation ring the element where it is rather than where the list left it. The tracker's in production, a fake in tests.
	extents          func(ctx context.Context, ref string) (x, y, w, h int, err error)
	apiKey           string
	model            atomic.Value
	voice            atomic.Value
	isMuted          atomic.Bool
	writeMu          sync.Mutex         // protects websocket writes
	TextChan         chan string        // this is for tui text input
	TextResponseChan chan ResponseChunk // results for tui text resp — tagged with IsThought so the UI never infers it from content
	ErrorChan        chan error         // websocket connection crashes
	ToolApprovalChan chan ToolRequest
	// ToolActivityChan carries a Started/Finished pair for every tool call, so a UI can show live "tool running" status. Buffered 20 to absorb a burst of concurrent tool calls in one turn; sends are non-blocking like every other Agent channel — a missed event is cosmetic, never a correctness issue.
	ToolActivityChan chan ToolActivity
	AllowedCmds      sync.Map // session allowlist for shell commands
	// ReconnectChan lets callers (e.g. /voice) force the live session to drop and re-dial, so a config change fixed at handshake (like TTS voice) applies immediately. Buffered 1, non-blocking send — a redundant trigger is a no-op.
	ReconnectChan chan struct{}
	// resumeHandle stores the latest Gemini Live session-resumption handle (from LiveServerSessionResumptionUpdate), so a reconnect can resume via SessionResumptionConfig instead of cold-starting. atomic.Value, same pattern as model/voice above.
	resumeHandle atomic.Value
	// voiceUsage holds the Live API token usage of this session's most recently completed voice turn. receiveLoop writes it once per finished turn while whatever goroutine is draining TextResponseChan reads it, so it is an atomic.Value, the same pattern as resumeHandle above; unset means no turn has finished yet. See VoiceUsage in connect.go.
	voiceUsage atomic.Value

	// subtaskClientOnce/subtaskClient/subtaskClientErr back branch()'s side-call engine (subtask.go): a second, independent *genai.Client, lazily built once and reused for the process lifetime — same pattern as memory.GeminiSummarizer.
	subtaskClientOnce sync.Once
	subtaskClient     *genai.Client
	subtaskClientErr  error
	// subtaskModelFactory returns the model behind branch's side-call loop — defaultSubtaskModel in production, overridable in tests to inject a fake (same seam as liveSession in connect.go).
	subtaskModelFactory func() (subtaskModel, error)
	// branchCalls counts branch() invocations in the current live session — reset at the top of each Connect() call. maxBranchesPerSession (see subtask.go) bounds it.
	branchCalls atomic.Int32
	// typedTurnActive is true while the model is answering a message the user TYPED: set by textSendLoop on send, cleared at the next turn boundary. receiveLoop reads it to tell an ambient-room interruption of a typed answer apart from a real spoken barge-in, which are the same server event but mean opposite things to the user.
	typedTurnActive atomic.Bool
	// toolResponseAt is the wall-clock time (unix nanoseconds) of the most recent INTERRUPT-scheduled FunctionResponse send, or 0 when none is outstanding. The Live server interrupts its own generation to fold such a result in and reports that with the same ServerContent.Interrupted flag a user barge-in uses; receiveLoop consumes this to tell the two apart. See toolInterruptWindow in connect.go.
	toolResponseAt atomic.Int64
}

// markToolResponseSent records that an INTERRUPT-scheduled tool result is being delivered right now, so the interrupt the server raises to fold it in isn't mistaken for the user cutting in.
// Input: the moment of the send. Output: none.
func (a *Agent) markToolResponseSent(now time.Time) {
	a.toolResponseAt.Store(now.UnixNano())
}

// consumeToolDeliveryInterrupt reports whether an Interrupted event is the server folding in a tool result rather than a user barge-in, and clears the record so only the first interrupt after each send is attributed to it.
// Input: the moment the Interrupted event arrived. Output: true when an INTERRUPT-scheduled tool response was sent within toolInterruptWindow of it.
func (a *Agent) consumeToolDeliveryInterrupt(now time.Time) bool {
	at := a.toolResponseAt.Load()
	if at == 0 || now.Sub(time.Unix(0, at)) > toolInterruptWindow {
		return false
	}
	a.toolResponseAt.Store(0)
	return true
}

// markTypedTurn records that the turn now starting was initiated by typed text rather than speech.
func (a *Agent) markTypedTurn() {
	a.typedTurnActive.Store(true)
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

// NewAgent keeps the compiler param for signature compatibility — a non-nil compiler seeds bufferProvider for any in-process caller (the client process has no compiler and never did; it wires its own provider afterward via SetBufferProvider — see cmd/client.go's IPC-backed one).
// RequestGate decides whether one more Gemini request against a model may go out today. It is the same one-method shape memory.GeminiSummarizer takes, so the daemon hands both the one shared daily count.
type RequestGate interface {
	Allow(model string) error
}

// SetRequestGate installs the daily request gate the interactive paths consult. Input: the gate, or nil to run unmetered.
func (a *Agent) SetRequestGate(g RequestGate) { a.gate = g }

// allowGemini asks the installed gate whether a Gemini request against model may go out now. Input: the model the request is for. Output: nil, or the gate's refusal, which carries the 429 shape GeminiCannotAnswer recognises so the ask hands over to Codex and Claude instead of failing.
func (a *Agent) allowGemini(model string) error {
	if a.gate == nil {
		return nil
	}
	return a.gate.Allow(model)
}

func NewAgent(mic audio.Microphone, speaker audio.Speaker, brain ContextReader, compiler *memory.Compiler, apiKey string) *Agent {
	a := &Agent{
		mic:              mic,
		speaker:          speaker,
		brain:            brain,
		observe:          tracker.Observe,
		capture:          tracker.CaptureFront,
		doAction:         tracker.DoAction,
		scrollTo:         tracker.ScrollTo,
		verify:           tracker.Verify,
		focused:          tracker.Focused,
		extents:          tracker.Extents,
		apiKey:           apiKey,
		TextChan:         make(chan string, 100),
		TextResponseChan: make(chan ResponseChunk, 100),
		ErrorChan:        make(chan error, 10),
		ToolApprovalChan: make(chan ToolRequest, 1),
		ToolActivityChan: make(chan ToolActivity, 20),
		ReconnectChan:    make(chan struct{}, 1),
	}
	if compiler != nil {
		a.bufferProvider = compiler.GetCurrentBuffer
	}
	a.subtaskModelFactory = a.defaultSubtaskModel
	return a
}

// SetBufferProvider wires the source of the handshake's "[working]" current-activity context — the client process (which has no in-process compiler) uses this to plug in an IPC-backed provider instead. nil (the default when no compiler was passed to NewAgent either) skips that part of the handshake entirely.
func (a *Agent) SetBufferProvider(p func() []tracker.Activity) {
	a.bufferProvider = p
}
