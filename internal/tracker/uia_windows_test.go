//go:build windows

package tracker

import (
	"context"
	"testing"
	"time"

	"june/internal/act"
)

// TestUIAReadsTheWindowInFront is a smoke test to run on a Windows desktop: it reads the window in front (the terminal running the test), checks that a listed element can be verified and measured through its ref, and that a capture returns text.
func TestUIAReadsTheWindowInFront(t *testing.T) {
	// The first call also starts PowerShell, which can take longer than one Observe's budget, so it is retried for a while.
	var (
		app, title string
		nodes      []act.Node
		err        error
	)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		app, title, nodes, err = Observe(context.Background())
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	t.Logf("%s · %s: %d nodes", app, title, len(nodes))
	if len(nodes) == 0 {
		t.Fatal("the window in front listed no actionable element")
	}
	for _, n := range nodes {
		if n.Label == "" {
			continue
		}
		if err := Verify(context.Background(), n.Ref, n.Role, n.Label, 0, 0, 0, 0); err != nil {
			t.Errorf("Verify(%q %s %q) = %v", n.Ref, n.Role, n.Label, err)
		}
		x, y, w, h, err := Extents(context.Background(), n.Ref)
		t.Logf("%s %q at %d,%d %dx%d, err %v", n.Role, n.Label, x, y, w, h, err)
		break
	}
	text, _ := extractText()
	t.Logf("captured %d characters", len(text))
	if text == "" {
		t.Error("extractText returned nothing for the window in front")
	}
}
