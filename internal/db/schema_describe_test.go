package db_test

import (
	"context"
	"strings"
	"testing"

	"june/internal/db"
)

// TestDescribeSchema covers what the description a model reads must and must not say. The description is read from the store instead of written by hand, because a written one goes stale silently: the hand-written version once claimed notes.kind included "action_item", the real value is "action", and the model dutifully counted zero open items against twenty-five real ones. Each subtest checks one property: tables and columns are listed, FTS5's shadow tables are not (storage internals, answer no question anyone would ask), a column holding a small fixed vocabulary has its values listed (so a query never asks for a value that has never existed), a column of long unique text is not treated as a vocabulary (or the store's own text would leak into every prompt), and a column of dates is not treated as a vocabulary either (or the description would change shape with the calendar).
func TestDescribeSchema(t *testing.T) {
	cases := []struct {
		name            string
		setup           func(t *testing.T, ctx context.Context, store *db.Store)
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "lists tables and columns",
			wantContains: []string{"episodes:", "notes:", "created_at", "screen_text"},
		},
		{
			name:            "leaves out full-text shadow tables",
			wantNotContains: []string{"_fts_data", "_fts_idx", "_fts_docsize", "_fts_config"},
		},
		{
			name: "names the values of a small vocabulary",
			setup: func(t *testing.T, ctx context.Context, store *db.Store) {
				for _, kind := range []string{"action", "fact", "meeting", "action", "fact"} {
					if _, err := store.LogNote(ctx, "something "+kind, kind); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantContains: []string{"action", "fact", "meeting"},
		},
		{
			name: "does not list freeform text",
			setup: func(t *testing.T, ctx context.Context, store *db.Store) {
				for i := 0; i < 30; i++ {
					if _, err := store.LogNote(ctx, strings.Repeat("a long and entirely unique note body ", 4)+string(rune('a'+i)), "fact"); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantNotContains: []string{"a long and entirely unique note body"},
		},
		{
			name: "does not treat dates as a vocabulary",
			setup: func(t *testing.T, ctx context.Context, store *db.Store) {
				for _, day := range []string{"2026-08-29", "2026-08-30", "2026-08-31"} {
					if err := store.SetDiaryEntry(ctx, day, "day", "a diary entry with no date inside it, number "+day[8:]); err != nil {
						t.Skipf("diary write unavailable: %v", err)
					}
				}
			},
			wantNotContains: []string{"2026-08-29"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			store := memStore(t)
			if c.setup != nil {
				c.setup(t, ctx, store)
			}

			out, err := store.DescribeSchema(ctx)
			if err != nil {
				t.Fatal(err)
			}

			for _, want := range c.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("description is missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range c.wantNotContains {
				if strings.Contains(out, unwanted) {
					t.Errorf("description includes %q, want it excluded:\n%s", unwanted, out)
				}
			}
		})
	}
}
