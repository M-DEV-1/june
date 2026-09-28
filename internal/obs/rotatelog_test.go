package obs_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ora/internal/obs"
)

// TestRotatingWriter_RotatesPastTheCapAndKeepsThreeGenerations writes past the cap enough times to produce more than three rolled-aside generations, and checks that ora.log.1 through ora.log.3 all exist, that content written before the first rotation survived into ora.log.1, and that nothing past .3 is ever kept.
func TestRotatingWriter_RotatesPastTheCapAndKeepsThreeGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ora.log")
	const cap = 200 // bytes — tiny, so the test needs no real disk pressure to prove rotation.

	w, err := obs.NewRotatingWriter(path, cap, 0600)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	line := strings.Repeat("x", 50) + "\n"
	// Five generations' worth of writes: enough to roll the log aside past .1, .2 and .3, and prove the oldest is dropped rather than kept forever.
	for i := 0; i < 30; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	for _, gen := range []string{".1", ".2", ".3"} {
		if _, err := os.Stat(path + gen); err != nil {
			t.Errorf("expected %s to exist after enough writes to roll past it, got: %v", path+gen, err)
		}
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("a fourth generation exists; only three old files should ever be kept")
	}
}

// TestRotatingWriter_KeepsPriorContentAcrossRotation writes one line short of the cap, then a second write that pushes it over, and checks the first line survives in ora.log.1 rather than being lost when the file is rolled aside.
func TestRotatingWriter_KeepsPriorContentAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ora.log")
	const cap = 100

	w, err := obs.NewRotatingWriter(path, cap, 0600)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	first := "first line, kept across the rotation\n"
	if _, err := w.Write([]byte(first)); err != nil {
		t.Fatalf("write first: %v", err)
	}
	second := strings.Repeat("y", 90) + "\n"
	if _, err := w.Write([]byte(second)); err != nil {
		t.Fatalf("write second: %v", err)
	}

	rolled, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read %s: %v", path+".1", err)
	}
	if !strings.Contains(string(rolled), first) {
		t.Errorf("ora.log.1 = %q, want it to contain the first line written before rotation", rolled)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(current), second) {
		t.Errorf("ora.log = %q, want it to contain the line that triggered rotation", current)
	}
}

// TestRotatingWriter_ConcurrentWritesDoNotRace writes from many goroutines at once with a small cap, so rotation is guaranteed to fire mid-stream, and relies on the race detector (run via `go test -race`) to catch any write or rotation that was not properly serialized.
func TestRotatingWriter_ConcurrentWritesDoNotRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ora.log")

	w, err := obs.NewRotatingWriter(path, 150, 0600)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := w.Write([]byte("concurrent line\n")); err != nil {
					t.Errorf("concurrent write: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}
