package ipc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A chat reply's link posts here instead of the webview trying window.open, so the test only ever checks what URL this handler would have handed to the system opener — nothing here launches a real browser.
func TestOpen_StartsTheCommandForHTTPAndHTTPS(t *testing.T) {
	for _, u := range []string{"http://example.com", "https://example.com/path?q=1"} {
		var got string
		run := func(url string) error { got = url; return nil }
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/open", strings.NewReader(`{"url":"`+u+`"}`))
		Open(run)(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: status %d, want %d", u, rec.Code, http.StatusNoContent)
		}
		if got != u {
			t.Errorf("%s: runner called with %q", u, got)
		}
	}
}

// A scheme that is neither http nor https is refused before the runner is ever called, so a reply cannot smuggle a javascript: or file: link into a command this handler starts.
func TestOpen_RejectsAnyOtherScheme(t *testing.T) {
	for _, u := range []string{"javascript:alert(1)", "file:///etc/passwd", "ftp://example.com", "", "not a url"} {
		called := false
		run := func(url string) error { called = true; return nil }
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/open", strings.NewReader(`{"url":"`+u+`"}`))
		Open(run)(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want %d", u, rec.Code, http.StatusBadRequest)
		}
		if called {
			t.Errorf("%q: runner was called for a rejected scheme", u)
		}
	}
}

// A body that will not decode as JSON is refused the same way every other route's DecodeJSON refuses one.
func TestOpen_RejectsUnparseableBody(t *testing.T) {
	run := func(url string) error { return nil }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/open", strings.NewReader(`not json`))
	Open(run)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// A runner that fails to start the command is reported as a server error rather than a silent 204, so the window knows the link did not open.
func TestOpen_ReportsARunnerFailure(t *testing.T) {
	run := func(url string) error { return errors.New("no such program") }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/open", strings.NewReader(`{"url":"https://example.com"}`))
	Open(run)(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}
