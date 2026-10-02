package input

import (
	"reflect"
	"runtime"
	"testing"
	"unsafe"
)

// PressKey must take the same names on Windows as on Linux, so a key the model learns on one is not "unknown key" on the other. The two tables are kept by hand in separate files, and this holds them to the same set of names.
func TestWindowsKeysMatchLinuxKeys(t *testing.T) {
	for name := range namedKeys {
		if _, ok := vkKeys[name]; !ok {
			t.Errorf("%q is a Linux key with no Windows virtual-key code", name)
		}
	}
	for name := range vkKeys {
		if _, ok := namedKeys[name]; !ok {
			t.Errorf("%q is a Windows key the Linux keyboard refuses", name)
		}
	}
	got, err := vkChord("ctrl+Page_Up")
	if err != nil || !reflect.DeepEqual(got, []uint16{vkControl, vkPrior}) {
		t.Errorf("vkChord(ctrl+Page_Up) = %v, %v", got, err)
	}
}

// Text on Windows goes out as UTF-16 units, so a character outside the Basic Multilingual Plane is two keys, and the characters Linux drops are dropped here too.
func TestTextStrokes(t *testing.T) {
	got := textStrokes("a\r\n\t😀\x7fé")
	want := []stroke{{unit: 'a'}, {vk: vkReturn}, {vk: vkTab}, {unit: 0xD83D}, {unit: 0xDE00}, {unit: 0xE9}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("textStrokes = %v, want %v", got, want)
	}
}

// SendInput refuses every event when its cbSize is not sizeof(INPUT), which is 40 bytes on 64-bit Windows; Go lays these structs out by architecture, not by system, so the size measured here is the one Windows sees.
func TestInputStructSize(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("sizeof(INPUT) checked for 64-bit only")
	}
	if k, p := unsafe.Sizeof(keyInput{}), unsafe.Sizeof(pointerInput{}); k != 40 || p != 40 {
		t.Errorf("keyInput is %d bytes and pointerInput %d, both must be 40", k, p)
	}
}
