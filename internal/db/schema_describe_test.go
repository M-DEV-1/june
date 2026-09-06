package db_test

import (
	"context"
	"strings"
	"testing"
)

// The description is read from the store instead of written by hand, because a written one goes stale silently: the hand-written version claimed notes.kind included "action_item", the real value is "action", and the model dutifully counted zero open items against twenty-five real ones.
func TestDescribeSchema_ListsTablesAndColumns(t *testing.T) {
	out, err := memStore(t).DescribeSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"episodes:", "notes:", "created_at", "screen_text"} {
		if !strings.Contains(out, want) {
			t.Errorf("description is missing %q:\n%s", want, out)
		}
	}
}

// A full-text index carries five shadow tables of its own — _data, _idx, _content, _docsize, _config — which are storage internals and answer no question anyone would ask.
func TestDescribeSchema_LeavesOutFullTextShadowTables(t *testing.T) {
	out, err := memStore(t).DescribeSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, unwanted := range []string{"_fts_data", "_fts_idx", "_fts_docsize", "_fts_config"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("description includes the shadow table %q:\n%s", unwanted, out)
		}
	}
}

// The values a column actually holds are the half a schema cannot give. "kind TEXT" is true and useless; knowing it is one of action, fact or meeting is what stops a query asking for a value that has never existed.
func TestDescribeSchema_NamesTheValuesOfASmallVocabulary(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()
	for _, kind := range []string{"action", "fact", "meeting", "action", "fact"} {
		if _, err := store.LogNote(ctx, "something "+kind, kind); err != nil {
			t.Fatal(err)
		}
	}

	out, err := store.DescribeSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"action", "fact", "meeting"} {
		if !strings.Contains(out, want) {
			t.Errorf("the values of notes.kind are not listed, %q is missing:\n%s", want, out)
		}
	}
}

// A column holding a different long string in every row is not a vocabulary, and printing its contents would put the store's text into every prompt.
func TestDescribeSchema_DoesNotListFreeformText(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if _, err := store.LogNote(ctx, strings.Repeat("a long and entirely unique note body ", 4)+string(rune('a'+i)), "fact"); err != nil {
			t.Fatal(err)
		}
	}

	out, err := store.DescribeSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out, "a long and entirely unique note body") {
		t.Errorf("free-form content was listed as if it were a vocabulary:\n%s", out)
	}
}

// A column of dates is not a vocabulary. Today there are six days in the diary and they would fit; next month there are forty and the column would silently stop being described at all, so the description would change shape with the calendar rather than with the schema.
func TestDescribeSchema_DoesNotTreatDatesAsAVocabulary(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()
	for _, day := range []string{"2026-08-29", "2026-08-30", "2026-08-31"} {
		if err := store.SetDiaryEntry(ctx, day, "day", "a diary entry with no date inside it, number "+day[8:]); err != nil {
			t.Skipf("diary write unavailable: %v", err)
		}
	}

	out, err := store.DescribeSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out, "2026-08-29") {
		t.Errorf("dates were listed as a vocabulary:\n%s", out)
	}
}
