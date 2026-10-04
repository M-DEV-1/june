//go:build !windows

package proactive

// startMenuPrograms has no meaning off Windows, where notices go over the session bus and no toast is ever posted.
func startMenuPrograms() string { return "" }

// shortcutAppID has nothing to read off Windows, where there are no Start-menu shortcuts.
func shortcutAppID(string) string { return "" }
