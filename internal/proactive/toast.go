package proactive

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"june/internal/util"
)

// The environment variables a toast's title, body and sender travel in, so the text a brain or a meeting title produced is never parsed as PowerShell.
const (
	toastTitleEnv = "JUNE_TOAST_TITLE"
	toastBodyEnv  = "JUNE_TOAST_BODY"
	toastAppEnv   = "JUNE_TOAST_APP"
)

// The AppUserModelIDs a toast can be posted under. Windows shows a toast only for an AUMID that a Start-menu shortcut carries, and on some builds drops the rest without an error. The installer's "June" shortcut carries juneAppID (packaging/windows/june.iss); Windows PowerShell's own shortcut carries powerShellAppID on every Windows, so a portable or development copy, which has no shortcut of its own, posts under that.
const (
	juneAppID       = "M-DEV-1.June"
	juneShortcut    = "June.lnk"
	powerShellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`
)

// toastAppID picks the AUMID to post under. Input: the user's Start-menu Programs folder, or "" when it is unknown. Output: juneAppID when the June shortcut there carries it, else powerShellAppID. The ID in the shortcut is read rather than its name trusted: a June.lnk the installer did not make, another program's or one the user made to a portable copy, carries another ID or none, and every toast posted under juneAppID would then be dropped without a word. It is looked up for every toast, so installing or uninstalling June while the daemon runs takes effect at the next one.
func toastAppID(programs string) string {
	if programs != "" && shortcutAppID(filepath.Join(programs, juneShortcut)) == juneAppID {
		return juneAppID
	}
	return powerShellAppID
}

// toastScript shows one Windows toast whose two lines are read from toastTitleEnv and toastBodyEnv, posted under the AUMID in toastAppEnv. It must run under Windows PowerShell 5.1 (powershell.exe), since PowerShell 7 cannot load the WinRT types. It holds no double quotes, so Go's argument quoting passes it through unchanged.
// ponytail: a click on the toast does nothing for June, and a copy the installer did not put there posts as "Windows PowerShell". Upgrade path: a toast activator registered for juneAppID (a COM server, or protocol activation) so a click opens the window.
const toastScript = `$ErrorActionPreference='Stop'; ` +
	`[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime] | Out-Null; ` +
	`[Windows.Data.Xml.Dom.XmlDocument,Windows.Data.Xml.Dom.XmlDocument,ContentType=WindowsRuntime] | Out-Null; ` +
	`$x = New-Object Windows.Data.Xml.Dom.XmlDocument; ` +
	`$x.LoadXml('<toast><visual><binding template=''ToastGeneric''><text/><text/></binding></visual></toast>'); ` +
	`$t = $x.GetElementsByTagName('text'); ` +
	`[void]$t.Item(0).AppendChild($x.CreateTextNode($env:` + toastTitleEnv + `)); ` +
	`[void]$t.Item(1).AppendChild($x.CreateTextNode($env:` + toastBodyEnv + `)); ` +
	`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($env:` + toastAppEnv + `).Show([Windows.UI.Notifications.ToastNotification]::new($x))`

// toastCommand builds the PowerShell command that shows one toast. Input: a context bounding the run, the title and the body. Output: the command, not yet started, with the text and the AUMID to post under in its environment and a fixed script as its only argument.
func toastCommand(ctx context.Context, title, body string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", toastScript)
	cmd.Env = append(os.Environ(), toastTitleEnv+"="+title, toastBodyEnv+"="+body, toastAppEnv+"="+toastAppID(startMenuPrograms()))
	util.HideConsole(cmd)
	return cmd
}

// toast shows a Windows toast notification and waits up to notifySendTimeout for PowerShell to post it. Input: the title and body. Output: nothing; a failure is logged, since a missing notification must never take down the work that produced it.
func toast(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), notifySendTimeout)
	defer cancel()
	if out, err := toastCommand(ctx, title, body).CombinedOutput(); err != nil {
		slog.Debug("toast failed", "title", title, "error", err, "output", string(out))
	}
}

// toastNotifier is the Windows notifier: a text-only toast. A PowerShell toast cannot report which button was pressed back to the process that posted it, so it offers no buttons and never calls chose; Done and the snoozes are answered from the June window's own card, which is the surface every notice prefers whenever a window is listening.
type toastNotifier struct{}

// Notify posts the notice as a text-only toast. Output: always nil, since toast logs its own failure.
func (toastNotifier) Notify(_, title, body string, _ []Action, _ func(string)) error {
	toast(title, body)
	return nil
}

// Close is a no-op: the toast was posted by a PowerShell process that has already exited, and nothing here holds a handle to it.
func (toastNotifier) Close(string) error { return nil }
