//go:build linux

package input

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLivePortalShiftTap opens a real RemoteDesktop session against the user's own session bus and taps Shift (press then release), which is harmless on its own. It pops the portal's consent dialog the first time it runs on a machine with no saved restore_token.
//
// Guarded behind JUNE_PORTAL_INPUT=1 and skipped otherwise: this is the one test in the package that touches the live desktop and must only run in an announced, coordinator-driven slot, never as part of a normal `go test ./...`.
func TestLivePortalShiftTap(t *testing.T) {
	if os.Getenv("JUNE_PORTAL_INPUT") != "1" {
		t.Skip("set JUNE_PORTAL_INPUT=1 to run the live portal test (opens a real session, may show a consent dialog)")
	}

	dataDir := t.TempDir()
	hadToken := loadToken(dataDir) != ""

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	if err := sess.PressKey("Shift"); err != nil {
		t.Fatalf("PressKey(Shift): %v", err)
	}

	t.Logf("live portal session opened; had saved token before this run: %v; monitors covered: %d", hadToken, sess.Monitors())
}
