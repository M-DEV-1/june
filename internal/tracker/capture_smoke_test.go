//go:build windows && smoke

package tracker

import (
	"testing"
)

// Run with: go test -tags smoke ./internal/tracker/...
// Requires a real Windows display.
func TestCapture_TakeAndExtract_Smoke(t *testing.T) {
	text, err := extractText()
	if err != nil {
		t.Fatalf("extractText() failed: %v", err)
	}
	words := smokeCountWords(text)
	t.Logf("UIA: %d chars, %d words", len(text), words)
	t.Logf("--- UIA TEXT ---\n%s\n---", text)

	if words == 0 {
		t.Error("expected UIA to extract at least some words from the focused window")
	}
}

func smokeCountWords(s string) int {
	n, inWord := 0, false
	for _, c := range s {
		sp := c == ' ' || c == '\n' || c == '\r' || c == '\t'
		if !sp && !inWord {
			n++
			inWord = true
		} else if sp {
			inWord = false
		}
	}
	return n
}
