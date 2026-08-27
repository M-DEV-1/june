package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ora/internal/db"
	"ora/internal/ipctoken"
)

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

