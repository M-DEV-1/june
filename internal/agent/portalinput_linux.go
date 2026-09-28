//go:build linux

package agent

import (
	"context"

	"june/internal/input"
	"june/internal/tracker"
)

// openPortalInput opens the desktop portal's RemoteDesktop session that press_key, click_at and scroll_at drive. Input: a context bounding the portal calls, and the directory the restore token is kept in so the user is asked to allow remote control once rather than on every restart. Output: the session, or the portal's error, which is also what a declined consent dialog looks like from here.
func openPortalInput(ctx context.Context, dataDir string) (InputDevice, error) {
	// Every point June sends is in logical desktop pixels, while the stream the portal grants is sized in the monitor's own device pixels, so the mapping between them needs to know how big that monitor is logically. The tracker's read of the monitor layout is the only place that knows, and this is the one seam where both packages are in scope.
	input.UseMonitorLayout(tracker.MonitorLogicalSize)
	return input.Open(ctx, dataDir)
}
