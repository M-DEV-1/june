//go:build linux

package tracker

import (
	"context"
	"strings"
	"testing"
)

// A look sends the brain a picture of the window in front, and CaptureFront filtered nothing at all: with KeePassXC or 1Password in front the picture is of the open vault. The blocklist the capture loop applies has to reach this path too, and it has to refuse before the screen is grabbed rather than after, since the fallback region when a window reports no rectangle is the whole screen.
func TestCaptureFront_RefusesABlocklistedApplication(t *testing.T) {
	restore := frontApp
	frontApp = func() (string, bool) { return "KeePassXC", true }
	SetBlocklist([]string{"keepassxc"})
	t.Cleanup(func() { frontApp = restore; SetBlocklist(nil) })

	_, err := CaptureFront(context.Background())
	if err == nil || !strings.Contains(err.Error(), "blocklist") {
		t.Errorf("CaptureFront on a blocked application = %v, want a refusal naming the blocklist", err)
	}
}
