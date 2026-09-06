package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"ora/internal/embed"
	"path/filepath"
	"strings"
	"testing"

	"ora/internal/db"
	"ora/internal/ipctoken"
	"ora/internal/tracker"
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

// TestBufferProvider_NonOKStatus_ReturnsNil verifies a 204 (daemon has no compiler wired) or any other non-200 degrades to nil, not an error or a panic — a dead/half-configured daemon must not break the handshake.
func TestBufferProvider_NonOKStatus_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	b := &bufferProvider{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	if got := b.Get(); got != nil {
		t.Errorf("expected nil for a non-200 response, got %+v", got)
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

// TestHTTPVectorIndex_Add_NonOKStatus_ReturnsError verifies a non-200 daemon response surfaces as a Go error rather than being silently swallowed — HybridSearch's resilience (item 5) depends on Add/Search/Delete actually propagating failures.
func TestHTTPVectorIndex_Add_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := &httpVectorIndex{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	if err := h.Add(context.Background(), "note:5", "hello", []float32{0.1}, nil); err == nil {
		t.Error("expected an error for a non-200 daemon response, got nil")
	}
}

// TestHTTPVectorIndex_Search_ParsesResults verifies Search round-trips the daemon's {"results": [...]} response into []db.Result correctly, including Similarity.
func TestHTTPVectorIndex_Search_ParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vector/search" {
			t.Errorf("expected POST to /vector/search, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []db.Result{
				{ID: "episode:9", Content: "that GPU rabbit hole", Similarity: 0.87, Metadata: map[string]string{"domain": "work"}},
			},
		})
	}))
	defer srv.Close()

	h := &httpVectorIndex{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	results, err := h.Search(context.Background(), []float32{0.1, 0.2}, 10, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ID != "episode:9" || results[0].Similarity != 0.87 {
		t.Errorf("unexpected results: %+v", results)
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

// TestHTTPVectorIndex_Delete_PostsID verifies Delete POSTs the id to /vector/delete.
func TestHTTPVectorIndex_Delete_PostsID(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vector/delete" {
			t.Errorf("expected POST to /vector/delete, got %q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := &httpVectorIndex{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	if err := h.Delete(context.Background(), "note:5"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotBody["id"] != "note:5" {
		t.Errorf("expected id %q in delete request, got %+v", "note:5", gotBody)
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

// TestHTTPEmbedder_NonOKStatus_ReturnsError verifies a daemon that is down or on the Gemini path (503 from /embed) surfaces as an error, which HybridSearch degrades to lexical-only from rather than failing the query.
func TestHTTPEmbedder_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := &httpEmbedder{daemonClient{baseURL: srv.URL, client: srv.Client()}}
	if _, err := h.Embed(context.Background(), embed.TaskRetrievalQuery, "hi"); err == nil {
		t.Fatal("expected an error when the daemon has no local embedder")
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
		{name: "preflight from the dev server when ORA_DEV_ORIGIN names it", method: http.MethodOptions, origin: "http://localhost:1420", devOrigin: "http://localhost:1420", wantCode: http.StatusNoContent, wantACAO: "http://localhost:1420"},
		{name: "the dev server gets nothing in a plain run", method: http.MethodOptions, origin: "http://localhost:1420", wantCode: http.StatusNoContent, wantACAO: ""},
		{name: "request from the window with the token", method: http.MethodGet, origin: "http://tauri.localhost", token: "the-real-token", wantCode: http.StatusOK, wantACAO: "http://tauri.localhost", wantCalled: true},
		{name: "request from the window without the token", method: http.MethodGet, origin: "tauri://localhost", wantCode: http.StatusUnauthorized, wantACAO: "tauri://localhost"},
		{name: "request from any other origin gets no CORS headers", method: http.MethodGet, origin: "https://evil.example", token: "the-real-token", wantCode: http.StatusOK, wantACAO: "", wantCalled: true},
		{name: "preflight from any other origin gets nothing", method: http.MethodOptions, origin: "https://evil.example", wantCode: http.StatusNoContent, wantACAO: ""},
		{name: "no origin at all, as the CLI sends", method: http.MethodGet, token: "the-real-token", wantCode: http.StatusOK, wantACAO: "", wantCalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORA_DEV_ORIGIN", tc.devOrigin)
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

// TestRequireIPCToken_PreflightNamesDelete checks that a preflight from the window is told DELETE is allowed, because the browser refuses to send the window's delete-a-conversation request unless the preflight names the method; on 2026-09-05 the list said only GET and POST and the delete button did nothing.
func TestRequireIPCToken_PreflightNamesDelete(t *testing.T) {
	h := requireIPCToken("the-real-token", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodOptions, "/conversations/1", nil)
	req.Header.Set("Origin", "tauri://localhost")
	req.Header.Set("Access-Control-Request-Method", http.MethodDelete)
	rec := httptest.NewRecorder()
	h(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "DELETE") {
		t.Errorf("Access-Control-Allow-Methods = %q, want DELETE named so the window can delete a conversation", got)
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
