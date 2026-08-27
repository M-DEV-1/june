package cmd

import (
	"ora/internal/embed"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ora/internal/db"
	"ora/internal/ipctoken"
	"ora/internal/tracker"
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

// TestRequireIPCToken_EmptyToken_Returns401 verifies that when the daemon's own token is empty (ipctoken.Generate failed at startup), requireIPCToken rejects every request rather than authenticating it. subtle.ConstantTimeCompare([]byte(""), []byte("")) returns 1, so without an explicit guard a request with no header (or an explicit empty header) would match an empty token and auth would fail open.
func TestRequireIPCToken_EmptyToken_Returns401(t *testing.T) {
	called := false
	handler := requireIPCToken("", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/vector/count", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-header request against an empty token: expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("expected the wrapped handler not to run against an empty token with no header")
	}

	called = false
	req2 := httptest.NewRequest(http.MethodGet, "/vector/count", nil)
	req2.Header.Set(ipctoken.HeaderName, "")
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("empty-header request against an empty token: expected 401, got %d", rec2.Code)
	}
	if called {
		t.Error("expected the wrapped handler not to run against an empty token with an empty header")
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

// TestHTTPVectorIndex_Add_PostsExpectedPayload verifies Add POSTs to /vector/add with the id/content/embedding/metadata fields the daemon handler expects.
func TestHTTPVectorIndex_Add_PostsExpectedPayload(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := &httpVectorIndex{baseURL: srv.URL, client: srv.Client()}
	err := h.Add(context.Background(), "note:5", "hello", []float32{0.1, 0.2}, map[string]string{"source": "note"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if gotPath != "/vector/add" {
		t.Errorf("expected POST to /vector/add, got %q", gotPath)
	}
	if gotBody["id"] != "note:5" || gotBody["content"] != "hello" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
}

// TestHTTPVectorIndex_Add_NonOKStatus_ReturnsError verifies a non-200 daemon response surfaces as a Go error rather than being silently swallowed — HybridSearch's resilience (item 5) depends on Add/Search/Delete actually propagating failures.
func TestHTTPVectorIndex_Add_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := &httpVectorIndex{baseURL: srv.URL, client: srv.Client()}
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

	h := &httpVectorIndex{baseURL: srv.URL, client: srv.Client()}
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

	h := &httpVectorIndex{baseURL: srv.URL, client: srv.Client()}
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

	h := &httpVectorIndex{baseURL: srv.URL, client: srv.Client()}
	if err := h.Delete(context.Background(), "note:5"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotBody["id"] != "note:5" {
		t.Errorf("expected id %q in delete request, got %+v", "note:5", gotBody)
	}
}

// TestHTTPVectorIndex_AttachesIPCToken verifies every request carries the daemon's IPC auth token — without it, every /vector/* call now gets 401'd by requireIPCToken (see W2's security fix).
func TestHTTPVectorIndex_AttachesIPCToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "ipc-token")
	token, err := ipctoken.Generate(tokenPath)
	if err != nil {
		t.Fatalf("ipctoken.Generate: %v", err)
	}

	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(ipctoken.HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := &httpVectorIndex{baseURL: srv.URL, client: srv.Client(), tokenPath: tokenPath}
	if err := h.Add(context.Background(), "note:5", "hello", []float32{0.1}, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if gotToken != token {
		t.Errorf("expected the %s header to carry %q, got %q", ipctoken.HeaderName, token, gotToken)
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

	h := &httpEmbedder{baseURL: srv.URL, client: srv.Client()}
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

	h := &httpEmbedder{baseURL: srv.URL, client: srv.Client()}
	if _, err := h.Embed(context.Background(), embed.TaskRetrievalQuery, "hi"); err == nil {
		t.Fatal("expected an error when the daemon has no local embedder")
	}
}

func TestHTTPEmbedderSatisfiesEmbedder(t *testing.T) {
	var _ embed.Embedder = newHTTPEmbedder()
}
