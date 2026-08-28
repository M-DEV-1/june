package memory

import (
	"context"
	"fmt"
	"ora/internal/config"
	"ora/internal/obs"
	"strings"

	"google.golang.org/genai"
)

// DeriveState synthesizes a short present-tense working-state summary (<= ~120 words, addressed to the user as "you", in their own words for their own work) from recent episodic summaries and stable notes.
// The result is stored as a single-row cache (working_state) and injected into GetImplicitContext in place of the raw summary dump.
// Returns "", nil immediately when both inputs are empty — no API call made.
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

	prompt := fmt.Sprintf(`You are the working-memory module for an OS companion.
From what's below, write a SHORT present-tense paragraph (<= 120 words) addressed to the user as "you", in the words they would use for their own work. Name the thing, not the file or app it lives in. Say what they decided and what they're still deciding.
Plain text only — no JSON, no bullets, no headings, one sentence if there's little to go on.

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
