package agent

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

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

// defaultBrowserID is the default https handler's executable stem ("brave", "chrome", "msedge", "firefox"), read from the ProgId Windows records for the user's choice, or "" when it cannot say. A variable so a test can name one without the registry.
var defaultBrowserID = func() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	progID, _, err := k.GetStringValue("ProgId")
	if err != nil {
		return ""
	}
	return browserFromProgID(progID)
}
