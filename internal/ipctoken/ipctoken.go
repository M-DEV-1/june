// Package ipctoken generates and reads the shared secret that authenticates ORA's local daemon<->client IPC (127.0.0.1:6942) — without it, /vector/add, /vector/delete, /pause, /resume, /status, and /buffer are reachable (and, per the same-origin-less nature of localhost, reachable from any webpage the user has open) by any local process with no proof it's actually part of ORA.
package ipctoken

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HeaderName is the HTTP header IPC clients must set to the token value.
const HeaderName = "X-Ora-Token"

// DefaultPath is where the daemon writes the token and where every IPC client (the TUI, httpVectorIndex, the tray) reads it from — relative to the process's working directory, same convention as "ora-db/db" and "ora-db/vectors".
const DefaultPath = "ora-db/ipc-token"

// tokenBytes is the random token length in bytes (32 → 64 hex chars) — comfortably beyond brute-force range for a same-machine, process-lifetime secret.
const tokenBytes = 32

// Generate creates a fresh random token and writes it to path with 0600 permissions, returning the token. Called by the daemon at startup; regenerating (rather than reusing an existing file) on every start invalidates any token a leftover/stale process might still be holding.
func Generate(path string) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate ipc token: %w", err)
	}
	token := hex.EncodeToString(buf)

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("create ipc token directory: %w", err)
		}
	}
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return "", fmt.Errorf("write ipc token: %w", err)
	}
	return token, nil
}

// Read reads the token written by Generate — used by IPC clients (the TUI's daemon-status poll, httpVectorIndex, the tray's pause/resume) to authenticate requests.
func Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read ipc token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
