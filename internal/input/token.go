//go:build linux

package input

import (
	"os"
	"path/filepath"
	"strings"
)

// tokenFile is the name of the file holding the RemoteDesktop restore_token, under the caller's data dir, so consent is asked once per machine instead of on every session.
const tokenFile = "portal-input-token"

// loadToken reads a previously saved restore_token. Returns "" if none is stored yet.
func loadToken(dataDir string) string {
	data, err := os.ReadFile(filepath.Join(dataDir, tokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveToken writes the restore_token returned by the portal so the next Open can skip the consent dialog. A blank token leaves the stored file alone: the portal answers Start with no restore_token on a session it restored from the saved one (measured on this desk on 2026-09-08, when four clicks through a restored session were followed by the file being gone and the next open would have asked for consent again), and the saved token is still the one that restores the grant.
func saveToken(dataDir, token string) error {
	if token == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dataDir, tokenFile), []byte(token), 0600)
}

// forgetToken removes the saved restore_token so the next Open asks for consent again. A blank dataDir, or no file, is nothing to do.
func forgetToken(dataDir string) {
	if dataDir != "" {
		os.Remove(filepath.Join(dataDir, tokenFile))
	}
}
