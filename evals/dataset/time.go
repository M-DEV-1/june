package dataset

import (
	"context"
	"strconv"
	"strings"
	"time"

	"ora/internal/db"
)

const (
	HorizonDurable = "durable" // identity notes — present tense is correct
	HorizonNow     = "now"     // last few hours / working state
	HorizonToday   = "today"   // last ~day
	HorizonRecent  = "recent"  // live-thread window (2 days)
	HorizonStale   = "stale"   // older episodic memory — must be dated, not called "recent"
)

const (
	nowWindow    = 6 * time.Hour
	todayWindow  = 36 * time.Hour
	recentWindow = 48 * time.Hour
)

// Stamp fills AsOf/Horizon/AgeLabel on each question from the store, relative to LatestMemoryTime rather than wall-clock. Input: store + questions. Output: the same slice, mutated. A question with no matching row keeps its kind-based horizon (identity stays durable, current stays now).
func Stamp(ctx context.Context, store *db.Store, questions []Question) ([]Question, error) {
	clock, err := store.LatestMemoryTime(ctx)
	if err != nil {
		return questions, err
	}
	if clock.IsZero() {
		clock = time.Now()
	}
	for i := range questions {
		stampOne(ctx, store, clock, &questions[i])
	}
	return questions, nil
}

func stampOne(ctx context.Context, store *db.Store, clock time.Time, q *Question) {
	if q.Source != "" {
		if ts, err := store.MemoryAsOf(ctx, q.Source); err == nil && !ts.IsZero() {
			q.AsOf = ts
		}
	}
	q.Horizon = classifyHorizon(q.Kind, q.AsOf, clock)
	if !q.AsOf.IsZero() {
		q.AgeLabel = ageLabel(clock.Sub(q.AsOf))
	}
}

func classifyHorizon(kind string, asOf, clock time.Time) string {
	switch strings.ToLower(kind) {
	case "identity", "negative":
		return HorizonDurable
	case "current", "synthesis":
		return HorizonNow
	}
	if asOf.IsZero() || clock.IsZero() {
		if kind == "timeline" {
			return HorizonNow
		}
		return HorizonStale
	}
	age := clock.Sub(asOf)
	if age < 0 {
		age = 0
	}
	switch {
	case age <= nowWindow:
		return HorizonNow
	case age <= todayWindow:
		return HorizonToday
	case age <= recentWindow:
		return HorizonRecent
	default:
		return HorizonStale
	}
}

func ageLabel(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Hour:
		m := int(age.Minutes())
		if m < 1 {
			m = 1
		}
		return fmtMinutes(m)
	case age < 24*time.Hour:
		return fmtHours(int(age.Hours()))
	default:
		return fmtDays(int(age.Hours() / 24))
	}
}

func fmtMinutes(m int) string { return strconv.Itoa(m) + "m ago" }
func fmtHours(h int) string   { return strconv.Itoa(h) + "h ago" }
func fmtDays(d int) string    { return strconv.Itoa(d) + "d ago" }
