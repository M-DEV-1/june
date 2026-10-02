package tracker

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// info dump
// dll - dynamic link library (shared toolbox for win)
// proc - procedure (tool within toolbox)
// W (ref '90s windows text handling) - A (ANSI/ASCII), W (WIDE) - A was 1 byte/char, W was 2 bytes/char but can handle everything in the world (go strings are UTF-8, this helps convert to UTF-16)

var (
	user32                         = windows.NewLazySystemDLL("user32.dll")         // user interface
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")       // system core info
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")          // current active window
	procGetWindowTextW             = user32.NewProc("GetWindowTextW")               // title bar text
	procGetWindowThreadProcessId   = user32.NewProc("GetWindowThreadProcessId")     // return pid for window
	procOpenProcess                = kernel32.NewProc("OpenProcess")                // given process id, get details
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW") // name of .exe for this process
	procCloseHandle                = kernel32.NewProc("CloseHandle")                // close the handler
)

/* overall process for this segment
1. get a handle to focused window
2. get title get PID
3. get app name with PID
4. clean up handles
*/

type winTracker struct{}

func New() (Tracker, error) {
	return &winTracker{}, nil
}

func (w *winTracker) GetActiveWindow() (*Activity, error) {
	// get window handle HWND (handle to a window)
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		// no window has focus, e.g. mid-switch or on the lock screen; Unknown/Unknown is what the daemon skips, the same as Linux with no focus
		return Normalize("Unknown", "Unknown"), nil
	}

	// window title
	titleBuf := make([]uint16, 512)

	// dlls are written in C, hence convert memory pointer returns to string later
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
	title := windows.UTF16ToString(titleBuf)

	return Normalize(windowApp(hwnd), title), nil
}

// windowApp names the application that owns a window: its executable's file name without ".exe", so June's own window reads "june" the way IsJuneWindow expects and app names match Linux's bare names. Input: a window handle. Output: the name, or "" when the process cannot be opened (an elevated process denies a normal one).
func windowApp(hwnd uintptr) string {
	var pid uint32
	// use unsafe only when you're crossing borders
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))

	// hProcess (handle to process)
	// 0x1000 - windows constant for PROCESS_QUERY_LIMITED_INFORMATION
	// returns 0 for access denied
	hProcess, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid))
	if hProcess == 0 {
		return ""
	}
	defer procCloseHandle.Call(hProcess)
	appBuf := make([]uint16, 1024)
	size := uint32(len(appBuf))
	// hProcess, dwFlags (0 for win32 path) lpExeName, lpdwSize
	if ok, _, _ := procQueryFullProcessImageNameW.Call(hProcess, 0, uintptr(unsafe.Pointer(&appBuf[0])), uintptr(unsafe.Pointer(&size))); ok == 0 {
		return ""
	}

	// C:\\something\\something\\something.exe
	fullPath := windows.UTF16ToString(appBuf[:size])
	app := filepath.Base(fullPath)
	// the on-disk casing varies (JUNE.EXE, june.exe), so the suffix is matched without case
	if strings.EqualFold(filepath.Ext(app), ".exe") {
		app = app[:len(app)-len(".exe")]
	}
	return app
}
