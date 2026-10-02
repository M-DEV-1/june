//go:build linux

package input

import "testing"

// The table used to hold letters, arrows and a handful of names, so Print, the function keys, digits and punctuation were "unknown key": on 2026-09-09 a job could not press Print to open GNOME's screenshot overlay. Every key on a laptop keyboard resolves.
// The underscored and spaced spellings resolve too. The table holds "pagedown", but a model asked to page down writes the X11 keysym it has read a million times, Page_Down, and on 2026-09-12 a run trying to scroll a YouTube page was refused twice for it and then spent its whole step budget pressing Down instead.
func TestChordCoversTheWholeKeyboard(t *testing.T) {
	for _, name := range []string{"Print", "Page_Down", "Page Down", "ctrl+alt+shift+r"} {
		if _, err := chord(name); err != nil {
			t.Errorf("chord(%q): %v", name, err)
		}
	}
}
