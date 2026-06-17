//go:build windows

package cmd

import (
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"os"

	"context"

	"github.com/getlantern/systray"
)

//go:embed tray_icon.ico
var trayIcon []byte

func runDaemonSupervisor(ctx context.Context, listener net.Listener) {
	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("Ora")
		systray.SetTooltip("Ora Context Runtime is active")

		mStatus := systray.AddMenuItem("🟢 Ora is tracking", "Current tracking status")
		mStatus.Disable()
		systray.AddSeparator()
		mPause := systray.AddMenuItem("Pause Tracking", "Pause workspace activity tracking")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit Ora", "Stop the background tracker and database")

		stop, _, err := startDaemonServices(ctx, listener)
		if err != nil {
			slog.Error("failed to start daemon services", "error", err)
			listener.Close()
			systray.Quit()
			return
		}

		slog.Info("Daemon running in background.")

		paused := false

		go func() {
			for {
				select {
				case <-mPause.ClickedCh:
					if paused {
						paused = false
						mStatus.SetTitle("🟢 Ora is tracking")
						mPause.SetTitle("Pause Tracking")
						mPause.SetTooltip("Pause workspace activity tracking")
						go http.Get("http://127.0.0.1:" + DaemonPort + "/resume") //nolint:errcheck
						slog.Info("tracking resumed via tray")
					} else {
						paused = true
						mStatus.SetTitle("🟡 Tracking is paused")
						mPause.SetTitle("Resume Tracking")
						mPause.SetTooltip("Resume workspace activity tracking")
						go http.Get("http://127.0.0.1:" + DaemonPort + "/pause") //nolint:errcheck
						slog.Info("tracking paused via tray")
					}
				case <-mQuit.ClickedCh:
					slog.Info("Quit requested via System Tray")
					systray.Quit()
					stop()
					return
				case <-ctx.Done():
					slog.Info("Context cancelled, shutting down systray")
					systray.Quit()
					stop()
					return
				}
			}
		}()
	}

	onExit := func() {
		slog.Info("Ora Daemon has completely shut down.")
	}

	systray.Run(onReady, onExit)
}
