package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"ora/internal/config"
	"strings"
)

// ConsolidateNotes implements the NoteConsolidator interface internal/db's NoteCompactor asks for, on GeminiSummarizer.
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

	text, err := g.text(ctx, config.JobNoteConsolidation, prompt, true)
	if err != nil {
		return nil, fmt.Errorf("consolidate notes: %w", err)
	}

	var out []string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("parse consolidated notes: %w", err)
	}
	return out, nil
}
