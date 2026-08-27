package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileIdentity_StableForSameFile verifies calling fileIdentity twice on the same unchanged file returns the same string — the daemon and a freshly-relaunched client built from the same binary must agree when nothing has actually changed.
func TestFileIdentity_StableForSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora-bin")
	if err := os.WriteFile(path, []byte("a build of ora"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	id1 := fileIdentity(path)
	id2 := fileIdentity(path)

	if id1 != id2 {
		t.Errorf("expected a stable identity for an unchanged file, got %q then %q", id1, id2)
	}
	if id1 == "unknown" {
		t.Errorf("expected a real identity for a file that exists, got %q", id1)
	}
}

// TestFileIdentity_DiffersAfterRebuild verifies fileIdentity changes when the file at path is overwritten (a rebuild) — this is the whole point: an old daemon process holds an identity captured at ITS startup, while a freshly-relaunched client reads whatever's on disk now.
func TestFileIdentity_DiffersAfterRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ora-bin")
	if err := os.WriteFile(path, []byte("old build"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := fileIdentity(path)

	if err := os.WriteFile(path, []byte("a new, longer build of ora"), 0755); err != nil {
		t.Fatalf("WriteFile (rebuild): %v", err)
	}
	after := fileIdentity(path)

	if before == after {
		t.Errorf("expected a different identity after the file was rewritten, got the same %q both times", before)
	}
}

// TestFileIdentity_MissingFile_ReturnsUnknown verifies a stat failure degrades to "unknown" instead of panicking or erroring out — this is a diagnostic, not something that should ever block startup.
func TestFileIdentity_MissingFile_ReturnsUnknown(t *testing.T) {
	got := fileIdentity(filepath.Join(t.TempDir(), "does-not-exist"))
	if got != "unknown" {
		t.Errorf(`expected "unknown" for a missing file, got %q`, got)
	}
}
