package tracker_test

import (
	"ora/internal/config"
	"ora/internal/tracker"
	"testing"
)

// Sensitive apps (password managers) must stay blocked on Linux too, but AT-SPI/X11/Wayland app names never carry Windows' ".exe" suffix — an exact-match-only matcher against the Windows-only default blocklist would silently never block them here.
func TestMatchesBlocklist_LinuxAppNames(t *testing.T) {
	cases := []struct {
		name      string
		app       string // realistic Linux-style activity.App value
		blocklist []string
		want      bool
	}{
		// realistic Linux app-name forms for sensitive apps, matched against the (fixed) DefaultBlocklist.
		{
			name:      "1Password native Linux binary (X11 WM_CLASS / app_id)",
			app:       "1Password",
			blocklist: config.DefaultBlocklist,
			want:      true,
		},
		{
			name:      "1Password lowercase app_id (Sway/Hyprland style)",
			app:       "1password",
			blocklist: config.DefaultBlocklist,
			want:      true,
		},
		{
			name:      "Bitwarden desktop app",
			app:       "Bitwarden",
			blocklist: config.DefaultBlocklist,
			want:      true,
		},
		{
			name:      "Bitwarden flatpak reverse-DNS app id",
			app:       "com.bitwarden.desktop",
			blocklist: config.DefaultBlocklist,
			want:      true,
		},
		{
			name:      "KeePassXC reverse-DNS AT-SPI/X11 class",
			app:       "org.keepassxc.KeePassXC",
			blocklist: config.DefaultBlocklist,
			want:      true,
		},
		{
			name:      "KeePassXC plain app name",
			app:       "keepassxc",
			blocklist: config.DefaultBlocklist,
			want:      true,
		},

		// existing Windows behavior must keep working unchanged.
		{
			name:      "Windows exact match still blocks",
			app:       "1Password.exe",
			blocklist: []string{"1Password.exe"},
			want:      true,
		},
		{
			name:      "Windows case-insensitive match still blocks",
			app:       "TASKMGR.EXE",
			blocklist: []string{"Taskmgr.exe"},
			want:      true,
		},

		// unrelated apps must never be blocked.
		{
			name:      "unrelated app is not blocked",
			app:       "VSCode",
			blocklist: config.DefaultBlocklist,
			want:      false,
		},
		{
			name:      "unrelated app is not blocked (Linux browser)",
			app:       "firefox",
			blocklist: config.DefaultBlocklist,
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tracker.MatchesBlocklist(tc.app, tc.blocklist)
			if got != tc.want {
				t.Errorf("MatchesBlocklist(%q, %v) = %v, want %v", tc.app, tc.blocklist, got, tc.want)
			}
		})
	}
}
