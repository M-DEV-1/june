package dataset

import (
	"testing"
	"time"
)

func TestClassifyHorizon_RelativeToClock(t *testing.T) {
	clock := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		kind string
		age  time.Duration
		want string
	}{
		{"thread", 2 * time.Hour, HorizonNow},
		{"thread", 20 * time.Hour, HorizonToday},
		{"thread", 40 * time.Hour, HorizonRecent},
		{"thread", 8 * 24 * time.Hour, HorizonStale},
		{"identity", 60 * 24 * time.Hour, HorizonDurable},
		{"negative", 20 * 24 * time.Hour, HorizonDurable},
		{"current", 20 * 24 * time.Hour, HorizonNow},
		{"synthesis", time.Hour, HorizonNow},
	}
	for _, tc := range cases {
		got := classifyHorizon(tc.kind, clock.Add(-tc.age), clock)
		if got != tc.want {
			t.Errorf("%s age=%s: got %s want %s", tc.kind, tc.age, got, tc.want)
		}
	}
}

func TestAgeLabel(t *testing.T) {
	if got := ageLabel(3 * time.Minute); got != "3m ago" {
		t.Errorf("minutes: %s", got)
	}
	if got := ageLabel(5 * time.Hour); got != "5h ago" {
		t.Errorf("hours: %s", got)
	}
	if got := ageLabel(8 * 24 * time.Hour); got != "8d ago" {
		t.Errorf("days: %s", got)
	}
}
