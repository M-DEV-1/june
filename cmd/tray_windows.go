//go:build windows

package cmd

import (
	"context"
	_ "embed"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"june/internal/tracker"

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
	stop, tracking, err := startDaemonServices(ctx, listener)
	if err != nil {
		slog.Error("failed to start daemon services", "error", err)
		listener.Close()
		return
	}
	if !trayWanted() {
		slog.Info("Daemon running in background (no tray icon: JUNE_NO_TRAY is set).")
		<-ctx.Done()
		stop()
		return
	}

	quitCh := make(chan struct{}, 1)
	// Quit June, and Windows ending the session, are recorded as a quit the way POST /quit is, before the shutdown starts: a restart asked for during it — setup's, put off until a recording ended, which this shutdown ends — is then refused rather than bringing June back after the user quit it.
	quit := func() {
		beginQuit()
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
		systray.Run(func() { trayMenu(ctx, tracking, quit) }, func() {
			// systray.Quit below destroys the tray's window at the end of every shutdown, a restart's included, and that reaches here too; by then stopped is closed, and a quit recorded then would stop the replacement the restart started.
			select {
			case <-stopped:
				return
			default:
			}
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

// trayMenu builds the menu and handles its clicks until ctx ends. Input: the daemon's context, the tracker whose pause state the menu shows, and the function that asks the supervisor to quit. It runs on the goroutine systray starts for onReady, after the tray window exists.
func trayMenu(ctx context.Context, tracking *tracker.Daemon, quit func()) {
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
		meetingRecorder.AddStateObserver(refreshMeeting)
	}

	// The window pauses and resumes observation through the same /pause and /resume, so the labels are drawn from the tracker's own state rather than from a flag only the tray's clicks flipped, which read "Pause" over a paused tracker and made the next click pause it again.
	var pauseMu sync.Mutex
	shownStatus, shownItem := "Observing", "Pause Observation" // what the items above were created with
	refreshPause := func() {
		pauseMu.Lock()
		defer pauseMu.Unlock()
		statusLabel, itemLabel := pauseLabels(tracking.IsPaused())
		if statusLabel == shownStatus && itemLabel == shownItem {
			return
		}
		shownStatus, shownItem = statusLabel, itemLabel
		status.SetTitle(statusLabel)
		pause.SetTitle(itemLabel)
	}
	refreshPause()
	// ponytail: polled, because systray gives no "menu about to open" callback to redraw from and the tracker tells no one when it is paused; a pause observer on tracker.Daemon would make this event-driven.
	pausePoll := time.NewTicker(time.Second)
	defer pausePoll.Stop()

	for {
		select {
		case <-open.ClickedCh:
			go authedDaemonGet("http://127.0.0.1:" + DaemonPort + "/window?action=open")
		case <-pause.ClickedCh:
			paused := tracking.IsPaused()
			go func() {
				clickPauseItem(paused)
				refreshPause()
			}()
		case <-pausePoll.C:
			refreshPause()
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
