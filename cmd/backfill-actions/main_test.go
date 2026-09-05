package main

import (
	"testing"

	"ora/internal/memory"
)

// filterMine must keep exactly the items internal/recorder/recorder.go's liftActionItems keeps live — the user's own bullets and any item nobody was named for — so a backfill rerun files the same rows the daemon would have on the day the meeting happened.
func TestFilterMine(t *testing.T) {
	items := []memory.ActionItem{
		{Owner: memory.MeOwner, Text: "send the deck"},
		{Owner: "me", Text: "case-insensitive Me"},
		{Owner: memory.UnknownOwner, Text: "nobody named"},
		{Owner: "Vikram", Text: "not the user"},
		{Owner: "Priya Shah", Text: "also not the user"},
	}

	got := filterMine(items)

	if len(got) != 3 {
		t.Fatalf("filterMine kept %d items, want 3: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Owner == "Vikram" || a.Owner == "Priya Shah" {
			t.Errorf("filterMine kept another person's item: %+v", a)
		}
	}
}

// A meeting with nothing the user owed keeps none — matching liftActionItems, which never falls back to filing another person's items.
func TestFilterMine_NoneKept(t *testing.T) {
	items := []memory.ActionItem{
		{Owner: "Vikram", Text: "not the user"},
	}
	if got := filterMine(items); len(got) != 0 {
		t.Fatalf("filterMine kept %d items, want 0: %+v", len(got), got)
	}
}
