package memory

import (
	"context"
	"fmt"
	"log/slog"
	"ora/internal/config"
	"strings"
	"time"

	"google.golang.org/genai"
)

// NodeRef is a lightweight handle to a node row (id + content).
// Defined here to avoid import cycles: db imports memory, not the other way round.
type NodeRef struct {
	ID      int64
	Content string
}

// SummaryGroup is one day's worth of old summary nodes, returned by CompactStore.OldSummaryGroups and consumed by Compactor.Compact.
type SummaryGroup struct {
	DayID     int64
	Day       string
	Summaries []NodeRef
}

// Digester merges a slice of summary texts into one coarse prose digest.
type Digester interface {
	Digest(ctx context.Context, summaries []string) (string, error)
}

// CompactStore is the persistence side of episodic compaction.
// db.Store implements this interface.
type CompactStore interface {
	OldSummaryGroups(ctx context.Context, olderThan time.Duration) ([]SummaryGroup, error)
	ReplaceSummariesWithDigest(ctx context.Context, dayID int64, summaryIDs []int64, digest string) error
}

// Compactor rolls old fine-grained summaries up into one daily digest per day, mirroring the LSM-tree "compact older levels" pattern.
type Compactor struct {
	llm   Digester
	store CompactStore
}

func NewCompactor(llm Digester, store CompactStore) *Compactor {
	return &Compactor{llm: llm, store: store}
}

// Compact digests and replaces each day's summary group (2+ summaries) with one merged digest node.
// Single-summary days are left alone. A failed group is logged and skipped, not fatal — Compact only returns an error if the initial store lookup fails.
func (c *Compactor) Compact(ctx context.Context, olderThan time.Duration) error {
	groups, err := c.store.OldSummaryGroups(ctx, olderThan)
	if err != nil {
		return fmt.Errorf("compaction: fetch groups: %w", err)
	}

	for _, g := range groups {
		if len(g.Summaries) < 2 {
			// a single summary has nothing to merge with; skip until more accumulate.
			continue
		}

		contents := make([]string, len(g.Summaries))
		for i, s := range g.Summaries {
			contents[i] = s.Content
		}

		digest, err := c.llm.Digest(ctx, contents)
		if err != nil {
			slog.Warn("compaction: digest call failed, skipping day",
				"day", g.Day, "day_id", g.DayID, "error", err)
			continue
		}

		ids := make([]int64, len(g.Summaries))
		for i, s := range g.Summaries {
			ids[i] = s.ID
		}

		if err := c.store.ReplaceSummariesWithDigest(ctx, g.DayID, ids, digest); err != nil {
			slog.Warn("compaction: replace failed, skipping day",
				"day", g.Day, "day_id", g.DayID, "error", err)
			continue
		}

		slog.Info("compaction: compacted day", "day", g.Day, "summaries_merged", len(g.Summaries))
	}

	return nil
}

// Digest implements Digester on GeminiSummarizer, merging the summary texts into one coarse prose description of what the user did that day.
// Plain text out, no JSON needed.
func (g *GeminiSummarizer) Digest(ctx context.Context, summaries []string) (string, error) {
	if len(summaries) == 0 {
		return "", nil
	}

	numbered := make([]string, len(summaries))
	for i, s := range summaries {
		numbered[i] = fmt.Sprintf("%d. %s", i+1, s)
	}

	prompt := fmt.Sprintf(`You are a memory compaction engine for an OS companion.
Below are several task summaries from the same day. Merge them into ONE concise prose
paragraph (3-6 sentences) describing what the user accomplished that day. Preserve
important technical details (project names, tools, outcomes). Remove redundancy.
Do not invent facts. Plain text only — no JSON, no bullet points.

Summaries:
%s`, strings.Join(numbered, "\n"))

	resp, err := g.client.Models.GenerateContent(ctx, config.TextModel, genai.Text(prompt), nil)
	if err != nil {
		return "", fmt.Errorf("digest llm call: %w", err)
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("digest: empty response from model")
	}

	return strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text), nil
}
