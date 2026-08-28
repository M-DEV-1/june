package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ora/internal/db"
)

// TestWriteEpisode_StoresSecondMonitorFrames proves the frames from every other monitor land next to the primary as {id}-b.jpg, {id}-c.jpg and are reachable by EpisodeExtraImages. A meeting on one screen with notes on the other is one episode, so both screens must survive the write.
func TestWriteEpisode_StoresSecondMonitorFrames(t *testing.T) {
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	id, err := store.WriteEpisode(context.Background(), db.EpisodeWrite{
		App: "Zoom", Title: "standup", ScreenText: "call",
		ImageJPEG: []byte("primary-frame"),
		ExtraJPEG: [][]byte{[]byte("second-monitor"), []byte("third-monitor")},
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"1.jpg":   "primary-frame",
		"1-b.jpg": "second-monitor",
		"1-c.jpg": "third-monitor",
	} {
		body, err := os.ReadFile(filepath.Join(dir, "frames", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(body) != want {
			t.Fatalf("%s = %q, want %q", name, body, want)
		}
		info, err := os.Stat(filepath.Join(dir, "frames", name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s perms = %v, want 0600", name, info.Mode().Perm())
		}
	}

	got := store.EpisodeExtraImages(id)
	want := []string{"frames/1-b.jpg", "frames/1-c.jpg"}
	if len(got) != len(want) {
		t.Fatalf("EpisodeExtraImages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EpisodeExtraImages = %v, want %v", got, want)
		}
	}
}

// TestEpisodeExtraImages_EmptyForSingleMonitor proves a one-screen capture reports no extras rather than paths to files that do not exist.
func TestEpisodeExtraImages_EmptyForSingleMonitor(t *testing.T) {
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	id, err := store.WriteEpisode(context.Background(), db.EpisodeWrite{
		App: "Firefox", Title: "docs", ScreenText: "reading", ImageJPEG: []byte("only-frame"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.EpisodeExtraImages(id); len(got) != 0 {
		t.Fatalf("EpisodeExtraImages = %v, want none", got)
	}
}

// TestAgeEpisodeImages_DeletesExtraMonitorFrames is the retention guarantee: extras die with their primary, or -b.jpg files pile up in the frames dir forever with nothing left in the database pointing at them.
func TestAgeEpisodeImages_DeletesExtraMonitorFrames(t *testing.T) {
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	oldID, err := store.WriteEpisode(ctx, db.EpisodeWrite{
		App: "Zoom", Title: "standup", ScreenText: "call",
		ImageJPEG: []byte("primary"), ExtraJPEG: [][]byte{[]byte("second"), []byte("third")},
	})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := store.WriteEpisode(ctx, db.EpisodeWrite{
		App: "Zoom", Title: "retro", ScreenText: "call",
		ImageJPEG: []byte("primary"), ExtraJPEG: [][]byte{[]byte("second")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET created_at = datetime('now', '-20 days') WHERE id = ?`, oldID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AgeEpisodeImages(ctx, 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"1-b.jpg", "1-c.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, "frames", name)); !os.IsNotExist(err) {
			t.Fatalf("aged extra frame %s still on disk: %v", name, err)
		}
	}
	if got := store.EpisodeExtraImages(oldID); len(got) != 0 {
		t.Fatalf("aged episode still reports extras: %v", got)
	}
	if got := store.EpisodeExtraImages(newID); len(got) != 1 {
		t.Fatalf("recent episode lost its extra frame: %v", got)
	}
}

// TestPruneAncientEpisodes_DeletesExtraMonitorFrames covers the other path that reclaims frames: deleting the row must take every monitor's frame with it.
func TestPruneAncientEpisodes_DeletesExtraMonitorFrames(t *testing.T) {
	dir := t.TempDir()
	store, err := db.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	id, err := store.WriteEpisode(ctx, db.EpisodeWrite{
		App: "Zoom", Title: "standup", ScreenText: "call",
		ImageJPEG: []byte("primary"), ExtraJPEG: [][]byte{[]byte("second")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET created_at = datetime('now', '-400 days'), screen_text = '' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PruneAncientEpisodes(ctx, 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "frames", "1-b.jpg")); !os.IsNotExist(err) {
		t.Fatalf("pruned extra frame still on disk: %v", err)
	}
}
