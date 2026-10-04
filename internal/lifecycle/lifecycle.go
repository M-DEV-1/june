// Package lifecycle lets the parts of the daemon that can change what it was started with — a Gemini key saved in first-run setup, a local model installed from the window — ask for the daemon to restart or quit, without importing cmd. cmd sets the hooks at start; until then every call is a no-op, which is also what a test gets.
package lifecycle

import "sync/atomic"

var (
	restart atomic.Pointer[func()]
	quit    atomic.Pointer[func()]
	pending atomic.Bool
)

// SetHooks installs the daemon's own restart and quit. Input: the functions cmd runs for each. Output: none.
func SetHooks(onRestart, onQuit func()) {
	restart.Store(&onRestart)
	quit.Store(&onQuit)
}

// RequestRestart asks the daemon to restart itself now. It returns at once; the restart happens on the daemon's own goroutine.
func RequestRestart() {
	if fn := restart.Load(); fn != nil && *fn != nil {
		go (*fn)()
	}
}

// RequestQuit asks the daemon to shut down.
func RequestQuit() {
	if fn := quit.Load(); fn != nil && *fn != nil {
		go (*fn)()
	}
}

// SetRestartPending records that something changed which only takes effect after a restart, so the window can offer one.
func SetRestartPending(on bool) { pending.Store(on) }

// RestartPending reports whether a restart is waiting to apply a change.
func RestartPending() bool { return pending.Load() }
