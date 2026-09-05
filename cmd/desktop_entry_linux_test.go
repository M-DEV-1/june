//go:build linux

package cmd

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
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

// TestInstallDesktopEntry_WritesEveryIconSizeAndBothDesktopFiles drives installDesktopEntry against a throwaway XDG_DATA_HOME and checks every file it writes: one PNG per size under the hicolor theme, the application entry, and the hidden overlay entry.
func TestInstallDesktopEntry_WritesEveryIconSizeAndBothDesktopFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	recordIconCacheRefresh(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry() returned unexpected error: %v", err)
	}

	for _, size := range []int{16, 32, 48, 64, 128, 256} {
		path := filepath.Join(dir, "icons", "hicolor", fmt.Sprintf("%dx%d", size, size), "apps", "ora.png")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("expected an icon at %s: %v", path, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("icon at %s is not a readable PNG: %v", path, err)
		}
		if got := img.Bounds(); got.Dx() != size || got.Dy() != size {
			t.Errorf("expected the icon at %s to be %dx%d pixels, got %dx%d", path, size, size, got.Dx(), got.Dy())
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, "icons", "hicolor"))
	if err != nil {
		t.Fatalf("reading the hicolor directory failed: %v", err)
	}
	var sizeDirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			sizeDirs = append(sizeDirs, entry.Name())
		}
	}
	if len(sizeDirs) != 6 {
		t.Errorf("expected exactly the six icon size directories, got %v", sizeDirs)
	}

	desktopPath := filepath.Join(dir, "applications", "ora.desktop")
	data, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("expected ora.desktop to exist: %v", err)
	}
	entry := string(data)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable failed: %v", err)
	}
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=Ora",
		"Comment=Ora",
		"Exec=\"" + exe + "\" --daemon",
		"Path=" + filepath.Dir(exe),
		"Icon=ora",
		"Terminal=false",
		"StartupWMClass=ora",
		"NoDisplay=false",
		"Categories=Utility;",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("expected desktop entry to contain %q, got:\n%s", want, entry)
		}
	}

	overlayData, err := os.ReadFile(filepath.Join(dir, "applications", "ora-overlay.desktop"))
	if err != nil {
		t.Fatalf("expected ora-overlay.desktop to exist: %v", err)
	}
	overlay := string(overlayData)
	for _, want := range []string{
		"StartupWMClass=ora-overlay",
		"NoDisplay=true",
		"Icon=ora",
		"Exec=\"" + exe + "\" --daemon",
	} {
		if !strings.Contains(overlay, want) {
			t.Errorf("expected the overlay entry to contain %q, got:\n%s", want, overlay)
		}
	}
}

// TestInstallDesktopEntry_RefreshesTheIconCache checks that writing the icons is followed by a gtk-update-icon-cache run against the hicolor theme directory, which is what makes a desktop that reads that cache see the new files.
func TestInstallDesktopEntry_RefreshesTheIconCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	calls := recordIconCacheRefresh(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry() returned unexpected error: %v", err)
	}

	themeDir := filepath.Join(dir, "icons", "hicolor")
	if len(*calls) != 1 || (*calls)[0] != themeDir {
		t.Fatalf("expected one icon cache refresh of %s, got %v", themeDir, *calls)
	}
	if !iconCacheExists(themeDir) {
		t.Error("expected an icon-theme.cache in the hicolor directory after the refresh")
	}
}

// TestInstallDesktopEntry_SecondCallRewritesNothing checks that calling installDesktopEntry again with unchanged inputs leaves the files untouched and asks for no second cache refresh, so the daemon does not rewrite them on every start.
func TestInstallDesktopEntry_SecondCallRewritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	calls := recordIconCacheRefresh(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("first installDesktopEntry() returned unexpected error: %v", err)
	}

	watched := []string{
		filepath.Join(dir, "icons", "hicolor", "16x16", "apps", "ora.png"),
		filepath.Join(dir, "icons", "hicolor", "256x256", "apps", "ora.png"),
		filepath.Join(dir, "applications", "ora.desktop"),
		filepath.Join(dir, "applications", "ora-overlay.desktop"),
	}
	before := make(map[string]os.FileInfo, len(watched))
	for _, path := range watched {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s failed: %v", path, err)
		}
		before[path] = info
	}

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("second installDesktopEntry() returned unexpected error: %v", err)
	}

	for _, path := range watched {
		after, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s failed after the second call: %v", path, err)
		}
		if !before[path].ModTime().Equal(after.ModTime()) {
			t.Errorf("expected %s to be unchanged by a second call with identical content", path)
		}
	}
	if len(*calls) != 1 {
		t.Errorf("expected no second icon cache refresh when nothing changed, got %d refreshes", len(*calls))
	}
}

// TestInstallDesktopEntry_RefreshesAgainWhenAnIconChanges checks that an icon file someone else overwrote is written back and the cache refreshed again, which is the case the missing dock icon came from: a file on disk the desktop's cache does not describe.
func TestInstallDesktopEntry_RefreshesAgainWhenAnIconChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	calls := recordIconCacheRefresh(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("first installDesktopEntry() returned unexpected error: %v", err)
	}

	icon := filepath.Join(dir, "icons", "hicolor", "48x48", "apps", "ora.png")
	if err := os.WriteFile(icon, []byte("not the ora icon"), 0644); err != nil {
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

// TestScaleIcon_ShrinksTheMasterToEverySize checks the scaler on the embedded master: every size comes out square at the size asked for, and the master's solid corner stays solid instead of fading, which is what happens when a scaler averages the pixels outside the image in with the ones inside it.
func TestScaleIcon_ShrinksTheMasterToEverySize(t *testing.T) {
	master, err := png.Decode(bytes.NewReader(appIconPNG))
	if err != nil {
		t.Fatalf("decoding the embedded app icon failed: %v", err)
	}
	if got := master.Bounds(); got.Dx() != 512 || got.Dy() != 512 {
		t.Fatalf("expected the embedded master to be 512x512, got %dx%d", got.Dx(), got.Dy())
	}

	for _, size := range dockIconSizes {
		scaled := scaleIcon(master, size)
		if got := scaled.Bounds(); got.Dx() != size || got.Dy() != size {
			t.Errorf("expected scaleIcon to return %dx%d, got %dx%d", size, size, got.Dx(), got.Dy())
			continue
		}
		if _, _, _, alpha := scaled.At(0, 0).RGBA(); alpha != 0xffff {
			t.Errorf("expected the top left corner of the %dx%d icon to stay opaque, got alpha %d", size, size, alpha)
		}
	}
}

// TestScaleIcon_WeighsColourByAlpha checks that a transparent pixel contributes none of its colour: a block of one opaque red pixel and three transparent black ones averages to red at a quarter alpha, not to the dark quarter-red a plain average of the four would give.
func TestScaleIcon_WeighsColourByAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 0xff, A: 0xff})
	src.SetNRGBA(1, 0, color.NRGBA{})
	src.SetNRGBA(0, 1, color.NRGBA{})
	src.SetNRGBA(1, 1, color.NRGBA{})

	got := scaleIcon(src, 1).NRGBAAt(0, 0)

	if got.A != 0x3f && got.A != 0x40 {
		t.Errorf("expected one opaque pixel in four to average to a quarter alpha, got %d", got.A)
	}
	if got.R != 0xff || got.G != 0 || got.B != 0 {
		t.Errorf("expected the surviving colour to be the opaque pixel's red, got %v", got)
	}
}

// TestInstallDesktopEntry_LeavesAForeignEntryAlone checks that an ora.desktop already on disk without desktopEntryMarker in it — the shape a package's install.sh leaves behind — is left byte-for-byte untouched, while the icons and the hidden overlay entry, which no package ships, are still written.
func TestInstallDesktopEntry_LeavesAForeignEntryAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	recordIconCacheRefresh(t)

	appsDir := filepath.Join(dir, "applications")
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(appsDir, "ora.desktop")
	foreign := "[Desktop Entry]\nType=Application\nName=Ora\nExec=ora --daemon\nIcon=ora\nStartupWMClass=ora\n"
	if err := os.WriteFile(appPath, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry() returned unexpected error: %v", err)
	}

	got, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("reading ora.desktop back failed: %v", err)
	}
	if string(got) != foreign {
		t.Errorf("expected the foreign ora.desktop to be left untouched, got:\n%s", got)
	}

	if _, err := os.Stat(filepath.Join(appsDir, "ora-overlay.desktop")); err != nil {
		t.Errorf("expected the overlay entry to still be written even when ora.desktop is foreign: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icons", "hicolor", "48x48", "apps", "ora.png")); err != nil {
		t.Errorf("expected icons to still be written even when ora.desktop is foreign: %v", err)
	}
}

// TestInstallDesktopEntry_MarkerLetsTheDaemonReclaimItsOwnEntry checks that an ora.desktop carrying desktopEntryMarker — the daemon's own past write — is treated as ours and kept up to date, the same as when nothing was there at all.
func TestInstallDesktopEntry_MarkerLetsTheDaemonReclaimItsOwnEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	recordIconCacheRefresh(t)

	appsDir := filepath.Join(dir, "applications")
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(appsDir, "ora.desktop")
	stale := "[Desktop Entry]\nName=Ora (stale)\n" + desktopEntryMarker + "\n"
	if err := os.WriteFile(appPath, []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry() returned unexpected error: %v", err)
	}

	got, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("reading ora.desktop back failed: %v", err)
	}
	if string(got) == stale {
		t.Errorf("expected the daemon's own marked entry to be rewritten, got the stale content unchanged")
	}
	if !strings.Contains(string(got), "StartupWMClass=ora") {
		t.Errorf("expected the rewritten entry to be the real application entry, got:\n%s", got)
	}
}

// TestApplicationDesktopEntry_EscapesSpecialCharacters checks that a path containing a space and a backslash is escaped per the Desktop Entry Specification's quoting rules, reusing the same helpers as the autostart entry.
func TestApplicationDesktopEntry_EscapesSpecialCharacters(t *testing.T) {
	exe := `/home/user/My Apps/back\slash/ora`
	dir := filepath.Dir(exe)

	for name, entry := range map[string]string{
		"ora.desktop":         applicationDesktopEntry(exe, dir),
		"ora-overlay.desktop": overlayDesktopEntry(exe, dir),
	} {
		wantExec := `Exec="/home/user/My Apps/back\\slash/ora" --daemon`
		if !strings.Contains(entry, wantExec) {
			t.Errorf("expected %s to contain %q, got:\n%s", name, wantExec, entry)
		}
		wantPath := `Path=/home/user/My Apps/back\\slash`
		if !strings.Contains(entry, wantPath) {
			t.Errorf("expected %s to contain %q, got:\n%s", name, wantPath, entry)
		}
	}
}
