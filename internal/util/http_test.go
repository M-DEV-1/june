package util

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every shape a Retry-After header can arrive in, including the two the two former copies of this parser disagreed on: fractional seconds, which one of them refused, and a date that has already passed, which one of them reported as no delay at all rather than as a delay of none.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"integer seconds", "4", 4 * time.Second, true},
		{"zero seconds is a named delay of none", "0", 0, true},
		{"fractional seconds", "1.5", 1500 * time.Millisecond, true},
		{"negative seconds", "-3", 0, false},
		{"a future date", now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
		{"a date that has already passed means retry now", now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{"garbage", "soon", 0, false},
		{"no header at all", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.value != "" {
				h.Set("Retry-After", c.value)
			}
			got, ok := ParseRetryAfter(h, now)
			if got != c.want || ok != c.ok {
				t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", c.value, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestBodySnippet checks the read is trimmed and capped, so a failed HTTP response body never fills the log with an unbounded error page.
func TestBodySnippet(t *testing.T) {
	if got := BodySnippet(strings.NewReader("  hello  ")); got != "hello" {
		t.Errorf("BodySnippet trimmed = %q, want %q", got, "hello")
	}
	big := strings.Repeat("x", 1000)
	if got := BodySnippet(strings.NewReader(big)); len(got) != 512 {
		t.Errorf("BodySnippet returned %d bytes, want capped at 512", len(got))
	}
}
