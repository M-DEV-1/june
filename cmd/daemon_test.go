package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
