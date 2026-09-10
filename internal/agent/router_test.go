package agent

import (
	"ora/internal/config"
	"slices"
	"testing"
	"time"
)

// readyAll makes every provider on the card list usable, so a test says what it is about rather than depending on which CLIs this machine happens to have.
func readyAll(t *testing.T) {
	t.Helper()
	for _, id := range []string{ProviderCodex, ProviderClaude, ProviderAgy} {
		SetProviderReady(id, true)
	}
	t.Cleanup(ResetRouter)
}

// A question that needs the web goes to a provider that has it, even though that provider is ranked last for everything else. This is the whole point: on 2026-09-07 the chain fell to Claude because Claude was alive, and nobody had checked whether the providers before it could search at all.
func TestRoute_PutsAWebCapableProviderFirstForAWebQuestion(t *testing.T) {
	readyAll(t)
	got := Route(Need{Web: true})
	if len(got) == 0 {
		t.Fatal("no provider can answer a web question")
	}
	if got[0] != ProviderClaude {
		t.Errorf("a web question goes to %q first, want claude — the only provider with a web search of its own", got[0])
	}
	// A provider with no web at all must not be offered for a web question: it answers from memory and says it has no web access, which is the failure this replaces.
	if slices.Contains(got, ProviderCodex) {
		t.Errorf("a web question was routed to codex, which has no web search: %v", got)
	}
}

// An ordinary question keeps the order the user asked for: Gemini first because it is cheapest, Claude last because it is their coding workhorse.
func TestRoute_KeepsThePreferredOrderForAnOrdinaryQuestion(t *testing.T) {
	readyAll(t)
	got := Route(Need{})
	want := []string{ProviderGemini, ProviderCodex, ProviderAgy, ProviderClaude}
	if !slices.Equal(got, want) {
		t.Errorf("route = %v, want %v", got, want)
	}
}

// The brain the user picked answers first: on 2026-09-07 Claude was picked in Settings and every question still went to Antigravity, because the router ranked by cost alone and Claude was last. The rank still orders everything behind the pick.
func TestRoute_PutsThePickedProviderFirst(t *testing.T) {
	readyAll(t)
	SetPreferredProvider(config.BrainClaudeCLI)
	got := Route(Need{})
	want := []string{ProviderClaude, ProviderGemini, ProviderCodex, ProviderAgy}
	if !slices.Equal(got, want) {
		t.Errorf("route = %v, want the picked provider first then the rank, got %v", want, got)
	}
	// A pick the router has no card for changes nothing.
	SetPreferredProvider("ollama")
	if got := Route(Need{}); !slices.Equal(got, []string{ProviderGemini, ProviderCodex, ProviderAgy, ProviderClaude}) {
		t.Errorf("route after an unknown pick = %v, want the plain rank", got)
	}
}

// A provider this machine cannot run — a CLI nobody has logged into — is never offered, so no request pays its timeout to find out.
func TestRoute_DropsAProviderThatIsNotReady(t *testing.T) {
	readyAll(t)
	SetProviderReady(ProviderCodex, false)
	if got := Route(Need{}); slices.Contains(got, ProviderCodex) {
		t.Errorf("route = %v, want codex left out when it is not logged in", got)
	}
}

// A provider that just refused with a spent allowance is skipped until its breaker closes, rather than being asked again on every request and failing the same way each time.
func TestRoute_SkipsAProviderWhoseAllowanceJustRanOut(t *testing.T) {
	readyAll(t)
	ProviderFailed(ProviderGemini, breakerWindow)
	got := Route(Need{})
	if slices.Contains(got, ProviderGemini) {
		t.Errorf("route = %v, want gemini skipped while its breaker is open", got)
	}
	if len(got) == 0 || got[0] != ProviderCodex {
		t.Errorf("route = %v, want the next provider to lead", got)
	}
}

// The breaker closes on its own, so a spent allowance that has since reset is used again without a restart.
func TestRoute_UsesAProviderAgainOnceItsBreakerCloses(t *testing.T) {
	readyAll(t)
	ProviderFailed(ProviderGemini, time.Nanosecond)
	time.Sleep(time.Millisecond)
	if got := Route(Need{}); len(got) == 0 || got[0] != ProviderGemini {
		t.Errorf("route = %v, want gemini back at the front once its breaker closed", got)
	}
}

// With every provider either unready or broken the router says so with an empty list, rather than naming one that is going to fail.
func TestRoute_ReturnsNothingWhenNoProviderCanServe(t *testing.T) {
	readyAll(t)
	for _, id := range []string{ProviderGemini, ProviderCodex, ProviderClaude, ProviderAgy} {
		ProviderFailed(id, breakerWindow)
	}
	if got := Route(Need{}); len(got) != 0 {
		t.Errorf("route = %v, want nothing when every provider is spent", got)
	}
}

// hop builds a tool hop by name, for the action-repeat tests below.
func hop(name string) ToolHop { return ToolHop{Name: name} }

// The first provider that answers is the answer, and nothing after it is asked.
func TestAskInOrder_StopsAtTheFirstAnswer(t *testing.T) {
	t.Cleanup(ResetRouter)
	var asked []string
	tr, err := askInOrder([]string{ProviderGemini, ProviderClaude}, func(id string) (TurnTrace, error) {
		asked = append(asked, id)
		return TurnTrace{Answer: "from " + id}, nil
	})
	if err != nil {
		t.Fatalf("askInOrder: %v", err)
	}
	if tr.Answer != "from gemini" {
		t.Errorf("answer = %q, want the first provider's", tr.Answer)
	}
	if len(asked) != 1 {
		t.Errorf("asked %v, want only the first", asked)
	}
}

// A provider whose allowance is spent hands the question on, and is marked so the next question skips it instead of failing the same way again.
func TestAskInOrder_HandsOnFromASpentProviderAndRemembers(t *testing.T) {
	t.Cleanup(ResetRouter)
	readyAll(t)
	var asked []string
	tr, err := askInOrder([]string{ProviderGemini, ProviderClaude}, func(id string) (TurnTrace, error) {
		asked = append(asked, id)
		if id == ProviderGemini {
			return TurnTrace{}, codexHTTPError{Code: 429}
		}
		return TurnTrace{Answer: "from " + id}, nil
	})
	if err != nil {
		t.Fatalf("askInOrder: %v", err)
	}
	if tr.Answer != "from claude" {
		t.Errorf("answer = %q, want the provider it handed on to", tr.Answer)
	}
	if len(asked) != 2 {
		t.Errorf("asked %v, want both", asked)
	}
	if got := Route(Need{}); slices.Contains(got, ProviderGemini) {
		t.Errorf("route = %v, want gemini skipped after it reported a spent allowance", got)
	}
}

// A failure another provider would hit too — a bad request, a broken prompt — stops the walk rather than being retried three more times at three more costs.
func TestAskInOrder_StopsOnAFailureNobodyElseCanFix(t *testing.T) {
	t.Cleanup(ResetRouter)
	var asked []string
	_, err := askInOrder([]string{ProviderGemini, ProviderClaude}, func(id string) (TurnTrace, error) {
		asked = append(asked, id)
		return TurnTrace{}, errNoModelTextForTest
	})
	if err == nil {
		t.Fatal("askInOrder succeeded on a failure nobody can fix")
	}
	if len(asked) != 1 {
		t.Errorf("asked %v, want only the first — the rest would fail the same way", asked)
	}
}

// Once an action has run the question is never handed on: a read like observe_screen can be repeated on another provider and change nothing, where a click or a keystroke would happen twice.
func TestAskInOrder_NeverRepeatsAnActionOnAnotherProvider(t *testing.T) {
	t.Cleanup(ResetRouter)
	var asked []string
	_, err := askInOrder([]string{ProviderGemini, ProviderClaude}, func(id string) (TurnTrace, error) {
		asked = append(asked, id)
		return TurnTrace{ToolHops: []ToolHop{hop("click_at")}}, codexHTTPError{Code: 429}
	})
	if err == nil {
		t.Fatal("askInOrder succeeded where it should have stopped")
	}
	if len(asked) != 1 {
		t.Errorf("asked %v, want the walk to stop once a click had already happened", asked)
	}
}

// A read that has already run may be repeated, so a turn that only looked at the screen still hands on.
func TestAskInOrder_RepeatsAReadOnAnotherProvider(t *testing.T) {
	t.Cleanup(ResetRouter)
	var asked []string
	_, err := askInOrder([]string{ProviderGemini, ProviderClaude}, func(id string) (TurnTrace, error) {
		asked = append(asked, id)
		if id == ProviderGemini {
			return TurnTrace{ToolHops: []ToolHop{hop("observe_screen")}}, codexHTTPError{Code: 429}
		}
		return TurnTrace{Answer: "from " + id}, nil
	})
	if err != nil {
		t.Fatalf("askInOrder: %v", err)
	}
	if len(asked) != 2 {
		t.Errorf("asked %v, want the read repeated on the next provider", asked)
	}
}

// With every provider filtered out the caller is told so, rather than getting an empty answer that reads as success.
func TestAskInOrder_SaysWhenThereIsNobodyToAsk(t *testing.T) {
	t.Cleanup(ResetRouter)
	if _, err := askInOrder(nil, func(string) (TurnTrace, error) { return TurnTrace{}, nil }); err == nil {
		t.Error("askInOrder succeeded with no provider to ask")
	}
}

// errNoModelTextForTest stands in for a failure that has nothing to do with a provider's allowance.
var errNoModelTextForTest = errTestOnly{}

type errTestOnly struct{}

func (errTestOnly) Error() string { return "the prompt was rejected" }
