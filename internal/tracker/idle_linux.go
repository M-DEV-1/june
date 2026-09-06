//go:build linux

package tracker

import (
	"context"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// idleBus is the session bus connection the idle check rides, dialed once and kept — same reasoning as lockBus.
var (
	idleBus  *dbus.Conn
	idleOnce sync.Once
)

// gnomeInputIdle asks Mutter's IdleMonitor how long since the last real keyboard or mouse input. Any failure — no session bus, no idle-monitor service — is returned as an error, since callers need to know the read failed rather than silently treating it as fresh input.
func gnomeInputIdle() (time.Duration, error) {
	idleOnce.Do(func() { idleBus, _ = dbus.SessionBus() })
	if idleBus == nil {
		return 0, errNoIdleProbe
	}
	// Bounded for the same reason the lock probe is: a plain Call waits on a reply that may never come, and the dreaming loop that asks this question runs unattended overnight.
	ctx, cancel := context.WithTimeout(context.Background(), dbusProbeTimeout)
	defer cancel()
	var ms uint64
	obj := idleBus.Object("org.gnome.Mutter.IdleMonitor", dbus.ObjectPath("/org/gnome/Mutter/IdleMonitor/Core"))
	if err := obj.CallWithContext(ctx, "org.gnome.Mutter.IdleMonitor.GetIdletime", 0).Store(&ms); err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func init() { inputIdle = gnomeInputIdle }
