package input

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// keyName folds the separators a key name is written with, so Page_Down, "Page Down" and PageDown all reach the one "pagedown" the table holds. The X11 keysym spelling is the one a model reaches for first, and it was refused. No name in the table contains an underscore or a space, so nothing is made ambiguous by dropping them; "-" and "minus" are untouched.
var keyName = strings.NewReplacer("_", "", " ", "")

// keyPart turns one part of a chord as the model wrote it into the lowercase name the key tables hold. Input: one part, such as " Page_Down". Output: "pagedown".
func keyPart(p string) string {
	return keyName.Replace(strings.ToLower(strings.TrimSpace(p)))
}

// Windows virtual-key codes (WinUser.h) that the tables below and the text typing need by name.
const (
	vkBack     uint16 = 0x08
	vkTab      uint16 = 0x09
	vkReturn   uint16 = 0x0D
	vkShift    uint16 = 0x10
	vkControl  uint16 = 0x11
	vkMenu     uint16 = 0x12
	vkPrior    uint16 = 0x21
	vkNext     uint16 = 0x22
	vkEnd      uint16 = 0x23
	vkHome     uint16 = 0x24
	vkLeft     uint16 = 0x25
	vkUp       uint16 = 0x26
	vkRight    uint16 = 0x27
	vkDown     uint16 = 0x28
	vkSnapshot uint16 = 0x2C
	vkInsert   uint16 = 0x2D
	vkDelete   uint16 = 0x2E
	vkLWin     uint16 = 0x5B
	vkApps     uint16 = 0x5D
)

// vkKeys maps the same lowercase names keys.go's namedKeys holds to their Windows virtual-key code, so PressKey takes the same names on both systems; a Linux test holds the two tables to the same set of names. Punctuation is the US layout's OEM keys. Modifiers are the left-hand keys, as on Linux.
var vkKeys = map[string]uint16{
	"enter": vkReturn, "return": vkReturn,
	"escape": 0x1B, "esc": 0x1B,
	"tab": vkTab, "space": 0x20,
	"backspace": vkBack, "delete": vkDelete,
	"up": vkUp, "down": vkDown, "left": vkLeft, "right": vkRight,
	"ctrl": vkControl, "control": vkControl,
	"shift": vkShift,
	"alt":   vkMenu,
	"super": vkLWin, "meta": vkLWin, "cmd": vkLWin,
	"a": 'A', "b": 'B', "c": 'C', "d": 'D', "e": 'E', "f": 'F', "g": 'G', "h": 'H',
	"i": 'I', "j": 'J', "k": 'K', "l": 'L', "m": 'M', "n": 'N', "o": 'O', "p": 'P',
	"q": 'Q', "r": 'R', "s": 'S', "t": 'T', "u": 'U', "v": 'V', "w": 'W', "x": 'X',
	"y": 'Y', "z": 'Z',
	"1": '1', "2": '2', "3": '3', "4": '4', "5": '5', "6": '6', "7": '7', "8": '8', "9": '9', "0": '0',
	"-": 0xBD, "minus": 0xBD, "=": 0xBB, "equal": 0xBB, "[": 0xDB, "]": 0xDD, ";": 0xBA, "semicolon": 0xBA,
	"'": 0xDE, "apostrophe": 0xDE, "`": 0xC0, "grave": 0xC0, "\\": 0xDC, "backslash": 0xDC,
	",": 0xBC, "comma": 0xBC, ".": 0xBE, "period": 0xBE, "/": 0xBF, "slash": 0xBF,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75, "f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
	"home": vkHome, "end": vkEnd, "pageup": vkPrior, "pgup": vkPrior, "pagedown": vkNext, "pgdn": vkNext, "insert": vkInsert,
	"print": vkSnapshot, "printscreen": vkSnapshot, "sysrq": vkSnapshot, "pause": 0x13, "capslock": 0x14, "menu": vkApps,
	"volumeup": 0xAF, "volumedown": 0xAE, "mute": 0xAD, "playpause": 0xB3, "nexttrack": 0xB0, "previoustrack": 0xB1,
}

// vkExtended reports whether a virtual key sits on the keyboard's extended (E0-prefixed) scan codes, which SendInput must be told with KEYEVENTF_EXTENDEDKEY or the key arrives as its numeric-keypad twin. Input: a code from vkKeys. Output: true for the navigation block, the arrows, the Windows and menu keys, Print Screen and the media keys.
func vkExtended(vk uint16) bool {
	switch vk {
	case vkPrior, vkNext, vkEnd, vkHome, vkLeft, vkUp, vkRight, vkDown, vkInsert, vkDelete, vkSnapshot, vkLWin, vkApps:
		return true
	}
	return vk >= 0xAD && vk <= 0xB3 // mute through play/pause
}

// vkChord splits a key name like "Ctrl+L" into its virtual-key codes, in press order (modifiers first, main key last). Input: the name as the model wrote it. Output: the codes, or the same "unknown key" error the Linux keyboard gives.
func vkChord(name string) ([]uint16, error) {
	parts := strings.Split(name, "+")
	codes := make([]uint16, 0, len(parts))
	for _, p := range parts {
		code, ok := vkKeys[keyPart(p)]
		if !ok {
			return nil, fmt.Errorf("unknown key %q", p)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// stroke is one key TypeText presses on Windows: a virtual key when vk is set, else one UTF-16 code unit sent as a Unicode key.
type stroke struct {
	vk, unit uint16
}

// textStrokes turns text into the keys that type it on Windows, dropping the same characters the Linux keyboard drops (see runeKeysym). Input: the text. Output: newline, tab and backspace as their virtual keys, every other printable character as its UTF-16 units (two for a character outside the Basic Multilingual Plane), and nothing for carriage return, DEL or any other control character.
func textStrokes(text string) []stroke {
	var out []stroke
	for _, r := range text {
		switch {
		case r == '\n':
			out = append(out, stroke{vk: vkReturn})
		case r == '\t':
			out = append(out, stroke{vk: vkTab})
		case r == '\b':
			out = append(out, stroke{vk: vkBack})
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// dropped, as on Linux
		default:
			for _, u := range utf16.Encode([]rune{r}) {
				out = append(out, stroke{unit: u})
			}
		}
	}
	return out
}

// keybdInput and mouseInput are Win32's KEYBDINPUT and MOUSEINPUT. They live in this untagged file so a Linux test can check the INPUT sizes below on the same architecture Windows runs.
type keybdInput struct {
	vk, scan    uint16
	flags, time uint32
	extraInfo   uintptr
}

type mouseInput struct {
	dx, dy                 int32
	mouseData, flags, time uint32
	extraInfo              uintptr
}

// keyInput and pointerInput are Win32's INPUT with the keyboard and the mouse member of its union. The union is as wide as MOUSEINPUT, so the keyboard form is padded by the 8 bytes KEYBDINPUT is shorter; both come to 40 bytes on 64-bit and 28 on 32-bit, which is the cbSize SendInput checks.
type keyInput struct {
	typ uint32
	ki  keybdInput
	_   [8]byte
}

type pointerInput struct {
	typ uint32
	mi  mouseInput
}
