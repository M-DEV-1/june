package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"june/internal/act"
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
	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.TitleContains, "value": "S16 E8", "timeout_ms": 5000.0})
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
	start := time.Now()
	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.TitleContains, "value": "S16 E8", "timeout_ms": 600.0})
	if !strings.HasPrefix(got, act.WaitFailPrefix) {
		t.Fatalf("wait_for = %q, want a failure", got)
	}
	// timeout_ms is read off the arguments as a JSON number, so an int here would fall through to the five-second default and this test would quietly wait eight times as long as it says it does.
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("wait_for waited %s for a 600ms timeout, want the timeout it was given rather than the default", took)
	}
	if !strings.Contains(got, "PR #13") {
		t.Errorf("wait_for = %q, want it to say what was actually on screen", got)
	}
}

// TestWaitFor_ItemAbsentAndPresent checks the two list checks read the same numbered listing observe_screen builds.
func TestWaitFor_ItemAbsentAndPresent(t *testing.T) {
	a, _ := observingAgent(t)
	if got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.ItemPresent, "value": "Merge", "timeout_ms": 600.0}); !strings.HasPrefix(got, act.WaitPassPrefix) {
		t.Errorf("item_present Merge = %q, want a pass", got)
	}
	if got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.ItemAbsent, "value": "Merge", "timeout_ms": 600.0}); !strings.HasPrefix(got, act.WaitFailPrefix) {
		t.Errorf("item_absent Merge = %q, want a failure", got)
	}
}

// TestExecuteAskTool_KeepsTheGate checks the entry point a job's step loop uses is the ask's own gated one: a tool an ask may not run is refused there too, rather than a job having a way round the gate.
func TestExecuteAskTool_KeepsTheGate(t *testing.T) {
	a, _ := observingAgent(t)
	if got := a.ExecuteAskTool(context.Background(), "shell_exec", map[string]any{"command": "rm -rf /"}); !strings.Contains(got, "not available in an ask") {
		t.Errorf("ExecuteAskTool(shell_exec) = %q, want the ask gate's refusal", got)
	}
}

// item_present and item_absent are matched against a walk of whatever window is in front when the poll happens, so a window that took focus after the step acted can satisfy either check with a list from somewhere else entirely — and item_absent passes on a window that never held the item at all. Both refuse to pass while the front window is not the one the list came from.
func TestWaitFor_ItemChecksRefuseWhenAnotherWindowIsInFront(t *testing.T) {
	a, _ := observingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.observe = func(context.Context) (string, string, []act.Node, error) {
		return "slack", "general", []act.Node{{Role: "push button", Label: "Merge", W: 10, H: 10, Showing: true, Ref: "r-merge"}}, nil
	}

	present := a.executeTool(ctx, "wait_for", map[string]any{"kind": act.ItemPresent, "value": "Merge", "timeout_ms": 600.0})
	if !strings.HasPrefix(present, act.WaitFailPrefix) || !strings.Contains(present, "slack") {
		t.Errorf("item_present = %q, want a failure naming the window that is in front", present)
	}
	absent := a.executeTool(ctx, "wait_for", map[string]any{"kind": act.ItemAbsent, "value": "Checks", "timeout_ms": 600.0})
	if !strings.HasPrefix(absent, act.WaitFailPrefix) || !strings.Contains(absent, "slack") {
		t.Errorf("item_absent = %q, want a failure naming the window that is in front", absent)
	}
}
