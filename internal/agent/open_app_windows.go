package agent

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"june/internal/util"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// startAppsTTL is how long one Get-StartApps listing is reused. PowerShell takes about a second to start, and the installed applications change rarely.
const startAppsTTL = 5 * time.Minute

// startApps is the last Get-StartApps listing and when it was read.
var startApps struct {
	sync.Mutex
	at      time.Time
	entries map[string]string
}

// readDesktopEntries lists the installed applications, entry to display name, from Get-StartApps, which covers Store apps (Calculator, Settings, new Teams) and gives their localized names. The listing is kept for startAppsTTL. When PowerShell fails or lists nothing, it falls back to the .lnk shortcuts in the Start menu's Programs folders for all users and for this user, and that fallback is not kept, so the next call tries PowerShell again.
func readDesktopEntries() map[string]string {
	startApps.Lock()
	defer startApps.Unlock()
	if startApps.entries != nil && time.Since(startApps.at) < startAppsTTL {
		return startApps.entries
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8; ConvertTo-Json -Compress -InputObject @(Get-StartApps | Select-Object Name,AppID)")
	util.HideConsole(cmd)
	out, err := cmd.Output()
	if entries := startAppEntries(out); err == nil && len(entries) > 0 {
		startApps.at, startApps.entries = time.Now(), entries
		return entries
	}
	slog.Warn("Get-StartApps listed nothing, falling back to Start-menu shortcuts", "error", err)
	return shortcutEntries(
		filepath.Join(os.Getenv("ProgramData"), `Microsoft\Windows\Start Menu\Programs`),
		filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`),
	)
}

// sFalse is the HRESULT CoInitializeEx returns when the thread was already initialised, which still needs its own CoUninitialize.
const sFalse = syscall.Errno(1)

// launchEntry starts an application from its entry. A shell:AppsFolder entry from Get-StartApps is handed to explorer.exe, which starts desktop and Store apps alike the way a click on the Start tile does. A .lnk shortcut goes through ShellExecute, the same call a click on the shortcut makes, so the target, arguments and working directory stored in it are all honoured. Input: the entry. Output: the start error, if any; the application is not a child of the daemon and outlives it.
func launchEntry(entry string) error {
	if strings.HasPrefix(entry, appsFolder) {
		cmd := exec.Command("explorer.exe", entry)
		util.Detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		// explorer.exe hands the start to the shell and exits, often with status 1, so its exit says nothing about the application; it is only waited on to release the process handle.
		go cmd.Wait()
		return nil
	}
	// ShellExecute resolves a shortcut through COM, which is initialised per OS thread, so the goroutine is held on one thread for the call.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err == nil || err == sFalse {
		defer windows.CoUninitialize()
	}
	file, err := windows.UTF16PtrFromString(entry)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, file, nil, nil, windows.SW_SHOWNORMAL)
}

// patchAccessibility does nothing on Windows: Chromium-based applications there build their UI Automation tree as soon as a client asks for it, with no command-line flag. Output: false, since nothing was patched.
func patchAccessibility(string) bool { return false }

// treelessNote is empty on Windows, where a Chromium-based application started plain still exposes its tree.
func (a *Agent) treelessNote(string, string, bool) string { return "" }

// defaultBrowserID names the default https handler, or "" when it cannot say: the browser its ProgId stands for ("comet", "opera", "msedge"), then its executable's stem when that differs, space-separated, the order raiseBrowser tries them in. The shell is asked which ProgId and which program open https, the same resolution a link click goes through; the ProgId in the registry is the fallback for when it will not say. A variable so a test can name one without the shell.
// The shell is asked first because the registry key alone is not where Windows 11 keeps the choice: on this desk https\UserChoice still named Edge while UserChoiceLatest named Comet and every link opened in Comet, so open_url raised whatever Edge window was open and left the page behind it. The executable's stem is what the raiser names a window's program by (its WmClass), so the two meet without a table of browsers, except for a browser whose handler is a launcher: Opera registers ...\Opera\launcher.exe for https while its windows belong to opera.exe, which is why the ProgId's browser is tried first.
var defaultBrowserID = func() string {
	exe := assocString("https", assocStrExecutable)
	if progID := assocString("https", assocStrProgID); progID != "" || exe != "" {
		ids := []string{}
		if id := browserFromProgID(progID); id != "" {
			ids = append(ids, id)
		}
		if stem := strings.ToLower(strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))); exe != "" && !slices.Contains(ids, stem) {
			ids = append(ids, stem)
		}
		return strings.Join(ids, " ")
	}
	for _, key := range []string{`https\UserChoiceLatest\ProgId`, `https\UserChoice`} {
		k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\Shell\Associations\UrlAssociations\`+key, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		progID, _, err := k.GetStringValue("ProgId")
		k.Close()
		if err == nil && progID != "" {
			return browserFromProgID(progID)
		}
	}
	return ""
}

var procAssocQueryString = windows.NewLazySystemDLL("shlwapi.dll").NewProc("AssocQueryStringW")

// What AssocQueryStringW is asked for: ASSOCSTR_EXECUTABLE, the path of the program a verb on an association runs, and ASSOCSTR_PROGID, the ProgId the association resolves to.
const (
	assocStrExecutable = 2
	assocStrProgID     = 20
)

// assocString asks the shell about a URL scheme's association, through AssocQueryStringW for the open verb, the one a link click runs; measured on Windows 11 26200, it answers Comet's comet.exe and CometHTM.<id> where https\UserChoice still names Edge, the choice the shell itself follows. Input: the scheme, such as "https", and what to ask for. Output: the answer, or "" when the shell has none or the call fails.
func assocString(scheme string, what uintptr) string {
	assoc, err := windows.UTF16PtrFromString(scheme)
	if err != nil {
		return ""
	}
	verb, _ := windows.UTF16PtrFromString("open")
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if hr, _, _ := procAssocQueryString.Call(0, what, uintptr(unsafe.Pointer(assoc)), uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); hr != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// shellPid is the pid of the process that owns the desktop, the shell's explorer.exe. The raiser leaves the desktop out of its List, so windowPids would miss this process though it is on screen before any launch; and a shell:AppsFolder launch is handed to it, so a window it opens on the way (the File Explorer window shown for an AppID that no longer resolves) would be taken for the launched application's. Output: the pid, 0 when there is no shell.
func shellPid() uint32 {
	var pid uint32
	if h := windows.GetShellWindow(); h != 0 {
		windows.GetWindowThreadProcessId(h, &pid) //nolint:errcheck
	}
	return pid
}
