package tracker

import "golang.org/x/sys/windows"

// dpiAwarenessPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, which Win32 defines as the handle value -4.
const dpiAwarenessPerMonitorV2 = ^uintptr(3)

// init makes this whole process per-monitor DPI aware before main runs, so Windows reports every coordinate in physical pixels: the screen grab, monitor and window rectangles, the cursor, UI Automation bounding rectangles, and the points SendInput and SetCursorPos take. Without it Windows scales all of these by each monitor's zoom and a point read off a screenshot lands in the wrong place on any monitor not at 100%.
// The setting is process-wide and can be set only once; every other package in the daemon relies on it having happened here.
// SetProcessDpiAwarenessContext needs Windows 10 1703; older systems fall back to SetProcessDPIAware, which is system-wide awareness and is right only when every monitor shares one scale. A failure because a manifest already set the awareness is ignored.
func init() {
	user := windows.NewLazySystemDLL("user32.dll")
	if p := user.NewProc("SetProcessDpiAwarenessContext"); p.Find() == nil {
		if ok, _, _ := p.Call(dpiAwarenessPerMonitorV2); ok != 0 {
			return
		}
	}
	if p := user.NewProc("SetProcessDPIAware"); p.Find() == nil {
		p.Call() //nolint:errcheck
	}
}
