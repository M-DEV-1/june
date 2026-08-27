//go:build !windows && !linux

package tracker

import (
	"context"
	"errors"
)

// extractText is a no-op on non-Windows/Linux platforms.
// macOS: not planned (no dev access -> would not ship tastefully).

func extractText() (string, error) {
	return "", nil
}

func grabScreen(_ context.Context) ([]byte, error) {
	return nil, errors.New("screenshot not supported on this platform")
}
