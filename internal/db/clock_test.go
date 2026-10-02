package db_test

import (
	"testing"
	"time"

	"june/internal/db"
)

// TestDayStart_UTCInstantMapsToLocalDay checks that DayStart uses the local calendar day, not the UTC one, when the daemon's zone is set to IST — a UTC instant whose IST wall-clock time has already rolled past midnight into the next day must land on that later IST day.
func TestDayStart_UTCInstantMapsToLocalDay(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no tzdata available: " + err.Error())
	}
	old := time.Local
	time.Local = ist
	defer func() { time.Local = old }()

	// 2026-09-05 20:00 UTC is 2026-09-06 01:30 IST: past midnight, so the IST day is Sep 6 even though the UTC day is still Sep 5.
	instant := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 6, 0, 0, 0, 0, ist)
	if got := db.DayStart(instant); !got.Equal(want) {
		t.Fatalf("DayStart(%v) = %v, want %v", instant, got, want)
	}
}
