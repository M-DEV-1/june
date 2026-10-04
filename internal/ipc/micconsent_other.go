//go:build !windows

package ipc

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jfreymuth/pulse"
)

// micConsent has nothing to read outside Windows: PipeWire and PulseAudio keep no per-app microphone switch a desktop program is held to, so the mic test's heard is the whole answer.
func micConsent() string { return "unknown" }

// defaultMicName is the description the audio server gives its default input, for the mic test to name the device it heard. It only asks the server and never opens a record stream. Output: the name, or "" when no audio server answers.
func defaultMicName() string {
	c, err := pulse.NewClient(pulse.ClientApplicationName("June"))
	if err != nil {
		return ""
	}
	defer c.Close()
	src, err := c.DefaultSource()
	if err != nil {
		return ""
	}
	return src.Name()
}

// micSettingsApp is one program that opens sound settings: the desktops it belongs to, as XDG_CURRENT_DESKTOP names them, none for one that works on any, and its command with fixed arguments that never come from a request.
type micSettingsApp struct {
	desktops []string
	argv     []string
}

// micSettingsApps are the sound settings programs, the desktop's own first and in this order among them. Budgie comes before GNOME because a Budgie session also says GNOME. gnome-control-center refuses to run outside a session that says GNOME, and exits at once, so it is never tried elsewhere. Plasma 5 named its settings program systemsettings5 before Plasma 6 named it systemsettings; kcmshell opens the one page on its own where neither is installed. pavucontrol is the fallback for every other desktop, on its Input Devices tab.
var micSettingsApps = []micSettingsApp{
	{[]string{"Budgie"}, []string{"budgie-control-center", "sound"}},
	{[]string{"GNOME"}, []string{"gnome-control-center", "sound"}},
	{[]string{"KDE"}, []string{"systemsettings", "kcm_pulseaudio"}},
	{[]string{"KDE"}, []string{"systemsettings5", "kcm_pulseaudio"}},
	{[]string{"KDE"}, []string{"kcmshell6", "kcm_pulseaudio"}},
	{[]string{"KDE"}, []string{"kcmshell5", "kcm_pulseaudio"}},
	{[]string{"X-Cinnamon", "Cinnamon"}, []string{"cinnamon-settings", "sound"}},
	{[]string{"MATE"}, []string{"mate-volume-control", "--page=input"}},
	{nil, []string{"pavucontrol", "--tab=4"}},
}

// settingsQuitWindow is how long a started settings program is watched before it counts as open. One that quits with an error inside it opened nothing, such as gnome-control-center started outside GNOME, and the next is tried; one that quits cleanly handed the page to a copy already running, which is open.
const settingsQuitWindow = 1500 * time.Millisecond

// openMicSettings opens the desktop's sound settings on its input devices. Output: nil once one of micSettingsApps is running or has handed over to one that is, errNoMicSettings when none for this desktop is installed, or why the last one tried failed.
func openMicSettings() error {
	if runtime.GOOS != "linux" {
		return errNoMicSettings
	}
	desktops := strings.Split(os.Getenv("XDG_CURRENT_DESKTOP"), ":")
	var lastErr error
	for _, app := range micSettingsApps {
		if app.desktops != nil && !slices.ContainsFunc(app.desktops, func(d string) bool {
			return slices.ContainsFunc(desktops, func(cur string) bool { return strings.EqualFold(cur, d) })
		}) {
			continue
		}
		path, err := exec.LookPath(app.argv[0])
		if err != nil {
			continue
		}
		if lastErr = startSettings(path, app.argv[1:]); lastErr == nil {
			return nil
		}
		slog.Warn("setup: a sound settings program did not open, trying the next", "program", app.argv[0], "error", lastErr)
	}
	if lastErr != nil {
		return lastErr
	}
	return errNoMicSettings
}

// startSettings starts one settings program and watches it for settingsQuitWindow. Input: its path and arguments. Output: nil when it is still running then or quit cleanly, otherwise the error starting it or the status it quit with.
func startSettings(path string, args []string) error {
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped on its own goroutine so the settings window, once closed, does not stay a zombie in the daemon's process table.
	quit := make(chan error, 1)
	go func() { quit <- cmd.Wait() }()
	select {
	case err := <-quit:
		if err != nil {
			return fmt.Errorf("%s quit at once: %w", path, err)
		}
		return nil
	case <-time.After(settingsQuitWindow):
		return nil
	}
}
