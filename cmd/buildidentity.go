package cmd

import (
	"fmt"
	"os"
)

// buildIdentity is computed once, here at package init, from the currently-running executable's own size+mtime — not lazily on every /ping call. A daemon process that's been running since before a rebuild must keep reporting the OLD binary's identity even after the file on disk has been overwritten with a new build; recomputing per-request would just report whatever's CURRENTLY on disk, indistinguishable from a fresh build and defeating the whole point (see cmd/root.go's checkDaemonBuildMismatch).
var buildIdentity = computeBuildIdentity()

// computeBuildIdentity resolves the path to the currently-running executable and fingerprints it. No build tooling changes (no injected version/ldflags) — just what's already on disk.
func computeBuildIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	return fileIdentity(exe)
}

// fileIdentity stats path and combines its size and modification time into a stable identity string, "unknown" if the stat fails. Extracted from computeBuildIdentity for testability — os.Executable() itself isn't something a test can point at a fixture file.
func fileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}
