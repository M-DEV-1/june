//go:build linux

package tracker

import "testing"

// On GNOME Wayland, XWayland's _NET_ACTIVE_WINDOW reports 0x0 for native Wayland windows, so the X11 backend is blind there and AT-SPI must take priority. On native X11 the X11 backend wins instead (faster, no a11y dep).
func TestSelectBackend(t *testing.T) {
	cases := []struct {
		name               string
		haveSway, haveHypr bool
		wayland, haveX11   bool
		want               backend
	}{
		{"sway wins over everything", true, true, true, true, backendSway},
		{"hypr before atspi/x11", false, true, true, true, backendHypr},
		{"gnome wayland with xwayland -> atspi", false, false, true, true, backendATSPI},
		{"pure wayland -> atspi", false, false, true, false, backendATSPI},
		{"native x11 -> x11", false, false, false, true, backendX11},
		{"nothing -> none", false, false, false, false, backendNone},
	}
	for _, c := range cases {
		if got := selectBackend(c.haveSway, c.haveHypr, c.wayland, c.haveX11); got != c.want {
			t.Errorf("%s: selectBackend=%v want %v", c.name, got, c.want)
		}
	}
}
