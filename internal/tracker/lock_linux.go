//go:build linux

package tracker

import (
	"context"
	"sync"

	"github.com/godbus/dbus/v5"
)

// lockBus is the session bus connection the lock check rides, dialed once and kept — one lock query per tracker tick is cheap on a local socket but not worth a new connection each time.
var (
	lockBus  *dbus.Conn
	lockOnce sync.Once
)

// gnomeSessionLocked reports whether GNOME's screen shield is up. Any failure — no session bus, no screensaver service — reads as unlocked, since wrongly refusing to capture is the harmful direction to fail in.
func gnomeSessionLocked() bool {
	lockOnce.Do(func() { lockBus, _ = dbus.SessionBus() })
	if lockBus == nil {
		return false
	}
	// godbus's plain Call carries context.Background() and godbus applies no reply timeout of its own, so a gnome-shell that accepted this message and never answered held the tick goroutine for good: no window polling, no dwell emission, no recapture. This probe runs first thing on every tick, ahead of the three capture calls daemon.go already bounds, so it gets the same kind of bound.
	ctx, cancel := context.WithTimeout(context.Background(), dbusProbeTimeout)
	defer cancel()
	var active bool
	obj := lockBus.Object("org.gnome.ScreenSaver", dbus.ObjectPath("/org/gnome/ScreenSaver"))
	if err := obj.CallWithContext(ctx, "org.gnome.ScreenSaver.GetActive", 0).Store(&active); err != nil {
		return false
	}
	return active
}

func init() { sessionLocked = gnomeSessionLocked }
