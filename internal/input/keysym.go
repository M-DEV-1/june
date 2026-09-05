//go:build linux

package input

// runeKeysym converts a rune to the X11 keysym NotifyKeyboardKeysym expects. Input: one rune. Output: the keysym and true, or (0, false) when the rune has no sensible keysym and should be dropped rather than sent.
// Return, Tab and BackSpace get their named keysym (matching the sibling keysymFor in internal/tracker/act_linux.go). DEL (0x7f) and the C1 control range (0x80-0x9f) sit inside the Latin-1 byte range but are not printable characters and have no named keysym, so they are dropped, along with every other control character below 0x20.
func runeKeysym(r rune) (int32, bool) {
	switch r {
	case '\n':
		return 0xff0d, true
	case '\t':
		return 0xff09, true
	case '\b':
		return 0xff08, true
	}
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return 0, false
	}
	if r <= 0xff {
		return int32(r), true
	}
	return int32(r) | 0x01000000, true
}
