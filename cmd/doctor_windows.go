//go:build windows

package cmd

import (
	"path/filepath"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// platformChecks reports what the desktop itself must provide: the WebView2 runtime the window renders in, and the microphone consent store call detection reads. Windows records through WASAPI, which every desk has, so there is no audio server to check. Input: ignored, it is $XDG_RUNTIME_DIR on Linux. Output: one check each.
func platformChecks(string) []doctorCheck {
	return []doctorCheck{webView2Check(), callDetectionCheck()}
}

// webView2ClientKey is the EdgeUpdate client key of the WebView2 Evergreen runtime; an installed runtime leaves its version in the key's pv value.
const webView2ClientKey = `Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

// webView2Check reports whether the WebView2 runtime is installed, per machine or per user, which is where Microsoft's own installers record it. Without it the window cannot start at all.
func webView2Check() doctorCheck {
	for _, loc := range []struct {
		root registry.Key
		name string
		path string
	}{
		{registry.LOCAL_MACHINE, "HKLM", `SOFTWARE\WOW6432Node\` + webView2ClientKey},
		{registry.LOCAL_MACHINE, "HKLM", `SOFTWARE\` + webView2ClientKey},
		{registry.CURRENT_USER, "HKCU", `Software\` + webView2ClientKey},
	} {
		k, err := registry.OpenKey(loc.root, loc.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pv, _, err := k.GetStringValue("pv")
		k.Close()
		if err == nil && pv != "" && pv != "0.0.0.0" {
			return doctorCheck{Name: "window runtime", Detail: "WebView2 " + pv + " at " + loc.name + `\` + loc.path, OK: true}
		}
	}
	return doctorCheck{Name: "window runtime", Detail: "no WebView2 runtime found under HKLM or HKCU; the June window cannot open without it", Fix: "install Microsoft's WebView2 Evergreen Bootstrapper from https://developer.microsoft.com/microsoft-edge/webview2/", Required: true}
}

// callDetectionCheck reports whether call detection can see which programs are using the microphone. It reads the per-user microphone consent store (internal/recorder/meetingwatch_windows.go), so the check opens the same key.
func callDetectionCheck() doctorCheck {
	const key = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return doctorCheck{Name: "call detection", Detail: `cannot open HKCU\` + key + ": " + err.Error() + "; noticing a call and offering to record it is off", Fix: "turn on Settings > Privacy & security > Microphone, and let desktop apps access the microphone"}
	}
	k.Close()
	return doctorCheck{Name: "call detection", Detail: `microphone consent store readable at HKCU\` + key, OK: true}
}

// procGetExtendedTcpTable is iphlpapi's listener table with each socket's owning pid, which golang.org/x/sys/windows does not wrap.
var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// portHolder finds the process listening on a TCP port, so a message about the port June cannot have names who has it instead of guessing. It reads the listener tables with owning pids (GetExtendedTcpTable, TCP_TABLE_OWNER_PID_LISTENER), and the program's file name from the pid. Input: the port. Output: the pid; the program's file name, "" when the process will not say, as an elevated one will not to a normal user; and false when no listener on the port was found.
func portHolder(port string) (pid uint32, name string, ok bool) {
	want, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, "", false
	}
	// A row is read as DWORDs: MIB_TCPROW_OWNER_PID is six, with the local port at 2 and the pid at 5, and MIB_TCP6ROW_OWNER_PID is fourteen, its two sixteen-byte addresses and scope ids putting them at 5 and 13. The IPv6 table is read too because a dual-stack listener on [::] holds the IPv4 port as well and is listed only there.
	for _, fam := range []struct{ af, rowLen, portAt, pidAt int }{{windows.AF_INET, 6, 2, 5}, {windows.AF_INET6, 14, 5, 13}} {
		for _, row := range listenerRows(fam.af, fam.rowLen) {
			// dwLocalPort carries the port in network byte order in its low 16 bits.
			local := row[fam.portAt] & 0xffff
			if uint64(local>>8|(local&0xff)<<8) == want {
				return row[fam.pidAt], processName(row[fam.pidAt]), true
			}
		}
	}
	return 0, "", false
}

// listenerRows reads one address family's TCP listener table with owning pids. Input: the family, AF_INET or AF_INET6, and its row length in DWORDs. Output: the rows, nil when the table could not be read. The table is a DWORD row count followed by the rows, and it can grow between the call that sizes it and the one that fills it, hence the few rounds.
func listenerRows(af, rowLen int) [][]uint32 {
	const tcpTableOwnerPidListener = 3
	buf := make([]uint32, 1)
	size := uint32(4)
	for round := 0; ; round++ {
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPidListener, 0)
		if r == 0 {
			break
		}
		if windows.Errno(r) != windows.ERROR_INSUFFICIENT_BUFFER || round == 3 {
			return nil
		}
		buf = make([]uint32, size/4+1)
	}
	var rows [][]uint32
	for i := 0; i < int(buf[0]) && 1+(i+1)*rowLen <= len(buf); i++ {
		rows = append(rows, buf[1+i*rowLen:1+(i+1)*rowLen])
	}
	return rows
}

// processName is the file name of the program a pid runs, such as "node.exe". Output: "" when the process cannot be opened or will not give its path.
func processName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	path := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(path))
	if err := windows.QueryFullProcessImageName(h, 0, &path[0], &size); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(path[:size]))
}
