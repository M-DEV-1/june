package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// localTestServer stands in for llama-server's /v1/embeddings, recording the request body and replying with vec.
func localTestServer(t *testing.T, vec []float32, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("expected path /v1/embeddings, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if captured != nil {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Errorf("request body was not JSON: %v", err)
			}
			*captured = m
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": vec, "index": 0}},
		})
	}))
}

func TestLocalEmbedderPrefixesDocumentAndQuery(t *testing.T) {
	cases := []struct {
		name   string
		task   TaskType
		text   string
		expect string
	}{
		{"document", TaskRetrievalDocument, "the mitochondria", "title: none | text: the mitochondria"},
		{"query", TaskRetrievalQuery, "what is a mitochondrion", "task: search result | query: what is a mitochondrion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := localTestServer(t, []float32{0.1, 0.2}, &got)
			defer srv.Close()

			e := NewLocalEmbedder(srv.URL, "embeddinggemma-300m")
			vec, err := e.Embed(context.Background(), tc.task, tc.text)
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if len(vec) != 2 || vec[0] != 0.1 {
				t.Fatalf("unexpected vector: %v", vec)
			}
			if got["model"] != "embeddinggemma-300m" {
				t.Errorf("model = %v, want embeddinggemma-300m", got["model"])
			}
			input, ok := got["input"].([]any)
			if !ok || len(input) != 1 {
				t.Fatalf("input = %v, want a one-element array", got["input"])
			}
			if input[0] != tc.expect {
				t.Errorf("input[0] = %q, want %q", input[0], tc.expect)
			}
		})
	}
}

func TestLocalEmbedderUnknownTaskIsDocument(t *testing.T) {
	var got map[string]any
	srv := localTestServer(t, []float32{1}, &got)
	defer srv.Close()

	if _, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskType("SOMETHING_ELSE"), "hi"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got["input"].([]any)[0] != "title: none | text: hi" {
		t.Errorf("unknown task should use the document prefix, got %v", got["input"])
	}
}

func TestLocalEmbedderTrimsTrailingSlashOnBaseURL(t *testing.T) {
	srv := localTestServer(t, []float32{1}, nil)
	defer srv.Close()

	if _, err := NewLocalEmbedder(srv.URL+"/", "m").Embed(context.Background(), TaskRetrievalQuery, "hi"); err != nil {
		t.Fatalf("Embed with trailing slash in base URL: %v", err)
	}
}

func TestLocalEmbedderRejectsEmptyText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called for empty text")
	}))
	defer srv.Close()

	if _, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskRetrievalDocument, "   \n "); err == nil {
		t.Fatal("expected an error for whitespace-only text")
	}
}

func TestLocalEmbedderErrorsOnHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskRetrievalDocument, "hi")
	if err == nil {
		t.Fatal("expected an error on a 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should name the status code, got %v", err)
	}
}

func TestLocalEmbedderErrorsOnEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskRetrievalDocument, "hi"); err == nil {
		t.Fatal("expected an error when the response carries no embeddings")
	}
}

// TestLocalEmbedderDoesNotRetryAServerFailure verifies a failure that has nothing to do with the input's length (here a 503 from a server still loading its model) costs exactly one request. Halving and retrying that quadruples the load on a server that is already struggling, and shorter text would not have helped anyway.
func TestLocalEmbedderDoesNotRetryAServerFailure(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskRetrievalDocument, strings.Repeat("b", 800)); err == nil {
		t.Fatal("expected an error on a 503")
	}
	if requests != 1 {
		t.Errorf("made %d requests for one unretryable failure, want 1", requests)
	}
}

// TestInputTooLong covers the classifier the retry hangs off: only a rejection about the input's size is worth sending shorter text for. Transport failures, cancelled contexts and a server that is merely unwell are not.
func TestInputTooLong(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"llama-server oversized input", http.StatusInternalServerError, "input is too large to process. increase the physical batch size", true},
		{"exceeds context", http.StatusInternalServerError, "the request exceeds the available context size", true},
		{"bad request", http.StatusBadRequest, "invalid input", true},
		{"model still loading", http.StatusServiceUnavailable, "model not loaded", false},
		{"plain server error", http.StatusInternalServerError, "unexpected failure", false},
		{"no response at all", 0, "", false},
	}
	for _, c := range cases {
		if got := inputTooLong(c.status, c.body); got != c.want {
			t.Errorf("%s: inputTooLong(%d, %q) = %v, want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// TestLocalEmbedderTruncatesLongText verifies the request body never carries more than maxEmbedRunes of the caller's text. EmbeddingGemma's context is 2048 tokens and llama-server rejects anything longer outright, so an untruncated 96k-character screen capture (the longest in the real store) would just fail and leave that episode with no vector at all.
func TestLocalEmbedderTruncatesLongText(t *testing.T) {
	var got map[string]any
	srv := localTestServer(t, []float32{1}, &got)
	defer srv.Close()

	long := strings.Repeat("a", maxEmbedRunes*3)
	if _, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskRetrievalDocument, long); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	sent := got["input"].([]any)[0].(string)
	body := strings.TrimPrefix(sent, localDocumentPrefix)
	if len([]rune(body)) != maxEmbedRunes {
		t.Errorf("sent %d runes of text, want it capped at %d", len([]rune(body)), maxEmbedRunes)
	}
}

// TestLocalEmbedderRetriesShorterAfterRejection verifies that a server rejecting an input as too long gets retried with half the text rather than the document being dropped. The rune cap is set from an average characters-per-token ratio; text denser than that average would otherwise silently lose its vector.
func TestLocalEmbedderRetriesShorterAfterRejection(t *testing.T) {
	var lengths []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		n := len([]rune(req.Input[0]))
		lengths = append(lengths, n)
		if n > 200 {
			http.Error(w, "input is too large to process", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1}}}})
	}))
	defer srv.Close()

	vec, err := NewLocalEmbedder(srv.URL, "m").Embed(context.Background(), TaskRetrievalDocument, strings.Repeat("b", 800))
	if err != nil {
		t.Fatalf("Embed should have succeeded after shortening: %v", err)
	}
	if len(vec) != 1 {
		t.Fatalf("unexpected vector: %v", vec)
	}
	if len(lengths) < 2 {
		t.Fatalf("expected at least one retry, got attempt lengths %v", lengths)
	}
	for i := 1; i < len(lengths); i++ {
		if lengths[i] >= lengths[i-1] {
			t.Errorf("attempt %d was not shorter than the one before: %v", i, lengths)
		}
	}
}
