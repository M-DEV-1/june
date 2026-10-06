//go:build !windows

package util

// PersistentEnvScope has nothing to read outside Windows, where a lasting variable lives in a shell profile or a unit file that only the shell or systemd reads. Output: always "".
func PersistentEnvScope(name string) string { return "" }
