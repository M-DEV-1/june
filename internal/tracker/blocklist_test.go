package tracker_test

import (
	"ora/internal/config"
	"ora/internal/tracker"
	"testing"
)

// Sensitive apps (password managers) must stay blocked on Linux too, but AT-SPI/X11/Wayland app names never carry Windows' ".exe" suffix — an exact-match-only matcher against the Windows-only default blocklist would silently never block them here.
func TestMatchesBlocklist_LinuxAppNames(t *testing.T) {
	cases := []struct {
		app       string
		blocklist []string
		want      bool
	}{
		// Realistic Linux app-name forms (WM_CLASS, Wayland app_id, flatpak reverse-DNS) against the default blocklist.
		{"1Password", config.DefaultBlocklist, true},
		{"1password", config.DefaultBlocklist, true},
		{"Bitwarden", config.DefaultBlocklist, true},
		{"com.bitwarden.desktop", config.DefaultBlocklist, true},
		{"org.keepassxc.KeePassXC", config.DefaultBlocklist, true},
		{"keepassxc", config.DefaultBlocklist, true},
		{"VSCode", config.DefaultBlocklist, false},
		{"firefox", config.DefaultBlocklist, false},

		// Windows exact and case-insensitive matching must keep working.
		{"1Password.exe", []string{"1Password.exe"}, true},
		{"TASKMGR.EXE", []string{"Taskmgr.exe"}, true},
	}

	for _, tc := range cases {
		if got := tracker.MatchesBlocklist(tc.app, tc.blocklist); got != tc.want {
			t.Errorf("MatchesBlocklist(%q, %v) = %v, want %v", tc.app, tc.blocklist, got, tc.want)
		}
	}
}
