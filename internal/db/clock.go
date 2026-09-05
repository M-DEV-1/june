// clock.go holds the one day-boundary rule every package that needs to know "today" shares: the daemon's local zone, because every day boundary the user sees is his wall clock, never UTC.
package db

import "time"

// DayStart returns midnight of t's calendar day in the daemon's local zone. Input: any instant, in any zone. Output: midnight of that instant's local calendar day, in time.Local.
func DayStart(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}
