//go:build linux

package cmd

import (
	"bytes"

	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// recordIconCacheRefresh replaces the gtk-update-icon-cache call for the length of a test and returns the directories it was asked to refresh. It also writes an icon-theme.cache file, because that is what the real program leaves behind and what installDesktopEntry looks for when deciding whether a refresh is still owed.
func recordIconCacheRefresh(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	previous := iconCacheRefresh
	iconCacheRefresh = func(themeDir string) error {
		calls = append(calls, themeDir)
		if err := os.MkdirAll(themeDir, 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(themeDir, "icon-theme.cache"), []byte("cache"), 0644)
	}
	t.Cleanup(func() { iconCacheRefresh = previous })
	return &calls
}

// TestInstallDesktopEntry_RefreshesAgainWhenAnIconChanges checks that an icon file someone else overwrote is written back and the cache refreshed again, which is the case the missing dock icon came from: a file on disk the desktop's cache does not describe.
func TestInstallDesktopEntry_RefreshesAgainWhenAnIconChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	calls := recordIconCacheRefresh(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("first installDesktopEntry() returned unexpected error: %v", err)
	}

	icon := filepath.Join(dir, "icons", "hicolor", "48x48", "apps", "june.png")
	if err := os.WriteFile(icon, []byte("not the june icon"), 0644); err != nil {
		t.Fatalf("overwriting the 48x48 icon failed: %v", err)
	}

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("second installDesktopEntry() returned unexpected error: %v", err)
	}

	data, err := os.ReadFile(icon)
	if err != nil {
		t.Fatalf("reading the 48x48 icon back failed: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("expected the overwritten icon to be written back as a PNG: %v", err)
	}
	if len(*calls) != 2 {
		t.Errorf("expected a second icon cache refresh after an icon changed, got %d refreshes", len(*calls))
	}
}

// TestInstallDesktopEntry_LeavesAForeignEntryAlone checks that an june.desktop already on disk without desktopEntryMarker in it — the shape a package's install.sh leaves behind — is left byte-for-byte untouched, while the icons and the hidden overlay entry, which no package ships, are still written.
func TestInstallDesktopEntry_LeavesAForeignEntryAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	recordIconCacheRefresh(t)

	appsDir := filepath.Join(dir, "applications")
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(appsDir, "june.desktop")
	foreign := "[Desktop Entry]\nType=Application\nName=June\nExec=june --daemon\nIcon=june\nStartupWMClass=june\n"
	if err := os.WriteFile(appPath, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry() returned unexpected error: %v", err)
	}

	got, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("reading june.desktop back failed: %v", err)
	}
	if string(got) != foreign {
		t.Errorf("expected the foreign june.desktop to be left untouched, got:\n%s", got)
	}

	if _, err := os.Stat(filepath.Join(appsDir, "june-overlay.desktop")); err != nil {
		t.Errorf("expected the overlay entry to still be written even when june.desktop is foreign: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icons", "hicolor", "48x48", "apps", "june.png")); err != nil {
		t.Errorf("expected icons to still be written even when june.desktop is foreign: %v", err)
	}
}
