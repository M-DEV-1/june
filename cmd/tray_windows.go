//go:build windows

package cmd

import (
	"context"
	_ "embed"
	"log/slog"
	"net"
	"runtime"
	"time"

	"github.com/getlantern/systray"
)

// trayIcon is the June logo as a multi-size .ico, generated from cmd/tray/*.png; the Windows tray loads icons through LoadImage, which takes .ico and not PNG.
//
//go:embed tray_icon.ico
var trayIcon []byte

// trayQuitWait is how long shutdown waits for the tray to remove its icon before the process exits; an icon left behind stays in the notification area until the pointer passes over it.
const trayQuitWait = time.Second

// runDaemonSupervisor on Windows starts the daemon's services, then shows a notification-area icon with the same menu as Linux: a status line, Open June, Pause/Resume Observation, Start/Stop meeting recording, and Quit June.
// The services start before the tray so a tray that cannot be created leaves the daemon running headless: systray only logs a failed setup and never calls onReady.
// It returns when ctx is cancelled, when Quit June is clicked, or when Windows ends the session.
func runDaemonSupervisor(ctx context.Context, listener net.Listener) {
	stop, _, err := startDaemonServices(ctx, listener)
	if err != nil {
		slog.Error("failed to start daemon services", "error", err)
		listener.Close()
		return
	}

	quitCh := make(chan struct{}, 1)
	quit := func() {
		select {
		case quitCh <- struct{}{}:
		default:
		}
	}
	stopped := make(chan struct{})
	trayDone := make(chan struct{})
	go func() {
		// The tray's window and its message loop must live on one OS thread, so this goroutine keeps its thread for as long as the loop runs. Any thread will do on Windows, which leaves the main goroutine free to wait on ctx like the Linux supervisor does.
		runtime.LockOSThread()
		defer close(trayDone)
		// onExit runs on WM_DESTROY and again on WM_ENDSESSION. A GUI-subsystem daemon receives no logoff signal, so the end of the session reaches it only here: it asks for a quit and holds the message loop until the services have stopped, which Windows allows for a few seconds.
		// ponytail: systray does not pass on WM_ENDSESSION's wParam, so a logoff another program cancels also quits June; handle WM_ENDSESSION in a window of June's own if that bites.
		systray.Run(func() { trayMenu(ctx, quit) }, func() {
			quit()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
			}
		})
	}()

	slog.Info("Daemon running in background.")
	select {
	case <-ctx.Done():
		slog.Info("Context cancelled, shutting down systray")
	case <-quitCh:
		slog.Info("Quit requested via System Tray")
	}
	stop()
	close(stopped)
	systray.Quit()
	// A tray that never came up never returns from Run, so the wait is bounded.
	select {
	case <-trayDone:
	case <-time.After(trayQuitWait):
	}
}

// trayMenu builds the menu and handles its clicks until ctx ends. Input: the daemon's context and the function that asks the supervisor to quit. It runs on the goroutine systray starts for onReady, after the tray window exists.
func trayMenu(ctx context.Context, quit func()) {
	systray.SetIcon(trayIcon)
	systray.SetTooltip("June")

	// ponytail: the status line is text only; Linux draws a coloured dot, which here needs a rendered .ico per state, add one if the label alone reads poorly.
	status := systray.AddMenuItem("Observing", "")
	status.Disable()
	systray.AddSeparator()
	open := systray.AddMenuItem("Open June", "")
	pause := systray.AddMenuItem("Pause Observation", "")
	meeting := systray.AddMenuItem(meetingLabel(meetingRecorder != nil && meetingRecorder.Active()), "")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit June", "")

	// A recording started or stopped by anything else, such as the microphone watcher, changes the label too.
	refreshMeeting := func() {
		meeting.SetTitle(meetingLabel(meetingRecorder != nil && meetingRecorder.Active()))
	}
	if meetingRecorder != nil {
		meetingRecorder.SetOnStateChange(refreshMeeting)
	}

	paused := false
	for {
		select {
		case <-open.ClickedCh:
			go authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
		case <-pause.ClickedCh:
			endpoint := "/pause"
			if paused {
				endpoint = "/resume"
			}
			paused = !paused
			go authedDaemonGet("http://127.0.0.1:" + DaemonPort + endpoint)
			if paused {
				status.SetTitle("Paused")
				pause.SetTitle("Resume Observation")
			} else {
				status.SetTitle("Observing")
				pause.SetTitle("Pause Observation")
			}
		case <-meeting.ClickedCh:
			toggleMeeting(ctx, meetingRecorder)
			refreshMeeting()
		case <-quitItem.ClickedCh:
			quit()
			return
		case <-ctx.Done():
			return
		}
	}
}
