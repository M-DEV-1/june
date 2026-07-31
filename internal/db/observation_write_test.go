package db_test

import (
	"context"
	"strings"
	"testing"

	"ora/internal/db"
)

// TestLogEpisode_StoresCleanContent keeps chrome out of screen_text while preserving context in columns (app/title/domain). Clean prose is stored as content; embeddings may still frame content with context separately.
func TestLogEpisode_StoresCleanContent(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	raw := strings.Repeat("⣿⣿⣿\n", 30) + "\nWatching Suits season six episode twelve The Painting in a courtroom scene.\n"
	id, err := store.LogEpisode(context.Background(), "Netflix", "Suits S6E12", raw)
	if err != nil {
		t.Fatal(err)
	}
	var text, domain, app, title string
	if err := store.DB().QueryRow(
		`SELECT screen_text, domain, app, title FROM episodes WHERE id=?`, id,
	).Scan(&text, &domain, &app, &title); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "⣿") {
		t.Fatalf("chrome leaked into screen_text: %q", text)
	}
	if !strings.Contains(text, "Suits") && !strings.Contains(text, "courtroom") {
		t.Fatalf("lost substance in screen_text: %q", text)
	}
	if app != "Netflix" || title != "Suits S6E12" {
		t.Fatalf("context columns: app=%q title=%q", app, title)
	}
	if domain != "personal" {
		t.Fatalf("domain=%q want personal", domain)
	}
}
