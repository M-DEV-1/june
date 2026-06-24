//go:build linux

package tracker

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestResponseURI(t *testing.T) {
	ok := []interface{}{
		uint32(0),
		map[string]dbus.Variant{"uri": dbus.MakeVariant("file:///tmp/shot.png")},
	}
	uri, err := responseURI(ok)
	if err != nil {
		t.Fatalf("success case: %v", err)
	}
	if uri != "file:///tmp/shot.png" {
		t.Fatalf("uri=%q", uri)
	}

	denied := []interface{}{uint32(1), map[string]dbus.Variant{}}
	if _, err := responseURI(denied); err == nil {
		t.Error("denied (code 1) should error")
	}

	noURI := []interface{}{uint32(0), map[string]dbus.Variant{}}
	if _, err := responseURI(noURI); err == nil {
		t.Error("missing uri should error")
	}

	if _, err := responseURI([]interface{}{uint32(0)}); err == nil {
		t.Error("short body should error")
	}
}

func TestScreenshotGrantedDenied(t *testing.T) {
	cases := []struct {
		name      string
		perms     map[string][]string
		wantGrant bool
		wantDeny  bool
	}{
		{"unsandboxed allow", map[string][]string{"": {"yes"}}, true, false},
		{"unsandboxed deny", map[string][]string{"": {"no"}}, false, true},
		{"named app allow", map[string][]string{"ora": {"yes"}}, true, false},
		{"never asked", map[string][]string{}, false, false},
		{"nil perms", nil, false, false},
		{"grant wins over unrelated", map[string][]string{"": {"yes"}, "other": {"no"}}, true, false},
	}
	for _, c := range cases {
		if got := screenshotGranted(c.perms); got != c.wantGrant {
			t.Errorf("%s: screenshotGranted=%v want %v", c.name, got, c.wantGrant)
		}
		if got := screenshotDenied(c.perms); got != c.wantDeny {
			t.Errorf("%s: screenshotDenied=%v want %v", c.name, got, c.wantDeny)
		}
	}
}
