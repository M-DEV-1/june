//go:build linux

package agent

import (
	"context"

	"ora/internal/input"
)

// openPortalInput opens the desktop portal's RemoteDesktop session that press_key, click_at and scroll_at drive. Input: a context bounding the portal calls, and the directory the restore token is kept in so the user is asked to allow remote control once rather than on every restart. Output: the session, or the portal's error, which is also what a declined consent dialog looks like from here.
func openPortalInput(ctx context.Context, dataDir string) (InputDevice, error) {
	return input.Open(ctx, dataDir)
}
