package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type OraConfig struct {
	Tracker TrackerConfig `json:"tracker"`
}

type TrackerConfig struct {
	Blocklist []string      `json:"blocklist"`
	DwellTime time.Duration `json:"dwell_time_ms"`
}

// DefaultBlocklist is matched via tracker.MatchesBlocklist, a case-insensitive,
// ".exe"-stripped SUBSTRING match, so entries here work across platforms
// without needing every possible app-id spelling: Windows app names carry a
// ".exe" suffix ("1Password.exe"), while Linux app identifiers from AT-SPI/
// X11/Wayland never do and often take lowercase-binary or reverse-DNS forms
// (e.g. "1password", "org.keepassxc.KeePassXC", "com.bitwarden.desktop") — all
// of which contain the lowercase entries below.
var DefaultBlocklist = []string{
	// Windows
	"1Password.exe",
	"Bitwarden.exe",
	"Taskmgr.exe",
	"LockApp.exe",
	// Linux / cross-platform password managers and secret stores
	"1password",
	"bitwarden",
	"keepassxc",
	"keepass",
	"proton pass",
	"lastpass",
	"dashlane",
	"enpass",
	"gnome-keyring",
	"seahorse",
}

// get or create
func LoadConfig() OraConfig {
	cfg := OraConfig{
		Tracker: TrackerConfig{
			Blocklist: DefaultBlocklist,
			// 3s is too less to be a dwell time, so 15s sounded better. honestly, it has to be tab switching + dwell, and im not sure what the right number is?
			DwellTime: 15000,
		},
	}

	configPath := filepath.Join("ora-db", "ora-config.json")

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		slog.Info("Creating default config file", "path", configPath)
		if err := os.MkdirAll("ora-db", 0755); err == nil {
			data, _ := json.MarshalIndent(cfg, "", "  ")
			os.WriteFile(configPath, data, 0644)
		}
		return cfg
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		slog.Error("Failed to read config file, using defaults", "error", err)
		return cfg
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("Failed to parse config file, using defaults", "error", err)
		return cfg
	}

	return cfg
}
