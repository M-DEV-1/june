package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/act"
)

// TestWaitFor_PassesAsSoonAsTheChangeComes checks the tool polls rather than sleeping out its timeout: the title changes on the third look, and wait_for reports the change without waiting the full five seconds.
func TestWaitFor_PassesAsSoonAsTheChangeComes(t *testing.T) {
	a, _ := observingAgent(t)
	var looks atomic.Int32
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		title := "PR #13 · GitHub"
		if looks.Add(1) >= 3 {
			title = "Watching S16 E8 · Netflix"
		}
		return "brave", title, []act.Node{{Role: "push button", Label: "Play", W: 10, H: 10, Showing: true, Ref: "r-play"}}, nil
	}

	start := time.Now()
	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.TitleContains, "value": "S16 E8", "timeout_ms": 5000})
	if !strings.HasPrefix(got, act.WaitPassPrefix) {
		t.Fatalf("wait_for = %q, want a pass", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("wait_for took %s, want it to return as soon as the change came", took)
	}
	if looks.Load() < 3 {
		t.Errorf("wait_for looked %d times, want it to have polled", looks.Load())
	}
}

// TestWaitFor_FailsWhenTheChangeNeverComes checks a change that does not arrive is reported as a failure that says how long it waited and what was actually there, which is the sentence a stuck question is built from.
func TestWaitFor_FailsWhenTheChangeNeverComes(t *testing.T) {
	a, _ := observingAgent(t)
	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.TitleContains, "value": "S16 E8", "timeout_ms": 600})
	if !strings.HasPrefix(got, act.WaitFailPrefix) {
		t.Fatalf("wait_for = %q, want a failure", got)
	}
	if !strings.Contains(got, "PR #13") {
		t.Errorf("wait_for = %q, want it to say what was actually on screen", got)
	}
}

// TestWaitFor_ItemAbsentAndPresent checks the two list checks read the same numbered listing observe_screen builds.
func TestWaitFor_ItemAbsentAndPresent(t *testing.T) {
	a, _ := observingAgent(t)
	if got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.ItemPresent, "value": "Merge", "timeout_ms": 600}); !strings.HasPrefix(got, act.WaitPassPrefix) {
		t.Errorf("item_present Merge = %q, want a pass", got)
	}
	if got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.ItemAbsent, "value": "Merge", "timeout_ms": 600}); !strings.HasPrefix(got, act.WaitFailPrefix) {
		t.Errorf("item_absent Merge = %q, want a failure", got)
	}
}

// TestWaitFor_MalformedCheckFailsAtOnce checks a check with no text to look for, or a kind that does not exist, comes back straight away rather than polling a screen that can never satisfy it.
func TestWaitFor_MalformedCheckFailsAtOnce(t *testing.T) {
	a, _ := observingAgent(t)
	start := time.Now()
	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.TitleContains, "timeout_ms": 5000})
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("wait_for with no value = %q, want an error", got)
	}
	if got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": "vibes", "value": "good", "timeout_ms": 5000}); !strings.HasPrefix(got, "error:") {
		t.Errorf("wait_for with an unknown kind = %q, want an error", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("a malformed check took %s, want it refused at once", took)
	}
}

// TestWaitFor_IsOfferedToAnAsk checks the tool is declared, is inside the ask gate, and counts as a screen tool, so a job's step loop and a plain ask reach exactly the same one.
func TestWaitFor_IsOfferedToAnAsk(t *testing.T) {
	var declared bool
	for _, d := range ToolDeclarations() {
		if d.Name == "wait_for" {
			declared = true
		}
	}
	if !declared {
		t.Error("wait_for is not declared to the model")
	}
	if !askAllowedTools["wait_for"] {
		t.Error("wait_for is outside the ask gate, so a job could not run it")
	}
	if !screenRoundTools["wait_for"] {
		t.Error("wait_for is not in the short screen tool set, so a screen round would lose it")
	}
	if !screenToolNames["wait_for"] {
		t.Error("wait_for's result names a window title and is not marked as screen text")
	}
}

// TestExecuteAskTool_KeepsTheGate checks the entry point a job's step loop uses is the ask's own gated one: a tool an ask may not run is refused there too, rather than a job having a way round the gate.
func TestExecuteAskTool_KeepsTheGate(t *testing.T) {
	a, _ := observingAgent(t)
	if got := a.ExecuteAskTool(context.Background(), "shell_exec", map[string]any{"command": "rm -rf /"}); !strings.Contains(got, "not available in an ask") {
		t.Errorf("ExecuteAskTool(shell_exec) = %q, want the ask gate's refusal", got)
	}
}
