package db

import (
	"context"
	"fmt"
	"time"

	"june/internal/obs"
)

// Fold is one branch() subtask's result that couldn't be delivered into the
// live session that requested it (the session ended first), staged here to
// surface at the start of the next session instead. Separate table from
// notes — a fold is a one-off task result, not a durable user fact.
type Fold struct {
	ID        int64
	Task      string
	Result    string
	CreatedAt time.Time
}

// SaveFold persists a branch() result that couldn't be delivered live.
func (s *Store) SaveFold(ctx context.Context, task, result string) (int64, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.SaveFold")
	defer span.End()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO folds (task, result) VALUES (?, ?)`,
		task, result)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert fold: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("read fold id: %w", err)
	}
	return id, nil
}

// UnconsumedFolds returns every fold not yet surfaced to the user, oldest first.
func (s *Store) UnconsumedFolds(ctx context.Context) ([]Fold, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.UnconsumedFolds")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task, result, created_at FROM folds WHERE consumed_at IS NULL ORDER BY id ASC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query unconsumed folds: %w", err)
	}
	defer rows.Close()

	var folds []Fold
	for rows.Next() {
		var f Fold
		if err := rows.Scan(&f.ID, &f.Task, &f.Result, &f.CreatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan fold: %w", err)
		}
		folds = append(folds, f)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate folds: %w", err)
	}
	return folds, nil
}

// ConsumeFold marks a fold as surfaced, so it doesn't repeat at the next session's handshake.
func (s *Store) ConsumeFold(ctx context.Context, id int64) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.ConsumeFold")
	defer span.End()

	if _, err := s.db.ExecContext(ctx,
		`UPDATE folds SET consumed_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		span.RecordError(err)
		return fmt.Errorf("consume fold: %w", err)
	}
	return nil
}
