// This file is the token ledger: one row per model call, and the three reads over it the user's cost view needs — what each provider and model has used since an instant, the newest calls as a live log, and per-day totals per provider for a bar chart. The rows are accounting, not memory: nothing here is indexed for search or fed back to a model.
package db

import (
	"context"
	"fmt"
	"time"

	"ora/internal/util"
)

// questionRuneCap bounds how much of the asked question is kept on a token_use row. Enough to recognise a call in a log view, short enough that a pasted page cannot bloat the ledger.
const questionRuneCap = 200

// TokenUse is one model call's token count, filed so the user can see what each provider is costing them.
type TokenUse struct {
	ID           int64
	Provider     string // "codex", "gemini", "claude", "ollama", "grok"
	Model        string // the model slug the call actually used, for example "gpt-5.5"
	Channel      string // "text", "voice", "dream", "eval", "subtask"
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	// CachedTokens is how much of InputTokens the provider answered out of its own prompt cache rather than reading afresh, as the provider reported it. Part of InputTokens, not extra to it; a provider that reports none (Gemini) leaves it at zero.
	CachedTokens int
	// Rounds is how many model calls this one question took — a screen task can spend a dozen, a memory question usually one or two — since InputTokens/OutputTokens are already summed over them and without this a cheap question and an expensive one of the same total look the same.
	Rounds     int
	DurationMS int64
	Question   string // the first 200 runes of what was asked, so a row can be recognised
	At         time.Time
}

// AddTokenUse files one model call. Input: the call, with At set to when it happened — a zero At is stamped with now. Output: the new row's id, or an error from the store. The question is cut to its first 200 runes, and a TotalTokens of zero is filled in as InputTokens + OutputTokens for the providers that report only the two halves. A call whose provider reported no usage at all is stored with zeroes rather than refused: the user was still charged for it, and a row missing from the ledger is a cost they cannot see.
func (s *Store) AddTokenUse(ctx context.Context, use TokenUse) (int64, error) {
	at := use.At
	if at.IsZero() {
		at = time.Now()
	}
	total := use.TotalTokens
	if total == 0 {
		total = use.InputTokens + use.OutputTokens
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO token_use (provider, model, channel, input_tokens, output_tokens, total_tokens, cached_tokens, rounds, duration_ms, question, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		use.Provider, use.Model, use.Channel, use.InputTokens, use.OutputTokens, total, use.CachedTokens, use.Rounds, use.DurationMS,
		util.Runes(use.Question, questionRuneCap), sqliteUTC(at))
	if err != nil {
		return 0, fmt.Errorf("add token use: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add token use: %w", err)
	}
	return id, nil
}

// TokenTotal is one provider-and-model's usage over a window.
type TokenTotal struct {
	Provider     string
	Model        string
	Calls        int
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	// CachedInputTokens sums CachedTokens over the window, so a cost view can show how much of the window's input was answered from the provider's own prompt cache instead of read afresh.
	CachedInputTokens int
}

// TokenTotalsSince sums usage per provider and model since the given time, newest-heaviest first. Input: the start of the window; a call made at exactly that instant is inside it, and a zero time reaches every call ever filed. Output: one row per provider-and-model pair that was called in the window, the heaviest by total tokens first, ties broken by provider then model so the order never shifts between reads.
func (s *Store) TokenTotalsSince(ctx context.Context, since time.Time) ([]TokenTotal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, model, COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(cached_tokens)
		FROM token_use
		WHERE created_at >= ?
		GROUP BY provider, model
		ORDER BY SUM(total_tokens) DESC, provider ASC, model ASC`, sqliteUTC(since))
	if err != nil {
		return nil, fmt.Errorf("token totals since: %w", err)
	}
	defer rows.Close()

	var out []TokenTotal
	for rows.Next() {
		var t TokenTotal
		if err := rows.Scan(&t.Provider, &t.Model, &t.Calls, &t.InputTokens, &t.OutputTokens, &t.TotalTokens, &t.CachedInputTokens); err != nil {
			return nil, fmt.Errorf("scan token total: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenUseRecent returns the newest calls, most recent first, for a live log view. Input: how many to return; zero or fewer returns nothing rather than an unbounded or broken query. Output: the calls, newest first, with two calls filed in the same second ordered by which was written last.
func (s *Store) TokenUseRecent(ctx context.Context, limit int) ([]TokenUse, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider, model, channel, input_tokens, output_tokens, total_tokens, cached_tokens, rounds, duration_ms, question, created_at
		FROM token_use
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("token use recent: %w", err)
	}
	defer rows.Close()

	var out []TokenUse
	for rows.Next() {
		var u TokenUse
		if err := rows.Scan(&u.ID, &u.Provider, &u.Model, &u.Channel, &u.InputTokens, &u.OutputTokens, &u.TotalTokens, &u.CachedTokens, &u.Rounds, &u.DurationMS, &u.Question, &u.At); err != nil {
			return nil, fmt.Errorf("scan token use: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TokenDay is one calendar day's usage for one provider.
type TokenDay struct {
	Day         string // "2026-09-04" in the user's local time
	Provider    string
	TotalTokens int
	Calls       int
}

// TokenDaysBack returns per-day, per-provider totals for the last n days including today, oldest day first. Input: how many days the window covers, counting today as one — 1 is today alone, 7 is today and the six days before it; zero or fewer returns nothing rather than a window running backwards. Output: one row per day and provider that had a call, oldest day first and providers in name order within a day. Days are the user's own calendar days: times are stored in UTC and converted to local before the grouping, so a call made at half past midnight belongs to the day the user would say it did.
func (s *Store) TokenDaysBack(ctx context.Context, days int) ([]TokenDay, error) {
	if days <= 0 {
		return nil, nil
	}
	now := time.Now()
	start := DayStart(now).AddDate(0, 0, -(days - 1))

	rows, err := s.db.QueryContext(ctx, `
		SELECT date(created_at, 'localtime') AS day, provider, SUM(total_tokens), COUNT(*)
		FROM token_use
		WHERE created_at >= ?
		GROUP BY day, provider
		ORDER BY day ASC, provider ASC`, sqliteUTC(start))
	if err != nil {
		return nil, fmt.Errorf("token days back: %w", err)
	}
	defer rows.Close()

	var out []TokenDay
	for rows.Next() {
		var d TokenDay
		if err := rows.Scan(&d.Day, &d.Provider, &d.TotalTokens, &d.Calls); err != nil {
			return nil, fmt.Errorf("scan token day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ModelDay is one calendar day's call count for one provider-and-model pair.
type ModelDay struct {
	Day         string // "2026-09-04" in the user's local time
	Provider    string
	Model       string
	Calls       int
	TotalTokens int
}

// ModelCallsBack returns per-day, per-provider-and-model call counts and token totals for the last n days including today, oldest day first. Input: how many days the window covers, counting today as one — 1 is today alone, 7 is today and the six days before it; zero or fewer returns nothing rather than a window running backwards. Output: one row per day, provider and model that had a call, oldest day first and providers then models in name order within a day. Days are the user's own calendar days: times are stored in UTC and converted to local before the grouping, so a call made at half past midnight belongs to the day the user would say it did. This is the read a per-model daily request cap is checked against, since a metered free tier's limit bites per model, not per provider.
func (s *Store) ModelCallsBack(ctx context.Context, days int) ([]ModelDay, error) {
	if days <= 0 {
		return nil, nil
	}
	now := time.Now()
	start := DayStart(now).AddDate(0, 0, -(days - 1))

	rows, err := s.db.QueryContext(ctx, `
		SELECT date(created_at, 'localtime') AS day, provider, model, COUNT(*), SUM(total_tokens)
		FROM token_use
		WHERE created_at >= ?
		GROUP BY day, provider, model
		ORDER BY day ASC, provider ASC, model ASC`, sqliteUTC(start))
	if err != nil {
		return nil, fmt.Errorf("model calls back: %w", err)
	}
	defer rows.Close()

	var out []ModelDay
	for rows.Next() {
		var d ModelDay
		if err := rows.Scan(&d.Day, &d.Provider, &d.Model, &d.Calls, &d.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan model day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ModelCallsOn returns one local calendar day's per-provider-and-model call counts and token totals, heaviest call count first, ties broken by provider then model. Input: any instant within the local day to report on — only its local year, month and day are used. Output: one row per provider-and-model pair called that day, the model that spent the most calls first, so this is the read that answers which job spent today's allowance.
func (s *Store) ModelCallsOn(ctx context.Context, day time.Time) ([]ModelDay, error) {
	start := DayStart(day)
	end := start.AddDate(0, 0, 1)

	rows, err := s.db.QueryContext(ctx, `
		SELECT date(created_at, 'localtime') AS day, provider, model, COUNT(*), SUM(total_tokens)
		FROM token_use
		WHERE created_at >= ? AND created_at < ?
		GROUP BY day, provider, model
		ORDER BY COUNT(*) DESC, provider ASC, model ASC`, sqliteUTC(start), sqliteUTC(end))
	if err != nil {
		return nil, fmt.Errorf("model calls on: %w", err)
	}
	defer rows.Close()

	var out []ModelDay
	for rows.Next() {
		var d ModelDay
		if err := rows.Scan(&d.Day, &d.Provider, &d.Model, &d.Calls, &d.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan model day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
