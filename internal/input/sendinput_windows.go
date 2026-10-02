package input

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// NewLazySystemDLL loads from System32 only, so a user32.dll planted beside june.exe is never picked up.
	user32               = windows.NewLazySystemDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procSetCursorPos     = user32.NewProc("SetCursorPos")
	procMapVirtualKeyW   = user32.NewProc("MapVirtualKeyW")
	procMonitorFromPoint = user32.NewProc("MonitorFromPoint")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

// Win32 constants from WinUser.h.
const (
	inputMouse    = 0
	inputKeyboard = 1

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfUnicode     = 0x0004

	mouseeventfLeftDown  = 0x0002
	mouseeventfLeftUp    = 0x0004
	mouseeventfRightDown = 0x0008
	mouseeventfRightUp   = 0x0010
	mouseeventfWheel     = 0x0800

	wheelDelta = 120

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	smSwapButton      = 23
)

// Session is the keyboard and pointer on Windows, driven through SendInput. The zero value is ready to use; there is no consent dialog and nothing to open or close.
type Session struct {
	// acting is held for the whole of PressKey, TypeText, ClickAt and ScrollAt, each of which is several separate events paced keyDelay apart. One Session is shared by every ask, and without this one ask's modifier press lands between another's key down and key up.
	acting sync.Mutex
}

// ValidChord reports whether a key name or chord is one this keyboard can press. Input: the name as the model wrote it. Output: nil when every part resolves, else the error PressKey would have returned.
func ValidChord(name string) error {
	_, err := vkChord(name)
	return err
}

// send hands events to SendInput. Input: a pointer to the first INPUT, how many there are, and the size of one. Output: an error when Windows inserted fewer events than it was given, which is what input aimed at a window running as administrator, or at the lock screen, looks like.
func send(first unsafe.Pointer, n int, size uintptr) error {
	got, _, errno := procSendInput.Call(uintptr(n), uintptr(first), size)
	if int(got) != n {
		msg := fmt.Sprintf("input: Windows took %d of %d synthesized input events; the window under it may be running as administrator, or the screen is locked", got, n)
		if errno != syscall.Errno(0) {
			msg += fmt.Sprintf(" (%v)", errno)
		}
		return errors.New(msg)
	}
	return nil
}

// sendKey sends one key event. Input: the virtual key, or 0 with a UTF-16 unit in unit for a Unicode character, and whether this is the release. Output: SendInput's refusal, or nil.
func sendKey(vk, unit uint16, up bool) error {
	in := keyInput{typ: inputKeyboard}
	if vk != 0 {
		// The scan code rides along because Chromium builds KeyboardEvent.code from it, and a web page's shortcut that checks code does nothing with 0.
		scan, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
		in.ki.vk, in.ki.scan = vk, uint16(scan)
		if vkExtended(vk) {
			in.ki.flags |= keyeventfExtendedKey
		}
	} else {
		in.ki.scan = unit
		in.ki.flags = keyeventfUnicode
	}
	if up {
		in.ki.flags |= keyeventfKeyUp
	}
	return send(unsafe.Pointer(&in), 1, unsafe.Sizeof(in))
}

// sendPointer sends one mouse event at wherever the cursor is. Input: the MOUSEEVENTF flags and the wheel amount. Output: SendInput's refusal, or nil.
func sendPointer(flags, data uint32) error {
	in := pointerInput{typ: inputMouse, mi: mouseInput{flags: flags, mouseData: data}}
	return send(unsafe.Pointer(&in), 1, unsafe.Sizeof(in))
}

// PressKey sends a full press-then-release for the named key or chord (e.g. "Enter", "Ctrl+L"), paced keyDelay apart. Modifiers are pressed first and released last, in reverse order. Every key that went down is released even when a later press fails, because a modifier left held is chorded into whatever is sent next. Input: the key or chord name. Output: the first error hit, pressing or releasing, or nil.
func (s *Session) PressKey(name string) error {
	codes, err := vkChord(name)
	if err != nil {
		return err
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	held := 0
	for _, c := range codes {
		if err = sendKey(c, 0, false); err != nil {
			break
		}
		held++
		time.Sleep(keyDelay)
	}
	for i := held - 1; i >= 0; i-- {
		if e := sendKey(codes[i], 0, true); e != nil && err == nil {
			err = e
		}
		time.Sleep(keyDelay)
	}
	return err
}

// TypeText presses and releases each character of text in turn, paced keyDelay apart: newline, tab and backspace as their keys, everything else as Unicode characters, so any text types whatever the keyboard layout. Characters Linux drops are dropped (see textStrokes). Input: the text. Output: the first refusal, or nil.
func (s *Session) TypeText(text string) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	for _, k := range textStrokes(text) {
		if err := sendKey(k.vk, k.unit, false); err != nil {
			return err
		}
		time.Sleep(keyDelay)
		if err := sendKey(k.vk, k.unit, true); err != nil {
			sendKey(k.vk, k.unit, true) // one more try, so the key is not left held
			return err
		}
		time.Sleep(keyDelay)
	}
	return nil
}

// moveTo puts the cursor on a desktop point. Input: the point in physical virtual-desktop pixels, the space screenshots and window rectangles are measured in once the daemon is per-monitor DPI aware; it is negative on a monitor left of or above the main one. Output: an error naming the desktop's extent when the point is on no monitor, including a gap between two, or when Windows refused the move.
func moveTo(x, y float64) error {
	ix, iy := int32(math.Round(x)), int32(math.Round(y))
	// MonitorFromPoint takes a POINT by value, which 64-bit Windows passes as one 8-byte register, x in the low half; 0 is MONITOR_DEFAULTTONULL.
	if m, _, _ := procMonitorFromPoint.Call(uintptr(uint32(ix))|uintptr(uint32(iy))<<32, 0); m == 0 {
		vx, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
		vy, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
		vw, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
		vh, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
		left, top := int32(vx), int32(vy)
		return fmt.Errorf("input: %d,%d is not on any monitor; the desktop spans %d,%d to %d,%d", ix, iy, left, top, left+int32(vw)-1, top+int32(vh)-1)
	}
	if ok, _, errno := procSetCursorPos.Call(uintptr(ix), uintptr(iy)); ok == 0 {
		return fmt.Errorf("input: Windows would not move the pointer to %d,%d (%v)", ix, iy, errno)
	}
	return nil
}

// ClickAt moves the pointer to (x, y), in physical virtual-desktop pixels, and clicks the primary button.
func (s *Session) ClickAt(x, y float64) error {
	return s.click(x, y, mouseeventfLeftDown, mouseeventfLeftUp)
}

// RightClickAt moves the pointer to (x, y), in physical virtual-desktop pixels, and clicks the secondary button, which is what opens a context menu.
func (s *Session) RightClickAt(x, y float64) error {
	return s.click(x, y, mouseeventfRightDown, mouseeventfRightUp)
}

// click moves the pointer and presses and releases one button there. SendInput's left and right are the physical buttons, which Windows swaps for a user who set the mouse left-handed, so they are swapped back here. A refused release is sent once more, because a button left held drags whatever the pointer touches next. Input: the point and the button's down and up flags. Output: the first error from the move or either half of the press.
func (s *Session) click(x, y float64, down, up uint32) error {
	if swapped, _, _ := procGetSystemMetrics.Call(smSwapButton); swapped != 0 {
		down ^= mouseeventfLeftDown | mouseeventfRightDown
		up ^= mouseeventfLeftUp | mouseeventfRightUp
	}
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := moveTo(x, y); err != nil {
		return err
	}
	if err := sendPointer(down, 0); err != nil {
		return err
	}
	if err := sendPointer(up, 0); err != nil {
		sendPointer(up, 0) // one more try, so the button is not left held
		return err
	}
	return nil
}

// ScrollAt moves the pointer to (x, y), in physical virtual-desktop pixels, and turns the wheel dy notches (positive is down, as on Linux; Windows counts toward the user as negative).
func (s *Session) ScrollAt(x, y float64, dy int32) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := moveTo(x, y); err != nil {
		return err
	}
	return sendPointer(mouseeventfWheel, uint32(-dy*wheelDelta))
}
