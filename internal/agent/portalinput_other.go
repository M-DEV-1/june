//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
)

// openPortalInput reports that there is no keyboard or pointer to drive: internal/input drives them only on Linux, through the desktop portal, and on Windows, through SendInput.
func openPortalInput(ctx context.Context, dataDir string) (InputDevice, error) {
	return nil, errors.New("the keyboard and pointer can only be driven on Linux and Windows")
}
