package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"june/internal/window"
)

// Spotify, opened plain on 2026-09-08, showed observe_screen nothing: a Chromium-based application builds no accessibility tree on this desk unless it is started with the flag. One is known by the pak file beside its binary and is run from its own Exec line with the flag added; anything else goes through gio launch untouched.
func TestLaunchEntry_AddsTheAccessibilityFlagToAChromiumApp(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "player")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$(dirname \"$0\")/argv\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, chromiumMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "player.desktop")
	if err := os.WriteFile(entry, []byte("[Desktop Entry]\nName=Player\nExec="+bin+" --quiet %U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := launchEntry(entry); err != nil {
		t.Fatal(err)
	}
	var argv []byte
	for i := 0; i < 50 && len(argv) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		argv, _ = os.ReadFile(filepath.Join(dir, "argv"))
	}
	if got := strings.TrimSpace(string(argv)); got != "--quiet\n"+accessibilityFlag {
		t.Errorf("argv = %q, want the entry's own arguments, the placeholder dropped, and the flag added", got)
	}
	if isChromium("/bin/sh") {
		t.Error("a binary with no pak file beside it must not count as Chromium")
	}
}

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

// A Chromium desktop entry gets a patched copy in the user's own applications directory, with the flag added to Exec under the main entry and under every desktop action, since XDG resolves that directory before /usr/share, /var/lib/snapd or /var/lib/flatpak, so a click on the icon then launches with the flag. Every other line, and the %U placeholder, survive untouched.
func TestPatchAccessibility_WritesTheFlagIntoTheUserCopyAndItsActions(t *testing.T) {
	src := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(src, "player")
	os.WriteFile(bin, nil, 0o755)
	os.WriteFile(filepath.Join(src, chromiumMarker), nil, 0o644)
	entry := filepath.Join(src, "player.desktop")
	original := "[Desktop Entry]\nName=Player\nExec=" + bin + " %U\nActions=NewWindow\n\n[Desktop Action NewWindow]\nName=New Window\nExec=" + bin + " --new-window %U\n"
	os.WriteFile(entry, []byte(original), 0o644)

	patchAccessibility(entry)

	dest := filepath.Join(home, ".local/share/applications", "player.desktop")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read patched copy: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	var execLines []string
	for _, l := range lines {
		if strings.HasPrefix(l, "Exec=") {
			execLines = append(execLines, l)
		}
	}
	want := []string{
		"Exec=" + bin + " %U " + accessibilityFlag,
		"Exec=" + bin + " --new-window %U " + accessibilityFlag,
	}
	if len(execLines) != 2 || execLines[0] != want[0] || execLines[1] != want[1] {
		t.Errorf("Exec lines = %v, want %v: the flag on both, the %%U placeholder kept", execLines, want)
	}
	if !strings.Contains(string(got), "Name=Player") || !strings.Contains(string(got), "Name=New Window") || !strings.Contains(string(got), "Actions=NewWindow") {
		t.Errorf("patched copy = %q, want every non-Exec line preserved", got)
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

// A snap's "current" is a symlink to its revision, and a directory walk does not step through a symlink at its root, so the marker under it was never seen and the Spotify snap was launched without its accessibility flag on 2026-09-08.
func TestIsChromium_StepsThroughASymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	rev := filepath.Join(dir, "99", "usr", "share", "player")
	if err := os.MkdirAll(rev, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rev, chromiumMarker), nil, 0o644)
	if err := os.Symlink(filepath.Join(dir, "99"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	if !isChromiumUnder(filepath.Join(dir, "current")) {
		t.Error("the marker under the symlinked root should be found")
	}
}
