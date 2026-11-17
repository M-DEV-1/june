package agent

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/config"
)

// stallingBackend answers every request for stalledModel by holding it open until the client gives up, and every other request with a one-line answer. It records the model each request named.
func stallingBackend(t *testing.T, stalledModel string) (url string, models func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body is read first so the server notices the client hanging up.
		io.Copy(io.Discard, r.Body)
		model := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ":generateContent")
		mu.Lock()
		seen = append(seen, model)
		mu.Unlock()
		if model == stalledModel {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"answered"}]}}]}`)
	}))
	t.Cleanup(backend.Close)
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })
	return backend.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A request that stalls past geminiRequestTimeout fails as errRequestStalled long before the ask's wall clock, and askGemini then asks the fallback model, the same as it does for a 503.
func TestAskGemini_StalledRequestFallsBackToAnotherModel(t *testing.T) {
	_, models := stallingBackend(t, config.TextModel)
	old := geminiRequestTimeout
	geminiRequestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { geminiRequestTimeout = old })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	start := time.Now()
	tr, err := a.askGemini(t.Context(), nil, "hello")
	if err != nil {
		t.Fatalf("askGemini: %v", err)
	}
	if tr.Answer != "answered" {
		t.Errorf("answer = %q, want the fallback model's", tr.Answer)
	}
	if got := models(); len(got) != 2 || got[0] != config.TextModel || got[1] != config.TextFallbackModel {
		t.Errorf("models asked = %v, want %s then %s", got, config.TextModel, config.TextFallbackModel)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, want the stall cut at the request timeout", elapsed)
	}
}

// With no model left to answer, the stalled request's error says so and satisfies errRequestStalled.
func TestAskText_StalledRequestFailsAsStalled(t *testing.T) {
	stallingBackend(t, "gemini-test")
	old := geminiRequestTimeout
	geminiRequestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { geminiRequestTimeout = old })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	_, err := a.askText(t.Context(), "gemini-test", nil, "hello")
	if !errors.Is(err, errRequestStalled) {
		t.Fatalf("err = %v, want errRequestStalled", err)
	}
}
