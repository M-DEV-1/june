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

type Summarizer interface {
	Summarize(ctx context.Context, activities []tracker.Activity, currentTask string) (*TaskSummary, error)
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

	// temp prompt
	prompt := fmt.Sprintf(`
	You are the memory compiler for an OS agent.
		Given these recent window activities and the current task name, determine:
		1. Is the user still on the same task?
		2. What is a one-shot semantic summary of what they did?
		3. What is a concise name for this task?
		4. Any stable facts about the USER worth remembering across sessions?
		   notes rules — store ONLY durable traits of the person: identity, lasting
		   preferences, habits, beliefs, skills, relationships, and ongoing projects
		   ("user prefers terse replies", "user is building the ORA companion").
		   DO NOT store task-specific or transient detail: specific commands, CLI
		   flags, file paths, version numbers, one-off debugging settings, or what
		   they did today ("avoid --kv-cache-dtype fp8", "set max-model-len 40960",
		   "user is typing in VSCode") — that belongs in the summary, not notes.
		   When in doubt, leave it out. Empty array if nothing durable.

		Current task: "%s"
		Recent activities:
		%s

	Respond strictly in JSON format:
	{
		"same_task": true/false,
		"task_name": "string",
		"summary": "string",
		"notes": []
	}`,
		currentTask, strings.Join(activityList, "\n"))

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
}

const wordFlushLimit = 1500
const minFlushWords = 25

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

	totalWords := c.wordCount
	for _, act := range c.buffer {
		totalWords += countWords(act.Title)
	}
	if totalWords < minFlushWords {
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
		for _, n := range summary.Notes {
			_, _ = c.store.LogNote(ctx, n, "fact")
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
