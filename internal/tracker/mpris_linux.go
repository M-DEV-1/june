//go:build linux

package tracker

import (
	"context"
	"strings"

	"github.com/godbus/dbus/v5"
)

// mprisPrefix is the well-known bus name prefix every MPRIS-compliant media player registers under (browsers, Spotify, VLC, mpv, video-call clients, etc).
const mprisPrefix = "org.mpris.MediaPlayer2."

// mediaPlaying reports whether any MPRIS media player on the session bus is currently "Playing". Used to force the vision tier even when AT-SPI text looks rich — a video or call tab has plenty of chrome text but none of it describes the screen.
// Best-effort: any D-Bus error yields false rather than propagating — this must never fail a capture tick.
func mediaPlaying(ctx context.Context) bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}

	var names []string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return false
	}

	for _, name := range names {
		if ctx.Err() != nil {
			return false
		}
		if !strings.HasPrefix(name, mprisPrefix) {
			continue
		}
		if playerIsPlaying(ctx, conn, name) {
			return true
		}
	}
	return false
}

// playerIsPlaying reads the PlaybackStatus property of a single MPRIS player object and reports whether it equals "Playing".
func playerIsPlaying(ctx context.Context, conn *dbus.Conn, busName string) bool {
	obj := conn.Object(busName, "/org/mpris/MediaPlayer2")
	var status dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.mpris.MediaPlayer2.Player", "PlaybackStatus").Store(&status); err != nil {
		return false
	}
	s, _ := status.Value().(string)
	return s == "Playing"
}
