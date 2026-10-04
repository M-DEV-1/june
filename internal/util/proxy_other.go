//go:build !windows

package util

import (
	"net/http"
	"net/url"
)

// osProxy is no proxy off Windows: the variables SystemProxy already read are how a Linux program is told about one. GNOME's own proxy setting, which only programs built on GLib follow, is not read.
func osProxy(*http.Request) (*url.URL, error) { return nil, nil }
