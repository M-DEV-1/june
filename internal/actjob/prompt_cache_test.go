package actjob

import (
	"strings"
	"testing"

	"june/internal/act"
)

// A provider's prompt cache matches a prefix, so a forty-round job pays full price on every round unless the front of its prompt is the same bytes each time. Everything a round changes — where the job has got to, the screen, the tool results, the budget — has to sit behind everything it does not: the instruction, the goal, the window, the user's answers and the plan. This walks a job through the rounds it actually takes and checks the head of the prompt never moves.
func TestBuildPrompt_TheHeadIsTheSameOnEveryRound(t *testing.T) {
	job := Job{
		Goal:    "play the eighth episode",
		Window:  "Brave · Netflix",
		Plan:    "open the season list, scroll to episode eight, play it",
		Budget:  Budget{Steps: 40, InputTokens: 200000},
		Answers: []string{"the one from season 16"},
	}
	head := func(j Job) string {
		p := BuildPrompt(j)
		i := strings.Index(p, "\n\nWHERE YOU HAVE GOT TO\n")
		if i < 0 {
			t.Fatalf("the prompt no longer carries the progress heading the changing part starts at:\n%s", p)
		}
		return p[:i]
	}
	want := head(job)
	if !strings.HasPrefix(want, systemPrompt) {
		t.Error("the prompt must open on the fixed instruction")
	}
	if !strings.Contains(want, job.Plan) {
		t.Error("the plan is written once and left alone, so it belongs in the head")
	}
	if !strings.Contains(want, "the one from season 16") {
		t.Error("the user's answers belong in the head")
	}
	for round := 1; round <= 5; round++ {
		job.Steps = append(job.Steps, Step{N: round, Tool: "click", Expect: act.Check{Kind: act.TitleContains, Value: "S16"}, Outcome: "pass", Why: "the title changed"})
		job.Observations = pushCapped(job.Observations, "brave · a page\n[1] link \"Episode 8\"", keptObservations, observationCap)
		job.Results = pushCapped(job.Results, "click: clicked [1]", keptResults, resultCap)
		job.Summary = "got as far as the season list"
		job.Next = "click episode eight"
		job.Spend.Input += 3000
		if got := head(job); got != want {
			t.Fatalf("round %d changed the head of the prompt:\n%s\n\n%s", round, got, want)
		}
	}
}

// An answer from the user arrives mid-job and is the one thing in the head that can still grow, so it goes last in the head: behind the plan rather than in front of it, or a single answer would push the plan to a new offset and throw away the cached prefix that held it.
func TestBuildPrompt_ANewAnswerLeavesThePlanWhereItWas(t *testing.T) {
	job := Job{
		Goal:   "play the eighth episode",
		Plan:   "open the season list, scroll to episode eight, play it",
		Budget: Budget{Steps: 40, InputTokens: 200000},
	}
	before := BuildPrompt(job)
	job.Answers = append(job.Answers, "the one from season 16")
	after := BuildPrompt(job)

	planAt := strings.Index(before, job.Plan)
	if planAt < 0 || strings.Index(after, job.Plan) < 0 {
		t.Fatal("the plan is missing from one of the prompts")
	}
	if before[:planAt] != after[:planAt] {
		t.Error("an answer changed the prompt ahead of the plan; the answers must come after it")
	}
	if !strings.Contains(after, "the one from season 16") {
		t.Error("the answer must still reach the model")
	}
}
