package db_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

func TestWriteEpisode_StoresStructureAndJPEG(t *testing.T) {
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	id, err := store.WriteEpisode(context.Background(), db.EpisodeWrite{
		App:          "Firefox",
		Title:        "Suits",
		ScreenText:   "chrome dump",
		UserActivity: "watching Suits courtroom",
		VisibleText:  []string{"Harvey: object", "Donna: noted"},
		ImageJPEG:    []byte("not-a-real-jpeg-but-stored"),
	})
	if err != nil {
		t.Fatal(err)
	}

	eps, err := store.ListEpisodes(context.Background(), db.EpisodeQuery{Limit: 5, NewestFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes", len(eps))
	}
	e := eps[0]
	if e.ID != id || e.UserActivity != "watching Suits courtroom" {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.ScreenText, "watching Suits courtroom") || !strings.Contains(e.ScreenText, "Harvey: object") {
		t.Fatalf("screen_text=%q", e.ScreenText)
	}
	if e.ImagePath != "frames/1.jpg" {
		t.Fatalf("image_path=%q", e.ImagePath)
	}
	body, err := os.ReadFile(filepath.Join(dir, "frames", "1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "not-a-real-jpeg-but-stored" {
		t.Fatalf("jpeg bytes = %q", body)
	}
}

func TestListEpisodes_FiltersAppAndRecency(t *testing.T) {
	store := memStore(t)
	ctx := context.Background()

	if _, err := store.LogEpisode(ctx, "Slack", "ora", "thread about retrieval"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogEpisode(ctx, "Firefox", "Suits", "watching a show"); err != nil {
		t.Fatal(err)
	}

	slack, err := store.ListEpisodes(ctx, db.EpisodeQuery{App: "slack", Limit: 10, NewestFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(slack) != 1 || slack[0].App != "Slack" {
		t.Fatalf("app filter: %+v", slack)
	}

	recent, err := store.ListEpisodes(ctx, db.EpisodeQuery{Limit: 1, NewestFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].App != "Firefox" {
		t.Fatalf("recent: %+v", recent)
	}

	// Window in the past should miss both (created_at is now).
	old, err := store.ListEpisodes(ctx, db.EpisodeQuery{
		Since: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC),
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 0 {
		t.Fatalf("expected empty past window, got %+v", old)
	}
}

func TestAgeEpisodeImages_DropsOldJPEGsKeepsDescription(t *testing.T) {
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	oldID, err := store.WriteEpisode(ctx, db.EpisodeWrite{
		App: "Firefox", Title: "lecture", ScreenText: "week 4",
		UserActivity: "watching lecture week 4", ImageJPEG: []byte("old-jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := store.WriteEpisode(ctx, db.EpisodeWrite{
		App: "Firefox", Title: "lecture", ScreenText: "week 5",
		UserActivity: "watching lecture week 5", ImageJPEG: []byte("new-jpeg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET created_at = datetime('now', '-20 days') WHERE id = ?`, oldID); err != nil {
		t.Fatal(err)
	}

	n, err := store.AgeEpisodeImages(ctx, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("dropped %d images, want 1", n)
	}

	var oldPath, newPath, activity string
	if err := store.DB().QueryRow(`SELECT image_path, user_activity FROM episodes WHERE id = ?`, oldID).Scan(&oldPath, &activity); err != nil {
		t.Fatal(err)
	}
	if oldPath != "" {
		t.Fatalf("old image_path still %q", oldPath)
	}
	if activity != "watching lecture week 4" {
		t.Fatalf("description was dropped: %q", activity)
	}
	if _, err := os.Stat(filepath.Join(dir, "frames", fmt.Sprintf("%d.jpg", oldID))); !os.IsNotExist(err) {
		t.Fatalf("old jpeg file still on disk: %v", err)
	}
	if err := store.DB().QueryRow(`SELECT image_path FROM episodes WHERE id = ?`, newID).Scan(&newPath); err != nil {
		t.Fatal(err)
	}
	if newPath == "" {
		t.Fatal("recent jpeg was dropped")
	}
	if _, err := os.Stat(filepath.Join(dir, "frames", fmt.Sprintf("%d.jpg", newID))); err != nil {
		t.Fatalf("recent jpeg missing: %v", err)
	}
}
