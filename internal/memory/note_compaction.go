package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"ora/internal/config"
	"strings"

	"google.golang.org/genai"
)

// minNotesToConsolidate is the floor below which the notes table is small enough that periodic consolidation isn't worth an LLM call.
const minNotesToConsolidate = 20

// NoteConsolidator collapses the full notes set into a deduplicated, durable-only canonical list.
// It's the curation counterpart to per-flush ReconcileNotes: reconciliation guards the front door, consolidation cleans the whole room.
type NoteConsolidator interface {
	ConsolidateNotes(ctx context.Context, notes []string) ([]string, error)
}

// NoteStore is the persistence side of note consolidation. db.Store implements it.
// Both methods are scoped to notes of kind "fact": ExistingNotes returns only facts and ReplaceAllNotes replaces only facts. Consolidation asks a model to drop everything that is not a durable fact about the user, so notes of other kinds — meeting minutes, which are the only record of what was said in a call — must never be shown to it or rewritten by it.
type NoteStore interface {
	ExistingNotes(ctx context.Context) ([]NoteRef, error)
	ReplaceAllNotes(ctx context.Context, contents []string) error
}

// NoteCompactor periodically rewrites the notes table into a smaller canonical set, merging near-duplicates and dropping ephemeral task config that leaked in as "facts."
// This is the defence against notes-layer context rot — per-flush reconciliation under-merges over time, so the whole set gets curated.
type NoteCompactor struct {
	llm   NoteConsolidator
	store NoteStore
}

func NewNoteCompactor(llm NoteConsolidator, store NoteStore) *NoteCompactor {
	return &NoteCompactor{llm: llm, store: store}
}

// Compact loads every note, asks the model for the canonical set, and rewrites the table — but only when doing so is safe and useful. Guards:
//   - below minNotesToConsolidate: skip (table is small).
//   - empty model result: skip (never wipe the table on a bad response).
//   - no reduction in count: skip (not worth churning rows).
func (nc *NoteCompactor) Compact(ctx context.Context) error {
	existing, err := nc.store.ExistingNotes(ctx)
	if err != nil {
		return fmt.Errorf("note consolidation: load notes: %w", err)
	}
	if len(existing) < minNotesToConsolidate {
		return nil
	}

	contents := make([]string, len(existing))
	for i, n := range existing {
		contents[i] = n.Content
	}

	merged, err := nc.llm.ConsolidateNotes(ctx, contents)
	if err != nil {
		return fmt.Errorf("note consolidation: llm call: %w", err)
	}

	// Safety: never wipe the table on an empty/garbage response, and don't churn rows when the model failed to actually reduce the set.
	if len(merged) == 0 || len(merged) >= len(existing) {
		return nil
	}

	return nc.store.ReplaceAllNotes(ctx, merged)
}

// ConsolidateNotes implements NoteConsolidator on GeminiSummarizer.
func (g *GeminiSummarizer) ConsolidateNotes(ctx context.Context, notes []string) ([]string, error) {
	if len(notes) == 0 {
		return nil, nil
	}

	// The genai client has no HTTP timeout of its own, so this call gets one here.
	ctx, cancel := context.WithTimeout(ctx, summarizerCallTimeout)
	defer cancel()

	numbered := make([]string, len(notes))
	for i, n := range notes {
		numbered[i] = fmt.Sprintf("%d. %s", i+1, n)
	}

	prompt := fmt.Sprintf(`You are the curator of an OS companion's long-term memory of the USER.
Below is the current stored list of "facts" about the user. It has accumulated
redundancy, and some entries are not durable facts at all — they are transient task
details (specific commands, CLI flags, file paths, one-off debugging settings,
today's bug) that should never have been stored as lasting facts.

Produce the CLEAN canonical set:
- MERGE near-duplicates and overlapping entries into one well-worded fact.
- DROP entries that are task-specific or ephemeral config rather than a durable
  trait, preference, identity, project, relationship, belief, or skill of the user.
  Example: drop "avoid --kv-cache-dtype fp8" and "set max-model-len to 40960";
  keep "user runs a local vLLM inference backend".
- KEEP stable identity, preferences, habits, beliefs, skills, and ongoing projects.
- When unsure whether something is durable, keep it but generalize the wording.
- Do not invent facts.

Return ONLY a JSON array of strings — the final canonical fact set, no commentary.

Current facts:
%s`, strings.Join(numbered, "\n"))

	model := config.BackgroundModel(config.JobNoteConsolidation)
	var resp *genai.GenerateContentResponse
	err := g.allow(model)
	if err == nil {
		resp, err = g.client.Models.GenerateContent(ctx, model, genai.Text(prompt), &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		})
	}
	if err != nil {
		return nil, fmt.Errorf("consolidate notes llm call: %w", err)
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("consolidate notes: empty response from model")
	}

	var out []string
	if err := json.Unmarshal([]byte(resp.Candidates[0].Content.Parts[0].Text), &out); err != nil {
		return nil, fmt.Errorf("parse consolidated notes: %w", err)
	}
	return out, nil
}
