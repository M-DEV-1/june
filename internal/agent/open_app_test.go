package agent

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// The Start menu keeps shortcuts in nested folders beside desktop.ini files, and the same application can have a shortcut in both the machine's and the user's menu; every .lnk anywhere under either root is an application, named by its file name.
func TestShortcutEntries_ReadsEveryShortcutUnderBothMenus(t *testing.T) {
	machine, user := t.TempDir(), t.TempDir()
	for _, f := range []string{
		filepath.Join(machine, "Spotify.lnk"),
		filepath.Join(machine, "Accessories", "Notepad.LNK"),
		filepath.Join(machine, "desktop.ini"),
		filepath.Join(user, "Brave Software", "Brave.lnk"),
	} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, nil, 0o644)
	}
	got := shortcutEntries(machine, user, filepath.Join(machine, "missing"))
	want := map[string]string{
		filepath.Join(machine, "Spotify.lnk"):                "Spotify",
		filepath.Join(machine, "Accessories", "Notepad.LNK"): "Notepad",
		filepath.Join(user, "Brave Software", "Brave.lnk"):   "Brave",
	}
	if !maps.Equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if pickDesktopEntry(got, "brave") != filepath.Join(user, "Brave Software", "Brave.lnk") {
		t.Error("a shortcut's name should be picked the same way a desktop entry's is")
	}
}

// Windows names the default browser by a ProgId; raiseBrowser matches its words against a window's class, which on Windows is the browser's executable, so the ProgId is cut down to that stem.
func TestBrowserFromProgID(t *testing.T) {
	for progID, want := range map[string]string{
		"BraveHTML":                   "brave",
		"ChromeHTML":                  "chrome",
		"ChromeBHTML":                 "chrome",
		"MSEdgeHTM":                   "msedge",
		"FirefoxURL-308046B0AF4A39CB": "firefox",
		"OperaStable":                 "opera",
		"VivaldiHTM.ABCDEF":           "vivaldi",
		"":                            "",
	} {
		if got := browserFromProgID(progID); got != want {
			t.Errorf("browserFromProgID(%q) = %q, want %q", progID, got, want)
		}
	}
}
