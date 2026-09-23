package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// screenActions are the tools that move, type into or switch what is on screen, so two tasks running them at once fight over the same windows.
var screenActions = map[string]bool{
	"click": true, "click_at": true, "type_text": true, "press_key": true,
	"scroll_at": true, "scroll_to": true, "switch_window": true, "open_app": true, "open_url": true,
}

// screenIdleRelease is how long a task may go without acting before another task may take the screen from it. Across 473 gaps between one task's own screen actions from 2026-09-01 to 09-23, the median was 7 seconds, the 90th percentile 18 and the 99th 58, so a task quiet for longer than a minute has stopped (paused, stuck on a question, or ended in a way nobody reported).
const screenIdleRelease = 60 * time.Second

// screenHold is the task driving the screen: its screen state, which identifies it, the done channel of its context, which closes when it ends, its question for the refusal to name, and when it last acted.
type screenHold struct {
	mu    sync.Mutex
	scope *askLookState
	done  <-chan struct{}
	label string
	last  time.Time
}

// screenBusy reports whether another task has the screen, for a screen action about to run: one whose context has not ended and which acted within screenIdleRelease. Input: the call's context and the tool's name. Output: "" when the call may go ahead, or the refusal to hand the model, naming the task that has the screen.
func (a *Agent) screenBusy(ctx context.Context, name string) string {
	if !screenActions[name] {
		return ""
	}
	h := &a.screen
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scope == nil || h.scope == a.askState(ctx) || ended(h.done) || time.Since(h.last) >= screenIdleRelease {
		return ""
	}
	label := h.label
	if label == "" {
		label = "a computer-use job"
	}
	return fmt.Sprintf("another task is using the screen right now (%q), and acting too would fight it over the same windows; tell the user, and wait for it to finish or ask whether to stop it", label)
}

// holdScreen gives the screen to the task whose screen action just went through; an action refused or stopped at the stop line claims nothing. Input: the call's context and the tool's name. Output: none.
func (a *Agent) holdScreen(ctx context.Context, name string) {
	if !screenActions[name] {
		return
	}
	h := &a.screen
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scope, h.done, h.label, h.last = a.askState(ctx), ctx.Done(), questionFrom(ctx), time.Now()
}

// ended reports whether a context's done channel has closed; a context that can never end has a nil channel and never has.
func ended(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
