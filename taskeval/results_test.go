//go:build taskeval && linux

package taskeval

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// result is one line of out/results.jsonl: one run of one task.
type result struct {
	Time      string `json:"time"`
	Mode      string `json:"mode"`
	Suite     string `json:"suite"`
	Task      string `json:"task"`
	Seed      int64  `json:"seed"`
	Split     string `json:"split"`
	Pass      bool   `json:"pass"`
	Steps     int    `json:"steps"`
	WallMS    int64  `json:"wall_ms"`
	PeakRSSKB int64  `json:"peak_rss_kb"`
	Note      string `json:"note,omitempty"`
}

var resultsMu sync.Mutex

// record appends r to out/results.jsonl, stamping the time and mode. A write failure fails the test, because a run nobody can read back is a run that did not happen.
func record(t *testing.T, r result) {
	t.Helper()
	r.Time = time.Now().UTC().Format(time.RFC3339)
	r.Mode = "scripted"
	if live {
		r.Mode = "live"
	}
	line, _ := json.Marshal(r)

	resultsMu.Lock()
	defer resultsMu.Unlock()
	if err := os.MkdirAll("out", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile("out/results.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
		t.Fatal(err)
	}
}
