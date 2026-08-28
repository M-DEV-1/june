package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"ora/internal/config"
	"ora/internal/obs"
	"ora/internal/tracker"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

type TaskSummary struct {
	SameTask bool     `json:"same_task"`
	TaskName string   `json:"task_name"`
	Summary  string   `json:"summary"`
	Notes    []string `json:"notes,omitempty"`
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

// GeminiSummarizer implements Summarizer via the genai SDK. Other providers can implement the same interface later.
type GeminiSummarizer struct {
	client *genai.Client
}

func NewGeminiSummarizer(apiKey string) (*GeminiSummarizer, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}

	return &GeminiSummarizer{
		client: client,
	}, nil
}

// ReconcileNotes calls TextModel with structured JSON output to decide, per candidate, whether to add, update, or skip relative to existing notes.
func (g *GeminiSummarizer) ReconcileNotes(ctx context.Context, existing []NoteRef, candidates []string) ([]NoteOp, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.ReconcileNotes")
	defer span.End()

	var existingLines []string
	for _, n := range existing {
		existingLines = append(existingLines, fmt.Sprintf("%d: %s", n.ID, n.Content))
	}

	prompt := fmt.Sprintf(`You are a memory deduplication engine. You will be given a list of existing stored facts (id: content) and a list of candidate new facts. For EACH candidate decide:
- "add"    — genuinely new, durable information not covered by ANY existing note. Use sparingly.
- "update" — relates to, refines, supersedes, or overlaps an existing note. STRONGLY prefer this over "add" whenever the candidate is about the same topic/project/preference as an existing note; provide that note's id and the merged content. Default to merging.
- "skip"   — already known, OR the candidate is transient task detail (a command, flag, path, version, today's bug) rather than a durable user trait. Drop those.

Bias hard toward "update" and "skip": the goal is a small, stable set of facts, NOT an ever-growing list. Only "add" when nothing existing is even tangentially related.

Existing notes:
%s

Candidate facts:
%s

Return a JSON array with one object per candidate in the same order:
[{"action":"add"|"update"|"skip","id":<existing_id or 0>,"content":"<text for add/update, empty for skip>"}]
Be deterministic. Do not invent facts. Merge wording when updating.`,
		strings.Join(existingLines, "\n"),
		strings.Join(candidates, "\n"))

	_, genSpan := tracer.Start(ctx, "Gemini.GenerateContent.ReconcileNotes")
	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, genai.Text(prompt), &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
	})
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		return nil, fmt.Errorf("reconcile notes llm call: %w", err)
	}
	genSpan.End()

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty reconcile response from model")
	}

	var ops []NoteOp
	if err := json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &ops); err != nil {
		return nil, fmt.Errorf("parse reconcile ops: %w", err)
	}
	return ops, nil
}

// attributionRules is the rule block of the attribution prompt. "summary" is the line that ends up in memory as the record of a slice of the day, so it is told to name the activity rather than the application it happened in; "state" is held to the same voice for the same reason.
const attributionRules = `Rules:
- Reuse a thread id when the activity continues that throughline; use 0 only for a new one.
- Emit MULTIPLE threads for concurrent activities. NEVER collapse entertainment into work or vice-versa.
- "state" is the SPECIFIC position within the thread, from the screen: the scene of a show, the section of an article, the feature being worked on. Name the thing, not the app; no counts or times.
- "summary" is what the user did here, in plain words: the activity itself, never the app or site it happened in.
- "subject" is what the user would call this to a friend: short, stable, plain-spoken; no title case, ampersands, app or file names, or report labels.
- "novel" is true only if this throughline appears genuinely new.
- "identity" holds ONLY durable facts about the PERSON (identity, lasting preferences, skills, relationships). Projects and shows are threads, NOT identity. Usually empty.`

// AttributeThreads maps recent screen activity onto ongoing threads, one update per concurrent throughline (so watching + coding never collapse into one thread) with the SPECIFIC state within each, plus any durable PERSON facts as identity.
func (g *GeminiSummarizer) AttributeThreads(ctx context.Context, activities []tracker.Activity, existing []Thread) (*ThreadAttribution, error) {
	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.AttributeThreads")
	defer span.End()

	var activityList []string
	for _, a := range activities {
		entry := fmt.Sprintf("- %s: %s", a.App, a.Title)
		if a.ScreenText != "" {
			entry += fmt.Sprintf("\n  screen: %s", a.ScreenText)
		}
		activityList = append(activityList, entry)
	}

	var existingLines []string
	for _, t := range existing {
		existingLines = append(existingLines, fmt.Sprintf("id=%d [%s] %s :: %s", t.ID, t.Kind, t.Subject, t.State))
	}

	prompt := fmt.Sprintf(`You are the memory compiler for an ambient OS companion. The user often does several things AT ONCE (e.g. watching a show while coding). Attribute the recent screen activity to ongoing "threads" — durable throughlines in the user's life — and say where they currently are within each.

You are given EXISTING THREADS (id, kind, subject :: current state) and RECENT ACTIVITIES (app, window title, and any screen text/description).

%s

EXISTING THREADS:
%s

RECENT ACTIVITIES:
%s

Respond strictly as JSON:
{"threads":[{"id":0,"subject":"","kind":"work|project|entertainment|learning|routine|person","state":"","summary":"","novel":false}],"identity":[]}`,
		attributionRules,
		strings.Join(existingLines, "\n"),
		strings.Join(activityList, "\n"))

	_, genSpan := tracer.Start(ctx, "Gemini.GenerateContent.AttributeThreads")
	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, genai.Text(prompt), &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
	})
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		return nil, fmt.Errorf("attribute threads llm call: %w", err)
	}
	genSpan.End()

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty attribution response from model")
	}

	var attr ThreadAttribution
	if err := json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &attr); err != nil {
		return nil, fmt.Errorf("parse thread attribution: %w", err)
	}
	return &attr, nil
}

const screenSightPrompt = `You are the eyes of an ambient OS companion. Extract what is on this screen as JSON so it can be searched later.
Be factual and concise. Present tense. Do not invent.

- user_activity: one short phrase for what the user is doing (editing a file, watching a scene, reading an article).
- visible_text: the meaningful visible strings as separate items — messages as "Name: text", terminal errors, headings, code symbols. Not chrome (tabs, buttons, cookies).
- summary: one sentence backup if the lists are thin.

{"user_activity":"","visible_text":[],"summary":""}`

// AnalyzeScreen sends a screenshot to the multimodal model and returns a structured moment (activity + visible chunks). Zero value on any failure so the caller can fall back to accessibility text.
func (g *GeminiSummarizer) AnalyzeScreen(ctx context.Context, png []byte) ScreenSight {
	if len(png) == 0 {
		return ScreenSight{}
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.AnalyzeScreen")
	defer span.End()

	parts := []*genai.Part{
		genai.NewPartFromText(screenSightPrompt),
		genai.NewPartFromBytes(png, "image/png"),
	}
	contents := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}

	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, contents, &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
	})
	if err != nil {
		span.RecordError(err)
		return ScreenSight{}
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return ScreenSight{}
	}
	return ParseScreenSight(resp.Candidates[0].Content.Parts[0].Text)
}

// DescribeScreen is AnalyzeScreen flattened to one string. Kept for callers that only need text.
func (g *GeminiSummarizer) DescribeScreen(ctx context.Context, png []byte) string {
	s := g.AnalyzeScreen(ctx, png)
	return ComposeMoment(s.UserActivity, s.VisibleText, s.Summary)
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
}

const wordFlushLimit = 1500

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
	if len(c.buffer) > 0 {
		last := c.buffer[len(c.buffer)-1]
		appChanged := last.App != act.App
		wordLimitHit := c.wordCount+incoming >= wordFlushLimit
		hourElapsed := time.Since(c.lastFlush) >= time.Hour

		if appChanged || wordLimitHit || hourElapsed {
			flushedBuf = c.resetBufferLocked()
		}
	}

	c.wordCount += incoming
	c.buffer = append(c.buffer, act)
	c.mu.Unlock()

	if flushedBuf != nil {
		c.processFlush(ctx, flushedBuf)
	}
}

// resetBufferLocked hands the current buffer to the caller and resets the compiler's buffered state. Caller must hold c.mu.
func (c *Compiler) resetBufferLocked() []tracker.Activity {
	buf := c.buffer
	c.buffer = make([]tracker.Activity, 0)
	c.wordCount = 0
	c.lastFlush = time.Now()
	return buf
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
func (c *Compiler) processFlush(ctx context.Context, buf []tracker.Activity) {
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

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "Compiler.FlushBuffer")
	defer span.End()

	// attribute the buffer onto ongoing threads; existing threads seed the call so the model can reuse ids and keep the throughline stable over time.
	existingThreads, taErr := c.store.ThreadsForAttribution(ctx, 40)
	if taErr != nil {
		slog.Error("flush: ThreadsForAttribution failed", "err", taErr)
	}
	attr, err := c.llm.AttributeThreads(ctx, buf, existingThreads)
	if err != nil || attr == nil || len(attr.Threads) == 0 {
		// log raw activities if LLM fails or produces no usable attribution
		// app | title lines only — no ScreenText. This fallback fires when the LLM call itself failed, so there's no summarization happening at all; dumping every activity's full raw capture here would create exactly the tens-of-KB junk row other code (truncateUTF8, excerptContent) already defends against reading back out.
		var fallbackText strings.Builder
		for _, act := range buf {
			fallbackText.WriteString(act.App + " | " + act.Title + "\n")
		}
		fallbackSummary := TaskSummary{
			SameTask: false,
			TaskName: "Raw Activity Log",
			Summary:  truncateRunes(strings.TrimSpace(fallbackText.String()), fallbackSummaryMaxRunes),
		}
		if err := c.store.LogSemanticNode(ctx, fallbackSummary); err != nil {
			slog.Error("flush: LogSemanticNode (fallback) failed", "err", err)
		}
	} else {
		// one update per concurrent thread: refresh its state and log an episodic summary node so history, FTS, and compaction keep working unchanged.
		for _, u := range attr.Threads {
			if _, err := c.store.UpsertThread(ctx, u); err != nil {
				slog.Error("flush: UpsertThread failed", "subject", u.Subject, "err", err)
			}
			if err := c.store.LogSemanticNode(ctx, TaskSummary{SameTask: u.ID != 0, TaskName: u.Subject, Summary: u.Summary}); err != nil {
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
				return c.llm.ReconcileNotes(ctx, existing, attr.Identity)
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

func (c *Compiler) BufferSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buffer)
}

func (c *Compiler) ForceFlush(ctx context.Context) {
	c.mu.Lock()
	if len(c.buffer) == 0 {
		c.mu.Unlock()
		return
	}
	buf := c.resetBufferLocked()
	c.mu.Unlock()

	c.processFlush(ctx, buf)
}

// GetCurrentBuffer returns a copy so callers (the /buffer HTTP handler, Agent.Connect) never read a slice that Ingest/flush might be mutating concurrently.
func (c *Compiler) GetCurrentBuffer() []tracker.Activity {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := make([]tracker.Activity, len(c.buffer))
	copy(buf, c.buffer)
	return buf
}
