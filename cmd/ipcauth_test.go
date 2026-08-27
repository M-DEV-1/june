package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ora/internal/ipctoken"
)

// TestRequireIPCToken_MissingHeader_Returns401 verifies a request with no token header is rejected before the wrapped handler ever runs.
func TestRequireIPCToken_MissingHeader_Returns401(t *testing.T) {
	called := false
	handler := requireIPCToken("the-real-token", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/vector/count", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("expected the wrapped handler not to run without a token")
	}
}

// TestRequireIPCToken_WrongToken_Returns401 verifies an incorrect token is also rejected, not just a missing one.
func TestRequireIPCToken_WrongToken_Returns401(t *testing.T) {
	called := false
	handler := requireIPCToken("the-real-token", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/vector/count", nil)
	req.Header.Set(ipctoken.HeaderName, "wrong-token")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("expected the wrapped handler not to run with the wrong token")
	}
}

// TestRequireIPCToken_CorrectToken_CallsWrappedHandler verifies the right token lets the request through.
func TestRequireIPCToken_CorrectToken_CallsWrappedHandler(t *testing.T) {
	called := false
	handler := requireIPCToken("the-real-token", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/vector/count", nil)
	req.Header.Set(ipctoken.HeaderName, "the-real-token")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !called {
		t.Error("expected the wrapped handler to run with the correct token")
	}
}
