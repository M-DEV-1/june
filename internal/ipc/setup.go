// setup.go holds first-run setup: what the window's onboarding asks the daemon before it lets the user in, saving the Gemini key, testing the microphone, and marking setup done. Every answer is read live (the environment, the env file, the config, the registry) so an onboarding cut short by a restart resumes at the first step that is still not done.
package ipc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/audio"
	"june/internal/config"
	"june/internal/lifecycle"
	"june/internal/util"
)

// SetupDeps is what the first-run setup routes need from the daemon. Config is the live config every route shares; DataDir is where the env file and the rest of June's state live; ResumeTracking starts the tracker that stays paused until setup is done.
type SetupDeps struct {
	Config         *LiveConfig
	DataDir        string
	Version        string
	ResumeTracking func()
	// Server is whose /events stream the mic test's level readings go out on, so the window's meter moves while it listens. nil runs the test without them.
	Server *Server
	// RestartBlocker names what a restart right now would cut short: "recording" while a meeting is being recorded, "processing" while one is still being transcribed and written up, "downloading" while local features are downloading or installing, and "" when nothing would. Saving or removing the key restarts June, so both refuse while it names anything; the restart POST /setup/complete owes waits until it names nothing. nil is a daemon with nothing to protect.
	// cmd's blocker also names what the person is in the middle of: "voice" while a conversation is open, "dictating", "acting" while a screen job is running, paused or waiting on an answer, and "asking" while an answer is being written. The key routes refuse for those the same way, and the restart setup owes waits for them and then for a quiet spell (restartQuiet).
	RestartBlocker func() string
	// Hotkeys is the daemon's shortcut record, which setup's last screen reads the shortcut from. nil reports none.
	Hotkeys *Hotkeys
}

// SetupView is GET /setup. Hotkey, HotkeyStatus and HotkeyNote are GET /settings' own, so setup's last screen can say which keys open June, or that the user has to choose them.
type SetupView struct {
	Done           bool   `json:"done"`
	Version        string `json:"version"`
	GeminiKey      bool   `json:"gemini_key"`
	BrainReady     bool   `json:"brain_ready"`
	DefaultBrain   string `json:"default_brain"`
	Autostart      bool   `json:"autostart"`
	Hotkey         string `json:"hotkey"`
	HotkeyStatus   string `json:"hotkey_status"`
	HotkeyNote     string `json:"hotkey_note"`
	RestartPending bool   `json:"restart_pending"`
	DataDir        string `json:"data_dir"`
}

// MicTestResult is POST /setup/mic-test. Peak is on the same 0-1 scale as the level events the meter draws; Heard is whether the loudest moment reached what dictation counts as speech; Consent is "allowed", "blocked" or "unknown"; Error says why the microphone could not be opened, "" when it was.
type MicTestResult struct {
	Device  string  `json:"device"`
	Heard   bool    `json:"heard"`
	Peak    float64 `json:"peak"`
	Consent string  `json:"consent"`
	Error   string  `json:"error"`
}

// geminiKeyVar is the variable every part of June reads the Gemini key from.
const geminiKeyVar = "GEMINI_API_KEY"

// geminiModelsURL is the cheapest call that proves a key works: listing one model spends no quota and is refused for a bad key.
const geminiModelsURL = "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1"

// geminiKeyCheckTimeout bounds that call. Past it the window offers to save the key unchecked rather than leave the user staring at "Checking…".
const geminiKeyCheckTimeout = 10 * time.Second

// micTestLength is how long the mic test listens: long enough to say a few words.
const micTestLength = 3 * time.Second

// micTestID is the id the mic test's level events carry, so the window tells them from a voice session's.
const micTestID = "mic-test"

// restartRecheck is how often a restart POST /setup/complete put off asks again whether anything is still in its way.
const restartRecheck = 5 * time.Second

// restartQuiet is how long nothing the person started (an ask, a conversation, a dictation, a screen job) must have begun or ended before the restart setup owes is made on its own. Someone who has just had an answer is likely to ask the next thing, and a restart then reads as June falling over.
const restartQuiet = 2 * time.Minute

// settingsResetWait is how long the daemon waits for a window to connect before it says that its settings file was set aside. Past it the notice goes out anyway, as a desktop notification.
const settingsResetWait = 2 * time.Minute

// errNoMicSettings is what openMicSettings returns where there is no settings program to open, which the route answers 501.
var errNoMicSettings = errors.New("no sound settings program found")

// setupRoutes is the state the setup handlers share.
type setupRoutes struct {
	d SetupDeps
	// startKey is the Gemini key this process started with, the one every part of it was handed. A save os.Setenv's the new key for GET /setup's sake, so whether a save or a removal changes anything is judged against this rather than the environment: a save whose restart never came would otherwise count as unchanged when tried again, and June would go on running on the old key.
	startKey string
	// keyMu holds a save or a removal of the key from reading the env file to writing it back, so two at once cannot each write the file the other has just changed.
	keyMu sync.Mutex
	// micMu keeps to one mic test at a time.
	micMu sync.Mutex
	// waiting is set while a put-off restart waits for RestartBlocker to clear, so finishing setup twice does not start a second wait.
	waiting atomic.Bool
}

// SetupRoutes answers first-run setup: GET /setup, POST and DELETE /setup/gemini-key, POST /setup/mic-test, POST /setup/mic-settings and POST /setup/complete. It is called once as the daemon starts, after the env files are read, so the key in the environment then is the one the daemon runs on. Input: the daemon's dependencies. Output: the handlers keyed by their ServeMux pattern, for cmd to register behind the IPC token.
func SetupRoutes(d SetupDeps) map[string]http.HandlerFunc {
	s := &setupRoutes{d: d, startKey: os.Getenv(geminiKeyVar)}
	// A settings file that will not parse is replaced once, as the daemon starts, with the config it runs on, and kept beside it (see config.ReplaceUnreadable). Before the notice below, so the user is told at this start.
	if d.Config != nil {
		config.ReplaceUnreadable(d.Config.Get())
	}
	if aside := config.TakeSetAside(); aside != "" && d.Server != nil {
		go tellSettingsReset(d.Server, aside)
	}
	return map[string]http.HandlerFunc{
		"GET /setup":               s.get,
		"POST /setup/gemini-key":   s.saveKey,
		"DELETE /setup/gemini-key": s.deleteKey,
		"POST /setup/mic-test":     s.micTest,
		"POST /setup/mic-settings": s.micSettings,
		"POST /setup/complete":     s.complete,
	}
}

// get handles GET /setup. brain_ready and default_brain are the brain GET /brains marks default and whether it can answer, read without the provider checks /brains makes, because the window polls this on every start and a provider's refusal is what GET /brains?refresh=1 is for.
func (s *setupRoutes) get(w http.ResponseWriter, r *http.Request) {
	freshPath()
	cfg := s.d.Config.Get()
	home, _ := os.UserHomeDir()
	hotkey, hotkeyStatus, hotkeyNote := s.d.Hotkeys.View()
	view := SetupView{
		Done:           cfg.SetupDone,
		Version:        s.d.Version,
		GeminiKey:      os.Getenv(geminiKeyVar) != "",
		Autostart:      cfg.Autostart,
		Hotkey:         hotkey,
		HotkeyStatus:   hotkeyStatus,
		HotkeyNote:     hotkeyNote,
		RestartPending: lifecycle.RestartPending(),
		DataDir:        s.d.DataDir,
	}
	for _, b := range brainList(r.Context(), cfg, home, onPath, nil) {
		if b.Default {
			view.DefaultBrain, view.BrainReady = b.ID, b.SignedIn
		}
	}
	util.WriteJSON(w, view)
}

// saveKey handles POST /setup/gemini-key {"key": string, "force": bool}. The key is checked with Google, written to <data>/env, and the daemon restarts, because the key is read once at start and handed to about ten constructors. The replacement is started with the environment this daemon was started with and reads the key from the file afresh; os.Setenv here only serves this process's own reads, GET /setup above all, until it has shut down.
// Output: 200 {"ok":true,"restarting":bool}, restarting false when the key was already the one in use; 422 invalid_key when it is not a key or Google does not know it; 422 key_refused when Google knows it but will not serve it to June, with Google's reason and the fix in the message; 502 unreachable when Google could not be asked, which force skips; 409 env_var_set when a copy of the key June reads before its own file is set, so saving would change nothing; 409 with RestartBlocker's word ("recording", "processing", "downloading", "voice", "dictating", "acting", "asking") when the restart would cut that short, with nothing written; 500 when the env file cannot be written. An env file that will not parse is set aside and a new one started, rather than refused.
func (s *setupRoutes) saveKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Force bool   `json:"force"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	key := cleanKey(req.Key)
	if !plausibleKey.MatchString(key) {
		setupError(w, http.StatusUnprocessableEntity, "invalid_key", "That isn't a Gemini key. Copy the whole key from Google AI Studio.")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	path := s.envPath()
	if msg := keyElsewhere(true); msg != "" {
		setupError(w, http.StatusConflict, "env_var_set", msg)
		return
	}
	vars, err := util.ReadEnvFile(path)
	// godotenv loads nothing at all from a file it cannot parse, so nothing in this one reaches June today, and a key written beside the line it chokes on would never be read either. It is set aside before the key is written (keeping whatever else was in it for whoever wrote it), rather than the person being sent to mend a file they never opened.
	unreadable := err != nil
	if unreadable {
		slog.Warn("setup: June's env file cannot be read; it is set aside when the key is saved", "file", path, "error", err)
		vars = map[string]string{}
	}
	unchanged := vars[geminiKeyVar] == key && s.startKey == key
	if !unchanged && !s.restartAllowed(w, "Saving the key") {
		return
	}
	if !req.Force {
		if status, code, msg := checkGeminiKey(r.Context(), key); status != http.StatusOK {
			slog.Info("setup: the Gemini key was not saved", "reason", code, "detail", msg)
			setupError(w, status, code, msg)
			return
		}
	}
	if unchanged {
		util.WriteJSON(w, map[string]bool{"ok": true, "restarting": false})
		return
	}
	// Asked again because Google can take ten seconds to answer, time enough for a meeting to start recording or a download to begin.
	if !s.restartAllowed(w, "Saving the key") {
		return
	}
	if unreadable {
		aside := path + ".unreadable-" + time.Now().Format("20060102-150405")
		if err := os.Rename(path, aside); err != nil {
			slog.Error("setup: could not set the unreadable env file aside", "file", path, "error", err)
			setupError(w, http.StatusInternalServerError, "save_failed", keyNotSaved)
			return
		}
		slog.Warn("setup: set the unreadable env file aside", "file", path, "kept_as", aside)
	}
	if err := util.UpsertEnvFile(path, geminiKeyVar, key); err != nil {
		slog.Error("setup: could not save the Gemini key", "file", path, "error", err)
		setupError(w, http.StatusInternalServerError, "save_failed", keyNotSaved)
		return
	}
	os.Setenv(geminiKeyVar, key)
	slog.Info("setup: saved the Gemini key, restarting so every part of June uses it", "file", path, "checked", !req.Force)
	answerRestarting(w)
}

// deleteKey handles DELETE /setup/gemini-key: the key leaves <data>/env and this process's environment, and the daemon restarts without it. Output: 200 {"ok":true,"restarting":bool}, restarting false when there was no key to remove; 409 env_var_set when the key comes from somewhere June reads before its own file, which removing it from the file would not touch; 409 with RestartBlocker's word when the restart would cut that short, with nothing changed.
func (s *setupRoutes) deleteKey(w http.ResponseWriter, r *http.Request) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	path := s.envPath()
	if msg := keyElsewhere(false); msg != "" {
		setupError(w, http.StatusConflict, "env_var_set", msg)
		return
	}
	vars, _ := util.ReadEnvFile(path)
	had := vars[geminiKeyVar] != "" || s.startKey != ""
	if had && !s.restartAllowed(w, "Removing the key") {
		return
	}
	if err := util.UpsertEnvFile(path, geminiKeyVar, ""); err != nil {
		slog.Error("setup: could not remove the Gemini key", "file", path, "error", err)
		setupError(w, http.StatusInternalServerError, "save_failed", "June couldn't remove the key. Try again in a moment.")
		return
	}
	os.Unsetenv(geminiKeyVar)
	if !had {
		util.WriteJSON(w, map[string]bool{"ok": true, "restarting": false})
		return
	}
	slog.Info("setup: removed the Gemini key, restarting without it", "file", path)
	answerRestarting(w)
}

// keyNotSaved is the 500 a key save answers when the file could not be written. Why is in the log; the person can only try again.
const keyNotSaved = "June couldn't save the key. Try again in a moment."

// restartBlocker is what RestartBlocker names, "" when there is none to ask.
func (s *setupRoutes) restartBlocker() string {
	if s.d.RestartBlocker == nil {
		return ""
	}
	return s.d.RestartBlocker()
}

// restartAllowed answers 409 when a restart now would cut something short, with RestartBlocker's word as the error and a sentence beginning with doing, such as "Saving the key". It is the refusal POST /restart gives during a recording, since a key change is a restart too. Output: true when nothing is in the way and the caller may go on.
func (s *setupRoutes) restartAllowed(w http.ResponseWriter, doing string) bool {
	code := s.restartBlocker()
	if code == "" {
		return true
	}
	var msg string
	switch code {
	case "recording":
		msg = doing + " restarts June, which would end the meeting recording, and the rest of the call would not be recorded. Try again once the meeting is over."
	case "processing":
		msg = doing + " restarts June, which would stop the write-up of the meeting it just recorded. Try again in a few minutes, once the notes are ready."
	case "downloading":
		msg = doing + " restarts June, which would stop the local features downloading now. Try again once they finish, or cancel the download first."
	case "voice":
		msg = doing + " restarts June, which would end the conversation you are having with it. Try again once it is over."
	case "dictating":
		msg = doing + " restarts June, which would lose what you are dictating. Try again in a moment."
	case "acting":
		msg = doing + " restarts June, which would stop the task it is doing on screen. Try again once it finishes, or stop it first."
	case "asking":
		msg = doing + " restarts June, which would stop the answer it is working on. Try again once it arrives."
	default:
		msg = doing + " restarts June, which would cut short something it is in the middle of. Try again in a few minutes."
	}
	slog.Info("setup: refused a key change that would restart June mid-task", "busy", code)
	setupError(w, http.StatusConflict, code, msg)
	return false
}

// envPath is the env file June reads its keys from (see cmd/root.go's loadEnvFiles).
func (s *setupRoutes) envPath() string {
	return filepath.Join(s.d.DataDir, "env")
}

// keyElsewhere says whether the next start of June will get GEMINI_API_KEY from somewhere read before <data>/env, so a key setup saves or removes there would change nothing. Two places are: the environment this daemon was started with, which a restart hands its replacement unchanged, and the .env file in the working directory, which the replacement inherits too. The .env file is read afresh on each call, since the user may have just cleared it.
// Input: replacing, true for a save and false for a removal. A save loses only to a copy spelled exactly GEMINI_API_KEY, since that is the spelling godotenv tests before it lets the file's line in; on Windows a "Gemini_Api_Key" is overwritten by it. A removal leaves the file with no line at all, so a copy under any spelling the environment matches survives it.
// Output: "" when the env file decides, otherwise the sentence the 409 carries, naming the place.
func keyElsewhere(replacing bool) string {
	setIn := func(vars map[string]string) bool {
		if replacing {
			_, ok := vars[geminiKeyVar]
			return ok
		}
		_, ok := util.LookupEnvVar(vars, geminiKeyVar)
		return ok
	}
	if setIn(util.StartupEnvVars()) {
		return envVarMessage()
	}
	if cwdEnv, err := filepath.Abs(".env"); err == nil {
		vars, _ := util.ReadEnvFile(cwdEnv)
		if setIn(vars) {
			return "GEMINI_API_KEY is set in " + cwdEnv + ", which June reads before its own settings, so that copy wins. Change or remove it there, then restart June."
		}
	}
	return ""
}

// envVarMessage is the 409's sentence for a key in the environment June was started with. On Windows it says whether that is the user's or the system's lasting variables, or neither, in which case whatever started June passed it on, and starting June afresh from the Start menu is the fix.
func envVarMessage() string {
	const wins = ", and that copy wins over the key June saves."
	switch util.PersistentEnvScope(geminiKeyVar) {
	case "user":
		return "GEMINI_API_KEY is set in your Windows environment variables" + wins + " Change or remove it there (search Start for \"environment variables\"), then quit June from the tray and open it again."
	case "system":
		return "GEMINI_API_KEY is set in this computer's system environment variables" + wins + " Change or remove it there (search Start for \"environment variables\"), then quit June from the tray and open it again."
	}
	if runtime.GOOS == "windows" {
		return "June was started with GEMINI_API_KEY already set by the program that opened it, such as a terminal" + wins + " Quit June from the tray and open it again from the Start menu, then try again."
	}
	return "GEMINI_API_KEY is set in the environment June was started with, such as your shell profile or june.service" + wins + " Change or remove it there, then restart June."
}

// plausibleKey is what a pasted key may look like before it is worth asking Google. Anything else, a line break above all, is a paste gone wrong, and is never written to the env file.
var plausibleKey = regexp.MustCompile(`^[A-Za-z0-9._\-]{20,256}$`)

// cleanKey undoes what a paste commonly brings with it: surrounding space, quotes, and the "GEMINI_API_KEY=" of a line copied from an env file or a shell.
func cleanKey(raw string) string {
	key := strings.TrimSpace(raw)
	key = strings.TrimPrefix(key, "export ")
	key = strings.TrimPrefix(key, geminiKeyVar+"=")
	return strings.Trim(strings.TrimSpace(key), `"'`)
}

// checkGeminiKey asks Google whether key works. Input: the request's context, so a window that goes away stops the check, and the key, sent in the x-goog-api-key header and never in the URL. Output: 200 when it works, or the status, error code and message to answer with: 422 invalid_key when Google says it is no key of its own or an expired one, which copying it again or making a new one fixes; 422 key_refused, with Google's reason, when Google knows the key but will not serve it here (the API is off in its project, its restrictions shut June out, the country is not served), which no fresh copy fixes; 502 unreachable when Google could not be asked or did not answer.
// A 429 counts as working: Google only meters a key it recognises.
func checkGeminiKey(ctx context.Context, key string) (status int, code, message string) {
	ctx, cancel := context.WithTimeout(ctx, geminiKeyCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geminiModelsURL, nil)
	if err != nil {
		return http.StatusBadGateway, "unreachable", err.Error()
	}
	req.Header.Set("x-goog-api-key", key)
	resp, err := keyCheckClient.Do(req)
	if err != nil {
		slog.Info("setup: could not reach Google to check the key", "error", err)
		return http.StatusBadGateway, "unreachable", "June couldn't reach Google to check the key. Check your internet, or save the key without checking."
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusTooManyRequests:
		return http.StatusOK, "", ""
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		refusal := readGoogleError(resp.Body)
		if refusal.badKey() {
			return http.StatusUnprocessableEntity, "invalid_key", cmp.Or(refusal.message, "Google refused this key.")
		}
		return http.StatusUnprocessableEntity, "key_refused", refusal.advice()
	default:
		return http.StatusBadGateway, "unreachable", fmt.Sprintf("Google answered %s while checking the key.", resp.Status)
	}
}

// keyCheckClient is the client the key check asks Google with: the default transport, but through the proxy Windows' own settings name (see util.SystemProxy), since on a network that only lets traffic out through one the check otherwise failed while the browser that had just made the key worked.
var keyCheckClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = util.SystemProxy
	return &http.Client{Transport: t}
}()

// googleError is what a Google API error body, {"error": {"message", "status", "details": [{"reason"}]}}, says: the sentence, the RPC status ("INVALID_ARGUMENT", "PERMISSION_DENIED", "FAILED_PRECONDITION"), and the ErrorInfo reasons ("API_KEY_INVALID", "SERVICE_DISABLED", "API_KEY_HTTP_REFERRER_BLOCKED"). Every field is "" or empty when the body says nothing readable.
type googleError struct {
	message string
	status  string
	reasons []string
}

// readGoogleError reads a Google API error body, at most 16 KB of it.
func readGoogleError(body io.Reader) googleError {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(body, 16<<10)).Decode(&e) != nil {
		return googleError{}
	}
	g := googleError{message: strings.TrimSpace(e.Error.Message), status: e.Error.Status}
	for _, d := range e.Error.Details {
		if d.Reason != "" {
			g.reasons = append(g.reasons, d.Reason)
		}
	}
	return g
}

// badKey says the refusal is about the key itself: Google does not know it, or it has expired. The reason is what Google sets for that; the message is read too, for an answer that came without details, and an answer with nothing readable at all is taken as a bad key, the common case.
func (g googleError) badKey() bool {
	if slices.Contains(g.reasons, "API_KEY_INVALID") || slices.Contains(g.reasons, "API_KEY_EXPIRED") {
		return true
	}
	if g.message == "" && len(g.reasons) == 0 {
		return true
	}
	return strings.Contains(g.message, "API key not valid") || strings.Contains(g.message, "API key expired")
}

// advice is the sentence for a key Google knows but will not serve to June: Google's own reason, which names the project or the restriction, and what to do about it, since copying the same key again changes nothing.
func (g googleError) advice() string {
	reason := cmp.Or(g.message, "Google would not let June use this key.")
	switch {
	case slices.Contains(g.reasons, "SERVICE_DISABLED"):
		return "Google knows this key, but the Gemini API is turned off in its project: " + reason + " Turn it on there, or make a key in Google AI Studio, which has it on."
	case slices.ContainsFunc(g.reasons, func(r string) bool { return strings.HasPrefix(r, "API_KEY_") && strings.HasSuffix(r, "_BLOCKED") }):
		return "Google knows this key, but its restrictions keep June from using it: " + reason + " Remove the restriction in Google Cloud console, or make a key in Google AI Studio."
	case g.status == "FAILED_PRECONDITION":
		return "Google knows this key, but will not serve the Gemini API here: " + reason
	}
	return "Google knows this key, but would not let June use it: " + reason
}

// answerRestarting answers {"ok":true,"restarting":true} and then asks the daemon to restart. The answer is flushed first, so the window has it before the server goes down. The change is also marked pending, so a daemon whose restart hook is not set yet still says on GET /setup that one is wanted. Callers check restartAllowed first.
func answerRestarting(w http.ResponseWriter) {
	lifecycle.SetRestartPending(true)
	util.WriteJSON(w, map[string]bool{"ok": true, "restarting": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	lifecycle.RequestRestart()
}

// micTest handles POST /setup/mic-test: three seconds on the default microphone, with its level on /events as "level" events under the id "mic-test", the same {"mic","speaker"} detail a voice session sends. The test always listens, even when the registry says Windows blocks the microphone: those switches can be stale or overridden by policy, and anything heard proves the microphone reaches June, so a heard signal makes the consent "allowed" whatever the registry said. Output: 200 MicTestResult, with Error set when the microphone could not be opened; 409 busy while another test runs.
func (s *setupRoutes) micTest(w http.ResponseWriter, r *http.Request) {
	if !s.micMu.TryLock() {
		setupError(w, http.StatusConflict, "busy", "A microphone test is already running.")
		return
	}
	defer s.micMu.Unlock()
	res := MicTestResult{Device: defaultMicName(), Consent: micConsent()}
	peakRMS, err := s.listen(r.Context())
	if err != nil {
		slog.Warn("setup: the mic test could not open the microphone", "error", err)
		res.Error = err.Error()
	}
	res.Heard = peakRMS >= dictateSpeechRMS
	if res.Heard {
		res.Consent = "allowed"
	}
	// The level events' own scale (internal/audio's level), so the number matches what the meter showed.
	res.Peak = math.Round(math.Min(1, peakRMS/32768*3)*1000) / 1000
	util.WriteJSON(w, res)
}

// listen records for micTestLength and publishes the level as it goes. Input: the request's context, so a window that goes away ends the test. Output: the loudest chunk's RMS in sample units, the scale dictation's silence gate uses, or the error that kept the microphone from opening.
func (s *setupRoutes) listen(ctx context.Context) (float64, error) {
	mic, err := audio.NewMic()
	if err != nil {
		return 0, err
	}
	defer mic.Close()
	ctx, cancel := context.WithTimeout(ctx, micTestLength)
	defer cancel()
	chunks, err := mic.StartCapture(ctx)
	if err != nil {
		return 0, err
	}
	ticker := time.NewTicker(levelTickInterval)
	defer ticker.Stop()
	peak, last := 0.0, -1.0
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				// The meter is left at rest rather than on the last reading.
				s.publishLevel(0)
				return peak, nil
			}
			peak = math.Max(peak, chunkRMS(chunk))
		case <-ticker.C:
			if m := mic.CurrentAmplitude(); math.Abs(m-last) >= levelChangeThreshold {
				last = m
				s.publishLevel(m)
			}
		}
	}
}

// publishLevel sends one mic-test level event, when there is a server to send it on.
func (s *setupRoutes) publishLevel(mic float64) {
	if s.d.Server == nil {
		return
	}
	detail, _ := json.Marshal(map[string]float64{"mic": mic, "speaker": 0})
	s.d.Server.Publish(Event{ID: micTestID, Type: levelEventType, Detail: string(detail)})
}

// micSettings handles POST /setup/mic-settings: it opens the system's microphone settings, Windows' privacy page or the Linux desktop's sound settings, for a test that came back blocked or silent. Output: 204 once the settings are opening; 501 no_settings_app where there is nothing to open; 500 when starting it failed, with where to find the page by hand.
func (s *setupRoutes) micSettings(w http.ResponseWriter, r *http.Request) {
	err := openMicSettings()
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errNoMicSettings):
		setupError(w, http.StatusNotImplemented, "no_settings_app", "This desktop has no sound settings June knows how to open. Open your system's sound or privacy settings and check the microphone there.")
	default:
		slog.Error("setup: could not open the microphone settings", "error", err)
		msg := "June couldn't open the microphone settings. Open your sound or privacy settings and check the microphone there."
		if runtime.GOOS == "windows" {
			msg = "June couldn't open the microphone settings. Open Settings, then Privacy & security, then Microphone, and let desktop apps use it."
		}
		setupError(w, http.StatusInternalServerError, "open_failed", msg)
	}
}

// complete handles POST /setup/complete: setup_done is saved, screen tracking starts, and a restart a local feature asked for during setup happens now, unless RestartBlocker names something a restart would cut short, such as a download still running behind the Done screen. Then the restart stays pending and happens on its own once nothing is in the way, which is what the Done screen's "switches on by itself when it's done" promises. Output: 200 {"restarting": bool}, true when the daemon is restarting now, so the window shows "Restarting June…" rather than waiting on a daemon that is about to go away; 500 when the config could not be saved, in which case tracking stays paused.
func (s *setupRoutes) complete(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Config.Update(func(c *config.JuneConfig) { c.SetupDone = true }); err != nil {
		slog.Error("setup: could not save setup_done", "error", err)
		setupError(w, http.StatusInternalServerError, "save_failed", "June couldn't finish setup. Try again in a moment.")
		return
	}
	if s.d.ResumeTracking != nil {
		s.d.ResumeTracking()
	}
	pending := lifecycle.RestartPending()
	busy := ""
	if pending {
		busy = s.restartBlocker()
	}
	restarting := pending && busy == ""
	slog.Info("setup: first-run setup is done", "restarting", restarting, "restart_put_off_for", busy)
	util.WriteJSON(w, map[string]bool{"restarting": restarting})
	switch {
	case restarting:
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		lifecycle.RequestRestart()
	case pending:
		s.restartWhenFree()
	}
}

// restartWhenFree restarts the daemon once nothing is in the way, for the restart complete put off: RestartBlocker names nothing, which with cmd's blocker means no recording, download, conversation, dictation, screen job or answer on its way, and nothing the person started has begun or ended for restartQuiet (Server.LastUse). Nobody asked for this restart at the moment it happens, so it waits for a lull rather than taking the first gap between two questions. It waits on a goroutine of its own, and a restart pending that something else has since carried out ends the wait with this process. Output: none.
func (s *setupRoutes) restartWhenFree() {
	if !s.waiting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.waiting.Store(false)
		tick := time.NewTicker(restartRecheck)
		defer tick.Stop()
		var quietFrom time.Time
		for range tick.C {
			if !lifecycle.RestartPending() {
				return
			}
			if busy := s.restartBlocker(); busy != "" {
				quietFrom = time.Now()
				continue
			}
			if s.d.Server != nil {
				if last := s.d.Server.LastUse(); last.After(quietFrom) {
					quietFrom = last
				}
			}
			if time.Since(quietFrom) < restartQuiet {
				continue
			}
			slog.Info("setup: restarting now that nothing is in the way, to apply what changed during setup")
			lifecycle.RequestRestart()
			return
		}
	}()
}

// tellSettingsReset tells the person, once, that June's settings file could not be read and was set aside (see config.SaveConfig), so a brain or a choice that went back to its default does not look like June forgetting it. The notice waits for a window to connect, since one is opening as the daemon starts, and past settingsResetWait goes out anyway, which with no window is a desktop notification. Input: the server to say it through, and the name the broken file was kept under. Output: none.
func tellSettingsReset(server *Server, aside string) {
	deadline := time.Now().Add(settingsResetWait)
	for !server.Subscribed(0) && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	slog.Info("setup: telling the user their settings file was set aside", "kept_as", aside)
	n := Notice{
		Title:   "June's settings were reset",
		Body:    "June couldn't read its settings, so it started fresh and kept a copy of the old ones. Your memory is safe. Check your choices in Settings.",
		Place:   "settings",
		Kind:    "settings",
		Actions: []NoticeButton{{Key: "done", Label: "OK"}},
	}
	// Straight to the window's card when one is there, with the one OK button; the scheduler's own say, which also reaches the desktop's notifications, would give it the snooze buttons a reminder has, and a notice like this has nothing to come back about.
	if server.Subscribed(0) {
		server.Notice(n)
		return
	}
	server.sayNotice(n)
}

// setupError answers status with {"error": code, "message": message}: code is what the window branches on, message a sentence it can show as it is.
func setupError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
