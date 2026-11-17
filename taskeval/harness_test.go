//go:build taskeval && linux

package taskeval

import (
	"context"
	"testing"
	"time"

	"june/taskeval/fixture"
)

// A daemon started with no display, session bus or tray must come up, take an ask over HTTP and return the scripted model's answer. Every other test relies on this.
func TestHarnessAnswersAScriptedAsk(t *testing.T) {
	if live {
		t.Skip("scripted mode only")
	}
	d := startDaemon(t, fixture.Build(1))
	model.load([]scriptStep{{Say: "pong"}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got, err := d.ask(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "pong" {
		t.Errorf("answer = %q, want %q", got.Text, "pong")
	}
	if _, requests, problems := model.finish(); requests != 1 || len(problems) > 0 {
		t.Errorf("model saw %d requests, problems %v; want 1 request and none", requests, problems)
	}
}
