//go:build linux

package input

import "testing"

// Printable ASCII/Latin-1 characters map to the identical X11 keysym value.
func TestRuneKeysymLatin1(t *testing.T) {
	if got, ok := runeKeysym('A'); !ok || got != 0x41 {
		t.Fatalf("got (%#x, %v), want (0x41, true)", got, ok)
	}
	if got, ok := runeKeysym(' '); !ok || got != 0x20 {
		t.Fatalf("got (%#x, %v), want (0x20, true)", got, ok)
	}
}

// Characters outside Latin-1 use the X11 Unicode keysym range (codepoint | 0x01000000).
func TestRuneKeysymUnicode(t *testing.T) {
	// U+20AC EURO SIGN.
	if got, ok := runeKeysym('€'); !ok || got != 0x010020AC {
		t.Fatalf("got (%#x, %v), want (0x010020ac, true)", got, ok)
	}
}

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

// DEL and the C1 control range must not be identity-mapped: they aren't printable Latin-1 and have no named keysym, so they are dropped.
func TestRuneKeysymDelAndC1Dropped(t *testing.T) {
	for _, r := range []rune{0x7f, 0x80, 0x9f, 0x90} {
		if _, ok := runeKeysym(r); ok {
			t.Fatalf("runeKeysym(%#x) should be dropped, was not", r)
		}
	}
}

// Other control characters (outside \n, \t, \b) are dropped rather than turned into a bogus chord.
func TestRuneKeysymOtherControlsDropped(t *testing.T) {
	for _, r := range []rune{0x00, 0x01, 0x1b, 0x1f} {
		if _, ok := runeKeysym(r); ok {
			t.Fatalf("runeKeysym(%#x) should be dropped, was not", r)
		}
	}
}
