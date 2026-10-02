package tracker

import "golang.org/x/sys/windows"

var (
	procOpenInputDesktop = windows.NewLazySystemDLL("user32.dll").NewProc("OpenInputDesktop")
	procCloseDesktop     = windows.NewLazySystemDLL("user32.dll").NewProc("CloseDesktop")
)

// desktopSwitchDesktop is DESKTOP_SWITCHDESKTOP, the access right OpenInputDesktop is asked for.
const desktopSwitchDesktop = 0x0100

// winSessionLocked reports whether the lock screen is up. While it is, the desktop receiving input is the Winlogon desktop, which a user process is denied, so OpenInputDesktop fails with ERROR_ACCESS_DENIED; on the user's own desktop it succeeds and the handle is closed again. Any other failure reads as unlocked, the same direction as on Linux, since wrongly refusing to capture is the harmful way to fail.
// ponytail: the UAC prompt's secure desktop also reads as locked for the seconds it is up, which is harmless because nothing on it can be captured either. Upgrade path: WTSQuerySessionInformationW with WTSSessionInfoEx and its SessionFlags, if this ever misses a lock.
func winSessionLocked() bool {
	h, _, err := procOpenInputDesktop.Call(0, 0, desktopSwitchDesktop)
	if h == 0 {
		return err == windows.ERROR_ACCESS_DENIED
	}
	procCloseDesktop.Call(h) //nolint:errcheck
	return false
}

func init() { sessionLocked = winSessionLocked }
