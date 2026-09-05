package db

import (
	"context"
	"testing"
	"time"

	"ora/internal/memory"
)

func item(owner, text string) memory.ActionItem {
	return memory.ActionItem{
		Owner: owner, Text: text,
		Status: memory.StatusOpen, Priority: memory.PriorityNormal,
		Source: "md x mf tool", Raised: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	}
}

// AddActionItems stores each item once and returns only open ones, with no recency window — an owed task does not stop being owed because its meeting was a week ago.
func TestAddActionItems_StoresAndReadsBackOpen(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	added, err := store.AddActionItems(ctx, []memory.ActionItem{
		item("Me", "carry PR #13 through CI and merge."),
		item("Me", "reply on WhatsApp during his leave."),
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}

	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("want 2 open items, got %d: %+v", len(open), open)
	}
	if open[0].Owner != "Me" || open[0].Text != "carry PR #13 through CI and merge." {
		t.Errorf("first item = %+v", open[0])
	}
}

// OpenActionItems only surfaces the user's own owed work: rows already stored under another person's name (from before this filter existed, or ever) never appear, however many are on file.
func TestOpenActionItems_OnlyTheUsersOwn(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		item("Me", "send the deck by Friday."),
		item("Vikram", "carry PR #13 through CI and merge."),
		item(memory.UnknownOwner, "trial attaching walkthrough videos to PRs."),
	}); err != nil {
		t.Fatal(err)
	}

	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("want 2 open items, got %d: %+v", len(open), open)
	}
	for _, a := range open {
		if a.Owner == "Vikram" {
			t.Errorf("returned an item owed by somebody else: %+v", a)
		}
	}
}

// The zombie test: re-filing the same minutes after the user has closed an item must not resurrect it. LogNote dedupes on exact content, and a closed item's content differs by its status tag, so the match has to be on the work itself.
func TestAddActionItems_DoesNotResurrectAClosedItem(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "finish the acme-essentials setup.")}); err != nil {
		t.Fatal(err)
	}
	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetActionStatus(ctx, open[0].NoteID, memory.StatusDone); err != nil {
		t.Fatal(err)
	}

	// The recorder re-files the same meeting's minutes, as it does on every retry.
	added, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "finish the acme-essentials setup.")})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("re-filing added %d items, want 0", added)
	}
	open, err = store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("a closed item came back open: %+v", open)
	}
}

// Priority is the user's to set and survives independently of status.
func TestSetActionPriority(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "reply on WhatsApp during his leave.")}); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenActionItems(ctx)
	if err := store.SetActionPriority(ctx, open[0].NoteID, memory.PriorityLow); err != nil {
		t.Fatal(err)
	}
	open, _ = store.OpenActionItems(ctx)
	if len(open) != 1 || open[0].Priority != memory.PriorityLow {
		t.Fatalf("want one low-priority open item, got %+v", open)
	}
	if open[0].Status != memory.StatusOpen {
		t.Errorf("changing priority changed status to %q", open[0].Status)
	}
}

// An id the model invented must be reported, not silently ignored — reporting a correction as applied when it was not throws the user's words away.
func TestSetActionStatus_UnknownID(t *testing.T) {
	if err := newStore(t).SetActionStatus(context.Background(), 4242, memory.StatusDone); err == nil {
		t.Fatal("want an error for an id that is not an action note")
	}
}
