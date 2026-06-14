//go:build !windows && !linux

package tracker

// stubTracker is a no-op for platforms without a native implementation (e.g. macOS).
type stubTracker struct{}

func New() (Tracker, error) {
	return &stubTracker{}, nil
}

func (s *stubTracker) GetActiveWindow() (*Activity, error) {
	return Normalize("Unknown", "Unknown"), nil
}
