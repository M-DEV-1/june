package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"ora/internal/config"
	"ora/internal/obs"
	"ora/internal/tracker"
	"strings"

	"google.golang.org/genai"
)

type TaskSummary struct {
	SameTask bool   `json:"same_task"`
	TaskName string `json:"task_name"`
	Summary  string `json:"summary"`
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
		activityList = append(activityList, fmt.Sprintf("- %s: %s", a.App, a.Title))
	}

	// temp prompt
	prompt := fmt.Sprintf(`
	You are the memory compiler for an OS agent. 
		Given these recent window activities and the current task name, determine:
		1. Is the user still on the same task?
		2. What is a one-short  semantic summary of what they did?
		3. What is a concise name for this task?

		Current task: "%s"
		Recent activities:
		%s

	Respond strictly in JSON format:
	{
		"same_task": true/false,
		"task_name": "string",
		"summary": "string"
	s}`,
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
}

type Compiler struct {
	llm    Summarizer
	store  Storage
	buffer []tracker.Activity
}

func NewCompiler(llm Summarizer, store Storage) *Compiler {
	return &Compiler{
		llm:    llm,
		store:  store,
		buffer: make([]tracker.Activity, 0),
	}
}

func (c *Compiler) Ingest(ctx context.Context, act tracker.Activity) {
	if len(c.buffer) > 0 {
		last := c.buffer[len(c.buffer)-1]
		if last.App != act.App || len(c.buffer) >= 10 {
			c.flush(ctx)
		}
	}
	c.buffer = append(c.buffer, act)
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
			fallbackText.WriteString(act.App + " | " + act.Title + "\n")
		}
		fallbackSummary := TaskSummary{
			SameTask: false,
			TaskName: "Raw Activity Log",
			Summary:  strings.TrimSpace(fallbackText.String()),
		}
		_ = c.store.LogSemanticNode(ctx, fallbackSummary)
	} else {
		_ = c.store.LogSemanticNode(ctx, *summary)
	}

	c.buffer = make([]tracker.Activity, 0)
}

func (c *Compiler) BufferSize() int {
	return len(c.buffer)
}

func (c *Compiler) GetCurrentBuffer() []tracker.Activity {
	return c.buffer
}
