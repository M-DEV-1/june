//go:build vectorbench

package vector

// Measures ChromemIndex's real RSS footprint at 10k docs, full 3072-dim. Gated behind a build tag (not testing.Short()) since inserting 10k real vectors takes minutes, not the sub-second unit tests around it.
//
// Run with: go test -tags vectorbench ./internal/vector/ -run TestMeasureChromemIndexMemoryFootprint_10kVectors -v

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	benchNumVectors = 10000
	benchDims       = 3072
)

// readRSSKB reads RSS from /proc/self/status — what ps/top actually shows, unlike runtime.MemStats.HeapAlloc which excludes Go runtime overhead and anything outside the heap. Linux-only.
func readRSSKB(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("cannot read /proc/self/status (non-Linux?): %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.Atoi(fields[1])
				if err != nil {
					t.Fatalf("parse VmRSS %q: %v", line, err)
				}
				return kb
			}
		}
	}
	t.Fatal("VmRSS not found in /proc/self/status")
	return 0
}

// randomEmbedding generates a random-direction vector, unnormalized — cosine similarity only cares about direction, and normalizing wouldn't change the memory footprint this test measures.
func randomEmbedding(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	return v
}

// realisticContent mimics a real episode's screen_text so chromem's per-doc content storage (it keeps the raw text in RAM alongside the vector) gets measured realistically, not with a placeholder string.
func realisticContent(i int) string {
	return fmt.Sprintf(
		"episode %d: user was debugging a CUDA out-of-memory error in a Python training script, "+
			"reading Stack Overflow answers about gradient checkpointing and batch size, with a terminal "+
			"showing nvidia-smi output and a VS Code window open to model.py", i)
}

// TestMeasureChromemIndexMemoryFootprint_10kVectors populates ChromemIndex with 10,000 synthetic 3072-dim vectors plus realistic content and metadata, then logs the actual process RSS delta via t.Logf.
func TestMeasureChromemIndexMemoryFootprint_10kVectors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	runtime.GC()
	baselineKB := readRSSKB(t)

	idx, err := NewChromemIndex(dir, benchDims, benchNumVectors)
	if err != nil {
		t.Fatalf("NewChromemIndex: %v", err)
	}

	r := rand.New(rand.NewSource(42))
	domains := []string{"work", "personal", ""}
	for i := 0; i < benchNumVectors; i++ {
		id := fmt.Sprintf("episode:%d", i)
		vec := randomEmbedding(r, benchDims)
		content := realisticContent(i)
		meta := map[string]string{"domain": domains[i%len(domains)]}
		if err := idx.Add(ctx, id, content, vec, meta); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}

	if got := idx.Count(); got != benchNumVectors {
		t.Fatalf("expected %d documents after insert, got %d (cap/eviction bug?)", benchNumVectors, got)
	}

	runtime.GC()
	afterKB := readRSSKB(t)

	deltaMB := float64(afterKB-baselineKB) / 1024.0
	totalMB := float64(afterKB) / 1024.0
	t.Logf("RSS baseline: %.1f MB, after %d×%d-dim vectors: %.1f MB, delta: %.1f MB",
		float64(baselineKB)/1024.0, benchNumVectors, benchDims, totalMB, deltaMB)
}
