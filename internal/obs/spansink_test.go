package obs_test

import (
	"context"
	"encoding/json"
	"june/internal/obs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace/noop"
)

// A span started and ended through GetTracer is written to spans.jsonl in the data dir with its name, a duration at least as long as the span ran, and ids that link a child to its parent.
func TestInitTelemetry_WritesFinishedSpansToTheLocalFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JUNE_DATA_DIR", dir)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("JUNE_SPANS", "")

	shutdown, err := obs.InitTelemetry(context.Background(), false)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	ctx, parent := obs.GetTracer(context.Background(), "june.test").Start(context.Background(), "Test.Parent")
	_, child := obs.GetTracer(ctx, "june.test").Start(ctx, "Test.Child")
	child.SetAttributes(attribute.String("k", "v"), attribute.Int("n", 3))
	time.Sleep(30 * time.Millisecond)
	child.End()
	parent.End()
	_, poll := obs.GetTracer(ctx, "june.test").Start(ctx, "Tracker.PollActiveWindow")
	poll.End()
	// Shut down under an already-cancelled context, as main does after the signal that stops the daemon.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shutdown(cancelled); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "spans.jsonl"))
	if err != nil {
		t.Fatalf("read spans.jsonl: %v", err)
	}
	recs := map[string]obs.SpanRecord{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r obs.SpanRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %q is not a span record: %v", line, err)
		}
		recs[r.Name] = r
	}
	if _, ok := recs["Tracker.PollActiveWindow"]; ok {
		t.Error("the tracker's two-second poll span was written to spans.jsonl")
	}
	child2, parent2 := recs["Test.Child"], recs["Test.Parent"]
	if child2.DurationMS < 30 || child2.DurationMS > 2000 {
		t.Errorf("child duration_ms = %v, want 30 to 2000", child2.DurationMS)
	}
	if child2.Parent != parent2.SpanID || child2.TraceID != parent2.TraceID || parent2.Parent != "" {
		t.Errorf("parent/trace ids do not link child to parent: child %+v parent %+v", child2, parent2)
	}
	if child2.Attrs["k"] != "v" || child2.Attrs["n"] != float64(3) {
		t.Errorf("attrs = %v, want k=v n=3", child2.Attrs)
	}
}

// JUNE_SPANS=off leaves no spans.jsonl behind.
func TestInitTelemetry_SpansOffWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JUNE_DATA_DIR", dir)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("JUNE_SPANS", "off")

	shutdown, err := obs.InitTelemetry(context.Background(), false)
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	defer shutdown(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "spans.jsonl")); !os.IsNotExist(err) {
		t.Errorf("spans.jsonl exists with JUNE_SPANS=off: %v", err)
	}
}
