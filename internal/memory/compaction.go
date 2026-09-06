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

// Digester merges a slice of summary texts into one coarse prose digest. prior is the digest already written for that day, or "" when there is none: it is shown to the model so a day digested a second time comes back with one paragraph covering both the old material and the new, rather than a paragraph about the new batch alone that then overwrites the old one.
type Digester interface {
	Digest(ctx context.Context, prior string, summaries []string) (string, error)
}

// CompactStore is the persistence side of episodic compaction.
// db.Store implements this interface.
type CompactStore interface {
	OldSummaryGroups(ctx context.Context, olderThan time.Duration) ([]SummaryGroup, error)
	ExistingDigest(ctx context.Context, dayID int64) (string, error)
	ReplaceSummariesWithDigest(ctx context.Context, dayID int64, summaryIDs []int64, digest string) error
}

// digestBatchSize is how many of a day's summaries go into one Digest prompt. A day can hold hundreds of summaries, and all of them in one prompt runs past the model's input limit — the call then fails and the day is never digested at all. Each batch after the first is given the digest the previous batch produced, so the day still ends with one digest covering all of them.
const digestBatchSize = 50

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

		// The day's own digest, when it has one, is carried into the prompt so the call merges into what is already recorded for that day instead of replacing it with a digest of this batch alone.
		prior, err := c.store.ExistingDigest(ctx, g.DayID)
		if err != nil {
			slog.Warn("compaction: could not read the day's existing digest, merging without it",
				"day", g.Day, "day_id", g.DayID, "error", err)
			prior = ""
		}

		failed := false
		for start := 0; start < len(g.Summaries) && !failed; start += digestBatchSize {
			batch := g.Summaries[start:min(start+digestBatchSize, len(g.Summaries))]

			contents := make([]string, len(batch))
			ids := make([]int64, len(batch))
			for i, s := range batch {
				contents[i] = s.Content
				ids[i] = s.ID
			}

			digest, err := c.llm.Digest(ctx, prior, contents)
			if err != nil {
				slog.Warn("compaction: digest call failed, skipping the rest of the day",
					"day", g.Day, "day_id", g.DayID, "error", err)
				failed = true
				break
			}

			if err := c.store.ReplaceSummariesWithDigest(ctx, g.DayID, ids, digest); err != nil {
				slog.Warn("compaction: replace failed, skipping the rest of the day",
					"day", g.Day, "day_id", g.DayID, "error", err)
				failed = true
				break
			}
			prior = digest
		}
		if failed {
			continue
		}

		slog.Info("compaction: compacted day", "day", g.Day, "summaries_merged", len(g.Summaries))
	}

	return nil
}

// Digest implements Digester on GeminiSummarizer, merging the summary texts into one coarse prose description of what the user did that day.
// Plain text out, no JSON needed.
func (g *GeminiSummarizer) Digest(ctx context.Context, prior string, summaries []string) (string, error) {
	if len(summaries) == 0 {
		return "", nil
	}

	// The genai client has no HTTP timeout of its own, so this call gets one here.
	ctx, cancel := context.WithTimeout(ctx, summarizerCallTimeout)
	defer cancel()

	numbered := make([]string, len(summaries))
	for i, s := range summaries {
		numbered[i] = fmt.Sprintf("%d. %s", i+1, s)
	}

	priorBlock := ""
	if strings.TrimSpace(prior) != "" {
		priorBlock = fmt.Sprintf("\nWhat is already recorded for this day (keep what it says and fold the summaries below into it):\n%s\n", prior)
	}

	prompt := fmt.Sprintf(`You are a memory compaction engine for an OS companion.
Below are several task summaries from the same day. Merge them into ONE concise prose
paragraph (3-6 sentences) describing what the user accomplished that day. Preserve
important technical details (project names, tools, outcomes). Remove redundancy.
Do not invent facts. Plain text only — no JSON, no bullet points.
%s
Summaries:
%s`, priorBlock, strings.Join(numbered, "\n"))

	// The daily request gate is asked before the call so a spent Gemini allowance refuses here and falls through to the fallback text path like a real 429 would.
	model := config.BackgroundModel(config.JobEpisodicCompaction)
	var resp *genai.GenerateContentResponse
	err := g.allow(model)
	if err == nil {
		resp, err = g.client.Models.GenerateContent(ctx, model, genai.Text(prompt), nil)
	}
	if err != nil {
		if text, ok := g.fallbackText(ctx, err, prompt); ok {
			return text, nil
		}
		return "", fmt.Errorf("digest llm call: %w", err)
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("digest: empty response from model")
	}

	return strings.TrimSpace(resp.Candidates[0].Content.Parts[0].Text), nil
}
