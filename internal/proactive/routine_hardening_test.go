package proactive

import (
	"strings"
	"testing"
	"time"
)

// TestParseSchedule_SurvivesEmptyHugeAndNonUTF8Input feeds ParseSchedule the shapes a routine's schedule text can actually arrive in from a model or a mistyped user instruction — empty, whitespace, a megabyte of one letter, bytes that are not UTF-8, and numbers too large to be a duration — and requires that nothing panics and that anything it accepts describes a real repeat interval.
func TestParseSchedule_SurvivesEmptyHugeAndNonUTF8Input(t *testing.T) {
	inputs := []string{
		"", " ", "\x00",
		string([]byte{0xff, 0xfe, 0xc3, 0x28, 0x80}),
		strings.Repeat("a", 1<<20),
		"every day at " + strings.Repeat("9", 100000),
		"every " + strings.Repeat("9", 100000) + " hours",
		"every 9223372036854775807 hours",
		"every 999999999999 hours",
		"every 0 hours",
		"when " + strings.Repeat("\xff", 100000),
		"every day at " + strings.Repeat("9:", 50000),
		"weekdays at -5",
		"every day at 9:" + strings.Repeat("0", 100000),
	}
	now := time.Now()
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("input %d (%.40q) panicked: %v", i, in, r)
				}
			}()
			sch, err := ParseSchedule(in)
			if err != nil {
				return
			}
			// A schedule that parsed must describe a real moment: an interval that overflows time.Duration would report itself due on every single tick, firing a routine the user asked for once a day continuously.
			if sch.Kind == "interval" && time.Duration(sch.IntervalHours)*time.Hour <= 0 {
				t.Errorf("input %d (%.40q) parsed to a non-positive interval duration (IntervalHours=%d)", i, in, sch.IntervalHours)
			}
			sch.Due(now, time.Time{})
			sch.Due(now, now.Add(-time.Minute))
		}()
	}
}
