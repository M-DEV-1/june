//go:build !linux

package cmd

// installDesktopEntry is a no-op on platforms other than Linux: there is no dock or icon theme to name a desktop entry for.
func installDesktopEntry() error { return nil }
