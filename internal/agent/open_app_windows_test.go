package agent

import "testing"

// Smoke test for a real Windows desk: the Start menu lists applications and the registry names a default browser. It starts nothing.
func TestWindowsDesk_ListsShortcutsAndTheDefaultBrowser(t *testing.T) {
	entries := readDesktopEntries()
	if len(entries) == 0 {
		t.Fatal("no Start-menu shortcuts found under ProgramData or APPDATA")
	}
	t.Logf("%d shortcuts, for example %s", len(entries), nearEntries(entries, "edge"))
	if id := defaultBrowserID(); id == "" {
		t.Error("no default browser read from the https UserChoice ProgId")
	} else {
		t.Logf("default browser: %s", id)
	}
}
