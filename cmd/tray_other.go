//go:build !windows && !linux

package cmd

import (
	"context"
	"log/slog"
	"net"
)

// runDaemonSupervisor runs the daemon headlessly on platforms other than Linux and Windows.
// No system tray is available; the process simply blocks until ctx is cancelled.
func runDaemonSupervisor(ctx context.Context, listener net.Listener) {
	stop, _, err := startDaemonServices(ctx, listener)
	if err != nil {
		slog.Error("failed to start daemon services", "error", err)
		listener.Close()
		return
	}

	slog.Info("Daemon running in background (headless, no system tray).")
	<-ctx.Done()
	stop()
}
