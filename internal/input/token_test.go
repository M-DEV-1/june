//go:build linux

package input

import "testing"

// No token file yet means loadToken reports an empty string, not an error.
func TestLoadTokenMissing(t *testing.T) {
	if got := loadToken(t.TempDir()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// A saved token round-trips back through loadToken.
func TestSaveAndLoadToken(t *testing.T) {
	dir := t.TempDir()
	if err := saveToken(dir, "abc123"); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	if got := loadToken(dir); got != "abc123" {
		t.Fatalf("got %q, want abc123", got)
	}
}

// Saving an empty token clears any previously stored one, since the portal returns "" when persistence was not granted.
func TestSaveEmptyTokenClears(t *testing.T) {
	dir := t.TempDir()
	if err := saveToken(dir, "abc123"); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	if err := saveToken(dir, ""); err != nil {
		t.Fatalf("saveToken clear: %v", err)
	}
	if got := loadToken(dir); got != "" {
		t.Fatalf("got %q, want empty after clear", got)
	}
}
