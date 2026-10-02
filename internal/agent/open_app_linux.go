package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"june/internal/util"
)

// desktopDirs are where installed applications' .desktop entries live on this desk: the user's own, the system's, snap's and flatpak's, and whatever XDG_DATA_DIRS adds.
func desktopDirs() []string {
	dirs := []string{
		filepath.Join(os.Getenv("HOME"), ".local/share/applications"),
		"/usr/share/applications", "/usr/local/share/applications",
		"/var/lib/snapd/desktop/applications", "/var/lib/flatpak/exports/share/applications",
	}
	for _, d := range strings.Split(os.Getenv("XDG_DATA_DIRS"), ":") {
		if d != "" {
			dirs = append(dirs, filepath.Join(d, "applications"))
		}
	}
	return dirs
}

// readDesktopEntries lists the installed applications, path to display name, from every .desktop file in desktopDirs. Entries marked NoDisplay or Hidden are left out, since the user cannot see them in the shell either.
func readDesktopEntries() map[string]string {
	out := map[string]string{}
	for _, dir := range desktopDirs() {
		files, _ := filepath.Glob(filepath.Join(dir, "*.desktop"))
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			name, hidden := "", false
			for _, line := range strings.Split(string(data), "\n") {
				switch {
				case strings.HasPrefix(line, "[") && line != "[Desktop Entry]":
					// Only the main group names the application; actions below it carry their own Name.
					goto done
				case strings.HasPrefix(line, "Name=") && name == "":
					name = strings.TrimSpace(strings.TrimPrefix(line, "Name="))
				case line == "NoDisplay=true" || line == "Hidden=true":
					hidden = true
				}
			}
		done:
			if name != "" && !hidden {
				out[f] = name
			}
		}
	}
	return out
}

// accessibilityFlag is what a Chromium-based application (Brave, Spotify, Claude desktop, VS Code, a Teams PWA) needs on its command line before it builds an accessibility tree on this desk: without it observe_screen lists nothing in the window. Measured with Brave on 2026-09-05 and Spotify on 2026-09-08.
const accessibilityFlag = "--force-renderer-accessibility"

// chromiumMarker is a file every Chromium-based application ships beside its binary.
const chromiumMarker = "chrome_100_percent.pak"

// execOf reads a desktop entry's Exec line as a command: the binary and its arguments, with the %u/%U/%f/%F placeholders dropped. Output: the parts, or nil when the entry has no Exec.
func execOf(entry string) []string {
	data, err := os.ReadFile(entry)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Exec=") {
			continue
		}
		var parts []string
		for _, p := range strings.Fields(strings.TrimPrefix(line, "Exec=")) {
			p = strings.Trim(p, `"`)
			if strings.HasPrefix(p, "%") {
				continue
			}
			parts = append(parts, p)
		}
		return parts
	}
	return nil
}

// chromiumRoot is where to look for chromiumMarker for a binary: the snap's current revision for a /snap/bin launcher, otherwise the directory the binary resolves to.
func chromiumRoot(bin string) string {
	if strings.HasPrefix(bin, "/snap/bin/") {
		return filepath.Join("/snap", strings.TrimPrefix(bin, "/snap/bin/"), "current")
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return filepath.Dir(path)
}

// isChromium reports whether the application behind a binary is Chromium-based, by finding chromiumMarker within a few directories of it. Input: the binary as the desktop entry names it. Output: true when the marker is found.
func isChromium(bin string) bool {
	return isChromiumUnder(chromiumRoot(bin))
}

// isChromiumUnder reports whether chromiumMarker sits within a few directories of root. A symlinked root (a snap's "current") is resolved first, since the walk does not step through a symlink on its own.
func isChromiumUnder(root string) bool {
	if root == "" {
		return false
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	found := false
	depth := strings.Count(root, string(filepath.Separator)) + 6
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if d.IsDir() && strings.Count(path, string(filepath.Separator)) > depth {
			return filepath.SkipDir
		}
		if d.Name() == chromiumMarker {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// desktopEntryMarker is the first line patchAccessibility writes into a copy it makes, so a later run can tell its own copy from a file it did not write and must never overwrite.
const desktopEntryMarker = "# June added " + accessibilityFlag + " to Exec below; see internal/agent/open_app.go"

// patchAccessibility gives a Chromium-based desktop entry a copy in the user's own applications directory with accessibilityFlag appended to every Exec line, main entry and desktop actions alike, so the application reads on every future launch, however the user starts it: XDG looks in the user's own directory before /usr/share, /var/lib/snapd and /var/lib/flatpak. Input: the entry's path. Output: true when the application will start with support from now on (June's copy is written or already there, or the entry already carries the flag); false when it will not: a read or write failure, an entry that is not Chromium-based, or a destination file this function did not write, all of which leave the filesystem as it was.
func patchAccessibility(entry string) bool {
	if entry == "" {
		return false
	}
	cmd := execOf(entry)
	if len(cmd) == 0 || !isChromium(cmd[0]) {
		return false
	}
	data, err := os.ReadFile(entry)
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		if !strings.HasPrefix(line, "Exec=") || strings.Contains(line, accessibilityFlag) {
			continue
		}
		lines[i] = line + " " + accessibilityFlag
		changed = true
	}
	if !changed {
		return true // the entry itself already starts the application with support
	}
	dest := filepath.Join(os.Getenv("HOME"), ".local/share/applications", filepath.Base(entry))
	out := desktopEntryMarker + "\n" + strings.Join(lines, "\n")
	if existing, err := os.ReadFile(dest); err == nil {
		if !strings.HasPrefix(string(existing), desktopEntryMarker) {
			return false // a file June did not write: never overwrite it
		}
		if string(existing) == out {
			return true // already patched, nothing to do
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false
	}
	return os.WriteFile(dest, []byte(out), 0o644) == nil
}

// launchEntry starts an application from its desktop entry. A Chromium-based one is run from its Exec line with accessibilityFlag added, since gio launch cannot pass a flag and the tree is the whole point of opening it; anything else is started the way the shell would, through gio launch, so it gets the same environment and sandbox as a click on its icon. Input: the entry's path. Output: the start error, if any; the process is left to outlive the daemon.
func launchEntry(entry string) error {
	cmd := execOf(entry)
	if len(cmd) > 0 && isChromium(cmd[0]) {
		c := exec.Command(cmd[0], append(cmd[1:], accessibilityFlag)...)
		util.Detach(c)
		return c.Start()
	}
	return exec.Command("gio", "launch", entry).Run()
}

// processArgs is the command line of a running process, for treelessNote: /proc in production, a stub in tests.
var processArgs = func(pid uint32) string {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.ReplaceAll(string(data), "\x00", " ")
}

// treelessNote says when an application already running is one whose window observe_screen cannot read, so the model knows why the listing is empty and what its options are, instead of pressing on into nothing. Input: the application's desktop entry ("" when unknown), the how raiseWindow answered, which names the window's pid when the extension raised it, and whether patchAccessibility set the entry up to start with support. Output: the note to append, or "" when the application is not Chromium-based or was started with accessibilityFlag.
func (a *Agent) treelessNote(entry, how string, patched bool) string {
	var pid uint32
	if entry == "" || how == "" {
		return ""
	}
	if _, err := fmt.Sscanf(how, "pid %d", &pid); err != nil {
		return ""
	}
	cmd := execOf(entry)
	if len(cmd) == 0 || !isChromium(cmd[0]) || strings.Contains(processArgs(pid), accessibilityFlag) {
		return ""
	}
	note := "; it was started without accessibility support, so observe_screen will list nothing inside this window: work from look and click_at for now"
	if !patched {
		return note + "; June could not set it up to start with support, so this will stay true after a relaunch"
	}
	return note + "; the application has been patched to start with support, so it will read the next time it is launched, once the user closes this window and opens it again"
}

// defaultBrowserID is the desktop id of the browser the desktop opens links with, as xdg-settings reports it ("brave_brave.desktop", "firefox.desktop", "org.mozilla.firefox.desktop"), or "" when it cannot say. A variable so a test can name one without the desktop.
var defaultBrowserID = func() string {
	out, err := exec.Command("xdg-settings", "get", "default-web-browser").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
