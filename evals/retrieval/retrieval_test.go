// retrieval stage: does a natural-language question actually find the fact back. ORA's real failure mode is last-mile retrieval, not capture, so this is the stage that matters most.
// no direct internal/db calls — seeds a real file-backed sqlite store (evals/harness) behind a real *agent.Agent, then calls Agent.ExecuteTool for "query_memory" and "recall" exactly like the live model does.
package retrieval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ora/evals/dataset"
	"ora/evals/harness"
	"ora/internal/agent"
	"ora/internal/memory"
)

// noiseNotes are plausible distractor notes a real user's ORA would have alongside the facts under test — retrieval evals are meaningless without something to filter out.
var noiseNotes = []string{
	"User's favorite editor is VSCode with vim keybindings.",
	"The project uses Go 1.25 and modernc.org/sqlite for the embedded database.",
	"User is working on a personal finance tracker app on the side.",
	"Weekly grocery list includes oat milk, eggs, and sourdough bread.",
	"User's dentist appointment is scheduled for next Thursday.",
	"The living room lamp needs a new bulb.",
	"User watched three episodes of a sci-fi show last night.",
	"Team standup happens every day at 10am.",
	"User's car needs an oil change soon.",
	"The office wifi password was recently changed.",
	"User is reading a book about distributed systems.",
	"Reminder to renew the domain name registration in August.",
	"User prefers dark mode in all applications.",
	"The gym membership renews on the first of every month.",
}

// noiseThreads are plausible distractor throughlines.
// seeded the same way as the thread-kind facts (subject + state), so thread FTS rows aren't the only content in the table.
var noiseThreads = []memory.ThreadUpdate{
	{Subject: "Home renovation", Kind: "topic", State: "picking paint colors for the kitchen"},
	{Subject: "Job search", Kind: "topic", State: "applied to three companies this week"},
	{Subject: "Marathon training", Kind: "topic", State: "up to 10k long runs, race in October"},
}

// seedAgent opens a real file-backed sqlite store, seeds it with every fact from dataset.Load() plus noise, and wraps it in a real *agent.Agent.
// mic/speaker/compiler are nil — "query_memory" and "recall" never touch them, only the store.
func seedAgent(t *testing.T) (*agent.Agent, []dataset.Fact) {
	t.Helper()
	ctx := context.Background()
	store := harness.NewStore(t)

	facts, err := dataset.Load()
	if err != nil {
		t.Fatalf("dataset.Load: %v", err)
	}

	for _, f := range facts {
		switch f.Kind {
		case "note":
			if _, err := store.LogNote(ctx, f.Content, "fact"); err != nil {
				t.Fatalf("seed note %s: %v", f.ID, err)
			}
		case "thread":
			subject, state := splitThreadContent(f.Content)
			if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
				Subject: subject,
				Kind:    "topic",
				State:   state,
			}); err != nil {
				t.Fatalf("seed thread %s: %v", f.ID, err)
			}
		default:
			t.Fatalf("fact %s: unknown kind %q", f.ID, f.Kind)
		}
	}

	for _, n := range noiseNotes {
		if _, err := store.LogNote(ctx, n, "fact"); err != nil {
			t.Fatalf("seed noise note: %v", err)
		}
	}
	for _, th := range noiseThreads {
		if _, err := store.UpsertThread(ctx, th); err != nil {
			t.Fatalf("seed noise thread: %v", err)
		}
	}

	a := agent.NewAgent(nil, nil, store, nil, "")
	return a, facts
}

// splitThreadContent splits a thread fact's Content on the " — " separator.
// mirrors how threads_ai builds FTS content as subject || ' — ' || state.
// falls back to using the whole content as subject when the separator isn't there.
func splitThreadContent(content string) (subject, state string) {
	if i := strings.Index(content, " — "); i >= 0 {
		return content[:i], content[i+len(" — "):]
	}
	return content, ""
}

// containsAllFold reports whether haystack contains every needle, case-insensitively.
func containsAllFold(haystack string, needles []string) bool {
	lower := strings.ToLower(haystack)
	for _, n := range needles {
		if !strings.Contains(lower, strings.ToLower(n)) {
			return false
		}
	}
	return true
}

// rankOf returns the 1-based rank of the first line that contains every expected substring, case-insensitively.
// 0 if none does.
func rankOf(results []string, expect []string) int {
	for i, line := range results {
		if containsAllFold(line, expect) {
			return i + 1
		}
	}
	return 0
}

// topK is the rank cutoff used for the recall@K metric below.
const topK = 5

// minRecallAt5 is the minimum fraction of probes that must land in the top-5 query_memory results.
// not 1.0 on purpose — today's retrieval is known-imperfect, this just catches regressions, not perfection.
const minRecallAt5 = 0.55

// TestQueryMemoryTool_Scorecard drives the real "query_memory" tool with every probe query in the dataset.
// scores recall@5 and MRR, an actual number that can regress, not just pass/fail.
func TestQueryMemoryTool_Scorecard(t *testing.T) {
	a, facts := seedAgent(t)

	type probeResult struct {
		factID string
		query  string
		rank   int // 0 = not found anywhere in the returned list
	}
	var results []probeResult

	for _, f := range facts {
		for _, p := range f.Probes {
			out := a.ExecuteTool(context.Background(), "query_memory", map[string]any{"query": p.Query})
			var lines []string
			if out != "no memory matches" {
				lines = strings.Split(out, "\n")
			}
			rank := rankOf(lines, p.ExpectContains)
			results = append(results, probeResult{factID: f.ID, query: p.Query, rank: rank})

			if rank == 0 {
				t.Logf("MISS  fact=%-22s query=%q  expect=%v  got=%q", f.ID, p.Query, p.ExpectContains, out)
			} else {
				t.Logf("rank=%-2d fact=%-22s query=%q", rank, f.ID, p.Query)
			}
		}
	}

	if len(results) == 0 {
		t.Fatal("no probes were evaluated — dataset appears empty")
	}

	var hitsAt5 int
	var reciprocalSum float64
	for _, r := range results {
		if r.rank > 0 && r.rank <= topK {
			hitsAt5++
		}
		if r.rank > 0 {
			reciprocalSum += 1.0 / float64(r.rank)
		}
	}
	recallAt5 := float64(hitsAt5) / float64(len(results))
	mrr := reciprocalSum / float64(len(results))

	scorecard := fmt.Sprintf(
		"\n=== ORA retrieval eval: query_memory tool scorecard ===\nprobes total : %d\nrecall@%d   : %.3f (%d/%d)\nMRR          : %.3f\n=======================================================",
		len(results), topK, recallAt5, hitsAt5, len(results), mrr,
	)
	t.Log(scorecard)
	fmt.Println(scorecard)

	if recallAt5 < minRecallAt5 {
		t.Fatalf("recall@%d %.3f fell below minimum %.3f\n%s", topK, recallAt5, minRecallAt5, scorecard)
	}
}

// TestRecallTool_ThreadSubject drives the real "recall" tool's subject path (RecallSubject under the hood).
// query_memory only calls SearchMemory+RankedEpisodes, so this covers a path it never touches.
func TestRecallTool_ThreadSubject(t *testing.T) {
	a, facts := seedAgent(t)

	var f dataset.Fact
	for _, cand := range facts {
		if cand.ID == "north-star" {
			f = cand
			break
		}
	}
	if f.ID == "" {
		t.Fatal("fixture fact north-star not found in dataset")
	}
	subject, _ := splitThreadContent(f.Content)

	out := a.ExecuteTool(context.Background(), "recall", map[string]any{"subject": subject})
	if out == "no memory of that subject" {
		t.Fatalf("recall tool found nothing for subject %q", subject)
	}

	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[thread]") && containsAllFold(line, []string{"ambient"}) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a [thread] line mentioning ambient, got: %q", out)
	}
}
