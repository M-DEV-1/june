//go:build windows

package tracker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"june/internal/act"
)

// Observe bounds on Windows: the whole read gets the same 4 s the Linux walk does, and the script walks for at most uiaObserveWalkMS of it so a large page answers with what it read in time.
const (
	uiaObserveTimeout = 4 * time.Second
	uiaObserveWalkMS  = 2500
)

// Observe reads the window in front (the one behind it when June's own window is in front) into the nodes a model can act on. Input: a context. Output: the executable's name without ".exe", the window's title, one act.Node per element with an actionable role in tree order, with rectangles in physical virtual-desktop pixels, and an error when no window is in front, its application is on the blocklist, or UI Automation could not read it.
func Observe(ctx context.Context) (app, title string, nodes []act.Node, err error) {
	ctx, cancel := context.WithTimeout(ctx, uiaObserveTimeout)
	defer cancel()
	w, ok := frontWindow()
	if !ok {
		return "", "", nil, errors.New("no window is in front to read")
	}
	app = trimExe(w.exe)
	if Blocklisted(app) {
		return "", "", nil, fmt.Errorf("%s is on this machine's blocklist, so its windows are not read or pictured", app)
	}
	r, err := uiaCall(ctx, uiaRequest{Op: "walk", Hwnd: int64(w.hwnd), MS: uiaObserveWalkMS})
	if err != nil {
		return "", "", nil, fmt.Errorf("could not read %s through UI Automation: %w", app, err)
	}
	return app, w.title, uiaActNodes(r.Nodes), nil
}

// UseFocusReader is a no-op on Windows: GetForegroundWindow already says which window is in front.
func UseFocusReader(f func(ctx context.Context) (pid uint32, title string, ok bool)) {}
