package memory

import (
	"context"
	"fmt"
	"ora/internal/config"
	"ora/internal/obs"
	"strings"

	"google.golang.org/genai"
)

// StateDeriver synthesizes a short present-tense working-state summary from
// recent episodic summaries and stable notes. The result is stored as a
// single-row cache (working_state) and injected into GetImplicitContext,
// replacing the raw summary dump.
type StateDeriver interface {
	DeriveState(ctx context.Context, recentSummaries []string, notes []string) (string, error)
}

// DeriveState implements StateDeriver on GeminiSummarizer.
// It returns a plain-text, present-tense synthesis (<= ~120 words) of the
// user's active project, current focus, recent activity, and open loops.
// Returns "",nil immediately when both inputs are empty — no API call is made.
func (g *GeminiSummarizer) DeriveState(ctx context.Context, recentSummaries []string, notes []string) (string, error) {
	if len(recentSummaries) == 0 && len(notes) == 0 {
		return "", nil
	}

	tracer := obs.GetTracer(ctx, "ora.memory")
	ctx, span := tracer.Start(ctx, "GeminiSummarizer.DeriveState")
	defer span.End()

	var parts []string
	if len(notes) > 0 {
		parts = append(parts, "Stable facts about the user:\n"+strings.Join(notes, "\n"))
	}
	if len(recentSummaries) > 0 {
		numbered := make([]string, len(recentSummaries))
		for i, s := range recentSummaries {
			numbered[i] = fmt.Sprintf("%d. %s", i+1, s)
		}
		parts = append(parts, "Recent activity summaries (newest first):\n"+strings.Join(numbered, "\n"))
	}

	prompt := fmt.Sprintf(`You are the working-memory module for an OS companion agent.
Given the stable user facts and recent activity summaries below, write a SHORT
present-tense paragraph (<= 120 words) describing the user's current working state:
active project, current focus, recent activity, and open loops.
Plain text only — no JSON, no bullet points, no headings. If there is not enough
information to infer a meaningful state, respond with a single sentence.

%s`, strings.Join(parts, "\n\n"))

	_, genSpan := tracer.Start(ctx, "Gemini.GenerateContent.DeriveState")
	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, genai.Text(prompt), nil)
	if err != nil {
		genSpan.RecordError(err)
		genSpan.End()
		return "", fmt.Errorf("derive state llm call: %w", err)
	}
	genSpan.End()

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("derive state: empty response from model")
	}

	return strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text), nil
}
