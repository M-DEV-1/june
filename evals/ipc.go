package main

// This is the client half of the daemon IPC wire, reimplemented here because the original lives in package cmd, which is a command package the eval runner cannot import. It mirrors cmd/ipc.go: same endpoints, same token header, same JSON shapes. Only the two calls hybrid search needs are here — /embed and /vector/search — because the eval runner never writes.

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

// daemonAddr is the local daemon's IPC base URL. cmd.DaemonPort is unexported from a main-flavoured package, so the port is repeated here.
const daemonAddr = "http://127.0.0.1:6942"

// daemonClient posts JSON to the daemon with the shared-secret header attached, refusing anything that is not a 200.
type daemonClient struct {
	client *http.Client
}

// post marshals payload as JSON, POSTs it to the daemon at path with the auth header attached, and decodes the 200 body into out. Input: request context, endpoint path, request payload, pointer to decode into. Output: an error naming which step failed, or nil.
func (d daemonClient) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonAddr+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	ipctoken.Attach(req, ipctoken.DefaultPath)
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: daemon returned %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// httpEmbedder satisfies the embedder interface db.Store.SetEmbedder expects, forwarding each embed to the daemon's /embed endpoint. The daemon owns the llama-server child and its port, so this is the only route to a vector from outside it. The EmbeddingGemma prefixes are applied daemon-side, so text goes over the wire unmodified.
type httpEmbedder struct{ daemonClient }

// Embed sends task and text to the daemon and returns the vector it produced. Input: a task name ("RETRIEVAL_QUERY" or "RETRIEVAL_DOCUMENT") and the text. Output: the embedding, or an error.
func (h httpEmbedder) Embed(ctx context.Context, task, text string) ([]float32, error) {
	var parsed struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := h.post(ctx, "/embed", map[string]string{"task": task, "text": text}, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Embedding) == 0 {
		return nil, fmt.Errorf("/embed: daemon returned no embedding")
	}
	return parsed.Embedding, nil
}

// httpVectorIndex satisfies db.Store.SetVectorIndex by calling the daemon's /vector/search. chromem must stay exclusive to the daemon process, so the eval runner reads the semantic half of memory through the daemon exactly as the live client does. Add and Delete are never called by a read-only replay and return an error rather than silently succeeding.
type httpVectorIndex struct{ daemonClient }

func (h httpVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	var out struct {
		Results []db.Result `json:"results"`
	}
	err := h.post(ctx, "/vector/search", map[string]any{"embedding": queryEmbedding, "n": n, "where": where}, &out)
	return out.Results, err
}

func (h httpVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	return fmt.Errorf("eval runner is read-only: refusing vector add for %s", id)
}

func (h httpVectorIndex) Delete(ctx context.Context, id string) error {
	return fmt.Errorf("eval runner is read-only: refusing vector delete for %s", id)
}

func (h httpVectorIndex) IDs() []string { return nil }

// newDaemonClients builds the embedder and vector index the eval runner wires into a Store. The embed timeout covers a cold start where the daemon has to spawn llama-server and load the model first.
func newDaemonClients() (httpEmbedder, httpVectorIndex) {
	return httpEmbedder{daemonClient{&http.Client{Timeout: 60 * time.Second}}},
		httpVectorIndex{daemonClient{&http.Client{Timeout: 30 * time.Second}}}
}
