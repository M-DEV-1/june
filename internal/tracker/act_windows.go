//go:build windows

package tracker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"june/internal/act"
)

// uiaActTimeout bounds one call that acts on or measures an element, the same few seconds the Linux side gives one.
const uiaActTimeout = 4 * time.Second

// uiaActWaitMS is the longest an act may take in the host, finding the element and waiting for its action to return, before it answers that the action is still running, or that it was never fired when finding the element took it all (see the act op in uia.ps1). It sits well inside uiaActTimeout so that answer arrives before this side gives up on the host and kills it.
const uiaActWaitMS = 2000

// uiaOnRef sends one request about a listed element. Input: a context, the op, the node's Ref from act.Node, and the ms the op may spend (0 for one that takes none). Output: the reply, or an error when the ref is malformed, the element has gone or the host failed.
func uiaOnRef(ctx context.Context, op, ref string, ms int) (uiaReply, error) {
	i := strings.IndexByte(ref, ':')
	if i < 1 {
		return uiaReply{}, fmt.Errorf("not a node ref: %q", ref)
	}
	// The ref starts with the handle of the window the element was read from, which is what lets uiaCall refuse a hung window before a request into it hangs the host.
	hwnd, err := strconv.ParseInt(ref[:i], 10, 64)
	if err != nil {
		return uiaReply{}, fmt.Errorf("not a node ref: %q", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, uiaActTimeout)
	defer cancel()
	return uiaCall(ctx, uiaRequest{Op: op, Hwnd: hwnd, Ref: ref, MS: ms})
}

// DoAction fires an element's own UI Automation action, which is what a click does without moving the pointer: Invoke, else Toggle, else SelectionItem's Select, else ExpandCollapse. Input: a context and the node's Ref. Output: the name of the pattern fired (invoke, toggle, select, expand or collapse), or an error naming what failed, including when the element offers none of the four.
// An error wrapping ErrActionUnconfirmed means the action may have gone out and did not report back: it had not returned within uiaActWaitMS, which is what an Invoke that opens a modal dialog can do, or the host was lost after the request reached it. Any other error means nothing was fired.
func DoAction(ctx context.Context, ref string) (string, error) {
	r, err := uiaOnRef(ctx, "act", ref, uiaActWaitMS)
	if r.Pending {
		return r.Action, fmt.Errorf("%s had not returned when the wait ran out, which is what an action that opened a dialog does: %w", r.Action, ErrActionUnconfirmed)
	}
	if errors.As(err, new(uiaSent)) {
		return "", fmt.Errorf("%w: %w", ErrActionUnconfirmed, err)
	}
	return r.Action, err
}

// Verify checks that an element is still what observe_screen listed. Input: a context, the node's Ref, and the role and label the list showed; the rectangle is accepted for the caller's fixed signature and not compared, as on Linux. Output: nil when role and label still match, a *Relabelled when only the label changed, or an error naming what changed or that the element has gone.
func Verify(ctx context.Context, ref, role, label string, x, y, w, h int) error {
	r, err := uiaOnRef(ctx, "desc", ref, 0)
	if err != nil {
		return fmt.Errorf("the element has gone: %w", err)
	}
	return VerifyAgainst(uiaRole(r.uiaNode), uiaLabel(r.uiaNode), role, label)
}

// Extents reads where an element is on the screen right now. Input: a context and the node's Ref. Output: its bounding rectangle in physical virtual-desktop pixels, zero size when it has none, or an error when it cannot be read.
func Extents(ctx context.Context, ref string) (x, y, w, h int, err error) {
	r, err := uiaOnRef(ctx, "desc", ref, 0)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return r.X, r.Y, r.W, r.H, nil
}

// ScrollTo scrolls an element into view through its ScrollItem pattern. Input: a context and the node's Ref. Output: an error when the element has gone or has no ScrollItem pattern.
func ScrollTo(ctx context.Context, ref string) error {
	_, err := uiaOnRef(ctx, "scroll", ref, 0)
	return err
}

// Focused reports whether an element holds the keyboard, it or anything under it. Input: a context and the node's Ref. Output: true when it does, false when it does not, and an error when the element cannot be read.
func Focused(ctx context.Context, ref string) (bool, error) {
	r, err := uiaOnRef(ctx, "focused", ref, 0)
	return r.Focused, err
}

// FocusedElement reports which element of the window in front holds the keyboard. Input: a context. Output: that element's ref, role and name as an act.Node, and false when no window is in front, nothing holds the keyboard, or the element holding it belongs to another process than that window's application (June's own box while its window is up).
func FocusedElement(ctx context.Context) (act.Node, bool) {
	return focusedElement(ctx, false)
}

// FocusedContents is FocusedElement with the element's contents in Value, for a field_holds check: its ValuePattern value whole, up to a capture's cap, or where it publishes none its TextPattern text (Word, Windows Terminal and some rich edits publish only that). Input: a context. Output: the element, as FocusedElement's.
// It is a read of its own because the stop lines ask who holds the keyboard before every type_text and press_key and need none of it, and a long document's contents are a reply of a hundred thousand characters.
func FocusedContents(ctx context.Context) (act.Node, bool) {
	return focusedElement(ctx, true)
}

// focusedElement is FocusedElement and FocusedContents. Input: a context, and whether the contents are read whole. Output: the element.
func focusedElement(ctx context.Context, contents bool) (act.Node, bool) {
	w, ok := frontWindow()
	if !ok {
		return act.Node{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, uiaActTimeout)
	defer cancel()
	r, err := uiaCall(ctx, uiaRequest{Op: "focus", Hwnd: int64(w.hwnd), Text: contents})
	if err != nil || r.None || r.Pid != w.pid {
		return act.Node{}, false
	}
	value := r.Value
	if value == "" {
		value = r.Text
	}
	// The value's line breaks as "\n", the way they were typed (see uiaLineEnds): Notepad hands back a bare "\r" for every one.
	return act.Node{Role: uiaRole(r.uiaNode), Label: uiaLabel(r.uiaNode), Ref: r.Ref, Value: uiaLineEnds(value)}, true
}

// RefWindow names the window an element was read from, off its ref: the handle the ref starts with (see uiaOnRef). Two refs read from one window share it whatever that window's title has become since, and two windows of one application never do. Input: a node's Ref. Output: the handle as text, "" for anything that is not a ref of this tracker's.
func RefWindow(ref string) string {
	i := strings.IndexByte(ref, ':')
	if i < 1 {
		return ""
	}
	if _, err := strconv.ParseInt(ref[:i], 10, 64); err != nil {
		return ""
	}
	return ref[:i]
}

// UseWindowPlacer is a no-op on Windows: UI Automation already reports rectangles in screen pixels, so there is no window shift to correct.
func UseWindowPlacer(f func(ctx context.Context, pid uint32, title string) (x, y, w, h int, ok bool)) {
}
