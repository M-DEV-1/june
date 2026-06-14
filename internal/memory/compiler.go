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
		   notes rules: durable only ("user prefers terse replies", "user is debugging the React PR").
		   skip ephemeral state ("user is typing in VSCode") — that's already in the summary.
		   skip activity logs ("user opened browser"). empty array if nothing note-worthy.

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

type Storage interface {
	LogSemanticNode(ctx context.Context, summary TaskSummary) error
	LogNote(ctx context.Context, content, kind string) (int64, error)
}

const wordFlushLimit = 1500

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
			if _, err := c.store.LogNote(ctx, n, "fact"); err != nil {
				// best-effort
				_ = err
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
