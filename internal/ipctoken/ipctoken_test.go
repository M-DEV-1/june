package ipctoken_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"ora/internal/ipctoken"
)

func TestGenerate_WritesReadableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipc-token")

	written, err := ipctoken.Generate(path)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if written == "" {
		t.Fatal("expected a non-empty token")
	}

	got, err := ipctoken.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != written {
		t.Errorf("Read() = %q, want the token Generate wrote %q", got, written)
	}
}

func TestGenerate_RestrictsFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on Windows")
	}
	path := filepath.Join(t.TempDir(), "ipc-token")

	if _, err := ipctoken.Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("token file permissions = %o, want 0600", got)
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

func TestGenerate_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "ipc-token")

	if _, err := ipctoken.Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := ipctoken.Read(path); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

func TestRead_MissingFile_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := ipctoken.Read(path); err == nil {
		t.Error("expected an error reading a token file that doesn't exist")
	}
}

// The token file is 0600, but the directory it sits in was created 0755 here, so whoever created the data directory first decided whether anyone else on the machine could list it. It is the user's own directory and is created as such.
func TestGenerate_CreatesTheTokenDirectoryPrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ora")
	if _, err := ipctoken.Generate(filepath.Join(dir, "ipc-token")); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the token directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Errorf("expected the token directory to be 0700, got %o", got)
	}
}
