package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// launchWait is how long open_app waits for a launched application's window to show before saying it did not. The Spotify snap took more than 12 seconds to show its window on this desk on 2026-09-08, and the model can observe_screen after that.
const launchWait = 30 * time.Second

// launchPoll is how often the window list is read while waiting. A variable so tests cost no wall time.
var launchPoll = 500 * time.Millisecond

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

// appsFolder is the shell namespace every Start-menu application, desktop or Store, can be started from by its AppID: explorer.exe opens shell:AppsFolder\<AppID> the way a click on its Start tile does.
const appsFolder = `shell:AppsFolder\`

// startAppEntries reads what Windows' Get-StartApps printed as JSON, an array of {Name, AppID} for every Start-menu application including Store apps, into entries keyed by the shell:AppsFolder path each one starts from. Input: PowerShell's stdout, which may open with a UTF-8 byte order mark. Output: entry to the localized display name, or nil when the output does not parse.
func startAppEntries(out []byte) map[string]string {
	var apps []struct{ Name, AppID string }
	if json.Unmarshal(bytes.TrimPrefix(out, []byte("\xef\xbb\xbf")), &apps) != nil {
		return nil
	}
	entries := make(map[string]string, len(apps))
	for _, app := range apps {
		if app.Name != "" && app.AppID != "" {
			entries[appsFolder+app.AppID] = app.Name
		}
	}
	return entries
}

// shortcutEntries lists the Start-menu shortcuts under the given roots, path to display name, the Windows counterpart of readDesktopEntries. Input: the Start-menu Programs directories; one that does not exist is skipped. Output: every .lnk file found at any depth, named by its file name without the extension.
func shortcutEntries(roots ...string) map[string]string {
	out := map[string]string{}
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".lnk") {
				out[path] = strings.TrimSuffix(d.Name(), filepath.Ext(path))
			}
			return nil
		})
	}
	return out
}

// openApp brings an installed application to the front, starting it when it is not running. Input: the application as the user named it. Output: the window that came forward, in the same words switch_window uses; an error naming the application when nothing installed matches it, or when it started but no window of it showed within launchWait.
func (a *Agent) openApp(ctx context.Context, app string) string {
	if app == "" {
		return toolError("open_app needs the application to open")
	}
	before := a.frontWindowNow(ctx)
	// A task that began in one application stays there: a request about the window in front must not be answered by bringing some other application forward, whatever the model thought it needed. Carried over from switch_window, which is what this tool now also does.
	if inFront := frontApp(before); inFront != "" && !namesApp(questionFrom(ctx), app) && namesApp(questionFrom(ctx), inFront) {
		return toolError(fmt.Sprintf("I won't switch to %q: what was asked is about %q, and this task stays in the window it started in", app, inFront))
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
	// With no raiser there is no new window to find and nothing to raise, which is a desk without the GNOME extension, so the front window is read once and the launch reported instead of waiting out launchWait for a window nothing can see. A raiser that is there but failed one List keeps the wait below, since a busy shell answers a later call.
	if known == nil && !a.raiserAvailable(ctx) {
		if after := a.frontWindowAfterSwitch(ctx, app); frontIsApp(after, app) {
			return fmt.Sprintf("switched to %q, launched from %s; call observe_screen to see it", after, filepath.Base(entry))
		}
		return fmt.Sprintf("started %q from %s, but there is no window list here to confirm its window came forward; call observe_screen to see what is in front now", app, filepath.Base(entry))
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

// raiserAvailable reports whether a window raiser is wired and says it is up right now. Input: the call's context. Output: false when there is none, it is not loaded, or it did not answer within raiserTimeout.
func (a *Agent) raiserAvailable(ctx context.Context) bool {
	if a.raiser == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	ok, err := a.raiser.Available(ctx)
	return err == nil && ok
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
	if pid := shellPid(); pid != 0 {
		out[pid] = true
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
		if ok, err := a.raiseListed(ctx, w); err == nil && ok {
			return true, fmt.Sprintf("pid %d", w.Pid)
		}
	}
	return false, ""
}
