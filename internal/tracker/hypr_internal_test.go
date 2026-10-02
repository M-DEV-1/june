//go:build linux

package tracker

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHyprWindow_SilentSocketReturnsWithinTheDeadline checks a Hyprland socket that accepts the query but never answers cannot hang the tracker's sampling loop: the read gives up on its deadline and the window comes back unknown.
func TestHyprWindow_SilentSocketReturnsWithinTheDeadline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "t")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sock := filepath.Join(dir, "hypr", "t", ".socket.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			time.Sleep(5 * time.Second)
		}
	}()

	start := time.Now()
	act, err := (&linuxTracker{}).hyprWindow()
	if err != nil {
		t.Fatalf("hyprWindow: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("hyprWindow took %v against a silent socket, want under the deadline", took)
	}
	if act.App != "Unknown" {
		t.Fatalf("app %q, want Unknown", act.App)
	}
}
