package proactive

import (
	"context"
	"log/slog"
	"os"
	"os/exec"

	"june/internal/util"
)

// The environment variables a toast's title and body travel in, so the text a brain or a meeting title produced is never parsed as PowerShell.
const (
	toastTitleEnv = "JUNE_TOAST_TITLE"
	toastBodyEnv  = "JUNE_TOAST_BODY"
)

// toastScript shows one Windows toast whose two lines are read from toastTitleEnv and toastBodyEnv. It must run under Windows PowerShell 5.1 (powershell.exe), since PowerShell 7 cannot load the WinRT types. It holds no double quotes, so Go's argument quoting passes it through unchanged.
// ponytail: the toast is posted under Windows PowerShell's own AppUserModelID, because an AUMID with no Start-menu shortcut behind it shows nothing on some Windows builds; the toast is labelled "Windows PowerShell" and a click on it does nothing for June. Upgrade path: a June Start-menu shortcut carrying its own AUMID, plus protocol activation so a click opens the window.
const toastScript = `$ErrorActionPreference='Stop'; ` +
	`[Windows.UI.Notifications.ToastNotificationManager,Windows.UI.Notifications,ContentType=WindowsRuntime] | Out-Null; ` +
	`[Windows.Data.Xml.Dom.XmlDocument,Windows.Data.Xml.Dom.XmlDocument,ContentType=WindowsRuntime] | Out-Null; ` +
	`$x = New-Object Windows.Data.Xml.Dom.XmlDocument; ` +
	`$x.LoadXml('<toast><visual><binding template=''ToastGeneric''><text/><text/></binding></visual></toast>'); ` +
	`$t = $x.GetElementsByTagName('text'); ` +
	`[void]$t.Item(0).AppendChild($x.CreateTextNode($env:` + toastTitleEnv + `)); ` +
	`[void]$t.Item(1).AppendChild($x.CreateTextNode($env:` + toastBodyEnv + `)); ` +
	`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show([Windows.UI.Notifications.ToastNotification]::new($x))`

// toastCommand builds the PowerShell command that shows one toast. Input: a context bounding the run, the title and the body. Output: the command, not yet started, with the text in its environment and a fixed script as its only argument.
func toastCommand(ctx context.Context, title, body string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", toastScript)
	cmd.Env = append(os.Environ(), toastTitleEnv+"="+title, toastBodyEnv+"="+body)
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
