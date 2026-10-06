package cmd

import (
	"fmt"
	"net/http"
	"strings"

	"june/internal/config"
	"june/internal/ipctoken"
)

// applyUpdateCheck handles --update-check=on|off: it records whether June checks once a day for a new version in the config file, and tells a running June too, so its Settings show the choice at once rather than after its next start. The installer runs it when its update-check box is left unticked, because SignPath's terms ask for a way to turn off anything that contacts a server the user did not choose, and GitHub's release API is one.
// Input: "on" or "off" (case-insensitive). Output: an error for an unrecognised value, or for a config write that failed.
func applyUpdateCheck(value string) error {
	var on bool
	switch strings.ToLower(value) {
	case "on":
		on = true
	case "off":
		on = false
	default:
		return fmt.Errorf("--update-check expects on or off, got %q", value)
	}

	cfg := config.LoadConfig()
	cfg.UpdateCheck = &on
	if err := config.SaveConfig(cfg); err != nil {
		return err
	}
	// Best effort: a June that is not running reads the file when it starts, and one that refuses the request still does at its next start.
	if pingOnce() {
		tellDaemonSetting(fmt.Sprintf(`{"update_check":%t}`, on))
	}
	fmt.Printf("check for updates: %s\n", map[bool]string{true: "on", false: "off"}[on])
	return nil
}

// tellDaemonSetting sends the running daemon POST /settings with body, such as {"update_check": true}. The updater reads the file before each check, but GET /settings and GET /setup answer from the daemon's own copy of the config, which only a POST changes. Input: the JSON body. Output: none.
func tellDaemonSetting(body string) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+DaemonPort+"/settings", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	ipctoken.Attach(req, ipctoken.DefaultPath())
	if resp, err := daemonRequestClient.Do(req); err == nil {
		resp.Body.Close()
	}
}
