package tracker

// The lock gate consults the real GNOME session over D-Bus, so with the screen actually locked every daemon test on this machine would see no activity at all. Unit tests are about the daemon's logic, not the tester's screen state, so the gate is pinned open for the whole test binary.
func init() { sessionLocked = func() bool { return false } }
