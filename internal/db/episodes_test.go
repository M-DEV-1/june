package db_test

import (
	"context"
	"testing"
	"time"
)

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
