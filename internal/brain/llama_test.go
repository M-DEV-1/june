package brain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The happy path: the server's OpenAI-shaped reply is read off its message content, and the request carries the shadow's required shape — a single user message, and thinking left enabled so the reasoning trace comes back too.
func TestLlamaServer_HappyPath(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("server could not decode the request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"<thought>reasoning</thought>the answer"}}]}`))
	}))
	defer srv.Close()

	got, err := LlamaServer(srv.URL, 5)(context.Background(), "what happened tonight?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "<thought>reasoning</thought>the answer" {
		t.Errorf("got %q, want the message content verbatim", got)
	}

	messages, _ := gotBody["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want exactly one", gotBody["messages"])
	}
	msg := messages[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "what happened tonight?" {
		t.Errorf("message = %v, want the prompt as a single user message", msg)
	}
	kwargs, _ := gotBody["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != true {
		t.Errorf("chat_template_kwargs = %v, want enable_thinking:true so the shadow's reasoning trace comes back", gotBody["chat_template_kwargs"])
	}
}

// A non-2xx status is an error carrying what the server said, not an empty answer.
func TestLlamaServer_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("context size exceeded"))
	}))
	defer srv.Close()

	_, err := LlamaServer(srv.URL, 5)(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want it to name the 500 status", err)
	}
}

// A reply with no choices, or a choice with empty content, is an error rather than a silent empty string.
func TestLlamaServer_EmptyReply(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no choices", `{"choices":[]}`},
		{"blank content", `{"choices":[{"message":{"content":"   "}}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			if _, err := LlamaServer(srv.URL, 5)(context.Background(), "x"); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

// A slow server past the timeout fails instead of hanging the caller forever.
func TestLlamaServer_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	_, err := LlamaServer(srv.URL, 0)(context.Background(), "x")
	if err == nil {
		t.Fatal("expected the near-zero timeout to fail the call")
	}
}
