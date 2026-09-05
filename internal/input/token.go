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

// saveToken writes the restore_token returned by the portal so the next Open can skip the consent dialog. A blank token removes the stored file instead of writing an empty one, since the portal returns "" when persistence was not granted.
func saveToken(dataDir, token string) error {
	path := filepath.Join(dataDir, tokenFile)
	if token == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(path, []byte(token), 0600)
}
