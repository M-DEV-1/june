//go:build !windows && !linux

package tracker

import (
	"context"
	"errors"
	"image"
)

// extractText is a no-op on non-Windows/Linux platforms.
// macOS: not planned (no dev access -> would not ship tastefully).

func extractText() (string, error) {
	return "", nil
}

func grabScreen(_ context.Context) ([]byte, error) {
	return nil, errors.New("screenshot not supported on this platform")
}

// screenLayout has no monitor list to report on platforms without a screenshot path, so stored frames stay whole-canvas.
func screenLayout() ([]image.Rectangle, image.Point) {
	return nil, image.Pt(-1, -1)
}
