package tracker

import (
	"strings"
	"syscall"
	"unsafe"
)

// info dump
// dll - dynamic link library (shared toolbox for win)
// proc - procedure (tool within toolbox)
// W (ref '90s windows text handling) - A (ANSI/ASCII), W (WIDE) - A was 1 byte/char, W was 2 bytes/char but can handle everything in the world (go strings are UTF-8, this helps convert to UTF-16)

var (
	user32                         = syscall.NewLazyDLL("user32.dll")               // user interface
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")             // system core info
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
		return Normalize("Desktop", "None"), nil
	}

	// window title
	titleBuf := make([]uint16, 512)

	// dlls are written in C, hence convert memory pointer returns to string later
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
	title := syscall.UTF16ToString(titleBuf)

	var pid uint32
	// use unsafe only when you're crossing borders
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))

	// hProcess (handle to process)
	// 0x1000 - windows constant for PROCESS_QUERY_LIMITED_INFORMATION
	// returns 0 for access denied
	hProcess, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid))

	app := "Unknown" // safe init
	if hProcess != 0 {
		defer procCloseHandle.Call(hProcess)
		appBuf := make([]uint16, 1024)
		size := uint32(len(appBuf))
		// hProcess, dwFlags (0 for win32 path) lpExeName, lpdwSize
		procQueryFullProcessImageNameW.Call(hProcess, 0, uintptr(unsafe.Pointer(&appBuf[0])), uintptr(unsafe.Pointer(&size)))

		fullPath := syscall.UTF16ToString(appBuf)
		// C:\\something\\something\\something.exe
		parts := strings.Split(fullPath, "\\")
		app = parts[len(parts)-1]
	}

	return Normalize(app, title), nil
}
