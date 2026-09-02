package tally_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ora/internal/tally"
)

// Nothing in Ora measured how much text it sends a model — the tally counted calls and latency, and the CLI providers never report token counts at all. Characters are the one thing measurable on every provider, so they are what gets stored; the token estimate is done at read time with a stated divisor rather than baked into the data.
type usageRec struct {
	provider                string
	promptChars, replyChars int
}

func (r *usageRec) RecordUsage(provider string, ok bool, ms time.Duration, promptChars, replyChars int) error {
	r.provider, r.promptChars, r.replyChars = provider, promptChars, replyChars
	return nil
}

func TestWrap_RecordsHowMuchTextCrossedTheSeam(t *testing.T) {
	rec := &usageRec{}
	prompt := strings.Repeat("a", 4000)
	reply := strings.Repeat("b", 800)

	wrapped := tally.Wrap("claude-cli", func(context.Context, string) (string, error) { return reply, nil }, rec)
	if _, err := wrapped(context.Background(), prompt); err != nil {
		t.Fatal(err)
	}

	if rec.promptChars != 4000 || rec.replyChars != 800 {
		t.Errorf("recorded %d prompt / %d reply chars, want 4000 / 800", rec.promptChars, rec.replyChars)
	}
}

// A failed call still sent its prompt and still cost the quota, so it is still counted.
func TestWrap_CountsThePromptOfAFailedCall(t *testing.T) {
	rec := &usageRec{}
	wrapped := tally.Wrap("gemini", func(context.Context, string) (string, error) { return "", context.DeadlineExceeded }, rec)
	_, _ = wrapped(context.Background(), strings.Repeat("x", 1500))

	if rec.promptChars != 1500 {
		t.Errorf("got %d prompt chars on a failed call, want 1500 — it was still sent", rec.promptChars)
	}
}
