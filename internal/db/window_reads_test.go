package db

import (
	"context"
	"testing"
)

// TestLatestDigest checks that the newest digest node comes back and that a store with no digest at all is an empty string rather than an error.
func TestLatestDigest(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	got, err := store.LatestDigest(ctx)
	if err != nil {
		t.Fatalf("LatestDigest on an empty store: %v", err)
	}
	if got != "" {
		t.Fatalf("LatestDigest on an empty store = %q, want empty", got)
	}

	for _, content := range []string{"the older day", "the newer day"} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(parent_id, type, content) VALUES(NULL, 'digest', ?)`, content); err != nil {
			t.Fatalf("seed digest %q: %v", content, err)
		}
	}
	// A summary node written after both digests must not win: LatestDigest reads digests only.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(parent_id, type, content) VALUES(NULL, 'summary', 'a summary')`); err != nil {
		t.Fatalf("seed summary: %v", err)
	}

	got, err = store.LatestDigest(ctx)
	if err != nil {
		t.Fatalf("LatestDigest: %v", err)
	}
	if got != "the newer day" {
		t.Fatalf("LatestDigest = %q, want %q", got, "the newer day")
	}
}

// TestMentionCount checks the note mention count: notes that name the person are counted, notes that do not are not, summaries are not, and a blank name counts nothing.
func TestMentionCount(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	for _, n := range []struct{ content, kind string }{
		{"Priya Shah leads the value chain work", "fact"},
		{"# Sync\nPriya Shah walked through the demo", "meeting"},
		{"Sneha owns the PFP task", "fact"},
	} {
		if _, err := store.LogNote(ctx, n.content, n.kind); err != nil {
			t.Fatalf("seed note: %v", err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(parent_id, type, content) VALUES(NULL, 'digest', 'Priya Shah again')`); err != nil {
		t.Fatalf("seed digest: %v", err)
	}

	cases := []struct {
		name string
		want int
	}{
		{"Priya Shah", 2},
		{"Sneha", 1},
		{"Nobody At All", 0},
		{"", 0},
	}
	for _, c := range cases {
		got, err := store.MentionCount(ctx, c.name)
		if err != nil {
			t.Fatalf("MentionCount(%q): %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("MentionCount(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}
