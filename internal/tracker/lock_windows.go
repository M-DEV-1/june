package tracker

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procWTSQuerySessionInformation = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

// Values for WTSQuerySessionInformationW: the current session on this machine, the WTSSessionInfoEx info class, and the SessionFlags value of a locked session.
const (
	wtsCurrentSession   = 0xFFFFFFFF
	wtsSessionInfoEx    = 25
	wtsSessionStateLock = 0
)

// wtsInfoEx is WTSINFOEXW up to the field read here: Level, then WTSINFOEX_LEVEL1_W's SessionId, SessionState and SessionFlags. The union holding the level-1 struct is 8-byte aligned because that struct has LARGE_INTEGER fields, so it starts 4 bytes after Level.
type wtsInfoEx struct {
	level        uint32
	_            uint32
	sessionID    uint32
	sessionState int32
	sessionFlags int32
}

// winSessionLocked reports whether the lock screen is up, from the session's own SessionFlags. Any failure reads as unlocked, the same direction as on Linux, since wrongly refusing to capture is the harmful way to fail.
// OpenInputDesktop was used before and does not reliably fail on a Win+L lock on Windows 10 and 11. On Windows 7 and Server 2008 R2 SessionFlags is inverted (lock reads as unlock), which June does not support.
func winSessionLocked() bool {
	var buf *wtsInfoEx
	var n uint32
	ok, _, _ := procWTSQuerySessionInformation.Call(0, wtsCurrentSession, wtsSessionInfoEx, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if ok == 0 || buf == nil {
		return false
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return n >= uint32(unsafe.Sizeof(*buf)) && buf.level == 1 && buf.sessionFlags == wtsSessionStateLock
}

func init() { sessionLocked = winSessionLocked }
