package memory

import (
	"context"
	"june/internal/obs"
	"june/internal/tracker"
	"june/internal/util"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type TaskSummary struct {
	SameTask bool     `json:"same_task"`
	TaskName string   `json:"task_name"`
	Summary  string   `json:"summary"`
	Notes    []string `json:"notes,omitempty"`
	// Since is the start of the flush this summary was written from, and bounds the episodes the store votes over when it tags the summary with a domain. Not persisted: the summary node's content is this struct marshalled, and this field describes how the summary was made, not what it says.
	Since time.Time `json:"-"`
}

// NoteRef is a lightweight handle to an existing stored note.
// Defined here (not in db) to avoid an import cycle: db imports memory.
type NoteRef struct {
	ID      int64
	Content string
}

// NoteOp is the reconciliation decision for a single candidate fact.
type NoteOp struct {
	Action  string // "add" | "update" | "skip"
	ID      int64  // for "update": which existing note to overwrite
	Content string // for "add"/"update": the text to store
}

// Thread is an ongoing throughline in the user's life, with a current state describing where the user is *within* it.
type Thread struct {
	ID        int64
	Subject   string
	Kind      string // work | project | entertainment | learning | routine | person
	State     string // where they are within it right now
	Salience  float64
	TimesSeen int
	LastSeen  time.Time
	Status    string
}

// ThreadUpdate is the compiler's attribution of a buffer slice to one thread.
type ThreadUpdate struct {
	ID      int64  `json:"id"` // 0 = new thread
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Summary string `json:"summary"` // episodic: what happened in this slice
	Novel   bool   `json:"novel"`
}

// ThreadAttribution is the full result of attributing one flushed buffer.
type ThreadAttribution struct {
	Threads  []ThreadUpdate `json:"threads"`
	Identity []string       `json:"identity"` // durable PERSON facts only
}

type Summarizer interface {
	// ReconcileNotes decides, per candidate fact, whether to add, update (refines/supersedes an existing note), or skip (already known). Returns nil, nil when candidates is empty.
	ReconcileNotes(ctx context.Context, existing []NoteRef, candidates []string) ([]NoteOp, error)
	// AttributeThreads maps a flushed buffer onto ongoing threads, emitting one update per concurrent thread plus any durable PERSON facts.
	AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []Thread) (*ThreadAttribution, error)
}

// RequestGate refuses a Gemini request before it is made, so a shared daily quota can cover every direct call this summarizer makes, not only the Brain-wrapped call sites elsewhere in the daemon. Allow returns nil to let a call against model proceed, or an error to refuse it. Defined here rather than imported from internal/brain (whose QuotaState is the intended implementation, via a small adapter) because internal/brain imports internal/agent, which imports this package.
type RequestGate interface {
	Allow(model string) error
}

type Storage interface {
	LogSemanticNode(ctx context.Context, summary TaskSummary) error
	LogNote(ctx context.Context, content, kind string) (int64, error)
	ExistingNotes(ctx context.Context) ([]NoteRef, error)
	UpdateNote(ctx context.Context, id int64, content string) error
	// UpsertThread creates or refreshes a thread, returning its id.
	UpsertThread(ctx context.Context, u ThreadUpdate) (int64, error)
	// ThreadsForAttribution returns recent threads to seed the attribution call.
	ThreadsForAttribution(ctx context.Context, limit int) ([]Thread, error)
	// LinkEpisodesToThread records which captures a thread was attributed from. The attribution happens here on every flush and, until this existed, was discarded every time — leaving a store where a thread could say "reviewed the code, eleven findings" and nothing could reach the screens the findings were on.
	LinkEpisodesToThread(ctx context.Context, threadID int64, since, until time.Time) error
}

const wordFlushLimit = 1500

// minAppSwitchGap and minAppSwitchBuffer are the floor under the application-switch flush. Every flush costs one metered attribution call, and alt-tabbing between two windows produces a switch every few seconds; without a floor that is one call per switch, and once the daily gate refuses them every later switch writes a "Raw Activity Log" node into memory instead. A switch below either floor leaves the buffer alone, so the activity is still flushed by the word limit or the hourly tick.
const (
	minAppSwitchGap    = 3 * time.Minute
	minAppSwitchBuffer = 3
)

// summarizerCallTimeout bounds a single model call made by the compiler or by GeminiSummarizer. The genai client has no HTTP timeout of its own, and Ingest runs the attribution call inline on the daemon's episode-drain goroutine: one connection that never answers stops the drain, fills the event channel, and stops screen capture entirely until the daemon is restarted.
const summarizerCallTimeout = 2 * time.Minute

// trivialTitles is the minimal set of placeholder window titles that carry no signal.
var trivialTitles = map[string]struct{}{
	"new tab":  {},
	"untitled": {},
	"desktop":  {},
}

// IsSalient returns false for activities that are obviously noise and should never enter the buffer.
// Conservative by design: only drop what is clearly meaningless, so real work never gets silently discarded.
func IsSalient(act tracker.Activity) bool {
	title := strings.TrimSpace(act.Title)
	screenWords := CountWords(act.ScreenText)

	if title == "" && screenWords == 0 {
		return false
	}

	if _, trivial := trivialTitles[strings.ToLower(title)]; trivial && screenWords == 0 {
		return false
	}

	return true
}

type Compiler struct {
	llm   Summarizer
	store Storage

	mu        sync.Mutex
	buffer    []tracker.Activity
	wordCount int
	lastFlush time.Time
}

func NewCompiler(llm Summarizer, store Storage) *Compiler {
	return &Compiler{
		llm:       llm,
		store:     store,
		buffer:    make([]tracker.Activity, 0),
		lastFlush: time.Now(),
	}
}

func (c *Compiler) Ingest(ctx context.Context, act tracker.Activity) {
	if !IsSalient(act) {
		return
	}

	incoming := CountWords(act.ScreenText)

	// snapshot-and-reset happens under the lock; processFlush runs after, on the local copy, without holding c.mu — so slow LLM/store calls never block concurrent Ingest/GetCurrentBuffer/ForceFlush.
	c.mu.Lock()
	var flushedBuf []tracker.Activity
	var flushedSince, flushedUntil time.Time
	if len(c.buffer) > 0 {
		last := c.buffer[len(c.buffer)-1]
		appChanged := last.App != act.App && len(c.buffer) >= minAppSwitchBuffer && time.Since(c.lastFlush) >= minAppSwitchGap
		wordLimitHit := c.wordCount+incoming >= wordFlushLimit
		hourElapsed := time.Since(c.lastFlush) >= time.Hour

		if appChanged || wordLimitHit || hourElapsed {
			flushedBuf, flushedSince, flushedUntil = c.resetBufferLocked()
		}
	}

	c.wordCount += incoming
	c.buffer = append(c.buffer, act)
	c.mu.Unlock()

	if flushedBuf != nil {
		c.processFlush(ctx, flushedBuf, flushedSince, flushedUntil)
	}
}

// resetBufferLocked hands the current buffer to the caller and resets the compiler's buffered state. Caller must hold c.mu.
// It also returns the stretch of screen time this buffer covers: from the previous flush's timestamp to this one. Both ends are taken here rather than later because processFlush runs an attribution LLM call before it links anything, and reading the end after that call would sweep in every episode captured while the model was thinking — at a two-second capture poll, a thirty-second call is fifteen unrelated screens attached to the thread.
func (c *Compiler) resetBufferLocked() ([]tracker.Activity, time.Time, time.Time) {
	buf := c.buffer
	since := c.lastFlush
	c.buffer = make([]tracker.Activity, 0)
	c.wordCount = 0
	c.lastFlush = time.Now()
	return buf, since, c.lastFlush
}

// CountWords counts whitespace-separated tokens in s.
func CountWords(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	inWord := false
	for _, c := range s {
		isSpace := c == ' ' || c == '\n' || c == '\r' || c == '\t'
		if !isSpace && !inWord {
			n++
			inWord = true
		} else if isSpace {
			inWord = false
		}
	}
	return n
}

// fallbackSummaryMaxRunes caps processFlush's LLM-failure fallback ("Raw Activity Log") — this path stores raw app|title lines with no summarization, so an unbounded buffer (or a burst of long titles) would otherwise create a node the same size class as the tens-of-KB junk rows excerptContent/truncateUTF8 exist to defend against reading back.
const fallbackSummaryMaxRunes = 2000

// processFlush does the slow LLM/store work for a flushed buffer. Operates only on the local buf snapshot — never touches c.buffer/c.wordCount/c.lastFlush, which are already reset by resetBufferLocked. Never called while holding c.mu.
func (c *Compiler) processFlush(ctx context.Context, buf []tracker.Activity, since, until time.Time) {
	if len(buf) == 0 {
		return
	}

	// Drop only if the whole buffer has no title and no screen text — IsSalient already strips noise at ingestion, this is just the last-resort guard for empty flushes.
	// No word-count minimum: social/gaming/meeting sessions have real titles but thin screen text, and word count is a poor proxy for "worth remembering."
	hasSignal := false
	for _, act := range buf {
		if strings.TrimSpace(act.Title) != "" || act.ScreenText != "" {
			hasSignal = true
			break
		}
	}
	if !hasSignal {
		return
	}

	tracer := obs.GetTracer(ctx, "june.memory")
	ctx, span := tracer.Start(ctx, "Compiler.FlushBuffer")
	defer span.End()

	// attribute the buffer onto ongoing threads; existing threads seed the call so the model can reuse ids and keep the throughline stable over time.
	existingThreads, taErr := c.store.ThreadsForAttribution(ctx, 40)
	if taErr != nil {
		slog.Error("flush: ThreadsForAttribution failed", "err", taErr)
	}
	attrCtx, cancelAttr := context.WithTimeout(ctx, summarizerCallTimeout)
	attr, err := c.llm.AttributeThreads(attrCtx, buf, existingThreads)
	cancelAttr()
	if err != nil || attr == nil || len(attr.Threads) == 0 {
		c.writeFallbackNode(ctx, buf, since)
	} else {
		// one update per concurrent thread: refresh its state and log an episodic summary node so history, FTS, and compaction keep working unchanged.
		for _, u := range attr.Threads {
			id, err := c.store.UpsertThread(ctx, u)
			if err != nil {
				slog.Error("flush: UpsertThread failed", "subject", u.Subject, "err", err)
			} else if err := c.store.LinkEpisodesToThread(ctx, id, since, until); err != nil {
				// Best-effort: the thread and its summary are the record, and losing the edge costs the ability to walk from one to its evidence, not the memory itself.
				slog.Warn("flush: could not link this buffer's episodes to the thread", "subject", u.Subject, "err", err)
			}
			if err := c.store.LogSemanticNode(ctx, TaskSummary{SameTask: u.ID != 0, TaskName: u.Subject, Summary: u.Summary, Since: since}); err != nil {
				slog.Error("flush: LogSemanticNode failed", "subject", u.Subject, "err", err)
			}
		}

		// identity: durable PERSON facts only, reconciled against existing notes as before — usually empty.
		if len(attr.Identity) > 0 {
			existing, exErr := c.store.ExistingNotes(ctx)
			ops, recErr := func() ([]NoteOp, error) {
				if exErr != nil {
					return nil, exErr
				}
				recCtx, cancelRec := context.WithTimeout(ctx, summarizerCallTimeout)
				defer cancelRec()
				return c.llm.ReconcileNotes(recCtx, existing, attr.Identity)
			}()

			if recErr != nil {
				slog.Error("flush: note reconciliation failed, logging raw identity facts", "err", recErr)
				for _, n := range attr.Identity {
					if _, err := c.store.LogNote(ctx, n, "fact"); err != nil {
						slog.Error("flush: LogNote (raw identity) failed", "err", err)
					}
				}
			} else {
				for _, op := range ops {
					switch op.Action {
					case "add":
						if _, err := c.store.LogNote(ctx, op.Content, "fact"); err != nil {
							slog.Error("flush: LogNote (add) failed", "err", err)
						}
					case "update":
						if err := c.store.UpdateNote(ctx, op.ID, op.Content); err != nil {
							slog.Error("flush: UpdateNote failed", "id", op.ID, "err", err)
						}
					}
				}
			}
		}
	}
}

// writeFallbackNode records a drained buffer as one "Raw Activity Log" node, the record kept when no summary could be made of it. Input: ctx, the drained activities, and the start of the stretch they cover. Output: none; a write failure is logged.
// App and title lines only, no ScreenText: this path runs when there was no summarization at all, and dumping every activity's full capture would create exactly the tens-of-KB junk row other code (truncateUTF8, excerptContent) already defends against reading back out.
func (c *Compiler) writeFallbackNode(ctx context.Context, buf []tracker.Activity, since time.Time) {
	var fallbackText strings.Builder
	for _, act := range buf {
		fallbackText.WriteString(act.App + " | " + act.Title + "\n")
	}
	fallbackSummary := TaskSummary{
		SameTask: false,
		TaskName: "Raw Activity Log",
		Summary:  util.RunesEllipsis(strings.TrimSpace(fallbackText.String()), fallbackSummaryMaxRunes),
		Since:    since,
	}
	if err := c.store.LogSemanticNode(ctx, fallbackSummary); err != nil {
		slog.Error("flush: LogSemanticNode (fallback) failed", "err", err)
	}
}

func (c *Compiler) BufferSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buffer)
}

// ForceFlush drains the buffer and processes it, and returns when either that finishes or ctx is done. The buffer is emptied before the slow work starts, so a caller whose deadline expires first — the shutdown flush is the one that does — would otherwise walk away from activity that is no longer anywhere else. When the deadline wins, the drained activities are written as a raw-activity node under a context the deadline cannot cancel, so the stretch is still in memory as app and title lines even though it never got a summary. Input: a context whose deadline bounds the wait. Output: none.
func (c *Compiler) ForceFlush(ctx context.Context) {
	c.mu.Lock()
	if len(c.buffer) == 0 {
		c.mu.Unlock()
		return
	}
	buf, since, until := c.resetBufferLocked()
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.processFlush(ctx, buf, since, until)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		// The abandoned goroutine is running on the same cancelled ctx, so its own store writes fail rather than racing this one.
		c.writeFallbackNode(context.WithoutCancel(ctx), buf, since)
	}
}

// GetCurrentBuffer returns a copy so callers (the /buffer HTTP handler, Agent.Connect) never read a slice that Ingest/flush might be mutating concurrently.
func (c *Compiler) GetCurrentBuffer() []tracker.Activity {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := make([]tracker.Activity, len(c.buffer))
	copy(buf, c.buffer)
	return buf
}
