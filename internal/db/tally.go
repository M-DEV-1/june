package db

// This file is the self-accounting counters' storage: one upsert-on-conflict
// table (see the tally CREATE TABLE in db.go) that both brain-call counting
// (internal/tally.Wrap, through RecordTally) and the hybrid-search fusion
// counter (recordVectorContribution in hybrid.go) write into, keyed by local
// calendar day and provider name. internal/tally reads it back for the weekly
// system log.

import (
	"context"
	"fmt"
	"time"
)

// tallyDayFormat is the local calendar-day key tally rows are keyed by, same shape as the diary/dream_runs day keys elsewhere in this package.
const tallyDayFormat = "2006-01-02"

// TallyRow is one day's counters for one provider.
type TallyRow struct {
	Day      string
	Provider string
	Calls    int
	Failures int
	TotalMs  int64
}

// RecordTally upserts the outcome of one brain call into today's (local) row for provider: one call, one failure if ok is false, and ms added to the running latency total. Satisfies internal/tally.Recorder structurally, so a *Store can be handed straight to tally.Wrap.
func (s *Store) RecordTally(provider string, ok bool, ms time.Duration) error {
	failures := 0
	if !ok {
		failures = 1
	}
	return s.bumpTally(time.Now().Format(tallyDayFormat), provider, 1, failures, ms.Milliseconds())
}

// bumpTally adds calls/failures/ms to provider's row for day, creating the row if it doesn't exist yet. Shared by RecordTally (brain calls) and hybrid.go's recordVectorContribution (vector-search fusion counting).
func (s *Store) bumpTally(day, provider string, calls, failures int, ms int64) error {
	if _, err := s.db.Exec(
		`INSERT INTO tally (day, provider, calls, failures, total_ms) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(day, provider) DO UPDATE SET
			calls = calls + excluded.calls,
			failures = failures + excluded.failures,
			total_ms = total_ms + excluded.total_ms`,
		day, provider, calls, failures, ms); err != nil {
		return fmt.Errorf("bump tally: %w", err)
	}
	return nil
}

// TallyRowsSince returns every tally row whose day is on or after since (compared as a local 'YYYY-MM-DD' string), for the weekly system log to summarize.
func (s *Store) TallyRowsSince(ctx context.Context, since time.Time) ([]TallyRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, provider, calls, failures, total_ms FROM tally WHERE day >= ? ORDER BY day, provider`,
		since.Format(tallyDayFormat))
	if err != nil {
		return nil, fmt.Errorf("query tally rows since: %w", err)
	}
	defer rows.Close()

	var out []TallyRow
	for rows.Next() {
		var r TallyRow
		if err := rows.Scan(&r.Day, &r.Provider, &r.Calls, &r.Failures, &r.TotalMs); err != nil {
			return nil, fmt.Errorf("scan tally row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tally rows: %w", err)
	}
	return out, nil
}
