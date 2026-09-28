package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestQueryStore_RendersRows(t *testing.T) {
	store := memStore(t)
	if _, err := store.LogNote(context.Background(), "buy milk", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	out, err := store.QueryStore(context.Background(), "SELECT content, kind FROM notes", 40)
	if err != nil {
		t.Fatalf("QueryStore: %v", err)
	}
	if !strings.HasPrefix(out, "content\tkind\n") {
		t.Fatalf("expected header line, got %q", out)
	}
	if !strings.Contains(out, "buy milk\tfact") {
		t.Fatalf("expected row, got %q", out)
	}
}

func TestQueryStore_ReadOnlyHandleRejectsWrites(t *testing.T) {
	store := memStore(t)
	_, err := store.QueryStore(context.Background(), "INSERT INTO notes (content, kind) VALUES ('x', 'fact')", 40)
	if err == nil {
		t.Fatal("expected the read-only connection to reject a write, got nil error")
	}
	// The write must actually have failed at the sqlite layer (readonly database), not been rejected by string-sniffing the SQL text.
	if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected a sqlite readonly-database error, got: %v", err)
	}
}

func TestQueryStore_RejectsMultipleStatements(t *testing.T) {
	store := memStore(t)
	_, err := store.QueryStore(context.Background(), "SELECT 1; SELECT 2", 40)
	if err == nil {
		t.Fatal("expected an error for more than one statement")
	}
}

func TestQueryStore_RowCapTruncates(t *testing.T) {
	store := memStore(t)
	for i := 0; i < 60; i++ {
		if _, err := store.LogNote(context.Background(), fmt.Sprintf("note %d", i), "fact"); err != nil {
			t.Fatalf("LogNote: %v", err)
		}
	}
	out, err := store.QueryStore(context.Background(), "SELECT content FROM notes", 40)
	if err != nil {
		t.Fatalf("QueryStore: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// header + 60 rows would be 61 lines; the cap plus the truncation notice must land under that.
	if len(lines) >= 61 {
		t.Fatalf("expected the row cap to kick in, got %d lines", len(lines))
	}
	if !strings.Contains(out, "LIMIT") && !strings.Contains(out, "aggregate") {
		t.Fatalf("expected the truncation notice to suggest LIMIT or aggregating, got %q", out)
	}
}

// A row too large to render must not be reported the same way as a row that does not exist. Answering "no rows matched" for a note that is merely long would have the caller conclude the data is absent, and an ambient memory that denies its own contents is worse than one that returns too much.
func TestQueryStore_OneOversizedRowIsNotAnAbsence(t *testing.T) {
	store := memStore(t)
	if _, err := store.LogNote(context.Background(), strings.Repeat("x", 9000), "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	out, err := store.QueryStore(context.Background(), "SELECT content FROM notes", 40)

	if strings.HasPrefix(out, "no rows matched") {
		t.Fatal("a row that exists was reported as no rows matching")
	}
	if err == nil {
		t.Fatal("want an error telling the caller how to ask for less")
	}
	if !strings.Contains(err.Error(), "substr") {
		t.Errorf("error = %q, want it to say how to narrow the query", err)
	}
}

// Between 2026-09-16 and 09-23 the model guessed columns that do not exist five times (episodes.url, episodes.window_title, episodes.ocr_text, episodes_fts.content) and spent a round on each. The error names the columns the queried table really has, so the next try can be right.
func TestQueryStore_AMissingColumnNamesTheRealOnes(t *testing.T) {
	store := memStore(t)
	_, err := store.QueryStore(context.Background(), "SELECT e.window_title FROM episodes e WHERE e.id = 1", 40)
	if err == nil {
		t.Fatal("a query on a column that does not exist ran")
	}
	for _, want := range []string{"episodes", "title", "screen_text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}
