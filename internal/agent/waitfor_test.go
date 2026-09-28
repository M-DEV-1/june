package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"june/internal/act"
	"june/internal/tracker"
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

// When the camera can never take a single picture, waitForScreenChange used to run out the whole timeout and then say "the screen looks the same as before the action", which claims a comparison was made when none ever was. It must instead say plainly that no baseline screenshot was available.
func TestWaitFor_ScreenChangedSaysNoBaselineWasAvailable(t *testing.T) {
	a, _ := observingAgent(t)
	pressPoll = 0
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		return tracker.Capture{}, errors.New("camera unavailable")
	}
	ctx := context.Background()
	got := a.executeTool(ctx, "wait_for", map[string]any{"kind": act.ScreenChanged, "value": "a recording indicator", "timeout_ms": 50.0})
	if !strings.HasPrefix(got, act.WaitFailPrefix) {
		t.Fatalf("wait_for = %q, want a failure", got)
	}
	if strings.Contains(got, "looks the same") {
		t.Errorf("wait_for = %q, want it to say no baseline was available rather than claim a comparison was made", got)
	}
	if !strings.Contains(got, "no baseline") {
		t.Errorf("wait_for = %q, want it to say no baseline screenshot was available", got)
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

// TestWaitFor_MalformedCheckFailsAtOnce checks a check with no text to look for, or a kind that does not exist, comes back straight away rather than polling a screen that can never satisfy it.
func TestWaitFor_MalformedCheckFailsAtOnce(t *testing.T) {
	a, _ := observingAgent(t)
	start := time.Now()
	got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": act.TitleContains, "timeout_ms": 5000.0})
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("wait_for with no value = %q, want an error", got)
	}
	if got := a.executeTool(context.Background(), "wait_for", map[string]any{"kind": "vibes", "value": "good", "timeout_ms": 5000.0}); !strings.HasPrefix(got, "error:") {
		t.Errorf("wait_for with an unknown kind = %q, want an error", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("a malformed check took %s, want it refused at once", took)
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

// The window guard only covers the two list checks: a title check is about the window in front by definition, and holding it to the window the list came from would refuse every check written for a page that opens a new window.
func TestWaitFor_TitleCheckStillPassesInANewWindow(t *testing.T) {
	a, _ := observingAgent(t)
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.observe = func(context.Context) (string, string, []act.Node, error) {
		return "slack", "general", nil, nil
	}

	got := a.executeTool(ctx, "wait_for", map[string]any{"kind": act.TitleContains, "value": "general", "timeout_ms": 600.0})

	if !strings.HasPrefix(got, act.WaitPassPrefix) {
		t.Errorf("title_contains = %q, want a pass", got)
	}
}

// The reading a job takes before it acts costs one walk of the screen and nothing else. Going back through wait_for cost a poll loop and, on a list check that matched, a second walk to ask whether another window had come to the front — neither of which can tell it anything, because nothing has acted yet.
func TestCheckHolds_CostsOneWalk(t *testing.T) {
	a, _ := observingAgent(t)
	var walks atomic.Int32
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		walks.Add(1)
		return "brave", "PR #13 · GitHub", []act.Node{{Role: "push button", Label: "Merge", W: 10, H: 10, Showing: true, Ref: "r-merge"}}, nil
	}
	ctx := context.Background()
	a.executeTool(ctx, "observe_screen", map[string]any{})
	walks.Store(0)

	if !a.CheckHolds(ctx, act.Check{Kind: act.ItemPresent, Value: "Merge"}) {
		t.Error("item_present Merge did not hold on a screen that shows it")
	}
	if got := walks.Load(); got != 1 {
		t.Errorf("the pre-reading walked the screen %d times, want 1", got)
	}
	if a.CheckHolds(ctx, act.Check{Kind: act.TitleContains, Value: "Netflix"}) {
		t.Error("title_contains Netflix held on a window called PR #13 · GitHub")
	}
	if a.CheckHolds(ctx, act.Check{Kind: act.TitleContains, Value: ""}) {
		t.Error("a check with nothing to look for was reported as holding")
	}
	if got := walks.Load(); got != 2 {
		t.Errorf("three pre-readings walked the screen %d times, want one each for the two that name something to look for", got)
	}
}

// The same application renaming its window is not another window taking focus: Spotify's title becomes the song the moment Play is pressed, and on 2026-09-09 the check on that very press was failed for it. Only a different application in front fails a list check.
func TestWaitFor_ItemChecksAllowTheSameAppUnderANewTitle(t *testing.T) {
	a, _ := observingAgent(t)
	ctx := context.Background()
	app, _, _, _ := a.observe(ctx)
	a.executeTool(ctx, "observe_screen", map[string]any{})
	a.observe = func(context.Context) (string, string, []act.Node, error) {
		return app, "AC/DC - Back In Black", []act.Node{{Role: "push button", Label: "Pause", W: 10, H: 10, Showing: true, Ref: "r-pause"}}, nil
	}
	got := a.executeTool(ctx, "wait_for", map[string]any{"kind": act.ItemPresent, "value": "Pause", "timeout_ms": 600.0})
	if !strings.HasPrefix(got, act.WaitPassPrefix) {
		t.Errorf("item_present = %q, want a pass: same application, new title", got)
	}
}
