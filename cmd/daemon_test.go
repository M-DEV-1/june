package cmd

import (
	"context"

	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/tracker"
)

// TestPingHandler_ReturnsBuildIdentity verifies /ping's response body is this process's own build identity, not a static "pong" — the client compares this against its own identity to detect a stale daemon still running an old build (see checkDaemonBuildMismatch in root.go).
func TestPingHandler_ReturnsBuildIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(pingHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != buildIdentity {
		t.Errorf("expected /ping body to be buildIdentity %q, got %q", buildIdentity, string(body))
	}
}

// A daemon that cannot bind port 6942 is a second daemon: the first one is still running and still holds the port. It used to log a Warn and return nil, so the process exited 0, systemd called the restart a success, and the /ping that followed was answered by the old daemon running the old build. It must fail loudly instead.
func TestRunDaemon_ReturnsAnErrorWhenThePortIsAlreadyBound(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind a port for the test: %v", err)
	}
	defer held.Close()

	oldPort := DaemonPort
	DaemonPort = strconv.Itoa(held.Addr().(*net.TCPAddr).Port)
	defer func() { DaemonPort = oldPort }()

	if err := runDaemon(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("runDaemon returned nil for a port it could not bind, so the process would exit 0")
	}
}

// SIGTERM cancels the root context before stop() runs, so every episode still in the tracker's channel used to be written with an already-cancelled context and fail. drainEpisodes drops the cancellation so the last activities of a session are kept.
func TestDrainEpisodes_WritesAfterTheRootContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := make(chan tracker.Activity, 1)
	events <- tracker.Activity{App: "brave", Title: "a page open when the daemon was told to stop"}
	close(events)

	seen := make(chan error, 1)
	drainEpisodes(ctx, events, func(writeCtx context.Context, _ db.EpisodeWrite) (int64, error) {
		seen <- writeCtx.Err()
		return 1, nil
	}, nil)

	select {
	case err := <-seen:
		if err != nil {
			t.Fatalf("the episode write got a cancelled context: %v", err)
		}
	default:
		t.Fatal("the episode was never written")
	}
}

// The compiler's Ingest runs the attribution model call inline, so a hung call used to stop the drain, fill the tracker's event channel and end all capture. The ingest now runs off the drain and is dropped when it falls behind.
func TestDrainEpisodes_KeepsWritingWhileAnIngestIsStuck(t *testing.T) {
	const activities = ingestQueueDepth + 10
	events := make(chan tracker.Activity, activities)
	for i := 0; i < activities; i++ {
		events <- tracker.Activity{App: "brave", Title: "a page"}
	}
	close(events)

	stuck := make(chan struct{})
	defer close(stuck)
	var writes atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		drainEpisodes(context.Background(), events, func(context.Context, db.EpisodeWrite) (int64, error) {
			writes.Add(1)
			return 1, nil
		}, func(context.Context, tracker.Activity) { <-stuck })
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain stopped because one ingest never returned")
	}
	if got := writes.Load(); got != activities {
		t.Fatalf("wrote %d episodes, want all %d", got, activities)
	}
}

// The night traces and replay artifacts under <data>/dreams were never pruned, so the Sunday study re-read a growing directory forever.
func TestAgeDreamArtifacts_RemovesOldTracesAndReplays(t *testing.T) {
	dir := t.TempDir()
	dreams := filepath.Join(dir, "dreams")
	if err := os.MkdirAll(dreams, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-dreamArtifactRetention - time.Hour)
	write := func(name string, modTime time.Time) string {
		p := filepath.Join(dreams, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, modTime, modTime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldTrace := write("2026-01-01.jsonl", old)
	oldReplay := write("2026-01-01-replay.md", old)
	freshTrace := write("2026-09-05.jsonl", time.Now())
	notOurs := write("lessons.md", old)

	removed, err := ageDreamArtifacts(dir, dreamArtifactRetention)
	if err != nil {
		t.Fatalf("ageDreamArtifacts: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d files, want 2", removed)
	}
	for _, p := range []string{oldTrace, oldReplay} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s outlived the retention", filepath.Base(p))
		}
	}
	for _, p := range []string{freshTrace, notOurs} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed but should have been kept", filepath.Base(p))
		}
	}
}

// TestEveryMeteredAfter_RecordsEachRun checks the job's last run is written where the next start can read it, so the schedule survives a restart rather than beginning again.
func TestEveryMeteredAfter_RecordsEachRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := dbtest.Open(t)

	ran := make(chan struct{}, 4)
	go everyMeteredAfter(ctx, store, time.Millisecond, 24*time.Hour, "test-metered-job", func() { ran <- struct{}{} })

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the job never ran")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		marker, err := lastJobRun(ctx, store, "test-metered-job")
		if err != nil {
			t.Fatalf("lastJobRun: %v", err)
		}
		if !marker.IsZero() {
			if delay := firstRunDelay(marker, 24*time.Hour, jobFirstRunDelay, time.Now()); delay <= jobFirstRunDelay {
				t.Errorf("a restart right after the run would wait %v, want most of the 24 h interval", delay)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the job ran but recorded no last-run marker, so a restart would run it again two minutes in")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
