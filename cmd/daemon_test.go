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
	"testing"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/tracker"
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

// The weekly study used to glob "evals/replays/*.md" against the working directory alone, so it found material only when the daemon happened to be started from the repo. A packaged install keeps its replays under the data directory.
func TestWeeklyStudyMaterial_ReadsReplaysUnderTheDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "replays"), 0700); err != nil {
		t.Fatalf("make the replays directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replays", "monday.md"), []byte("a replay"), 0600); err != nil {
		t.Fatalf("write a replay: %v", err)
	}

	replays, _ := weeklyStudyMaterial(dir)
	if len(replays) != 1 {
		t.Fatalf("expected the replay under %s/replays to be found, got %v", dir, replays)
	}
}

// With no replays and no dream traces anywhere there is nothing for the study to read, and the caller skips it rather than calling study.Study with two empty lists and logging its "no material found" every Sunday.
func TestWeeklyStudyMaterial_IsEmptyWhenThereIsNothingToRead(t *testing.T) {
	replays, traces := weeklyStudyMaterial(t.TempDir())
	if len(replays) != 0 || len(traces) != 0 {
		t.Fatalf("expected nothing to read, got %d replays and %d traces", len(replays), len(traces))
	}
}

// The tally counters are grouped by provider name, and Codex and Ollama used to fall through to the default and be filed under gemini — which on this machine, whose config is codex-direct, meant every counter the daemon kept named the wrong brain.
func TestBrainProviderName_NamesCodexAndOllama(t *testing.T) {
	for _, provider := range []string{config.BrainCodex, config.BrainOllama, config.BrainClaudeCLI, config.BrainAgyCLI, config.BrainGrokCLI} {
		if got := brainProviderName(config.BrainConfig{Provider: provider}); got != provider {
			t.Errorf("brainProviderName(%q) = %q, want %q", provider, got, provider)
		}
	}
	if got := brainProviderName(config.BrainConfig{}); got != "gemini" {
		t.Errorf("an unset provider is the Gemini API path, got %q", got)
	}
}
