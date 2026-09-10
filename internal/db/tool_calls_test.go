package db

import (
	"context"
	"testing"
	"time"
)

// A filed call comes back whole, with the offered set intact — the field that lets a later pass tell a tool that was never shown from one that was shown and not chosen.
func TestAddToolCall_RoundTrips(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.AddToolCall(ctx, ToolCall{
		Path: "voice", ConversationID: 42, Name: "draw", Args: `"an octopus"`,
		Outcome: "ok", Result: "drew 12 shapes", DurationMS: 340,
		Offered: []string{"draw", "look", "observe_screen"},
	}); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}

	got, err := store.ToolCallsSince(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ToolCallsSince: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	c := got[0]
	if c.Path != "voice" || c.ConversationID != 42 || c.Name != "draw" || c.Outcome != "ok" || c.DurationMS != 340 {
		t.Errorf("call = %+v", c)
	}
	if len(c.Offered) != 3 || c.Offered[1] != "look" {
		t.Errorf("offered = %v, want the three names it was shown", c.Offered)
	}
}

// A call with nothing recorded as offered comes back with an empty list rather than one empty name, so a reader counting what was shown is not told a tool called "" existed.
func TestToolCallsSince_EmptyOfferedIsNoNames(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.AddToolCall(ctx, ToolCall{Path: "ask", Name: "recall", Outcome: "ok"}); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	got, err := store.ToolCallsSince(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Offered) != 0 {
		t.Errorf("offered = %v, want none", got[0].Offered)
	}
}
