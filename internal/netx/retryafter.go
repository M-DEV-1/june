// Package netx holds the small pieces of HTTP handling that more than one caller needs and the standard library does not provide.
package netx

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter reads the delay a server named in its Retry-After header, which RFC 9110 lets it write either as a number of seconds or as an HTTP date. Input: the response headers and the time to measure a date against. Output: the delay and true when the header names one — zero counts as a named delay, "retry now", and so does a date that has already passed — or false when there is no header, the value cannot be read, or it names a negative number of seconds.
func ParseRetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	at, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	if d := at.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}
