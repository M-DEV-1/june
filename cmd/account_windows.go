//go:build windows

package cmd

import (
	"errors"
	"os/user"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemProcess is what Windows' own process list says about one process. It tells any caller, where OpenProcess refuses a process another user is running, so it is what can say whose June holds the port.
type systemProcess struct {
	image   string
	session uint32
	started time.Time
}

// lookupProcess reads pid's entry from the system process list (NtQuerySystemInformation's SystemProcessInformation). Output: the entry, and false when there is no such process or the list could not be read. The list can grow between the call that sizes it and the one that fills it, hence the few rounds.
func lookupProcess(pid uint32) (systemProcess, bool) {
	size := uint32(512 << 10)
	for round := 0; round < 4; round++ {
		// uint64s, so the entries the kernel lays out on eight-byte boundaries land on them.
		buf := make([]uint64, size/8+1)
		var need uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)*8), &need)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			size = max(need, size) + 64<<10
			continue
		}
		if err != nil {
			return systemProcess{}, false
		}
		base := unsafe.Pointer(&buf[0])
		for off := uintptr(0); ; {
			p := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Add(base, off))
			if uint32(p.UniqueProcessID) == pid {
				ft := windows.Filetime{LowDateTime: uint32(p.CreateTime), HighDateTime: uint32(uint64(p.CreateTime) >> 32)}
				return systemProcess{image: p.ImageName.String(), session: p.SessionID, started: time.Unix(0, ft.Nanoseconds())}, true
			}
			if p.NextEntryOffset == 0 {
				return systemProcess{}, false
			}
			off += uintptr(p.NextEntryOffset)
		}
	}
	return systemProcess{}, false
}

// processStarted is when pid started. Output: the time, and false when it cannot be told.
func processStarted(pid uint32) (time.Time, bool) {
	p, ok := lookupProcess(pid)
	return p.started, ok
}

// portHeldByOtherAccount reports whether the program listening on port runs for another Windows account signed in on this PC, which fast user switching leaves running in a session of its own. Loopback ports are shared by every session, so that account's June holds June's port for everyone, and this account's requests to it are refused, its token being another's. Input: the port. Output: that account's user name, "" when Windows would not say; the program's file name, which the system's process list always gives; and false when the holder is this account's own, or not a signed-in account's at all, or cannot be told.
func portHeldByOtherAccount(port string) (account, program string, ok bool) {
	pid, _, found := portHolder(port)
	if !found {
		return "", "", false
	}
	p, found := lookupProcess(pid)
	if !found {
		return "", "", false
	}
	var mine uint32
	// Session 0 is where services and the kernel's own HTTP listener (pid 4, for a URL reserved with netsh http) run, for no signed-in account; the ordinary message names that program and says it is not June.
	if p.session == 0 || windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mine) != nil || p.session == mine {
		return "", "", false
	}
	account = sessionUser(p.session)
	// A program that is not June, in a session Windows names no one for, cannot be said to be anyone's; the ordinary message names it.
	if account == "" && !juneProgram(p.image) {
		return "", "", false
	}
	// The same person signed in twice, at the console and over Remote Desktop, has one data directory and one token, so their June in the other session takes this one's requests and is not someone else's.
	if me, err := user.Current(); err == nil && account != "" && strings.EqualFold(account, me.Username[strings.LastIndexByte(me.Username, '\\')+1:]) {
		return "", "", false
	}
	return account, p.image, true
}

// procWTSQuerySessionInformation is wtsapi32's session query, which golang.org/x/sys/windows does not wrap.
var procWTSQuerySessionInformation = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

// sessionUser is the user name signed in to a Windows session. Output: "" when Windows would not say.
func sessionUser(session uint32) string {
	const wtsUserName = 5
	var buf *uint16
	var size uint32
	if r, _, _ := procWTSQuerySessionInformation.Call(0, uintptr(session), wtsUserName, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&size))); r == 0 || buf == nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return windows.UTF16PtrToString(buf)
}
