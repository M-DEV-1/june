//go:build linux

package input

import "testing"

// A bare key name resolves to its single evdev keycode.
func TestChordSingleKey(t *testing.T) {
	codes, err := chord("Enter")
	if err != nil {
		t.Fatalf("chord: %v", err)
	}
	if len(codes) != 1 || codes[0] != keyEnter {
		t.Fatalf("got %v, want [%d]", codes, keyEnter)
	}
}

// A chord presses modifiers before the main key, in the order written.
func TestChordModifiers(t *testing.T) {
	codes, err := chord("Ctrl+L")
	if err != nil {
		t.Fatalf("chord: %v", err)
	}
	want := []int32{keyLeftCtrl, 38}
	if len(codes) != len(want) || codes[0] != want[0] || codes[1] != want[1] {
		t.Fatalf("got %v, want %v", codes, want)
	}
}

// Key names are case-insensitive and tolerate surrounding whitespace.
func TestChordCaseAndSpace(t *testing.T) {
	codes, err := chord(" ctrl + shift + tab ")
	if err != nil {
		t.Fatalf("chord: %v", err)
	}
	want := []int32{keyLeftCtrl, keyLeftShift, keyTab}
	if len(codes) != len(want) || codes[0] != want[0] || codes[1] != want[1] || codes[2] != want[2] {
		t.Fatalf("got %v, want %v", codes, want)
	}
}

// An unknown key name is reported instead of silently producing a wrong code.
func TestChordUnknownKey(t *testing.T) {
	if _, err := chord("Nonsense"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}
