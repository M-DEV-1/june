//go:build !windows && !linux

package tracker

import (
	"context"
	"errors"
)

// captureScreen and extractText are no-ops on non-Windows/Linux platforms.
// macOS: not planned (no dev access -> would not ship tastefully).

func captureScreen() ([]byte, error) {
	return nil, nil
}

func extractText() (string, error) {
	return "", nil
}

func grabScreen(_ context.Context) ([]byte, error) {
	return nil, errors.New("screenshot not supported on this platform")
}
