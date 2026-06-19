package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"ora/internal/config"
	"ora/internal/obs"
	"ora/internal/tracker"
	"strings"
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

type Summarizer interface {
	Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*TaskSummary, error)
	// ReconcileNotes decides, for each candidate fact, whether to add, update
	// (refines/supersedes an existing note), or skip (already-known duplicate).
	// Returns nil,nil immediately when candidates is empty.
	ReconcileNotes(ctx context.Context, existing []NoteRef, candidates []string) ([]NoteOp, error)
}

// GeminiSummarizer is for the genai sdk, will have other summarizers for provided model support
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

func (g *GeminiSummarizer) Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*TaskSummary, error) {
	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.Summarize")
	defer span.End()

	var activityList []string
	for _, a := range activities {
		entry := fmt.Sprintf("- %s: %s", a.App, a.Title)
		if a.ScreenText != "" {
			entry += fmt.Sprintf("\n  screen: %s", a.ScreenText)
		}
		activityList = append(activityList, entry)
	}

	prompt := fmt.Sprintf(`You are the memory compiler for an ambient OS companion.
Given the recent window activities below, produce a JSON object with four fields.

1. same_task (bool): true if the user is still on the same task as "%s".

2. task_name (string): a short label for what the user was doing.
   For work: the feature/bug/document name.
   For social/gaming/entertainment: the activity ("Playing Blackjack on Discord",
   "Video call with team", "Watching YouTube — cooking video").
   For browsing: the topic or site ("Reading about Rust async runtimes on MDN").

3. summary (string): a factual, specific sentence or two capturing WHAT HAPPENED.
   This is episodic memory — describe the actual activity, not just the app.
   Be specific: name the game, topic, people (if visible), or meeting context.
   Even if screen text is sparse, use the app + window title as the primary signal.
   Examples of good summaries:
     "User spent ~45 min on Discord in a voice channel with friends; game sounds
      and a Blackjack interface were visible on screen."
     "User was in a Zoom meeting titled 'Sprint Planning'; screen showed a shared
      Jira board."
     "User read an MDN article on Rust async runtimes and then browsed Hacker News."
   This field answers future questions like "what did I do last night?" or
   "was I in a meeting on Tuesday?"

4. notes (array of strings): ONLY durable facts about the PERSON — identity,
   lasting preferences, skills, relationships, ongoing projects.
   ("user prefers terse replies", "user is building the ORA companion").
   DO NOT store task-specific or session detail: commands, file paths, version
   numbers, what they did today. That belongs in summary, not notes.
   When in doubt, omit. Empty array is correct most of the time.

Current task: "%s"
Recent activities:
%s

Respond strictly in JSON:
{"same_task": bool, "task_name": "string", "summary": "string", "notes": []}`,
		currentTask, currentTask, strings.Join(activityList, "\n"))

	_, genSpan := tracer.Start(ctx, "Gemini.GenerateContent")
	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, genai.Text(prompt), &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
	})
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		return nil, err
	}
	genSpan.End()

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response from model")
	}

	var summary TaskSummary
	err = json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &summary)
	if err != nil {
		return nil, fmt.Errorf("failed to parse model response: %w", err)
	}

	return &summary, nil
}

// ReconcileNotes calls TextModel with structured JSON output to decide, for each
// candidate, whether to add, update, or skip relative to existing notes.
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

// DescribeScreen sends a screenshot to the multimodal model and returns a concise
// description for memory. Used as the vision tier when accessibility text is blind
// (browsers, video, games, canvas apps). Returns "" on any failure so the caller
// can fall back to whatever thin text it already has.
func (g *GeminiSummarizer) DescribeScreen(ctx context.Context, png []byte) string {
	if len(png) == 0 {
		return ""
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.DescribeScreen")
	defer span.End()

	const prompt = `You are the eyes of an ambient OS companion. Describe what is on this screen so the agent can remember it later. State the application, what the user is doing, and any meaningful visible content — terminal errors and their cause, the article or page being read, the video or game on screen. Be factual and concise, present tense, no preamble. Max 100 words.`

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
		genai.NewPartFromBytes(png, "image/png"),
	}
	contents := []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}

	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, contents, nil)
	if err != nil {
		span.RecordError(err)
		return ""
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return ""
	}
	return strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text)
}

type Storage interface {
	LogSemanticNode(ctx context.Context, summary TaskSummary) error
	LogNote(ctx context.Context, content, kind string) (int64, error)
	ExistingNotes(ctx context.Context) ([]NoteRef, error)
	UpdateNote(ctx context.Context, id int64, content string) error
}

const wordFlushLimit = 1500

// trivialTitles is the minimal set of placeholder window titles that carry no signal.
var trivialTitles = map[string]struct{}{
	"new tab":  {},
	"untitled": {},
	"desktop":  {},
}

// IsSalient returns false for activities that are obviously noise and should
// never enter the buffer. Conservative by design — only drop what is clearly
// meaningless, so real work is never silently discarded.
func IsSalient(act tracker.Activity) bool {
	title := strings.TrimSpace(act.Title)
	screenWords := countWords(act.ScreenText)

	if title == "" && screenWords == 0 {
		return false
	}

	if _, trivial := trivialTitles[strings.ToLower(title)]; trivial && screenWords == 0 {
		return false
	}

	return true
}

type Compiler struct {
	llm       Summarizer
	store     Storage
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

	incoming := countWords(act.ScreenText)

	if len(c.buffer) > 0 {
		last := c.buffer[len(c.buffer)-1]
		appChanged := last.App != act.App
		wordLimitHit := c.wordCount+incoming >= wordFlushLimit
		hourElapsed := len(c.buffer) > 0 && time.Since(c.lastFlush) >= time.Hour

		if appChanged || wordLimitHit || hourElapsed {
			c.flush(ctx)
		}
	}

	c.wordCount += incoming
	c.buffer = append(c.buffer, act)
}

func countWords(s string) int {
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

func (c *Compiler) flush(ctx context.Context) {
	if len(c.buffer) == 0 {
		return
	}

	// Drop only if every buffered activity has no title and no screen text —
	// i.e. the buffer carries zero signal. IsSalient already strips noise at
	// ingestion; this is the last-resort guard for pathological empty flushes.
	// Word-count minimums are intentionally gone: social/gaming/meeting
	// sessions produce real titles but sparse screen text, and word count is
	// a poor proxy for "worth remembering."
	hasSignal := false
	for _, act := range c.buffer {
		if strings.TrimSpace(act.Title) != "" || act.ScreenText != "" {
			hasSignal = true
			break
		}
	}
	if !hasSignal {
		c.buffer = make([]tracker.Activity, 0)
		c.wordCount = 0
		c.lastFlush = time.Now()
		return
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "Compiler.FlushBuffer")
	defer span.End()

	summary, err := c.llm.Summarize(ctx, c.buffer, "")
	if err != nil || summary == nil {
		// log raw activities if LLM fails
		var fallbackText strings.Builder
		for _, act := range c.buffer {
			line := act.App + " | " + act.Title
			if act.ScreenText != "" {
				line += " | " + act.ScreenText
			}
			fallbackText.WriteString(line + "\n")
		}
		fallbackSummary := TaskSummary{
			SameTask: false,
			TaskName: "Raw Activity Log",
			Summary:  strings.TrimSpace(fallbackText.String()),
		}
		_ = c.store.LogSemanticNode(ctx, fallbackSummary)
	} else {
		_ = c.store.LogSemanticNode(ctx, *summary)
		if len(summary.Notes) > 0 {
			existing, exErr := c.store.ExistingNotes(ctx)
			ops, recErr := func() ([]NoteOp, error) {
				if exErr != nil {
					return nil, exErr
				}
				return c.llm.ReconcileNotes(ctx, existing, summary.Notes)
			}()

			if recErr != nil {
				for _, n := range summary.Notes {
					_, _ = c.store.LogNote(ctx, n, "fact")
				}
			} else {
				for _, op := range ops {
					switch op.Action {
					case "add":
						_, _ = c.store.LogNote(ctx, op.Content, "fact")
					case "update":
						_ = c.store.UpdateNote(ctx, op.ID, op.Content)
					}
				}
			}
		}
	}

	c.buffer = make([]tracker.Activity, 0)
	c.wordCount = 0
	c.lastFlush = time.Now()
}

func (c *Compiler) BufferSize() int {
	return len(c.buffer)
}

func (c *Compiler) ForceFlush(ctx context.Context) {
	c.flush(ctx)
}

func (c *Compiler) GetCurrentBuffer() []tracker.Activity {
	return c.buffer
}
