package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// A capture whose every line is accessibility chrome leaves memory.Normalize with no content, and WriteEpisode then falls back to the raw capture so the row is not stored empty. The fallback is the whole capture, so without a cap of its own a 2000-word screenful of box-drawing glyphs is stored in full, indexed in full by FTS, and chunked in full for embedding. This holds the fallback to the same 120 words the normalized path is held to, and to the same removal of control characters.
func TestWriteEpisode_RawFallbackIsCappedAndStripped(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()

	raw := strings.Repeat("⣿⣿⣿\x07\n", 2000)
	id, err := store.LogEpisode(ctx, "Zoom", "", raw)
	if err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := store.DB().QueryRow(`SELECT screen_text FROM episodes WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if words := len(strings.Fields(stored)); words > 120 {
		t.Errorf("stored %d words of the raw fallback, want at most 120", words)
	}
	if strings.ContainsRune(stored, '\x07') {
		t.Errorf("a control character reached screen_text: %q", stored[:min(len(stored), 80)])
	}
}

// AgeEpisodeImages used to name every aged row in the UPDATE, one bound parameter each, which SQLite refuses past SQLITE_MAX_VARIABLE_NUMBER (32766 by default). A daemon that missed several aging runs would then fail on every run afterwards and never age an image again. Forty thousand aged rows is past that limit.
func TestAgeEpisodeImages_MoreRowsThanSQLiteTakesParameters(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()

	const rows = 40000
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO episodes (app, title, screen_text, created_at, image_path)
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		SELECT 'Firefox', 'lecture', '', datetime('now', '-20 days'), 'frames/' || i || '.jpg' FROM n`,
		rows); err != nil {
		t.Fatal(err)
	}

	n, err := store.AgeEpisodeImages(ctx, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("AgeEpisodeImages: %v", err)
	}
	if n != rows {
		t.Errorf("aged %d rows, want %d", n, rows)
	}
	var left int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM episodes WHERE image_path != ''`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d rows still hold an image_path", left)
	}
}

// TestListEpisodes_AppFilterIsTextNotAPattern pins that the app filter is matched as the text a caller typed. It reaches the query tool, so a model can put a "%" or "_" in it, and under a bare LIKE those are wildcards: "%" alone matched every episode in the store.
func TestListEpisodes_AppFilterIsTextNotAPattern(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()

	if _, err := store.LogEpisode(ctx, "Brave Browser", "", "reading the pricing page"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogEpisode(ctx, "100% Orange Juice", "", "playing a game"); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListEpisodes(ctx, db.EpisodeQuery{App: "%", Limit: 10})
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(got) != 1 || got[0].App != "100% Orange Juice" {
		t.Errorf("the app filter %q matched %d episodes (%+v), want the one app whose name actually contains a per cent sign", "%", len(got), got)
	}
}
