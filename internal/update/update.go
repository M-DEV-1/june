// Package update notices when a newer June release is out and, when the user asks, installs it: on Windows by downloading the release's installer, checking it against the release's SHA256SUMS and running it silently; elsewhere by pointing at the release page.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"june/internal/config"
)

// Options is what the updater needs from the daemon. Repo is the GitHub owner/name releases are read from; Version is the running build's version ("dev" for a build without one, which never updates itself); Publish puts one event on the window's stream; Blocker, when set, names what stopping June right now would cut short, in the words the daemon's restartBlocker uses ("recording", "processing", "downloading", or "" for nothing).
type Options struct {
	DataDir string
	ExeDir  string
	Version string
	Repo    string
	Publish func(typ, id, text, detail string, failed bool)
	Blocker func() string
}

// The states GET /update reports.
const (
	stateIdle        = "idle"
	stateChecking    = "checking"
	stateDownloading = "downloading"
	stateReady       = "ready"
	stateInstalling  = "installing"
	stateFailed      = "failed"
)

const (
	// startDelay and startJitter put the first check after the daemon's own start-up and spread machines that all start at the same hour across a few minutes.
	startDelay  = 30 * time.Second
	startJitter = 3 * time.Minute
	// startGap skips the start-up check when the last one is this recent: first-run setup restarts the daemon several times in a few minutes, and GitHub allows an unauthenticated address 60 requests an hour.
	startGap = time.Hour
	// checkEvery is how stale the last answer may get. The loop wakes every wakeEvery and compares wall-clock times rather than setting a timer for a day, because a timer does not count the time a suspended machine spends asleep.
	checkEvery = 24 * time.Hour
	wakeEvery  = time.Hour
	// checkTimeout bounds one check, scheduled or asked for from the window.
	checkTimeout = 20 * time.Second
)

// Checker watches for new releases and installs one when asked.
type Checker struct {
	opts Options

	// checkMu makes checks take turns, so a refresh from the window that lands during the scheduled check waits for its answer rather than racing it.
	checkMu sync.Mutex

	mu        sync.Mutex
	ctx       context.Context
	state     string
	err       string
	rel       *release
	etag      string
	checkedAt time.Time
	notBefore time.Time
	// announced is the version an "available" event already went out for in this run.
	announced string
}

// New makes the checker and restores what the last check found. It starts nothing.
func New(opts Options) *Checker {
	c := &Checker{opts: opts, state: stateIdle}
	if !c.dev() && opts.DataDir != "" {
		c.load()
	}
	return c
}

// dev reports whether this build must stay out of updating altogether: one without a release number, or with no repo to read releases from.
func (c *Checker) dev() bool {
	_, ok := parseVersion(c.opts.Version)
	return !ok || strings.Count(c.opts.Repo, "/") != 1
}

// Run checks for a new release shortly after start and then once a day until ctx is done, while the config leaves the check on (see scheduledOn). A check that fails is logged at debug level and nothing else: an offline laptop must not nag. Input: the daemon's context, which also bounds a download the window starts.
func (c *Checker) Run(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	if c.dev() {
		return
	}
	c.removeOldInstallers()
	timer := time.NewTimer(startDelay + rand.N(startJitter))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if c.since() >= startGap {
		c.scheduledCheck(ctx)
	}
	tick := time.NewTicker(wakeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if c.since() >= checkEvery {
			c.scheduledCheck(ctx)
		}
	}
}

// since is how long ago the last answer from GitHub came. Never, or a time ahead of the clock (it was set back), counts as overdue.
func (c *Checker) since() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := time.Since(c.checkedAt)
	if c.checkedAt.IsZero() || d < 0 {
		return checkEvery
	}
	return d
}

// scheduledCheck runs one check nobody is waiting on, unless the user turned the daily check off. A check the user asks for from the window (GET /update?refresh=1, POST /update) does not come through here and still runs: turning the daily one off is about June not contacting GitHub on its own.
func (c *Checker) scheduledCheck(ctx context.Context) {
	if !scheduledOn() {
		slog.Debug("update: the daily check is turned off in the config")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := c.check(ctx); err != nil {
		slog.Debug("update: check failed", "error", err)
	}
}

// scheduledOn reports whether the config leaves the daily check on. It reads the file each time rather than once at start, so turning the check off in Settings or with june --update-check off holds from the next hourly wake without a restart. A config that cannot be read counts as off for this round: the setting exists so June can be kept from contacting a server the user did not choose, and an hour later the file is read again.
func scheduledOn() bool {
	cfg, err := config.ReadConfig()
	return err == nil && cfg.UpdateCheckEnabled()
}

// Routes answers GET /update and POST /update, keyed by ServeMux pattern for cmd to register behind the IPC token.
func (c *Checker) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /update":  c.handleGet,
		"POST /update": c.handlePost,
	}
}

// status is GET /update's answer.
type status struct {
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Available  bool   `json:"available"`
	ReleaseURL string `json:"release_url"`
	CanInstall bool   `json:"can_install"`
	State      string `json:"state"`
	Error      string `json:"error"`
}

// handleGet answers GET /update with what the last check found. ?refresh=1 checks GitHub first, for a "Check for updates" button; a failed refresh is the one check failure the user hears about, as state "failed".
func (c *Checker) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" && !c.dev() {
		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		err := c.check(ctx)
		cancel()
		if err != nil {
			c.mu.Lock()
			if c.state == stateIdle || c.state == stateFailed {
				c.state, c.err = stateFailed, shown(err)
			}
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	st := c.statusLocked()
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

// handlePost answers POST /update: it starts downloading and installing the latest release and answers 202 with the new state, before the download is done; progress follows on /events as "update" events. A second press while one is under way gets the same 202. Output: 409 {"error":"dev_build"|"no_update"|"cannot_install"} when there is nothing this build can install, in which case the window links to release_url instead, and 409 {"error":"recording"|"processing"|"downloading","message":"…"} while stopping June would cut one of those short, as POST /restart answers.
func (c *Checker) handlePost(w http.ResponseWriter, r *http.Request) {
	if c.dev() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "dev_build"})
		return
	}
	c.mu.Lock()
	switch c.state {
	case stateDownloading, stateReady, stateInstalling:
		st := c.statusLocked()
		c.mu.Unlock()
		writeJSON(w, http.StatusAccepted, st)
		return
	}
	if !c.availableLocked() {
		c.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no_update"})
		return
	}
	if !c.installable(c.rel) {
		c.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot_install"})
		return
	}
	if code := c.blocker(); code != "" {
		c.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": code, "message": busyRefusal(code)})
		return
	}
	ver := c.rel.version()
	c.state, c.err = stateDownloading, ""
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	st := c.statusLocked()
	c.mu.Unlock()
	go c.install(ctx, ver)
	writeJSON(w, http.StatusAccepted, st)
}

// statusLocked builds GET /update's answer. Call with mu held.
func (c *Checker) statusLocked() status {
	st := status{Current: c.opts.Version, State: c.state, Error: c.err}
	if st.Current == "" {
		st.Current = "dev"
	}
	if c.rel != nil && !c.dev() {
		st.Latest = c.rel.version()
		st.ReleaseURL = c.releaseURL(c.rel)
		st.Available = c.availableLocked()
		st.CanInstall = st.Available && c.installable(c.rel)
	}
	return st
}

// availableLocked reports whether the last check found a release newer than this build. Call with mu held.
func (c *Checker) availableLocked() bool {
	if c.rel == nil {
		return false
	}
	cur, ok := parseVersion(c.opts.Version)
	if !ok {
		return false
	}
	latest, ok := parseVersion(c.rel.Tag)
	return ok && latest.compare(cur) > 0
}

// installable reports whether this copy of June can install rel itself: the platform can (see canSelfInstall), and the release carries both the installer and the checksums it is verified against. A release missing either is still offered, as a link to its page.
func (c *Checker) installable(rel *release) bool {
	return canSelfInstall(c.opts.ExeDir) && rel.asset(installerAsset) != nil && rel.asset(sumsAsset) != nil
}

// blocker names what installing now would cut short, "" for nothing. The installer stops June the way a quit does, which POST /quit never refuses and june.iss forces if it must, and the June it opens afterwards takes none of them up again: the rest of a call goes unrecorded and a meeting's write-up waits for a later sweep. So an update waits for them, as POST /restart does.
func (c *Checker) blocker() string {
	if c.opts.Blocker == nil {
		return ""
	}
	return c.opts.Blocker()
}

// busyRefusal is the sentence for an update refused because of what blocker named. Input: blocker's word. Output: the sentence the window shows.
func busyRefusal(code string) string {
	switch code {
	case "recording":
		return "Updating June now would end the meeting recording, and the rest of the call would not be recorded. Update once the meeting is over."
	case "processing":
		return "Updating June now would stop the write-up of the meeting it just recorded. Update in a few minutes, once the notes are ready."
	case "downloading":
		return "Updating June now would stop the local features it is downloading. Update once they finish, or cancel the download first."
	}
	return "June is in the middle of something that updating would cut short. Try again in a few minutes."
}

// releaseURL is the release's page on GitHub, which the window opens with /open; anything but a github.com page is replaced by the page built from the tag.
func (c *Checker) releaseURL(rel *release) string {
	if strings.HasPrefix(rel.HTMLURL, "https://github.com/") {
		return rel.HTMLURL
	}
	return "https://github.com/" + c.opts.Repo + "/releases/tag/" + rel.Tag
}

// publish sends one "update" event. Input: the version it is about, the step, a JSON detail or "", and whether it is a failure.
func (c *Checker) publish(id, text, detail string, failed bool) {
	if c.opts.Publish != nil {
		c.opts.Publish("update", id, text, detail, failed)
	}
}

// userError is a failure as the window shows it, a short sentence, with the cause kept for the log.
type userError struct {
	shown string
	cause error
}

func (e *userError) Error() string {
	if e.cause == nil {
		return e.shown
	}
	return e.shown + " (" + e.cause.Error() + ")"
}

func (e *userError) Unwrap() error { return e.cause }

// shown is the sentence the window shows for err.
func shown(err error) string {
	var u *userError
	if errors.As(err, &u) {
		return u.shown
	}
	return err.Error()
}

// writeJSON answers with v as JSON and the given status.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
