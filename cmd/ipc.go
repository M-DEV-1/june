package cmd

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ora/internal/db"
	"ora/internal/embed"
	"ora/internal/ipctoken"
	"ora/internal/tracker"
)

// This file is the daemon<->client wire. The daemon owns the sqlite store, the chromem vector index and the activity buffer; the client process owns the TUI and the live Gemini session. Anything the client needs from that state travels over local HTTP on 127.0.0.1:<DaemonPort>, authenticated with a shared token file. Server side is requireIPCToken; client side is attachIPCToken plus the three callers below.

// requireIPCToken wraps an HTTP handler so it only runs when the request carries the correct ipctoken.HeaderName value. The compare is constant-time so a timing side-channel can't leak the token a byte at a time. Every daemon IPC handler except /ping is wrapped with this (see startDaemonServices); /ping is the pre-spawn liveness probe root.go polls before the token file is guaranteed to exist, and it reveals nothing.
// An empty token is always rejected, never compared: subtle.ConstantTimeCompare([]byte(""), []byte("")) returns 1, so without this guard a request with no header (or an explicit empty header) would authenticate whenever the daemon's own token is empty — e.g. ipctoken.Generate failed at startup (see startDaemonServices). That would fail auth open instead of leaving IPC unreachable as intended.
func requireIPCToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(ipctoken.HeaderName)
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// attachIPCToken sets the auth header on req if the token file at path is readable. A read failure (daemon not started yet, file missing) just means the request goes out unauthenticated and the daemon 401s it — the caller handles that the same way it handles any other failure. Input: the request to modify and the token file path. Output: none; req is mutated in place.
func attachIPCToken(req *http.Request, path string) {
	if token, err := ipctoken.Read(path); err == nil {
		req.Header.Set(ipctoken.HeaderName, token)
	}
}

// authedDaemonGet fires a fire-and-forget authenticated GET at the local daemon. Used by the tray's pause/resume clicks on both platforms (tray_linux.go, tray_windows.go), which would otherwise each repeat the read-token-attach-header-GET dance.
func authedDaemonGet(url string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	attachIPCToken(req, ipctoken.DefaultPath)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// bufferProvider GETs the daemon's /buffer endpoint, which is how the client process's Agent.buildHandshakeContext sees current activity (the "[working]" lines) at all. Before this existed, Agent.Connect's compiler was always nil client-side and /buffer had no consumers.
type bufferProvider struct {
	baseURL   string
	client    *http.Client
	tokenPath string
}

// newBufferProvider builds a bufferProvider pointed at the daemon's IPC server. The 300ms client timeout is deliberate: this feeds the session handshake, and a dead or slow daemon must not delay the live session connecting.
func newBufferProvider() *bufferProvider {
	return &bufferProvider{
		baseURL:   "http://127.0.0.1:" + DaemonPort,
		client:    &http.Client{Timeout: 300 * time.Millisecond},
		tokenPath: ipctoken.DefaultPath,
	}
}

// Get returns the daemon's current activity buffer, or nil on any failure — unreachable daemon, timeout, non-200 or bad JSON. Agent.buildHandshakeContext treats nil as a no-op, so a dead daemon means the handshake proceeds without this context rather than blocking or erroring.
func (b *bufferProvider) Get() []tracker.Activity {
	req, err := http.NewRequest(http.MethodGet, b.baseURL+"/buffer", nil)
	if err != nil {
		return nil
	}
	attachIPCToken(req, b.tokenPath)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var buf []tracker.Activity
	if err := json.NewDecoder(resp.Body).Decode(&buf); err != nil {
		return nil
	}
	return buf
}

// httpVectorIndex satisfies db.Store's unexported vectorIndex interface (Add/Search/Delete/IDs) by calling the daemon's /vector/* endpoints instead of opening chromem directly. chromem must stay exclusive to the daemon process — two processes opening the same directory risks torn reads and corruption. This is what lets the client's query_memory, RetrieveRelevant and GetImplicitContext reach the semantic half of hybrid search instead of being lexical-only.
type httpVectorIndex struct {
	baseURL   string
	client    *http.Client
	tokenPath string
}

// newHTTPVectorIndex builds an httpVectorIndex pointed at the daemon's IPC server.
func newHTTPVectorIndex() *httpVectorIndex {
	return &httpVectorIndex{
		baseURL:   "http://127.0.0.1:" + DaemonPort,
		client:    &http.Client{Timeout: 10 * time.Second},
		tokenPath: ipctoken.DefaultPath,
	}
}

// post marshals payload as JSON, POSTs it to the daemon at h.baseURL+path with the auth header attached, and returns the response for the caller to decode. The caller must close the body. Input: request context, endpoint path, JSON-marshalable payload. Output: the 200 response, or an error naming which step failed.
func (h *httpVectorIndex) post(ctx context.Context, path string, payload any) (*http.Response, error) {
	return daemonPost(ctx, h.client, h.tokenPath, h.baseURL, path, payload)
}

// daemonPost is the one authenticated client-to-daemon POST every IPC caller in this file uses: marshal, attach the token, refuse anything that isn't a 200. The caller must close the returned body.
func daemonPost(ctx context.Context, client *http.Client, tokenPath, baseURL, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", path, err)
	}
	attachIPCToken(req, tokenPath)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: daemon returned %s", path, resp.Status)
	}
	return resp, nil
}

// httpEmbedder is the client's embed.Embedder: it forwards each embed to the daemon's /embed endpoint rather than talking to llama-server directly. The daemon owns the server process and its port, and routing through it means a client request also wakes a reaped engine and counts as presence, keeping it warm for the rest of the session.
type httpEmbedder struct {
	baseURL   string
	client    *http.Client
	tokenPath string
}

// newHTTPEmbedder builds an httpEmbedder pointed at the daemon's IPC server. The timeout covers a cold start: the daemon may have to spawn llama-server and load the model before it can answer.
func newHTTPEmbedder() *httpEmbedder {
	return &httpEmbedder{
		baseURL:   "http://127.0.0.1:" + DaemonPort,
		client:    &http.Client{Timeout: 60 * time.Second},
		tokenPath: ipctoken.DefaultPath,
	}
}

// Embed sends task and text to the daemon and returns the vector it produced. The EmbeddingGemma prefixes are applied daemon-side, so the text goes over the wire unmodified.
func (h *httpEmbedder) Embed(ctx context.Context, task embed.TaskType, text string) ([]float32, error) {
	resp, err := daemonPost(ctx, h.client, h.tokenPath, h.baseURL, "/embed", map[string]string{
		"task": string(task), "text": text,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode /embed response: %w", err)
	}
	if len(parsed.Embedding) == 0 {
		return nil, fmt.Errorf("/embed: daemon returned no embedding")
	}
	return parsed.Embedding, nil
}

func (h *httpVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	resp, err := h.post(ctx, "/vector/add", map[string]any{
		"id": id, "content": content, "embedding": embedding, "metadata": metadata,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (h *httpVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	resp, err := h.post(ctx, "/vector/search", map[string]any{"embedding": queryEmbedding, "n": n, "where": where})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Results []db.Result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode vector search response: %w", err)
	}
	return out.Results, nil
}

func (h *httpVectorIndex) Delete(ctx context.Context, id string) error {
	resp, err := h.post(ctx, "/vector/delete", map[string]string{"id": id})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// IDs is never called client-side — the reconciliation sweep (db.Store.ReconcileVectors) is daemon-owned and runs directly against the daemon's own *vector.ChromemIndex, not over IPC. No /vector/ids endpoint exists.
func (h *httpVectorIndex) IDs() []string { return nil }
