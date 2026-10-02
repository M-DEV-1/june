//go:build windows

package tracker

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"june/internal/util"
)

// Capture bounds on Windows. uiaCaptureTimeout is the whole read, the same 2.5 s the Linux capture gets; uiaCaptureWalkMS is how long the script itself walks before answering with what it has, kept well under the timeout so a slow but healthy walk returns partial text instead of having its host killed.
const (
	uiaCaptureTimeout = 2500 * time.Millisecond
	uiaCaptureWalkMS  = 1500
	uiaMaxTextLen     = 100000
)

// extractText reads the text of the foreground window through UI Automation. Output: the text, "" when there is no foreground window or the read failed or ran out of time; the error is always nil, as on Linux.
func extractText() (string, error) {
	h := windows.GetForegroundWindow()
	if h == 0 {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), uiaCaptureTimeout)
	defer cancel()
	return uiaWindowText(ctx, h), nil
}

// uiaWindowText walks one window and keeps the text documentText would keep from the Linux tree: a browser page's text without the browser around it, or the whole window's text for a native app. Input: a context bounding the read and the window handle. Output: the trimmed text, capped at uiaMaxTextLen runes, or "" when the read failed.
func uiaWindowText(ctx context.Context, h windows.HWND) string {
	r, err := uiaCall(ctx, uiaRequest{Op: "walk", Hwnd: int64(h), Text: true, MS: uiaCaptureWalkMS})
	if err != nil {
		slog.Debug("could not read a window's text through UI Automation", "error", err)
		return ""
	}
	return strings.TrimSpace(util.Runes(documentText(uiaTree(r.Nodes)), uiaMaxTextLen))
}

// extractMeetingWindow finds a call in progress anywhere on the desktop, focused or not, and reads it. Output: the owning executable's file name (the same form the tracker's GetActiveWindow reports), the window title, its text, and ok false when no open window's title or executable says it is a call (see IsMeetingWindow).
func extractMeetingWindow() (app, title, text string, ok bool) {
	for _, h := range topWindows() {
		if !winListable(h) {
			continue
		}
		w := winOf(h)
		if !IsMeetingWindow(w.exe, w.title) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), uiaCaptureTimeout)
		defer cancel()
		return w.exe, w.title, uiaWindowText(ctx, h), true
	}
	return "", "", "", false
}

// WindowTitleFor returns the title of an open window belonging to the named application, or "" when none is open. Input: the application's process name, such as "chrome" or "chrome.exe". Output: the longest title among the windows of every process whose executable name contains it or is contained by it, case-insensitively; the longest because a browser's short utility windows sit beside the one that names the call fully.
func WindowTitleFor(ctx context.Context, app string) string {
	want := strings.ToLower(trimExe(app))
	if want == "" {
		return ""
	}
	best := ""
	for _, h := range topWindows() {
		if !winListable(h) {
			continue
		}
		w := winOf(h)
		name := strings.ToLower(trimExe(w.exe))
		if name == "" || (!strings.Contains(name, want) && !strings.Contains(want, name)) {
			continue
		}
		if len(w.title) > len(best) {
			best = w.title
		}
	}
	return best
}
