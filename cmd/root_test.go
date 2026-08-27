package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCheckDaemonBuildMismatch_SameIdentity_ReturnsEmpty verifies a daemon reporting this process's own buildIdentity produces no warning — the common case, daemon and client from the same build.
func TestCheckDaemonBuildMismatch_SameIdentity_ReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(buildIdentity))
	}))
	defer srv.Close()

	if got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL); got != "" {
		t.Errorf("expected no warning for a matching identity, got %q", got)
	}
}

// TestCheckDaemonBuildMismatch_DifferentIdentity_ReturnsWarning verifies a daemon reporting a different identity than this process's own produces a non-empty warning mentioning the daemon is stale.
func TestCheckDaemonBuildMismatch_DifferentIdentity_ReturnsWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("some-other-build-identity"))
	}))
	defer srv.Close()

	got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL)
	if got == "" {
		t.Fatal("expected a non-empty warning for a mismatched identity")
	}
	if !strings.Contains(got, "older build") {
		t.Errorf("expected the warning to mention an older build, got %q", got)
	}
}

// TestCheckDaemonBuildMismatch_EmptyBody_ReturnsEmpty verifies an empty response body (e.g. an old daemon binary predating this whole feature, which still answers /ping with an empty 200 rather than 404) degrades to no warning instead of a false-positive mismatch.
func TestCheckDaemonBuildMismatch_EmptyBody_ReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if got := checkDaemonBuildMismatch(http.DefaultClient, srv.URL); got != "" {
		t.Errorf("expected no warning for an empty body, got %q", got)
	}
}

// TestCheckDaemonBuildMismatch_Unreachable_ReturnsEmpty verifies a failed request (daemon gone, network error) fails open — this is a diagnostic, not something that should ever block or warn incorrectly on its own failure.
func TestCheckDaemonBuildMismatch_Unreachable_ReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	if got := checkDaemonBuildMismatch(http.DefaultClient, url); got != "" {
		t.Errorf("expected no warning when the daemon is unreachable, got %q", got)
	}
}

