package obs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"june/internal/obs"
)

// TestRotatingWriter_RotatesPastTheCapAndKeepsThreeGenerations writes past the cap enough times to produce more than three rolled-aside generations, and checks that june.log.1 through june.log.3 all exist, that content written before the first rotation survived into june.log.1, and that nothing past .3 is ever kept.
func TestRotatingWriter_RotatesPastTheCapAndKeepsThreeGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "june.log")
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

// A rotation whose rename fails, as one does on Windows while another june process holds the log open, must leave the writer writing to june.log rather than to a closed file for the rest of the run. Two non-empty directories at .2 and .3 make the rename fail on every platform.
func TestRotatingWriter_KeepsWritingAfterARotationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "june.log")
	for _, gen := range []string{".2", ".3"} {
		if err := os.MkdirAll(filepath.Join(path+gen, "full"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	w, err := obs.NewRotatingWriter(path, 10, 0600)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	for _, line := range []string{"first line\n", "after the failed rotation\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "after the failed rotation") {
		t.Errorf("june.log = %q, want the line written after the failed rotation", got)
	}
}
