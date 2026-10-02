package tracker_test

import (
	"june/internal/config"
	"june/internal/tracker"
	"testing"
)

// Sensitive apps (password managers) must stay blocked on Linux too, but AT-SPI/X11/Wayland app names never carry Windows' ".exe" suffix — an exact-match-only matcher against the Windows-only default blocklist would silently never block them here.
func TestMatchesBlocklist_LinuxAppNames(t *testing.T) {
	cases := []struct {
		app       string
		blocklist []string
		want      bool
	}{
		// A Linux app name against the default blocklist, which lists both the Windows and the bare form.
		{"org.keepassxc.KeePassXC", config.DefaultBlocklist, true},
		{"firefox", config.DefaultBlocklist, false},
		// A Windows name matches whatever its case.
		{"TASKMGR.EXE", []string{"Taskmgr.exe"}, true},
		// A custom entry in the Windows form still blocks the Linux name, which only works if ".exe" is trimmed off the entry.
		{"slack", []string{"Slack.exe"}, true},
	}

	for _, tc := range cases {
		if got := tracker.MatchesBlocklist(tc.app, tc.blocklist); got != tc.want {
			t.Errorf("MatchesBlocklist(%q, %v) = %v, want %v", tc.app, tc.blocklist, got, tc.want)
		}
	}
}
