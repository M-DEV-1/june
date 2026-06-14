//go:build linux

package tracker

// captureScreen and extractText are title-only stubs for Linux.
// AT-SPI (xdg-desktop-portal ScreenCast via godbus) is the planned upgrade —
// see internal/tracker/CONTEXT.md for the full Linux roadmap.

func captureScreen() ([]byte, error) {
	return nil, nil
}

func extractText() (string, error) {
	return "", nil
}
