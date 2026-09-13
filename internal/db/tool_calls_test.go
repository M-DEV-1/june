package db

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ora/internal/util"
)

// A filed call comes back whole, with the offered set intact — the field that lets a later pass tell a tool that was never shown from one that was shown and not chosen — and with its full output text and turn id, added by migration 0011.
func TestAddToolCall_RoundTrips(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.AddToolCall(ctx, ToolCall{
		Path: "voice", ConversationID: 42, Name: "draw", Args: `"an octopus"`,
		Outcome: "ok", Result: "drew 12 shapes", Output: "drew 12 shapes across the whole canvas", TurnID: "turn-abc123",
		DurationMS: 340,
		Offered:    []string{"draw", "look", "observe_screen"},
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
	if c.Output != "drew 12 shapes across the whole canvas" {
		t.Errorf("output = %q, want the full result text back", c.Output)
	}
	if c.TurnID != "turn-abc123" {
		t.Errorf("turn_id = %q, want turn-abc123", c.TurnID)
	}
	if len(c.Offered) != 3 || c.Offered[1] != "look" {
		t.Errorf("offered = %v, want the three names it was shown", c.Offered)
	}
}

// The migration that added output and turn_id (0011) applies cleanly to a store created before it existed: newStore already runs every migration in order, so a store that opens at all here proves it. A row written afterward carries both columns with their zero-value defaults when neither is set.
func TestAddToolCall_OutputAndTurnIDDefaultToEmpty(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.AddToolCall(ctx, ToolCall{Path: "ask", Name: "recall", Outcome: "ok"}); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	got, err := store.ToolCallsSince(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Output != "" || got[0].TurnID != "" {
		t.Errorf("call = %+v, want empty output and turn_id when neither was set", got[0])
	}
}

// util.UTF8Bytes is what caps ToolRecord.Output before it reaches the store, so the boundary the store actually sees is tested at that call, right at the cap, one byte over it, and against a multi-byte rune that straddles it.
func TestUTF8Bytes_ToolCallOutputCapBoundary(t *testing.T) {
	const cap = 2048

	exact := strings.Repeat("a", cap)
	if got := util.UTF8Bytes(exact, cap); got != exact {
		t.Errorf("a string of exactly %d bytes was cut, want it returned unchanged", cap)
	}

	oneOver := strings.Repeat("a", cap+1)
	got := util.UTF8Bytes(oneOver, cap)
	if len(got) != cap {
		t.Errorf("a string one byte over the cap truncated to %d bytes, want exactly %d", len(got), cap)
	}

	// A 3-byte rune (e.g. U+FFFC OBJECT REPLACEMENT CHARACTER) placed so it straddles byte 2048: 2047 bytes of filler put its first byte at index 2047, its last at 2049, which the cut must back off from rather than split.
	straddling := strings.Repeat("a", cap-1) + "￼" + "tail"
	got = util.UTF8Bytes(straddling, cap)
	if !strings.HasSuffix(got, strings.Repeat("a", cap-1)) {
		t.Errorf("cutting at a straddling rune left %q, want the rune dropped whole rather than split", got)
	}
	if len(got) != cap-1 {
		t.Errorf("cutting a string with a rune straddling the cap gave %d bytes, want %d (the rune backed off entirely)", len(got), cap-1)
	}
	if !utf8.ValidString(got) {
		t.Errorf("output %q is not valid UTF-8", got)
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
