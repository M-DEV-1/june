package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EmbeddingGemma is trained with these instruction prefixes and measurably loses retrieval quality without them, so they are applied here rather than left to callers. Source: the model card's "Prompt instructions" table (Retrieval-query and Retrieval-document rows).
const (
	localQueryPrefix    = "task: search result | query: "
	localDocumentPrefix = "title: none | text: "
)

// LocalEmbedder is the Embedder backed by a local OpenAI-compatible /v1/embeddings server (llama.cpp's llama-server running EmbeddingGemma-300M). Stdlib net/http only, so it adds no dependency and no cgo.
type LocalEmbedder struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewLocalEmbedder points a LocalEmbedder at baseURL (the server root, e.g. "http://127.0.0.1:8080", with or without a trailing slash) and the model name to send in the request body. llama-server ignores the model name but the field is required by OpenAI-compatible clients, so it is sent anyway.
func NewLocalEmbedder(baseURL, model string) *LocalEmbedder {
	return &LocalEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		// A local CPU embed of a long document takes tens of milliseconds, so a minute is generous; the timeout exists to stop a wedged server from hanging a reconcile sweep forever.
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// maxEmbedRunes caps how much of a document is sent for embedding. EmbeddingGemma's context is 2048 tokens and llama-server rejects — rather than truncates — anything longer, so a 96k-character screen capture would otherwise get no vector at all. Real captures in this store run about 2.5 characters per token, so 4000 runes lands near 1600 tokens with headroom for the prefix and for denser text.
const maxEmbedRunes = 4000

// embedShrinkAttempts is how many times Embed halves the text and tries again after the server rejects it as too long. The rune cap above is an average-case estimate; text far denser than average still needs a way through instead of silently losing its vector.
const embedShrinkAttempts = 3

// Embed applies the EmbeddingGemma prefix for task (query prefix for TaskRetrievalQuery, document prefix for everything else) and returns the 768-dimension vector the server produces. Text longer than maxEmbedRunes is truncated, and a server that still rejects it is retried with progressively shorter text. Errors instead of calling the server if text is empty or whitespace-only, and errors on a non-2xx status or a response carrying no embeddings.
func (l *LocalEmbedder) Embed(ctx context.Context, task TaskType, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("embed: text is empty/whitespace-only")
	}

	prefix := localDocumentPrefix
	if task == TaskRetrievalQuery {
		prefix = localQueryPrefix
	}

	runes := []rune(text)
	if len(runes) > maxEmbedRunes {
		runes = runes[:maxEmbedRunes]
	}

	var err error
	for attempt := 0; ; attempt++ {
		var vec []float32
		vec, err = l.embedOnce(ctx, prefix+string(runes))
		if err == nil {
			return vec, nil
		}
		if attempt >= embedShrinkAttempts || len(runes) <= 1 {
			return nil, err
		}
		runes = runes[:len(runes)/2]
	}
}

// embedOnce posts one already-prefixed string to the server and returns the vector it produced.
func (l *LocalEmbedder) embedOnce(ctx context.Context, input string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model": l.model,
		"input": []string{input},
	})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: post to %s: %w", l.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embed: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed: response contained zero embeddings")
	}
	return parsed.Data[0].Embedding, nil
}
