package tracker

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetLastInputInfo = windows.NewLazySystemDLL("user32.dll").NewProc("GetLastInputInfo")
	procGetTickCount     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount")
)

// lastInputInfo is Win32's LASTINPUTINFO: the struct's own size, which must be 8, and the tick count of the last keyboard or mouse input in this session.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// winInputIdle reports how long since the last real keyboard or mouse input in this session. Output: the idle time, or an error when GetLastInputInfo fails.
// Both tick counts are 32-bit milliseconds that wrap every 49.7 days; subtracting them as uint32 gives the right answer across a wrap.
func winInputIdle() (time.Duration, error) {
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if ok, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return 0, fmt.Errorf("GetLastInputInfo: %w", err)
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-info.dwTime) * time.Millisecond, nil
}

func init() { inputIdle = winInputIdle }
