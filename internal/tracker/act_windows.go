//go:build windows

package tracker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"june/internal/act"
)

// uiaActTimeout bounds one call that acts on or measures an element, the same few seconds the Linux side gives one.
const uiaActTimeout = 4 * time.Second

// uiaOnRef sends one request about a listed element. Input: a context, the op and the node's Ref from act.Node. Output: the reply, or an error when the ref is malformed, the element has gone or the host failed.
func uiaOnRef(ctx context.Context, op, ref string) (uiaReply, error) {
	if !strings.Contains(ref, ":") {
		return uiaReply{}, fmt.Errorf("not a node ref: %q", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, uiaActTimeout)
	defer cancel()
	return uiaCall(ctx, uiaRequest{Op: op, Ref: ref})
}

// DoAction fires an element's own UI Automation action, which is what a click does without moving the pointer: Invoke, else Toggle, else SelectionItem's Select, else ExpandCollapse. Input: a context and the node's Ref. Output: the name of the pattern fired (invoke, toggle, select, expand or collapse), or an error naming what failed, including when the element offers none of the four.
func DoAction(ctx context.Context, ref string) (string, error) {
	r, err := uiaOnRef(ctx, "act", ref)
	return r.Action, err
}

// Verify checks that an element is still what observe_screen listed. Input: a context, the node's Ref, and the role and label the list showed; the rectangle is accepted for the caller's fixed signature and not compared, as on Linux. Output: nil when role and label still match, a *Relabelled when only the label changed, or an error naming what changed or that the element has gone.
func Verify(ctx context.Context, ref, role, label string, x, y, w, h int) error {
	r, err := uiaOnRef(ctx, "desc", ref)
	if err != nil {
		return fmt.Errorf("the element has gone: %w", err)
	}
	return VerifyAgainst(uiaRole(r.uiaNode), uiaLabel(r.uiaNode), role, label)
}

// Extents reads where an element is on the screen right now. Input: a context and the node's Ref. Output: its bounding rectangle in physical virtual-desktop pixels, zero size when it has none, or an error when it cannot be read.
func Extents(ctx context.Context, ref string) (x, y, w, h int, err error) {
	r, err := uiaOnRef(ctx, "desc", ref)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return r.X, r.Y, r.W, r.H, nil
}

// ScrollTo scrolls an element into view through its ScrollItem pattern. Input: a context and the node's Ref. Output: an error when the element has gone or has no ScrollItem pattern.
func ScrollTo(ctx context.Context, ref string) error {
	_, err := uiaOnRef(ctx, "scroll", ref)
	return err
}

// Focused reports whether an element holds the keyboard, it or anything under it. Input: a context and the node's Ref. Output: true when it does, false when it does not, and an error when the element cannot be read.
func Focused(ctx context.Context, ref string) (bool, error) {
	r, err := uiaOnRef(ctx, "focused", ref)
	return r.Focused, err
}

// FocusedElement reports which element of the window in front holds the keyboard. Input: a context. Output: that element's ref, role and name as an act.Node, and false when no window is in front, nothing holds the keyboard, or the element holding it belongs to another process than that window's (June's own box while its window is up, or a store app whose content runs outside its frame process).
func FocusedElement(ctx context.Context) (act.Node, bool) {
	w, ok := frontWindow()
	if !ok {
		return act.Node{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, uiaActTimeout)
	defer cancel()
	r, err := uiaCall(ctx, uiaRequest{Op: "focus", Hwnd: int64(w.hwnd)})
	if err != nil || r.None || r.Pid != w.pid {
		return act.Node{}, false
	}
	return act.Node{Role: uiaRole(r.uiaNode), Label: uiaLabel(r.uiaNode), Ref: r.Ref}, true
}

// UseWindowPlacer is a no-op on Windows: UI Automation already reports rectangles in screen pixels, so there is no window shift to correct.
func UseWindowPlacer(f func(ctx context.Context, pid uint32, title string) (x, y, w, h int, ok bool)) {
}
