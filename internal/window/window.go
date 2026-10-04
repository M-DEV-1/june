// Package window lists open windows and brings an already-running window of another application to the front. On GNOME/Wayland it talks over D-Bus to a small GNOME Shell extension (packaging/gnome-extension/june@june.local) that runs inside the shell's own process, since the shell refuses this to any ordinary caller; on Windows it uses Win32 directly.
package window

// Window is one open window as the extension's List reports it. Input fields, filled from the extension's JSON: ID, the window's own id (its HWND on Windows); Pid, the pid of the process that owns it; WmClass, its WM_CLASS (or app id on a Wayland-native client); Title, its title; Focused, whether it currently has focus.
type Window struct {
	ID      int64  `json:"id"`
	Pid     uint32 `json:"pid"`
	WmClass string `json:"wm_class"`
	Title   string `json:"title"`
	Focused bool   `json:"focused"`
	// X, Y, W and H are the window's frame in logical screen pixels, as the shell reports it; all zero from an extension older than the one that reports them.
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"width"`
	H int `json:"height"`
}
