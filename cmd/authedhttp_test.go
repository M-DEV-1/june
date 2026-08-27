package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthedDaemonGet_HitsExpectedURL is a smoke test: authedDaemonGet must not panic and must actually reach the given URL (token attachment itself is covered by ipctoken's own tests and httpVectorIndex's TestHTTPVectorIndex_AttachesIPCToken, which exercises the identical read-and-set pattern).
func TestAuthedDaemonGet_HitsExpectedURL(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	authedDaemonGet(srv.URL)

	if !hit {
		t.Error("expected authedDaemonGet to reach the server")
	}
}
