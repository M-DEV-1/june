//go:build !linux

package agent

import (
	"context"
	"errors"
)

// openPortalInput reports that there is no keyboard or pointer to drive: the portal's RemoteDesktop interface is a Linux desktop's, and internal/input builds only there.
func openPortalInput(ctx context.Context, dataDir string) (InputDevice, error) {
	return nil, errors.New("the keyboard and pointer can only be driven on Linux")
}
