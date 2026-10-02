package window

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	dwmapi                         = syscall.NewLazyDLL("dwmapi.dll")
	procEnumWindows                = user32.NewProc("EnumWindows")
	procIsWindowVisible            = user32.NewProc("IsWindowVisible")
	procGetWindow                  = user32.NewProc("GetWindow")
	procGetWindowTextW             = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessId   = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow        = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop           = user32.NewProc("BringWindowToTop")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procIsIconic                   = user32.NewProc("IsIconic")
	procGetWindowRect              = user32.NewProc("GetWindowRect")
	procAttachThreadInput          = user32.NewProc("AttachThreadInput")
	procGetCurrentThreadId         = kernel32.NewProc("GetCurrentThreadId")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procDwmGetWindowAttribute      = dwmapi.NewProc("DwmGetWindowAttribute")
)

const (
	gwOwner                        = 4
	swRestore                      = 9
	dwmwaExtendedFrameBounds       = 9
	dwmwaCloaked                   = 14
	processQueryLimitedInformation = 0x1000
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

// List enumerates the windows a person would see in Alt+Tab: visible, unowned, titled, and not cloaked (a cloaked window is a suspended UWP app or one on another virtual desktop). Input: a context, unused because EnumWindows does not block. Output: one Window per such window, with WmClass set to the owning program's file name without ".exe" and the frame in physical pixels.
func (r *Raiser) List(ctx context.Context) ([]Window, error) {
	fg, _, _ := procGetForegroundWindow.Call()
	var out []Window
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if w, ok := describe(hwnd, fg); ok {
			out = append(out, w)
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out, nil
}

// describe reads one top-level window. Input: the window handle and the current foreground handle. Output: the Window, and false when it is not one a person would switch to.
func describe(hwnd, fg uintptr) (Window, bool) {
	if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
		return Window{}, false
	}
	if owner, _, _ := procGetWindow.Call(hwnd, gwOwner); owner != 0 {
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

// ByPid activates the first window owned by process pid. Output: true if one was found and came to the front.
func (r *Raiser) ByPid(ctx context.Context, pid uint32) (bool, error) {
	return r.activateFirst(ctx, func(w Window) bool { return w.Pid == pid })
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
			return activate(uintptr(w.ID)), nil
		}
	}
	return false, nil
}

// activate restores and brings a window to the front. Windows refuses SetForegroundWindow to a background process like the daemon, so the calling thread first attaches its input queue to the foreground window's thread, which lets the call through. Input: the window handle. Output: whether the window is now the foreground window.
func activate(hwnd uintptr) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	fg, _, _ := procGetForegroundWindow.Call()
	self, _, _ := procGetCurrentThreadId.Call()
	other, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
	if other != 0 && other != self {
		procAttachThreadInput.Call(self, other, 1)
		defer procAttachThreadInput.Call(self, other, 0)
	}
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	now, _, _ := procGetForegroundWindow.Call()
	return now == hwnd
}
