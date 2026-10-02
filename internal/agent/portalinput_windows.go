package agent

import (
	"context"

	"june/internal/input"
)

// openPortalInput hands back the keyboard and pointer Windows' SendInput drives. Windows asks for no consent and keeps no restore token, so the context and the data directory go unused and the open cannot fail.
func openPortalInput(ctx context.Context, dataDir string) (InputDevice, error) {
	return &input.Session{}, nil
}
