package ipctoken_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"june/internal/ipctoken"
)

// The token file is 0600 inside a 0700 directory, so no other user on the machine can read the token or list the directory it sits in.
func TestGenerate_KeepsTheTokenAndItsDirectoryPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on Windows")
	}
	dir := filepath.Join(t.TempDir(), "june")
	path := filepath.Join(dir, "ipc-token")

	if _, err := ipctoken.Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for p, want := range map[string]os.FileMode{path: 0600, dir: 0700} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions = %o, want %o", p, got, want)
		}
	}
}

func TestGenerate_EachCallProducesADifferentToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipc-token")

	first, err := ipctoken.Generate(path)
	if err != nil {
		t.Fatalf("Generate (first): %v", err)
	}
	second, err := ipctoken.Generate(path)
	if err != nil {
		t.Fatalf("Generate (second): %v", err)
	}
	if first == second {
		t.Error("expected two calls to Generate to produce different tokens (a restart must invalidate any stale token)")
	}

	got, err := ipctoken.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != second {
		t.Errorf("expected Read to return the most recently generated token %q, got %q", second, got)
	}
}
