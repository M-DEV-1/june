package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"june/internal/ipctoken"
)

// requireIPCToken is the daemon's auth boundary: only the exact token gets through, and the wrapped handler must never run otherwise. The empty-token rows cover a startup where ipctoken.Generate failed — subtle.ConstantTimeCompare("", "") returns 1, so without an explicit guard a request with no header would match and auth would fail open.
func TestRequireIPCToken(t *testing.T) {
	cases := []struct {
		name        string
		daemonToken string
		header      string
		setHeader   bool
		query       string
		path        string
		wantCode    int
	}{
		{name: "correct token", daemonToken: "the-real-token", header: "the-real-token", setHeader: true, wantCode: http.StatusOK},
		{name: "correct token in the query on /events, for EventSource which cannot set headers", daemonToken: "the-real-token", query: "the-real-token", path: "/events", wantCode: http.StatusOK},
		{name: "correct token in the query on any other route is ignored, so it never lands in that route's logs", daemonToken: "the-real-token", query: "the-real-token", path: "/ask", wantCode: http.StatusUnauthorized},
		{name: "wrong token", daemonToken: "the-real-token", header: "wrong-token", setHeader: true, wantCode: http.StatusUnauthorized},
		{name: "empty daemon token, empty header", setHeader: true, wantCode: http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := requireIPCToken(tc.daemonToken, func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			url := tc.path
			if url == "" {
				url = "/vector/count"
			}
			if tc.query != "" {
				url += "?token=" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tc.setHeader {
				req.Header.Set(ipctoken.HeaderName, tc.header)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if want := tc.wantCode == http.StatusOK; called != want {
				t.Errorf("wrapped handler called = %v, want %v", called, want)
			}
		})
	}
}

// The desktop window is a webview, so its requests carry an Origin and the browser refuses the response unless the daemon names that origin back. requireIPCToken handles this in one place for every route: the window's origins get the CORS headers, a preflight OPTIONS from them is answered 204 without a token, and any other origin gets no CORS headers at all so a web page on 127.0.0.1 still cannot read anything even with a stolen token in hand.
func TestRequireIPCToken_CORSForTheWindow(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		origin     string
		devOrigin  string
		token      string
		wantCode   int
		wantACAO   string
		wantCalled bool
	}{
		{name: "preflight from the packaged window needs no token", method: http.MethodOptions, origin: "tauri://localhost", wantCode: http.StatusNoContent, wantACAO: "tauri://localhost"},
		{name: "preflight from the dev server when JUNE_DEV_ORIGIN names it", method: http.MethodOptions, origin: "http://localhost:1420", devOrigin: "http://localhost:1420", wantCode: http.StatusNoContent, wantACAO: "http://localhost:1420"},
		{name: "the dev server gets nothing in a plain run", method: http.MethodOptions, origin: "http://localhost:1420", wantCode: http.StatusNoContent, wantACAO: ""},
		{name: "request from the window without the token", method: http.MethodGet, origin: "tauri://localhost", wantCode: http.StatusUnauthorized, wantACAO: "tauri://localhost"},
		{name: "request from any other origin gets no CORS headers", method: http.MethodGet, origin: "https://evil.example", token: "the-real-token", wantCode: http.StatusOK, wantACAO: "", wantCalled: true},
		{name: "no origin at all, as the CLI sends", method: http.MethodGet, token: "the-real-token", wantCode: http.StatusOK, wantACAO: "", wantCalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JUNE_DEV_ORIGIN", tc.devOrigin)
			called := false
			handler := requireIPCToken("the-real-token", func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(tc.method, "/context", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.token != "" {
				req.Header.Set(ipctoken.HeaderName, tc.token)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantACAO {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.wantACAO)
			}
			if tc.wantACAO != "" && rec.Header().Get("Access-Control-Allow-Headers") == "" {
				t.Errorf("Access-Control-Allow-Headers missing for an allowed origin")
			}
			// The browser refuses to send a method the preflight does not name: without DELETE deleting a conversation did nothing, and without PATCH every task owner change failed.
			if tc.method == http.MethodOptions && tc.wantACAO != "" {
				for _, m := range []string{http.MethodDelete, http.MethodPatch} {
					if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, m) {
						t.Errorf("Access-Control-Allow-Methods = %q, want %s named so the window can send it", got, m)
					}
				}
			}
			if called != tc.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

// Every route answers a CORS preflight, including the ones registered with a method in their pattern. Go's mux routes a pattern written "POST /x" to POST alone, so an OPTIONS preflight never reaches the handler requireIPCToken wraps and is answered 405 with no CORS headers — at which point the browser blocks the real request, the window's fetch rejects, and the card says "Could not do that" while the daemon logs nothing, because the POST was never sent. Every notice button was dead this way; the CORS test above missed it because it calls requireIPCToken directly rather than through a mux.
func TestWithPreflight_AnswersAMethodPrefixedRoute(t *testing.T) {
	auth := func(h http.HandlerFunc) http.HandlerFunc { return requireIPCToken("tok", h) }
	mux := http.NewServeMux()
	// The notice button's own registration, copied from registerDaemonRoutes, plus a plain route that always worked.
	mux.HandleFunc("POST /notices/{kind}/{id}/action", auth(func(w http.ResponseWriter, r *http.Request) {}))
	mux.HandleFunc("/ask", auth(func(w http.ResponseWriter, r *http.Request) {}))
	h := withPreflight(mux, auth)

	for _, path := range []string{"/notices/task/42/action", "/ask"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "tauri://localhost")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s = %d, want 204 — the browser blocks the real request without it", path, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "tauri://localhost" {
			t.Errorf("OPTIONS %s named the origin back as %q, want the window's own origin", path, got)
		}
	}

	// The catch-all answers preflights only: a real request to a path no route claims still gets the mux's 404, not a silent 204 that would make a typo in the window look like a request that worked.
	req := httptest.NewRequest(http.MethodPost, "/no/such/route", nil)
	req.Header.Set("Origin", "tauri://localhost")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST to a path no route claims = %d, want 404", rec.Code)
	}
}
