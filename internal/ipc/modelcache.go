// modelcache.go keeps the model rosters GET /brains reports without paying for them on the request. Asking a command line what models it can call is slow — `agy models` measured 3.5 seconds on 2026-09-07, and the window reads /brains on every settings render and again after every pick, so the picker took several seconds to answer a click. The roster changes about as often as the user changes plan, so it is read in the background and served from memory.
package ipc

import (
	"sync"
	"time"
)

// modelCacheTTL is how long a roster is served before it is read again. Long, because a CLI's model list changes when the user's plan does and not otherwise; short enough that a plan change shows up the same session.
const modelCacheTTL = 10 * time.Minute

// modelCache is one command's last known roster.
type modelCache struct {
	mu sync.Mutex
	// models is what the last successful read returned, served to every caller until it goes stale.
	models []string
	// readAt is when that read finished; the zero time means nothing has ever been read.
	readAt time.Time
	// reading is true while a background read is in flight, so a burst of requests starts one refresh rather than one each.
	reading bool
}

// get returns the cached roster, starting a background refresh when it is stale or has never been read. Input: how to read the roster, which runs on its own goroutine and must not be called by the caller. Output: the newest roster already in hand, which is empty only until the first read finishes.
// It never blocks: a request that arrives before the first read gets an empty list and the one a second later gets the real thing, which is the right trade when the alternative is holding the settings screen for three and a half seconds.
func (c *modelCache) get(read func() []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.reading && time.Since(c.readAt) > modelCacheTTL {
		c.reading = true
		go c.refresh(read)
	}
	return append([]string(nil), c.models...)
}

// refresh reads the roster and stores it. A read that comes back empty is stored too: a CLI the user has signed out of has no models, and holding the old list would report models it can no longer call.
func (c *modelCache) refresh(read func() []string) {
	models := read()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models, c.readAt, c.reading = models, time.Now(), false
}

// The rosters themselves, one cache per command.
var (
	agyCache    modelCache
	ollamaCache modelCache
)

// WarmModelCaches reads both rosters in the background so the first GET /brains after startup already has them. Without it the settings screen renders once with no models under Antigravity and only fills in on a later read, which for a screen that is read once looks like the row has none. Input: whether each command exists on this machine, so a machine without one starts no pointless read. Output: none; both reads run on their own goroutines.
func WarmModelCaches(has func(string) bool) {
	if has == nil {
		has = onPath
	}
	if has("agy") {
		agyCache.get(agyModels)
	}
	if has("ollama") {
		ollamaCache.get(ollamaList)
	}
}
