package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"june/internal/embed"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"june/internal/ipctoken"
	"june/internal/tracker"
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
		{name: "wrong token in the query on /events", daemonToken: "the-real-token", query: "wrong-token", path: "/events", wantCode: http.StatusUnauthorized},
		{name: "empty daemon token, a token in the query on /events still fails closed", query: "the-real-token", path: "/events", wantCode: http.StatusUnauthorized},
		{name: "correct token in the query on any other route is ignored, so it never lands in that route's logs", daemonToken: "the-real-token", query: "the-real-token", path: "/ask", wantCode: http.StatusUnauthorized},
		{name: "correct token in the query on the daemon root is ignored", daemonToken: "the-real-token", query: "the-real-token", path: "/vector/count", wantCode: http.StatusUnauthorized},
		{name: "missing header", daemonToken: "the-real-token", wantCode: http.StatusUnauthorized},
		{name: "wrong token", daemonToken: "the-real-token", header: "wrong-token", setHeader: true, wantCode: http.StatusUnauthorized},
		{name: "empty daemon token, no header", wantCode: http.StatusUnauthorized},
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

// TestBufferProvider_Unreachable_ReturnsNil verifies a dead daemon (connection refused) degrades to nil instead of blocking or erroring — the handshake must proceed without the working-buffer context rather than stall.
func TestBufferProvider_Unreachable_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	b := &bufferProvider{daemonClient{baseURL: url, client: http.DefaultClient}}
	if got := b.Get(); got != nil {
		t.Errorf("expected nil for an unreachable daemon, got %+v", got)
	}
}

// TestBufferProvider_AttachesIPCToken verifies the request carries the daemon's IPC auth token — /buffer is protected like every other IPC endpoint except /ping (W2) — and that a 200 response with a JSON activity array decodes correctly, which is what feeds Agent.buildHandshakeContext's "[working]" lines (F2).
func TestBufferProvider_AttachesIPCToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "ipc-token")
	token, err := ipctoken.Generate(tokenPath)
	if err != nil {
		t.Fatalf("ipctoken.Generate: %v", err)
	}

	var gotToken, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(ipctoken.HeaderName)
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode([]tracker.Activity{{App: "Code", Title: "main.go"}})
	}))
	defer srv.Close()

	b := &bufferProvider{daemonClient{baseURL: srv.URL, client: srv.Client(), tokenPath: tokenPath}}
	got := b.Get()

	if gotToken != token {
		t.Errorf("expected the %s header to carry %q, got %q", ipctoken.HeaderName, token, gotToken)
	}
	if gotPath != "/buffer" {
		t.Errorf("expected GET /buffer, got %s", gotPath)
	}
	if len(got) != 1 || got[0].App != "Code" || got[0].Title != "main.go" {
		t.Errorf("unexpected buffer: %+v", got)
	}
}

// TestHTTPVectorIndex_Add_PostsExpectedPayload verifies Add POSTs to /vector/add with the id/content/embedding/metadata fields the daemon handler expects, and that the request carries the daemon's IPC auth token — without it, every /vector/* call now gets 401'd by requireIPCToken (see W2's security fix).
func TestHTTPVectorIndex_Add_PostsExpectedPayload(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "ipc-token")
	token, err := ipctoken.Generate(tokenPath)
	if err != nil {
		t.Fatalf("ipctoken.Generate: %v", err)
	}

	var gotPath, gotToken string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get(ipctoken.HeaderName)
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := &httpVectorIndex{daemonClient{baseURL: srv.URL, client: srv.Client(), tokenPath: tokenPath}}
	if err := h.Add(context.Background(), "note:5", "hello", []float32{0.1, 0.2}, map[string]string{"source": "note"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if gotPath != "/vector/add" {
		t.Errorf("expected POST to /vector/add, got %q", gotPath)
	}
	if gotBody["id"] != "note:5" || gotBody["content"] != "hello" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	if gotToken != token {
		t.Errorf("expected the %s header to carry %q, got %q", ipctoken.HeaderName, token, gotToken)
	}
}

// TestHTTPVectorIndex_Search_ServerError_ReturnsError verifies a daemon-side failure (e.g. daemon down, connection refused) surfaces as an error instead of an empty/silent result — this is exactly the failure mode HybridSearch's degrade-to-lexical-only handling (item 5) exists for.
func TestHTTPVectorIndex_Search_ServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := &httpVectorIndex{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	if _, err := h.Search(context.Background(), []float32{0.1}, 10, nil); err == nil {
		t.Error("expected an error for a 500 daemon response, got nil")
	}
}

// TestHTTPEmbedder_PostsTaskAndText verifies the client's embedder reaches the daemon's /embed endpoint with the task and text, and returns the vector the daemon produced. The client cannot run its own llama-server — one process owns the child and the port — so this IPC hop is its only route to a local embedding.
func TestHTTPEmbedder_PostsTaskAndText(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{0.25, 0.5}})
	}))
	defer srv.Close()

	h := &httpEmbedder{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	vec, err := h.Embed(context.Background(), embed.TaskRetrievalQuery, "what did i do today")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/embed" {
		t.Errorf("expected POST to /embed, got %q", gotPath)
	}
	if gotBody["task"] != "RETRIEVAL_QUERY" || gotBody["text"] != "what did i do today" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	if len(vec) != 2 || vec[0] != 0.25 {
		t.Errorf("unexpected vector: %v", vec)
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
		{name: "request from the window with the token", method: http.MethodGet, origin: "http://tauri.localhost", token: "the-real-token", wantCode: http.StatusOK, wantACAO: "http://tauri.localhost", wantCalled: true},
		{name: "request from the window without the token", method: http.MethodGet, origin: "tauri://localhost", wantCode: http.StatusUnauthorized, wantACAO: "tauri://localhost"},
		{name: "request from any other origin gets no CORS headers", method: http.MethodGet, origin: "https://evil.example", token: "the-real-token", wantCode: http.StatusOK, wantACAO: "", wantCalled: true},
		{name: "preflight from any other origin gets nothing", method: http.MethodOptions, origin: "https://evil.example", wantCode: http.StatusNoContent, wantACAO: ""},
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
			if called != tc.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

// TestRequireIPCToken_PreflightNamesTheWindowsMethods checks that a preflight from the window is told every method the window sends is allowed, because the browser refuses to send a request whose method the preflight does not name.
// On 2026-09-05 the list said only GET and POST and deleting a conversation did nothing; on 2026-09-23 it lacked PATCH and every owner change (Mine, Theirs, Unclear) failed with "Could not change who owns that task".
func TestRequireIPCToken_PreflightNamesTheWindowsMethods(t *testing.T) {
	h := requireIPCToken("the-real-token", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, method := range []string{http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(http.MethodOptions, "/tasks/1", nil)
		req.Header.Set("Origin", "tauri://localhost")
		req.Header.Set("Access-Control-Request-Method", method)
		rec := httptest.NewRecorder()
		h(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, method) {
			t.Errorf("Access-Control-Allow-Methods = %q, want %s named so the window can send it", got, method)
		}
	}
}

// TestRequireIPCToken_CapsTheBody checks an authenticated request's body stops at ipcBodyLimit, so a local page that holds the token still cannot make the daemon buffer an arbitrarily large JSON body.
func TestRequireIPCToken_CapsTheBody(t *testing.T) {
	var readErr error
	handler := requireIPCToken("tok", func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})
	for _, c := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"a body under the cap reads whole", 1 << 10, false},
		{"a body over the cap fails to read", ipcBodyLimit + 1, true},
	} {
		readErr = nil
		req := httptest.NewRequest(http.MethodPost, "/ask", bytes.NewReader(make([]byte, c.size)))
		req.Header.Set(ipctoken.HeaderName, "tok")
		handler(httptest.NewRecorder(), req)
		if (readErr != nil) != c.wantErr {
			t.Errorf("%s: read error %v, want error %v", c.name, readErr, c.wantErr)
		}
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
}

// The catch-all answers preflights only. A real request to a path no route claims still gets the mux's own 404, rather than a silent 204 that would make a typo in the window look like a request that worked.
func TestWithPreflight_LeavesRealRequestsToTheirOwnRoutes(t *testing.T) {
	auth := func(h http.HandlerFunc) http.HandlerFunc { return requireIPCToken("tok", h) }
	mux := http.NewServeMux()
	h := withPreflight(mux, auth)

	req := httptest.NewRequest(http.MethodPost, "/no/such/route", nil)
	req.Header.Set("Origin", "tauri://localhost")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("POST to a path no route claims = %d, want 404", rec.Code)
	}
}
