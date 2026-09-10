package cmd

import (
	"strings"
	"testing"
)

// The report ends with the first blocker's own fix, so a desk that cannot place clicks says "log out and in" rather than leaving the reader to work it out from five lines.
func TestDoctorReport_NamesTheFirstBlockerAndItsFix(t *testing.T) {
	got := doctorReport([]doctorCheck{
		{Name: "accessibility bus", Detail: "reachable", OK: true},
		{Name: "window frames", Detail: "3 windows listed, none with a frame", Fix: "log out and in"},
		{Name: "daemon", Detail: "not answering", Fix: "run ora"},
	})
	for _, want := range []string{"ok    accessibility bus", "FAIL  window frames", "not ready: window frames, daemon", "next: log out and in"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(doctorReport([]doctorCheck{{Name: "daemon", OK: true}}), "ready: everything") {
		t.Error("a report with no blockers must say it is ready")
	}
}
