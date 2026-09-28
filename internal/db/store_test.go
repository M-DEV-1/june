package db

import (
	"path/filepath"
	"testing"
)

// TestNew_ReopenKeepsIndexTextOfEmptySummary opens a store twice over the same file with one summary node whose JSON $.summary is the empty string, and checks the row's memory_fts content is still the whole JSON after the second open. The nodes_ai_summary trigger indexes the full content for such a row (IFNULL(NULLIF(...), NEW.content)), and no sweep that runs at open may disagree with it: migration 7's rewrite guards on NULLIF against the empty string and runs once, so the text must survive.
func TestNew_ReopenKeepsIndexTextOfEmptySummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "june.db")
	const content = `{"same_task":true,"task_name":"June","summary":""}`

	first, err := New(path)
	if err != nil {
		t.Fatalf("New (first open): %v", err)
	}
	res, err := first.db.Exec(`INSERT INTO nodes (type, content) VALUES ('summary', ?)`, content)
	if err != nil {
		t.Fatalf("insert summary node: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := New(path)
	if err != nil {
		t.Fatalf("New (second open): %v", err)
	}
	defer second.Close()

	var indexed string
	if err := second.db.QueryRow(`SELECT content FROM memory_fts WHERE source = 'summary' AND ref_id = ?`, id).Scan(&indexed); err != nil {
		t.Fatalf("read the indexed text: %v", err)
	}
	if indexed != content {
		t.Errorf("indexed text after reopen = %q, want the whole content %q", indexed, content)
	}
}
