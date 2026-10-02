package db_test

import (
	"context"
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
