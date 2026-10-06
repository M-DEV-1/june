package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"june/internal/config"
	"june/internal/ipc"
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
	// Best effort, as --update-check does: a running June answers GET /setup from its own copy of the config, so without this its Settings switch showed the old choice until its next start.
	if pingOnce() {
		tellDaemonSetting(fmt.Sprintf(`{"autostart":%t}`, on))
	}
	fmt.Printf("start on login: %s\n", map[bool]string{true: "on", false: "off"}[on])
	return nil
}

// reconcileAutostart makes the on-disk login entry match the config's Autostart field. Input: the daemon's live config.
// The config file is the switch: whatever it says wins over whatever happens to be installed, so a hand-edited config or a stale entry left by an older build is corrected on the next daemon start.
// The one exception is a choice the user made in the system's own list of programs that start at sign-in (see autostartSystemChoice): that is a later choice than the config's, so the config takes it. Overruling it would have June switch itself back on at its next start after the user turned it off in Task Manager.
// An installed entry that is not the one setAutostart would write now (on Windows, after the unzipped folder was moved, or once junew.exe sits beside the binary) is rewritten.
// Never fatal — autostart failing is not a reason to refuse to run.
// ponytail: on Linux autostartCurrent always says yes, so a moved binary leaves Exec= pointing at the old path; comparing the file against desktopEntry would also rewrite it to a `go run .` temp build on every dev start.
func reconcileAutostart(cfg *ipc.LiveConfig) {
	want := cfg.Get().Autostart
	if choice, ok := autostartSystemChoice(); ok && choice != want {
		if err := cfg.Update(func(c *config.JuneConfig) { c.Autostart = choice }); err != nil {
			slog.Warn("start at sign-in was changed in the system's startup list, but the config could not be updated to match", "on", choice, "error", err)
			return
		}
		slog.Info("start at sign-in follows the system's startup list, where the user changed it", "on", choice)
		return
	}
	if autostartEnabled() == want && (!want || autostartCurrent()) {
		return
	}
	if err := setAutostart(want); err != nil {
		slog.Warn("failed to reconcile login autostart entry", "want", want, "error", err)
		return
	}
	slog.Info("login autostart entry updated", "enabled", want)
}

// settingsBodyLimit is the most of a POST /settings body withAutostartSetting reads, one byte past the settings handler's own limit (maxJSONBody in internal/ipc), so that handler still sees an oversized body as oversized and refuses it.
const settingsBodyLimit = 1<<20 + 1

// autostartMu keeps two changes of the start-at-sign-in switch from interleaving, which could leave the login entry saying one thing and the config the other until the next start.
var autostartMu sync.Mutex

// withAutostartSetting has POST /settings take the start-at-sign-in switch the window sends, {"autostart": bool}, which GET /setup reads back from the config. Input: the live config and the settings handler, which does not know the field and answers the request as it always does. Output: the wrapped handler.
// The choice goes through setAutostart before the config, never into the config alone: setAutostart also clears Windows' startup switch, and a switch left off in Task Manager would otherwise be read at the next start as the user's later choice and put the config back (see reconcileAutostart).
func withAutostartSetting(cfg *ipc.LiveConfig, settings http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			settings(w, r)
			return
		}
		// Read once and handed on, so the settings handler decodes the very same body and still answers one that is malformed or too big itself.
		body, err := io.ReadAll(io.LimitReader(r.Body, settingsBodyLimit))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Autostart *bool            `json:"autostart"`
			Hotkey    *json.RawMessage `json:"hotkey"`
		}
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) && typeErr.Field == "autostart" {
				http.Error(w, "autostart must be true or false", http.StatusBadRequest)
				return
			}
			settings(w, r)
			return
		}
		if req.Autostart == nil {
			settings(w, r)
			return
		}
		// A shortcut June cannot have refuses the whole body with nothing saved, so with one in the body the settings handler answers first, into a recorder, and the login entry is only touched once it has taken the rest.
		if req.Hotkey != nil {
			rec := httptest.NewRecorder()
			settings(rec, r)
			if rec.Code < 300 && !changeAutostart(cfg, *req.Autostart, w) {
				return
			}
			maps.Copy(w.Header(), rec.Header())
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		if !changeAutostart(cfg, *req.Autostart, w) {
			return
		}
		settings(w, r)
	}
}

// changeAutostart turns start at sign-in on or off for the window: the login entry first, then the config. Input: the live config, the choice, and the response, which a failure is answered on. Output: false when it failed and the response has been written.
func changeAutostart(cfg *ipc.LiveConfig, on bool, w http.ResponseWriter) bool {
	autostartMu.Lock()
	err := setAutostart(on)
	if err == nil {
		err = cfg.Update(func(c *config.JuneConfig) { c.Autostart = on })
	}
	autostartMu.Unlock()
	if err != nil {
		slog.Warn("could not change start at sign-in from the window", "on", on, "error", err)
		http.Error(w, "June couldn't change whether it starts when you sign in: "+err.Error(), http.StatusInternalServerError)
		return false
	}
	slog.Info("start at sign-in changed from the window", "on", on)
	return true
}
