package obs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The Windows uninstaller deletes only the top-level files it has a name for when the data folder is a custom one, so spans.jsonl and its rolled-aside copies must be in its JuneDataFiles list or /PURGE leaves the folder behind.
func TestSpansFileIsInTheInstallersPurgeList(t *testing.T) {
	iss, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows", "june.iss"))
	if err != nil {
		t.Skipf("installer script not found: %v", err)
	}
	var list string
	for _, line := range strings.Split(string(iss), "\n") {
		if _, rest, ok := strings.Cut(line, "JuneDataFiles = '"); ok {
			list, _, _ = strings.Cut(rest, "'")
		}
	}
	if !slices.Contains(strings.Split(list, "|"), spansFileName+"*") {
		t.Errorf("JuneDataFiles in june.iss = %q, want it to hold %q", list, spansFileName+"*")
	}
}

// Once the sink is shut down it never writes again: a span that ends afterwards leaves the file as it was and creates no rolled-aside copy.
func TestSpanSinkWritesNothingAfterShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), spansFileName)
	sink, err := newSpanSink(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sink))
	_, before := tp.Tracer("t").Start(context.Background(), "Test.Before")
	before.End()
	if err := sink.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	sizeAfterShutdown := sizeOf(t, path)
	if sizeAfterShutdown == 0 {
		t.Fatal("the span ended before shutdown was not flushed")
	}

	// Only the sink is shut down here, so the provider still hands it a span: the worst case for a late OnEnd.
	_, late := tp.Tracer("t").Start(context.Background(), "Test.Late")
	late.End()
	sink.ForceFlush(context.Background())
	if got := sizeOf(t, path); got != sizeAfterShutdown {
		t.Errorf("%s grew from %d to %d bytes after shutdown", spansFileName, sizeAfterShutdown, got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("a rolled-aside copy appeared after shutdown: %v", err)
	}
}

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
