package window

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                         = windows.NewLazySystemDLL("user32.dll")
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	dwmapi                         = windows.NewLazySystemDLL("dwmapi.dll")
	procEnumWindows                = user32.NewProc("EnumWindows")
	procIsWindowVisible            = user32.NewProc("IsWindowVisible")
	procGetWindow                  = user32.NewProc("GetWindow")
	procGetWindowTextW             = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessId   = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow        = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop           = user32.NewProc("BringWindowToTop")
	procShowWindowAsync            = user32.NewProc("ShowWindowAsync")
	procIsHungAppWindow            = user32.NewProc("IsHungAppWindow")
	procIsIconic                   = user32.NewProc("IsIconic")
	procGetWindowRect              = user32.NewProc("GetWindowRect")
	procAttachThreadInput          = user32.NewProc("AttachThreadInput")
	procGetShellWindow             = user32.NewProc("GetShellWindow")
	procGetWindowLongW             = user32.NewProc("GetWindowLongW")
	procSendInput                  = user32.NewProc("SendInput")
	procGetCurrentThreadId         = kernel32.NewProc("GetCurrentThreadId")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procDwmGetWindowAttribute      = dwmapi.NewProc("DwmGetWindowAttribute")
)

const (
	gwOwner                        = 4
	swRestore                      = 9
	wsExToolWindow                 = 0x80
	inputMouse                     = 0
	mouseeventfMove                = 0x0001
	dwmwaExtendedFrameBounds       = 9
	dwmwaCloaked                   = 14
	processQueryLimitedInformation = 0x1000
)

// gwlExStyle is GetWindowLong's index for the extended style. It is negative, so it is a variable: a constant cannot be converted to the uintptr the call takes.
var gwlExStyle = int32(-20)

// How activate waits for the switch to land. SetForegroundWindow returns before it has: GetForegroundWindow reads 0, or the old window, for a few milliseconds after the call (about 6 ms, measured on Windows 11 on 2026-10-03), so reading it once straight after reported a window that did come forward as one that did not, and the caller went on to raise the next candidate.
const (
	activateSettle = 500 * time.Millisecond
	activatePoll   = 10 * time.Millisecond
)

// Raiser lists and activates top-level windows through Win32. The zero value is ready to use and it is safe for concurrent use.
type Raiser struct{}

// New returns a Raiser. Input: none. Output: the Raiser and a nil error; nothing has to be dialled on Windows.
func New() (*Raiser, error) { return &Raiser{}, nil }

// Close does nothing; it exists so the Linux and Windows Raisers share one shape.
func (r *Raiser) Close() error { return nil }

// Available reports true: Win32 can always list and activate windows.
func (r *Raiser) Available(ctx context.Context) (bool, error) { return true, nil }

type rect struct{ Left, Top, Right, Bottom int32 }

// List enumerates the windows a person would see in Alt+Tab: visible, unowned, titled, not cloaked (a cloaked window is a suspended UWP app or one on another virtual desktop), not a tool window and not the desktop. Input: a context, unused because EnumWindows does not block. Output: one Window per such window, with WmClass set to the owning program's file name without ".exe" and the frame in physical pixels.
func (r *Raiser) List(ctx context.Context) ([]Window, error) {
	listMu.Lock()
	defer listMu.Unlock()
	listFg, _, _ = procGetForegroundWindow.Call()
	listOut = nil
	procEnumWindows.Call(listCallback, 0)
	return listOut, nil
}

// The EnumWindows callback is made once: Go never frees a callback made with syscall.NewCallback and the process dies after about two thousand of them. listMu guards the two variables it writes.
var (
	listMu       sync.Mutex
	listFg       uintptr
	listOut      []Window
	listCallback = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if w, ok := describe(hwnd, listFg); ok {
			listOut = append(listOut, w)
		}
		return 1
	})
)

// describe reads one top-level window. Input: the window handle and the current foreground handle. Output: the Window, and false when it is not one a person would switch to.
func describe(hwnd, fg uintptr) (Window, bool) {
	if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
		return Window{}, false
	}
	if owner, _, _ := procGetWindow.Call(hwnd, gwOwner); owner != 0 {
		return Window{}, false
	}
	// The desktop ("Program Manager") and tool windows are nothing a person switches to, and they share a process with windows that are: the desktop is explorer.exe's, like every File Explorer window, so raising explorer.exe by pid could land on the desktop.
	if shell, _, _ := procGetShellWindow.Call(); hwnd == shell {
		return Window{}, false
	}
	if ex, _, _ := procGetWindowLongW.Call(hwnd, uintptr(gwlExStyle)); uint32(ex)&wsExToolWindow != 0 {
		return Window{}, false
	}
	var cloaked uint32
	if hr, _, _ := procDwmGetWindowAttribute.Call(hwnd, dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), 4); hr == 0 && cloaked != 0 {
		return Window{}, false
	}
	buf := make([]uint16, 512)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	title := syscall.UTF16ToString(buf)
	if title == "" {
		return Window{}, false
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	var b rect
	if hr, _, _ := procDwmGetWindowAttribute.Call(hwnd, dwmwaExtendedFrameBounds, uintptr(unsafe.Pointer(&b)), unsafe.Sizeof(b)); hr != 0 {
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&b)))
	}
	return Window{
		ID: int64(hwnd), Pid: pid, WmClass: programName(pid), Title: title, Focused: hwnd == fg,
		X: int(b.Left), Y: int(b.Top), W: int(b.Right - b.Left), H: int(b.Bottom - b.Top),
	}, true
}

// programName returns the file name of a process's executable without ".exe", or "" when the process cannot be opened.
func programName(pid uint32) string {
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if ok, _, _ := procQueryFullProcessImageNameW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); ok == 0 {
		return ""
	}
	name := filepath.Base(syscall.UTF16ToString(buf[:size]))
	return strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".EXE")
}

// ByPid activates the first window owned by process pid, the one nearest the front. Many windows share a pid here (every Store app's frame is ApplicationFrameHost's, every browser window its browser's), so a caller holding a window from List raises it with ByWindow instead. Output: true if one was found and came to the front.
func (r *Raiser) ByPid(ctx context.Context, pid uint32) (bool, error) {
	return r.activateFirst(ctx, func(w Window) bool { return w.Pid == pid })
}

// ByWindow activates the very window List reported, by its handle. Input: a window from List. Output: true if it came to the front; false when it has closed since, when its handle now names another process's window (Windows reuses the handles of closed windows), or when it would not come forward.
func (r *Raiser) ByWindow(ctx context.Context, w Window) (bool, error) {
	hwnd := uintptr(w.ID)
	if now, ok := describe(hwnd, 0); !ok || now.Pid != w.Pid {
		return false, nil
	}
	return activate(ctx, hwnd), nil
}

// ByTitle activates the first window whose title contains substring, ignoring case. Output: true if one was found and came to the front.
func (r *Raiser) ByTitle(ctx context.Context, substring string) (bool, error) {
	sub := strings.ToLower(substring)
	return r.activateFirst(ctx, func(w Window) bool { return strings.Contains(strings.ToLower(w.Title), sub) })
}

// ByWmClass activates the first window whose program name equals wmClass, ignoring case. Output: true if one was found and came to the front.
func (r *Raiser) ByWmClass(ctx context.Context, wmClass string) (bool, error) {
	return r.activateFirst(ctx, func(w Window) bool { return strings.EqualFold(w.WmClass, wmClass) })
}

func (r *Raiser) activateFirst(ctx context.Context, match func(Window) bool) (bool, error) {
	windows, _ := r.List(ctx)
	for _, w := range windows {
		if match(w) {
			return activate(ctx, uintptr(w.ID)), nil
		}
	}
	return false, nil
}

// activate restores and brings a window to the front, then waits up to activateSettle for Windows to report it there. Windows refuses SetForegroundWindow to a background process like the daemon, so the calling thread first attaches its input queue to the foreground window's thread. That is not always enough: with the foreground locked (LockSetForegroundWindow, which the process in front may call) the call was refused with the attach as without it, and went through once the calling process had sent an input event of its own, which is what nudge sends before a refused call is made once more. Measured on Windows 11 on 2026-10-03. Input: a context that cuts the wait short, and the window handle. Output: whether the window became the foreground window.
func activate(ctx context.Context, hwnd uintptr) bool {
	// A caller whose time is spent is no longer watching: a window raised now would come forward after it had already been told no, and it would go on to the keyboard on top of it.
	if ctx.Err() != nil {
		return false
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Async, and no attach to a hung foreground thread: both calls otherwise wait on the other program's message loop, which never answers when it is hung.
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindowAsync.Call(hwnd, swRestore)
	}
	if !setForeground(hwnd) {
		nudge()
		if !setForeground(hwnd) {
			// A refusal is final: under a locked foreground three refused calls in a row each left the window behind for the 1.5 s watched after it (measured 2026-10-03), so there is no switch to wait for, and a caller trying window after window under one deadline must not spend activateSettle on each.
			now, _, _ := procGetForegroundWindow.Call()
			return now == hwnd
		}
	}
	deadline := time.Now().Add(activateSettle)
	for {
		if now, _, _ := procGetForegroundWindow.Call(); now == hwnd {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(activatePoll)
	}
}

// setForeground makes one SetForegroundWindow call for hwnd with the calling thread's input attached to the foreground window's thread for its duration. The caller holds its goroutine on one OS thread, since the attach belongs to the thread. Output: SetForegroundWindow's answer, false when Windows refused the switch.
func setForeground(hwnd uintptr) bool {
	fg, _, _ := procGetForegroundWindow.Call()
	self, _, _ := procGetCurrentThreadId.Call()
	other, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
	if hung, _, _ := procIsHungAppWindow.Call(fg); hung != 0 {
		other = 0
	}
	if other != 0 && other != self {
		procAttachThreadInput.Call(self, other, 1)
		defer procAttachThreadInput.Call(self, other, 0)
	}
	procBringWindowToTop.Call(hwnd)
	ok, _, _ := procSetForegroundWindow.Call(hwnd)
	return ok != 0
}

// pointerInput is Win32's INPUT with its MOUSEINPUT member, which is 40 bytes on 64-bit and 28 on 32-bit, the size SendInput checks.
type pointerInput struct {
	typ uint32
	mi  struct {
		dx, dy                 int32
		mouseData, flags, time uint32
		extraInfo              uintptr
	}
}

// nudge sends one mouse event that moves the pointer by nothing, which makes this process the one that sent the last input event, and Windows lets that process set the foreground window. It is a mouse move rather than the Alt press usually used for this because a lone Alt opens the menu bar of whichever window takes it, and an unused key such as F24 is one a macro tool may have bound.
func nudge() {
	in := pointerInput{typ: inputMouse}
	in.mi.flags = mouseeventfMove
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}
