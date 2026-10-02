//go:build linux

package input

import (
	"fmt"
	"strings"
)

// Linux evdev keycodes (linux/input-event-codes.h) for the keys PressKey and
// chord parsing need. NotifyKeyboardKeycode takes these directly, not X11
// keycodes (which are evdev+8).
const (
	keyEsc       int32 = 1
	keyBackspace int32 = 14
	keyTab       int32 = 15
	keyEnter     int32 = 28
	keyLeftCtrl  int32 = 29
	keySpace     int32 = 57
	keyLeftShift int32 = 42
	keyLeftAlt   int32 = 56
	keyLeftMeta  int32 = 125
	keyUp        int32 = 103
	keyLeft      int32 = 105
	keyRight     int32 = 106
	keyDown      int32 = 108
	keyDelete    int32 = 111
)

// namedKeys maps the lowercase key and modifier names PressKey accepts to their evdev keycode: letters, digits, punctuation, the function keys, the navigation keys, Print and the media keys, so any chord a desktop shortcut uses can be pressed.
var namedKeys = map[string]int32{
	"enter": keyEnter, "return": keyEnter,
	"escape": keyEsc, "esc": keyEsc,
	"tab": keyTab, "space": keySpace,
	"backspace": keyBackspace, "delete": keyDelete,
	"up": keyUp, "down": keyDown, "left": keyLeft, "right": keyRight,
	"ctrl": keyLeftCtrl, "control": keyLeftCtrl,
	"shift": keyLeftShift,
	"alt":   keyLeftAlt,
	"super": keyLeftMeta, "meta": keyLeftMeta, "cmd": keyLeftMeta,
	"a": 30, "b": 48, "c": 46, "d": 32, "e": 18, "f": 33, "g": 34, "h": 35,
	"i": 23, "j": 36, "k": 37, "l": 38, "m": 50, "n": 49, "o": 24, "p": 25,
	"q": 16, "r": 19, "s": 31, "t": 20, "u": 22, "v": 47, "w": 17, "x": 45,
	"y": 21, "z": 44,
	"1": 2, "2": 3, "3": 4, "4": 5, "5": 6, "6": 7, "7": 8, "8": 9, "9": 10, "0": 11,
	"-": 12, "minus": 12, "=": 13, "equal": 13, "[": 26, "]": 27, ";": 39, "semicolon": 39,
	"'": 40, "apostrophe": 40, "`": 41, "grave": 41, "\\": 43, "backslash": 43,
	",": 51, "comma": 51, ".": 52, "period": 52, "/": 53, "slash": 53,
	"f1": 59, "f2": 60, "f3": 61, "f4": 62, "f5": 63, "f6": 64, "f7": 65, "f8": 66, "f9": 67, "f10": 68, "f11": 87, "f12": 88,
	"home": 102, "end": 107, "pageup": 104, "pgup": 104, "pagedown": 109, "pgdn": 109, "insert": 110,
	"print": 99, "printscreen": 99, "sysrq": 99, "pause": 119, "capslock": 58, "menu": 127,
	"volumeup": 115, "volumedown": 114, "mute": 113, "playpause": 164, "nexttrack": 163, "previoustrack": 165,
}

// chord splits a key name like "Ctrl+L" into its evdev keycodes, in press order (modifiers first, main key last). A bare key name like "Enter" or "l" returns a single-element slice.
func chord(name string) ([]int32, error) {
	parts := strings.Split(name, "+")
	codes := make([]int32, 0, len(parts))
	for _, p := range parts {
		code, ok := namedKeys[keyPart(p)]
		if !ok {
			return nil, fmt.Errorf("unknown key %q", p)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// ValidChord reports whether a key name or chord is one this keyboard can press, without opening a portal session. Input: the name as the model wrote it. Output: nil when every part resolves, else the error PressKey would have returned.
// Exported so a test that stands in for the keyboard can refuse exactly what the real one refuses. A fake that accepts every string turns the model's own spelling — Page_Down rather than PageDown — into something no test above this package can see.
func ValidChord(name string) error {
	_, err := chord(name)
	return err
}
