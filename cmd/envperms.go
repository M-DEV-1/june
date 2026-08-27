package cmd

import (
	"log/slog"
	"os"
	"runtime"
)

// secureEnvFile restricts path (the .env file, which holds the Gemini API key) to 0600 if it exists — it commonly defaults to whatever umask created it (often 0644, world-readable on a multi-user machine). Best-effort and silent on a missing file (env vars set directly, the common case) or on Windows, where these POSIX bits don't apply.
func secureEnvFile(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.Chmod(path, 0600); err != nil {
		slog.Warn("failed to restrict .env file permissions", "error", err)
		return
	}
	slog.Info("restricted .env file permissions to 0600")
}
