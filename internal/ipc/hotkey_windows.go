//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procPeekMessageW     = user32.NewProc("PeekMessageW")
)

// The RegisterHotKey modifier flags. MOD_NOREPEAT is what the window's own registration passes too (global-hotkey's Windows backend).
const (
	modAlt      = 0x0001
	modControl  = 0x0002
	modShift    = 0x0004
	modWin      = 0x0008
	modNoRepeat = 0x4000
	pmRemove    = 0x0001
)

// probeHotkeyID is the id the test registration uses. An application's ids run from 0 to 0xBFFF, and the test thread registers nothing else.
const probeHotkeyID = 0xBFFF

// probeTimeout bounds one test, so a wedged thread cannot hold a request.
const probeTimeout = 2 * time.Second

// winMsg is MSG, which PeekMessageW fills.
type winMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       struct{ x, y int32 }
	lPrivate uint32
}

// probeRequest is one combination for the test thread to try.
type probeRequest struct {
	mods, vk uint32
	reply    chan error
}

var (
	probeOnce     sync.Once
	probeRequests chan probeRequest
)

// probeThread tries each combination asked of it: it registers it to this thread and, when that works, unregisters it at once. A hotkey registered with no window belongs to the calling thread's message queue, so the thread is locked for good and given its queue before the first try; it never unlocks, since the queue must outlive every test.
func probeThread() {
	runtime.LockOSThread()
	var msg winMsg
	procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 0)
	for req := range probeRequests {
		ok, _, err := procRegisterHotKey.Call(0, probeHotkeyID, uintptr(req.mods|modNoRepeat), uintptr(req.vk))
		if ok == 0 {
			req.reply <- err
			continue
		}
		procUnregisterHotKey.Call(0, probeHotkeyID)
		// A press that landed in the moment the test held the keys is queued here and would otherwise sit there for good.
		for {
			if got, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, pmRemove); got == 0 {
				break
			}
		}
		req.reply <- nil
	}
}

// probeHotkey tests whether a is free by registering it for a moment on a thread of June's own. Output: true when it registered, false when Windows says another program holds it or will not hand it out; an error when the test could not be run at all.
func probeHotkey(a Accel) (bool, error) {
	key, ok := hotkeyKeys[a.Key]
	if !ok {
		return false, fmt.Errorf("no key code for %q", a.Key)
	}
	var mods uint32
	if a.Ctrl {
		mods |= modControl
	}
	if a.Alt {
		mods |= modAlt
	}
	if a.Shift {
		mods |= modShift
	}
	if a.Super {
		mods |= modWin
	}
	probeOnce.Do(func() {
		probeRequests = make(chan probeRequest)
		go probeThread()
	})
	req := probeRequest{mods: mods, vk: key.vk, reply: make(chan error, 1)}
	timeout := time.After(probeTimeout)
	select {
	case probeRequests <- req:
	case <-timeout:
		return false, errors.New("the shortcut test thread is busy")
	}
	select {
	case err := <-req.reply:
		// ERROR_HOTKEY_ALREADY_REGISTERED is another program holding it. Windows also keeps some combinations for itself (F12 for debuggers, many with the Windows key) and refuses them with other errors; to June both are taken.
		return err == nil, nil
	case <-timeout:
		return false, errors.New("the shortcut test did not finish")
	}
}
