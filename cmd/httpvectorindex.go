package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ora/internal/db"
	"ora/internal/ipctoken"
)

// httpVectorIndex satisfies db.Store's unexported vectorIndex interface (Add/Search/Delete/IDs) by calling the daemon's /vector/* IPC endpoints instead of opening chromem directly — chromem must stay exclusive to the daemon process (two processes opening the same dir risks torn reads/corruption). This is what lets the client process's query_memory/RetrieveRelevant/GetImplicitContext actually reach the semantic half of hybrid search, instead of being lexical-only as it was before this existed.
type httpVectorIndex struct {
	baseURL   string
	client    *http.Client
	tokenPath string
}

// newHTTPVectorIndex builds an httpVectorIndex pointed at the daemon's IPC server on 127.0.0.1:<DaemonPort>.
func newHTTPVectorIndex() *httpVectorIndex {
	return &httpVectorIndex{
		baseURL:   "http://127.0.0.1:" + DaemonPort,
		client:    &http.Client{Timeout: 10 * time.Second},
		tokenPath: ipctoken.DefaultPath,
	}
}

// attachToken sets the IPC auth header if the token file is readable — every daemon handler except /ping requires it (see requireIPCToken). A read failure (daemon hasn't started yet, file not there) just means the request goes out without it and the daemon 401s it — same graceful-failure shape every other call site here already has.
func (h *httpVectorIndex) attachToken(req *http.Request) {
	if token, err := ipctoken.Read(h.tokenPath); err == nil {
		req.Header.Set(ipctoken.HeaderName, token)
	}
}

func (h *httpVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	body, err := json.Marshal(map[string]any{
		"id": id, "content": content, "embedding": embedding, "metadata": metadata,
	})
	if err != nil {
		return fmt.Errorf("marshal vector add request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/vector/add", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build vector add request: %w", err)
	}
	h.attachToken(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("vector add: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector add: daemon returned %s", resp.Status)
	}
	return nil
}

func (h *httpVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	body, err := json.Marshal(map[string]any{"embedding": queryEmbedding, "n": n, "where": where})
	if err != nil {
		return nil, fmt.Errorf("marshal vector search request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/vector/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build vector search request: %w", err)
	}
	h.attachToken(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vector search: daemon returned %s", resp.Status)
	}
	var out struct {
		Results []db.Result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode vector search response: %w", err)
	}
	return out.Results, nil
}

func (h *httpVectorIndex) Delete(ctx context.Context, id string) error {
	body, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		return fmt.Errorf("marshal vector delete request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/vector/delete", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build vector delete request: %w", err)
	}
	h.attachToken(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("vector delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector delete: daemon returned %s", resp.Status)
	}
	return nil
}

// IDs is never called client-side — the reconciliation sweep (db.Store.ReconcileVectors) is daemon-owned, run directly against the daemon's own concrete *vector.ChromemIndex, not over IPC. No /vector/ids endpoint exists.
func (h *httpVectorIndex) IDs() []string { return nil }
