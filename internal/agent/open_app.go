package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// launchWait is how long open_app waits for a launched application's window to show before saying it did not. The Spotify snap took more than 12 seconds to show its window on this desk on 2026-09-08, and the model can observe_screen after that.
const launchWait = 30 * time.Second

// launchPoll is how often the window list is read while waiting. A variable so tests cost no wall time.
var launchPoll = 500 * time.Millisecond

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

// pickDesktopEntry chooses the entry for an application the user named. Input: path to display name, and the name as said. Output: the path whose name equals it case-blind, else the path whose name or file name contains it, else "".
func pickDesktopEntry(entries map[string]string, app string) string {
	want := strings.ToLower(strings.TrimSpace(app))
	if want == "" {
		return ""
	}
	partial := ""
	for path, name := range entries {
		lower := strings.ToLower(name)
		if lower == want {
			return path
		}
		if strings.Contains(lower, want) || strings.Contains(strings.ToLower(filepath.Base(path)), want) {
			if partial == "" || len(name) < len(entries[partial]) {
				partial = path
			}
		}
	}
	return partial
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
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return c.Start()
	}
	return exec.Command("gio", "launch", entry).Run()
}

// openApp brings an installed application to the front, starting it when it is not running. Input: the application as the user named it. Output: the window that came forward, in the same words switch_window uses; an error naming the application when nothing installed matches it, or when it started but no window of it showed within launchWait.
func (a *Agent) openApp(ctx context.Context, app string) string {
	if app == "" {
		return toolError("open_app needs the application to open")
	}
	before := a.frontWindowNow(ctx)
	// A task that began in one application stays there: a request about the window in front must not be answered by bringing some other application forward, whatever the model thought it needed. Carried over from switch_window, which is what this tool now also does.
	if frontApp, _, _ := strings.Cut(before, windowSep); frontApp != "" && !namesApp(questionFrom(ctx), app) && namesApp(questionFrom(ctx), frontApp) {
		return toolError(fmt.Sprintf("I won't switch to %q: what was asked is about %q, and this task stays in the window it started in", app, frontApp))
	}
	entries := a.desktopEntries()
	entry := pickDesktopEntry(entries, app)
	// Every encounter with a Chromium-based entry is a chance to fix it for good: patch a copy into the user's own applications directory so the next launch, whoever starts it, builds an accessibility tree.
	patched := patchAccessibility(entry)
	if frontIsApp(before, app) {
		return fmt.Sprintf("%q is already the window in front; nothing was started", before) + a.treelessNote(entry, a.pidOf(ctx, app), patched)
	}
	if ok, how := a.raiseWindow(ctx, app); ok {
		return a.switchOutcome(ctx, app, before, how, nil) + a.treelessNote(entry, how, patched)
	}
	if entry == "" {
		return toolError(fmt.Sprintf("no installed application is named %q; %s; if it is a website, open_url is the way to it", app, nearEntries(entries, app)))
	}
	if a.launchApp == nil {
		return toolError(fmt.Sprintf("%q is not running and this session has no way to start applications", app))
	}
	// A window of it is open but would not come forward. Starting a second copy is the wrong answer and a visible one: a browser already running answers a second invocation by opening another window, which is where the tabs nobody asked for came from.
	if a.windowOpenFor(ctx, app) {
		return toolError(fmt.Sprintf("%q already open but its window would not come forward; nothing was started, so no second copy and no new window", app))
	}
	// The windows on screen before the launch, by pid: the launched application's window is whichever appears after, since its class and title need not carry the word the user used ("Files" is org.gnome.Nautilus with a window called "Home").
	known := a.windowPids(ctx)
	if err := a.launchApp(entry); err != nil {
		return toolError(fmt.Sprintf("could not start %q from %s: %v", app, entry, err))
	}
	deadline := time.Now().Add(launchWait)
	for {
		if ok, how := a.raiseNewWindow(ctx, known); ok {
			// The window that appeared need not carry the user's word for the application, so it is reported as what it is rather than judged against that word.
			after := a.frontWindowAfterSwitch(ctx, app)
			if after == "" || after == before {
				return toolError(fmt.Sprintf("started %q and a new window was raised by %s, but the window in front is still %q; call observe_screen to see", app, how, before))
			}
			return fmt.Sprintf("switched to %q, the window that appeared after launching %s, raised by %s; call observe_screen to see it", after, filepath.Base(entry), how)
		}
		if ok, how := a.raiseWindow(ctx, app); ok {
			return a.switchOutcome(ctx, app, before, "launched "+filepath.Base(entry)+", then "+how, nil)
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return toolError(fmt.Sprintf("started %q but no window of it showed within %s; call observe_screen to see what is in front now", app, launchWait))
		}
		time.Sleep(launchPoll)
	}
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

// pidOf names the process behind an application's window as raiseWindow would, "pid N", for treelessNote when nothing was raised. Output: "" when the extension is not there or lists no window of the application.
func (a *Agent) pidOf(ctx context.Context, app string) string {
	if a.raiser == nil {
		return ""
	}
	windows, err := a.raiser.List(ctx)
	if err != nil {
		return ""
	}
	for _, w := range windows {
		if namesApp(w.WmClass, app) || namesApp(w.Title, app) {
			return fmt.Sprintf("pid %d", w.Pid)
		}
	}
	return ""
}

// nearEntries says which installed applications share a word with what was asked for, so a miss leaves the model something to pick from, or the count of what is installed when nothing shares a word. Input: the desktop entries by path and name, and the name asked for. Output: one clause.
func nearEntries(entries map[string]string, app string) string {
	words := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(app)) {
		if len(w) > 2 {
			words[w] = true
		}
	}
	// One application can ship two desktop entries — a snap puts one in /var/lib/snapd/desktop/applications and June patches a copy into ~/.local/share/applications — and the entries are keyed by path, so both survive. Suggesting the same name twice ("Brave Web Browser, Brave Web Browser") reads as two different applications to pick between.
	seen := map[string]bool{}
	var near []string
	for _, name := range entries {
		if seen[name] {
			continue
		}
		for _, w := range strings.Fields(strings.ToLower(name)) {
			if words[strings.Trim(w, ".,()")] {
				near = append(near, name)
				seen[name] = true
				break
			}
		}
	}
	sort.Strings(near)
	if len(near) > 8 {
		near = near[:8]
	}
	if len(near) == 0 {
		return fmt.Sprintf("none of the %d installed applications shares a word with it", len(entries))
	}
	return "installed applications with a word in common: " + strings.Join(near, ", ")
}

// windowOpenFor reports whether the shell extension can see a window belonging to the named application, judged on WM_CLASS, which is the application itself rather than whatever the window happens to be showing. Input: a context and the application name. Output: false whenever there is no extension or the list cannot be read, so a store of no information never blocks a launch.
func (a *Agent) windowOpenFor(ctx context.Context, app string) bool {
	if a.raiser == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	windows, err := a.raiser.List(ctx)
	if err != nil {
		return false
	}
	for _, w := range windows {
		if namesApp(w.WmClass, app) {
			return true
		}
	}
	return false
}

// windowPids is the set of pids owning a window right now, or nil when the shell cannot be asked. Input: the call's context. Output: the set.
func (a *Agent) windowPids(ctx context.Context) map[uint32]bool {
	if a.raiser == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	windows, err := a.raiser.List(ctx)
	if err != nil {
		return nil
	}
	out := make(map[uint32]bool, len(windows))
	for _, w := range windows {
		out[w.Pid] = true
	}
	return out
}

// raiseNewWindow raises the first window owned by a pid that was not on screen before a launch. Input: the call's context and the pids known before. Output: whether one was raised, and how, in raiseWindow's words. Nothing is raised when known is nil, since then nothing can be told apart.
func (a *Agent) raiseNewWindow(ctx context.Context, known map[uint32]bool) (bool, string) {
	if a.raiser == nil || known == nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	windows, err := a.raiser.List(ctx)
	if err != nil {
		return false, ""
	}
	for _, w := range windows {
		if known[w.Pid] || w.Pid == 0 {
			continue
		}
		if ok, err := a.raiser.ByPid(ctx, w.Pid); err == nil && ok {
			return true, fmt.Sprintf("pid %d", w.Pid)
		}
	}
	return false, ""
}
