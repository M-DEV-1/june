package cmd

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"june/internal/config"
)

// pauseCheckEvery is how often a pause with an end is checked for having ended. The end is compared on the wall clock rather than left to a timer, because a timer does not count the time a suspended laptop spends asleep: a "pause for 1 hour" begun before an evening's sleep would otherwise run on for most of the next hour after waking.
const pauseCheckEvery = 5 * time.Second

// maxPauseMinutes bounds POST /pause {"minutes": N}: a week. A longer pause is "until I resume", which needs no number.
const maxPauseMinutes = 7 * 24 * 60

// pausable is the part of the tracker a pause drives.
type pausable interface {
	Pause()
	Resume()
}

// pauseControl is the user's own pause of screen observation: until they resume it, or until a time they chose. Every change goes through its lock, so a pause that ends on its own can never land between a new pause and its marker being written. It is kept on disk as well (see pauseMarkerName), so it outlasts the restarts June makes on its own account.
type pauseControl struct {
	mu        sync.Mutex
	tracker   pausable
	setupDone func() bool
	// until is when the pause ends by itself; zero while not paused, or paused until the user resumes. It never carries a monotonic reading, so comparing it with the time now is a wall-clock comparison (see pauseCheckEvery).
	until time.Time
}

// pauses is the daemon's one pause control, which the routes, the setup routes and the trays share. startDaemonServices binds it to the tracker before anything reads it.
var pauses pauseControl

// bind gives the control the tracker it pauses and what says whether observation may come on, which it may not while it waits for first-run setup (see setupWaits); a pause that ends by itself checks it before it resumes anything.
func (c *pauseControl) bind(t pausable, setupDone func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracker, c.setupDone = t, setupDone
}

// restore takes up the pause the last daemon left, pausing the tracker when it is still on. Output: true when observation stays paused. An empty marker is a pause until the user resumes, and so is one this build cannot read, since watching when the user asked not to is the worse mistake; a pause whose end passed while June was not running is dropped.
func (c *pauseControl) restore() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := os.ReadFile(pauseMarkerPath())
	if err != nil {
		return false
	}
	var until time.Time
	if text := strings.TrimSpace(string(raw)); text != "" {
		if t, err := time.Parse(time.RFC3339, text); err == nil {
			if !time.Now().Before(t) {
				c.writeMarker(false, time.Time{})
				slog.Info("the timed pause the user chose ended while June was not running, so observation is on again", "ended", t)
				return false
			}
			until = t
		} else {
			slog.Warn("could not read when the pause ends; it stays paused until the user resumes", "marker", text, "error", err)
		}
	}
	c.until = until
	c.tracker.Pause()
	return true
}

// pause pauses observation now. Input: when the pause ends by itself, zero for "until I resume". A new pause replaces whatever end the last one had.
func (c *pauseControl) pause(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracker.Pause()
	c.until = until.Round(0)
	c.writeMarker(true, c.until)
}

// resume resumes observation now and forgets any end the pause had.
func (c *pauseControl) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tracker.Resume()
	c.until = time.Time{}
	c.writeMarker(false, time.Time{})
}

// pausedUntil is when the pause ends by itself. Output: zero when there is no pause or it lasts until the user resumes.
func (c *pauseControl) pausedUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until
}

// watch ends a pause with an end once that end has passed, until ctx ends. While observation waits for first-run setup the pause is forgotten but observation is not turned on, as nothing is observed before setup.
func (c *pauseControl) watch(ctx context.Context) {
	tick := time.NewTicker(pauseCheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c.mu.Lock()
		if !c.until.IsZero() && !time.Now().Before(c.until) {
			c.until = time.Time{}
			c.writeMarker(false, time.Time{})
			if c.setupDone == nil || c.setupDone() {
				c.tracker.Resume()
				slog.Info("the pause the user chose for a while is over, so observation is on again")
			}
		}
		c.mu.Unlock()
	}
}

// writeMarker records whether the user has observation paused, and until when, so the next daemon starts the same way. Called with c.mu held. Best effort: a marker that cannot be written leaves the pause as it is in this daemon, and says so in the log.
func (c *pauseControl) writeMarker(paused bool, until time.Time) {
	path := pauseMarkerPath()
	var err error
	switch {
	case paused && until.IsZero():
		err = os.WriteFile(path, nil, 0600)
	case paused:
		err = os.WriteFile(path, []byte(until.Format(time.RFC3339)), 0600)
	default:
		if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		slog.Warn("could not record the pause for the next start of June", "paused", paused, "until", until, "error", err)
	}
}

// pauseMarkerPath is where the pause is kept (see pauseMarkerName).
func pauseMarkerPath() string {
	return filepath.Join(config.DataDir(), pauseMarkerName)
}
