package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// A text ask that makes one tool call and then answers records one Ask.Text span with an Ask.ModelRound span for each of its two model calls and an Ask.Tool span for the call, all children of the ask.
func TestAskText_RecordsAskRoundAndToolSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"add_task","args":{"title":"x"}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	if _, err := a.askText(t.Context(), "gemini-test", nil, "file a task"); err != nil {
		t.Fatal(err)
	}

	byName := map[string][]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = append(byName[s.Name], s)
	}
	if len(byName["Ask.Text"]) != 1 || len(byName["Ask.ModelRound"]) != 2 || len(byName["Ask.Tool"]) != 1 {
		t.Fatalf("spans = %v, want 1 Ask.Text, 2 Ask.ModelRound, 1 Ask.Tool", fmt.Sprint(byName))
	}
	ask := byName["Ask.Text"][0].SpanContext.SpanID()
	for _, name := range []string{"Ask.ModelRound", "Ask.Tool"} {
		for _, s := range byName[name] {
			if s.Parent.SpanID() != ask {
				t.Errorf("%s is not a child of Ask.Text", name)
			}
		}
	}
	if got := byName["Ask.Tool"][0].Attributes[0]; string(got.Key) != "tool.name" || got.Value.AsString() != "add_task" {
		t.Errorf("tool span attribute = %v, want tool.name=add_task", got)
	}
}
