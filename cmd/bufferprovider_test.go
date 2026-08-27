package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ora/internal/ipctoken"
	"ora/internal/tracker"
)

// TestBufferProvider_ParsesActivities verifies a 200 response with a JSON activity array decodes correctly — this is what feeds Agent.buildHandshakeContext's "[working]" lines (F2).
func TestBufferProvider_ParsesActivities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/buffer" {
			t.Errorf("expected GET /buffer, got %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]tracker.Activity{{App: "Code", Title: "main.go"}})
	}))
	defer srv.Close()

	b := &bufferProvider{baseURL: srv.URL, client: srv.Client()}
	got := b.Get()

	if len(got) != 1 || got[0].App != "Code" || got[0].Title != "main.go" {
		t.Errorf("unexpected buffer: %+v", got)
	}
}

// TestBufferProvider_NonOKStatus_ReturnsNil verifies a 204 (daemon has no compiler wired) or any other non-200 degrades to nil, not an error or a panic — a dead/half-configured daemon must not break the handshake.
func TestBufferProvider_NonOKStatus_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	b := &bufferProvider{baseURL: srv.URL, client: srv.Client()}
	if got := b.Get(); got != nil {
		t.Errorf("expected nil for a non-200 response, got %+v", got)
	}
}

// TestBufferProvider_Unreachable_ReturnsNil verifies a dead daemon (connection refused) degrades to nil instead of blocking or erroring — the handshake must proceed without the working-buffer context rather than stall.
func TestBufferProvider_Unreachable_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	b := &bufferProvider{baseURL: url, client: http.DefaultClient}
	if got := b.Get(); got != nil {
		t.Errorf("expected nil for an unreachable daemon, got %+v", got)
	}
}

// TestBufferProvider_AttachesIPCToken verifies the request carries the daemon's IPC auth token — /buffer is protected like every other IPC endpoint except /ping (W2).
func TestBufferProvider_AttachesIPCToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "ipc-token")
	token, err := ipctoken.Generate(tokenPath)
	if err != nil {
		t.Fatalf("ipctoken.Generate: %v", err)
	}

	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(ipctoken.HeaderName)
		json.NewEncoder(w).Encode([]tracker.Activity{})
	}))
	defer srv.Close()

	b := &bufferProvider{baseURL: srv.URL, client: srv.Client(), tokenPath: tokenPath}
	b.Get()

	if gotToken != token {
		t.Errorf("expected the %s header to carry %q, got %q", ipctoken.HeaderName, token, gotToken)
	}
}
