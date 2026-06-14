//go:build !windows && !linux

package tracker

// captureScreen and extractText are no-ops on non-Windows/Linux platforms.
// macOS: not planned (no dev access -> would not ship tastefully).

func captureScreen() ([]byte, error) {
	return nil, nil
}

func extractText() (string, error) {
	return "", nil
}
