package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"june/internal/window"
)

// Spotify was already running, started plain, when open_app raised it on 2026-09-08, and the model was handed an empty listing with no reason. The reason and the two ways on are said with the raise, and the entry is patched so the next launch reads.
func TestExecuteTool_OpenApp_SaysWhenARunningChromiumAppHasNoTree(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(dir, "player")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, chromiumMarker), nil, 0o644)
	entry := filepath.Join(dir, "player.desktop")
	os.WriteFile(entry, []byte("[Desktop Entry]\nName=Player\nExec="+bin+" %U\n"), 0o644)
	raiser := &fakeRaiser{available: true, windows: []window.Window{{Pid: 42, WmClass: "player", Title: "Player"}}, raises: map[string]bool{"pid 42": true}}
	a, _ := switchingAgent(t, func() (string, string) { return "Player", "Player" })
	a.UseWindowRaiser(raiser)
	a.desktopEntries = func() map[string]string { return map[string]string{entry: "Player"} }
	processArgs = func(pid uint32) string { return bin }
	got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "Player"})
	if !strings.Contains(got, "without accessibility support") {
		t.Errorf("result = %q, want the empty listing explained", got)
	}
	if !strings.Contains(got, "next time") && !strings.Contains(got, "next launch") {
		t.Errorf("result = %q, want it to say the app reads from the next launch on", got)
	}
	patched, err := os.ReadFile(filepath.Join(home, ".local/share/applications", "player.desktop"))
	if err != nil || !strings.Contains(string(patched), accessibilityFlag) {
		t.Errorf("patched copy = %q, err %v, want a copy in the user's own applications directory carrying the flag", patched, err)
	}
	processArgs = func(pid uint32) string { return bin + " " + accessibilityFlag }
	if got := a.executeTool(context.Background(), "open_app", map[string]any{"app": "Player"}); strings.Contains(got, "without accessibility") {
		t.Errorf("result = %q, want no note for an app started with the flag", got)
	}
}

// Running the patch twice must do nothing the second time, and a file at the destination that June did not write - no user hand-edited a copy there, say - must never be overwritten.
func TestPatchAccessibility_IsIdempotentAndNeverOverwritesAForeignFile(t *testing.T) {
	src := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(src, "player")
	os.WriteFile(bin, nil, 0o755)
	os.WriteFile(filepath.Join(src, chromiumMarker), nil, 0o644)
	entry := filepath.Join(src, "player.desktop")
	os.WriteFile(entry, []byte("[Desktop Entry]\nName=Player\nExec="+bin+" %U\n"), 0o644)

	patchAccessibility(entry)
	dest := filepath.Join(home, ".local/share/applications", "player.desktop")
	first, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read patched copy: %v", err)
	}

	patchAccessibility(entry)
	second, err := os.ReadFile(dest)
	if err != nil || string(second) != string(first) {
		t.Errorf("second patch changed the file: got %q, want it unchanged from %q", second, first)
	}

	// An entry that already carries the flag must not be rewritten at all.
	alreadyFlagged := filepath.Join(src, "flagged.desktop")
	os.WriteFile(alreadyFlagged, []byte("[Desktop Entry]\nName=Flagged\nExec="+bin+" "+accessibilityFlag+" %U\n"), 0o644)
	patchAccessibility(alreadyFlagged)
	if _, err := os.Stat(filepath.Join(home, ".local/share/applications", "flagged.desktop")); !os.IsNotExist(err) {
		t.Error("an entry that already carries the flag must not get a patched copy")
	}

	// A file at the destination that June did not write, marked by carrying no June marker, must survive untouched.
	foreign := filepath.Join(src, "foreign.desktop")
	os.WriteFile(foreign, []byte("[Desktop Entry]\nName=Foreign\nExec="+bin+" %U\n"), 0o644)
	foreignDest := filepath.Join(home, ".local/share/applications", "foreign.desktop")
	os.MkdirAll(filepath.Dir(foreignDest), 0o755)
	os.WriteFile(foreignDest, []byte("hand-edited by the user, not June"), 0o644)
	// The Teams PWA case: open_app told the model "the application has been patched" whatever happened here, and the model passed the promise on.
	if patchAccessibility(foreign) {
		t.Error("a patch that left a foreign file in place reported the application as patched")
	}
	if !patchAccessibility(entry) {
		t.Error("June's own patched copy reported as not patched")
	}
	if got, _ := os.ReadFile(foreignDest); string(got) != "hand-edited by the user, not June" {
		t.Errorf("foreign file = %q, want it left untouched", got)
	}
}
