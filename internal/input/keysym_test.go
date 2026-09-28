//go:build linux

package input

import "testing"

// Return, Tab and BackSpace get their named X11 keysym, not their identity value, matching internal/tracker/act_linux.go's keysymFor.
func TestRuneKeysymNamedControls(t *testing.T) {
	cases := map[rune]int32{
		'\n': 0xff0d,
		'\t': 0xff09,
		'\b': 0xff08,
	}
	for r, want := range cases {
		got, ok := runeKeysym(r)
		if !ok || got != want {
			t.Fatalf("runeKeysym(%q) = (%#x, %v), want (%#x, true)", r, got, ok, want)
		}
	}
}

// DEL, the C1 control range, and other control characters (outside \n, \t, \b) must not be identity-mapped or turned into a bogus chord: they aren't printable Latin-1 and have no named keysym, so they are dropped.
func TestRuneKeysymDelAndC1Dropped(t *testing.T) {
	for _, r := range []rune{0x7f, 0x80, 0x9f, 0x90, 0x00, 0x01, 0x1b, 0x1f} {
		if _, ok := runeKeysym(r); ok {
			t.Fatalf("runeKeysym(%#x) should be dropped, was not", r)
		}
	}
}
