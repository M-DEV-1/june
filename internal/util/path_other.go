//go:build !windows

package util

// RefreshPath has nothing to read outside Windows, where a lasting PATH lives in a shell profile that only a login shell reads.
func RefreshPath() {}

// withLastingPath leaves env as it is outside Windows (see RefreshPath).
func withLastingPath(env []string) []string { return env }
