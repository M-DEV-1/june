//go:build linux

package tracker

import (
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
	var active bool
	obj := lockBus.Object("org.gnome.ScreenSaver", dbus.ObjectPath("/org/gnome/ScreenSaver"))
	if err := obj.Call("org.gnome.ScreenSaver.GetActive", 0).Store(&active); err != nil {
		return false
	}
	return active
}

func init() { sessionLocked = gnomeSessionLocked }
