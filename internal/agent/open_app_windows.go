package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// readDesktopEntries lists the installed applications, shortcut path to display name, from the Start menu's Programs folders for all users and for this user.
func readDesktopEntries() map[string]string {
	return shortcutEntries(
		filepath.Join(os.Getenv("ProgramData"), `Microsoft\Windows\Start Menu\Programs`),
		filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`),
	)
}

// sFalse is the HRESULT CoInitializeEx returns when the thread was already initialised, which still needs its own CoUninitialize.
const sFalse = syscall.Errno(1)

// launchEntry starts an application from its Start-menu shortcut through ShellExecute, the same call a click on the shortcut makes, so the target, arguments and working directory stored in the .lnk are all honoured. Input: the shortcut's path. Output: the ShellExecute error, if any; the application is not a child of the daemon and outlives it.
func launchEntry(entry string) error {
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
