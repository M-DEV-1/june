//go:build !linux

package tracker

import "context"

// mediaPlaying is a no-op off Linux — MPRIS is a Linux session-bus convention, so other platforms never force the vision tier via this signal (the thin-text guard alone still applies).
func mediaPlaying(_ context.Context) bool {
	return false
}
