package cmd

import (
	"fmt"
	"log/slog"
	"strings"

	"june/internal/config"
)

// applyAutostart handles the --autostart=on|off flag: it records the choice in the config file and immediately makes the on-disk login entry match, so the user never has to hand-edit june-config.json.
// Input: "on" or "off" (case-insensitive). Output: an error for an unrecognised value, or for a config write or login-entry write that failed.
func applyAutostart(value string) error {
	var on bool
	switch strings.ToLower(value) {
	case "on":
		on = true
	case "off":
		on = false
	default:
		return fmt.Errorf("--autostart expects on or off, got %q", value)
	}

	cfg := config.LoadConfig()
	cfg.Autostart = on
	if err := config.SaveConfig(cfg); err != nil {
		return err
	}
	if err := setAutostart(on); err != nil {
		return err
	}
	fmt.Printf("start on login: %s\n", map[bool]string{true: "on", false: "off"}[on])
	return nil
}

// reconcileAutostart makes the on-disk login entry match want, which is the config's Autostart field.
// The config file is the switch: whatever it says wins over whatever happens to be installed, so a hand-edited config or a stale entry left by an older build is corrected on the next daemon start.
// Never fatal — autostart failing is not a reason to refuse to run.
// ponytail: the entry records whatever os.Executable() reports at install time and is never re-checked once installed, so a daemon started via `go run .` (a temp build directory that is deleted on exit) or a binary that is later moved leaves a login entry pointing at a path that no longer exists. Upgrade path: have reconcileAutostart compare the installed Exec= path against the current executable and rewrite when they differ.
func reconcileAutostart(want bool) {
	if autostartEnabled() == want {
		return
	}
	if err := setAutostart(want); err != nil {
		slog.Warn("failed to reconcile login autostart entry", "want", want, "error", err)
		return
	}
	slog.Info("login autostart entry updated", "enabled", want)
}
