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

// namedKeys maps the lowercase key and modifier names PressKey accepts to their evdev keycode.
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
}

// chord splits a key name like "Ctrl+L" into its evdev keycodes, in press order (modifiers first, main key last). A bare key name like "Enter" or "l" returns a single-element slice.
func chord(name string) ([]int32, error) {
	parts := strings.Split(name, "+")
	codes := make([]int32, 0, len(parts))
	for _, p := range parts {
		code, ok := namedKeys[strings.ToLower(strings.TrimSpace(p))]
		if !ok {
			return nil, fmt.Errorf("unknown key %q", p)
		}
		codes = append(codes, code)
	}
	return codes, nil
}
