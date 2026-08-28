package memory

// The prompts here are the only thing standing between the model and list-shaped memory: a comma-chain of everything the user touched, each item named after the app it happened in. These tests pin the rules that forbid that, and pin the word budget so the rules stay compressed in rather than piled on.

import (
	"strings"
	"testing"
)

// stateInstructionBudget and attributionRulesBudget are the word counts these prompts had before the voice rules went in. A prompt that grows past its budget is a rule that was appended instead of compressed in.
const (
	stateInstructionBudget = 74
	attributionRulesBudget = 152
)

func TestStateInstruction_ForbidsListShapedProse(t *testing.T) {
	lower := strings.ToLower(stateInstruction)
	for _, want := range []string{"one thing leads", "two topics", "comma-chain", "tool or pipeline", "not the file or app"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the state prompt has no %q rule:\n%s", want, stateInstruction)
		}
	}
	if n := len(strings.Fields(stateInstruction)); n > stateInstructionBudget {
		t.Errorf("the state prompt is %d words, over its %d-word budget", n, stateInstructionBudget)
	}
}

func TestAttributionRules_NameTheThingNotTheApp(t *testing.T) {
	lower := strings.ToLower(attributionRules)
	// The summary is the per-flush line that ends up in memory ("Browsing movie information for Creed on Amazon Prime Video"), so it needs its own rule; state gets the same voice.
	if !strings.Contains(lower, `"summary"`) {
		t.Errorf("the attribution prompt says nothing about what \"summary\" should read like:\n%s", attributionRules)
	}
	for _, want := range []string{"never the app or site it happened in", "name the thing, not the app", "no counts or times"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the attribution prompt has no %q rule:\n%s", want, attributionRules)
		}
	}
	if n := len(strings.Fields(attributionRules)); n > attributionRulesBudget {
		t.Errorf("the attribution rules are %d words, over their %d-word budget", n, attributionRulesBudget)
	}
}
