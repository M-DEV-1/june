package tracker

import (
	"os"
	"testing"
)

// TestMain switches off the real lock probe, so a desktop that happens to be locked while the tests run does not make every tick skip. Tests that need a lock answer set sessionLocked themselves.
func TestMain(m *testing.M) {
	sessionLocked = nil
	os.Exit(m.Run())
}
