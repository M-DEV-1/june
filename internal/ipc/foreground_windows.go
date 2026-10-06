package ipc

import "golang.org/x/sys/windows"

var procAllowSetForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

// asfwAny is ASFW_ANY, (DWORD)-1: any process may take the foreground.
const asfwAny = 0xFFFFFFFF

// letOpenedComeForward lets the window June is about to open (a browser tab, Windows' Settings) take the foreground rather than open behind the one in front. Windows lets only a process that may take the foreground itself pass that on, which the daemon may after a click on its own tray menu but not after a click in June's window, a separate process; there the call fails, harmlessly, and only the window calling it before it posts would bring the page forward. Output: none.
func letOpenedComeForward() {
	procAllowSetForegroundWindow.Call(asfwAny)
}
