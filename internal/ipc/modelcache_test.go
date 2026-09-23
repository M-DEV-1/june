package ipc

import (
	"sync"
	"testing"
	"time"
)

// The roster is served from memory and read in the background, because `agy models` takes about three and a half seconds and GET /brains runs on every settings render and again after every pick — paying for it on the request is what made the brain picker take seconds to answer a click.
func TestModelCache_AnswersWithoutWaitingForTheCommand(t *testing.T) {
	var c modelCache
	done := make(chan struct{})
	slow := func() []string {
		<-done
		return []string{"gemini-3.8-flash-high"}
	}

	// The first call must come back at once, before the read it started has finished.
	if got, _ := c.get(slow); len(got) != 0 {
		t.Errorf("first call = %v, want nothing yet rather than a wait for the command", got)
	}
	close(done)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := c.get(slow); len(got) == 1 && got[0] == "gemini-3.8-flash-high" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the roster never reached the cache")
}

// A burst of requests starts one read, not one each, or a settings screen that renders twice would run the command twice.
func TestModelCache_StartsOneReadForABurst(t *testing.T) {
	var c modelCache
	var mu sync.Mutex
	reads := 0
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	read := func() []string {
		mu.Lock()
		reads++
		mu.Unlock()
		entered <- struct{}{}
		<-release
		return []string{"one"}
	}

	for i := 0; i < 10; i++ {
		c.get(read)
	}
	// Wait for the one read to have actually started before counting, or the count races the goroutine that does the reading.
	<-entered
	close(release)

	mu.Lock()
	defer mu.Unlock()
	if reads != 1 {
		t.Errorf("ran the command %d times for one burst, want once", reads)
	}
}

// A cached roster is served again without another read until it goes stale, which is what keeps the picker instant.
func TestModelCache_ServesWhatItHasWithoutReadingAgain(t *testing.T) {
	c := modelCache{models: []string{"sonnet"}, readAt: time.Now()}
	reads := 0
	read := func() []string { reads++; return []string{"other"} }

	got, _ := c.get(read)
	if len(got) != 1 || got[0] != "sonnet" {
		t.Errorf("served %v, want the cached roster", got)
	}
	if reads != 0 {
		t.Errorf("ran the command %d times for a fresh cache, want none", reads)
	}
}
