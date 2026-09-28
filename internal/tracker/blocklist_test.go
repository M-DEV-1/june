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

// The default blocklist lists both the Windows and the bare form of each app ("1Password.exe" and "1password"), so substring matching alone blocks a Linux app name without ever needing the ".exe" strip. A user-supplied custom blocklist has no such redundancy: someone who adds only "Slack.exe" to their config and then runs on Linux, where the app reports itself as "slack", is relying entirely on normalizeAppIdentifier trimming the suffix off the blocklist entry. Nothing currently pins that, so removing the TrimSuffix leaves the whole suite green.
func TestMatchesBlocklist_CustomWindowsFormEntry_BlocksLinuxAppName(t *testing.T) {
	custom := []string{"Slack.exe"}

	cases := []struct {
		app  string
		want bool
	}{
		// The strip has to apply to the blocklist ENTRY, not just the app name: "slack" does not contain "slack.exe", so without trimming the entry down to "slack" this app is silently never blocked.
		{"slack", true},
		{"Slack", true},
		// The Windows form of the same app must still match once both sides normalize.
		{"Slack.exe", true},
		// A non-listed app must stay unblocked, so the test cannot pass by always returning true.
		{"firefox", false},
	}

	for _, tc := range cases {
		if got := tracker.MatchesBlocklist(tc.app, custom); got != tc.want {
			t.Errorf("MatchesBlocklist(%q, %v) = %v, want %v", tc.app, custom, got, tc.want)
		}
	}
}
