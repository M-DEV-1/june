package ipc

import (
	"sync/atomic"
	"time"
)

// lastUse is when the person last started or finished something with June: an ask, a voice conversation, a dictation, a screen job's step. What is running right now is cmd's to say (the restart blocker it hands SetupDeps); this is the part only the routes see, that something ended a moment ago, which is what a restart nobody asked for at that moment waits out (see setupRoutes.restartWhenFree).
type lastUse struct {
	// at is the moment, as Unix nanoseconds, 0 for never.
	at atomic.Int64
}

// touch records that something the person started has just begun, moved on or finished.
func (u *lastUse) touch() { u.at.Store(time.Now().UnixNano()) }

// LastUse is when an ask, a voice conversation, a dictation or a screen job last started, moved on or ended, the zero time when none has since the daemon started. Something that started and ended between two looks at what is running still moves this, so a caller can wait for a quiet spell rather than only for a moment when nothing happens to be running. Input: none. Output: the time.
func (s *Server) LastUse() time.Time {
	n := s.use.at.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
