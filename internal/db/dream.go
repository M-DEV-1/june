package db

import (
	"context"
	"database/sql"
	"fmt"

	"june/internal/obs"
)

// This file is the overnight dreaming loop's storage: the per-night run bookkeeping and the hypotheses table. Both are private working state with no FTS mirror — a deliberate anti-pollution decision, so an unvetted guess can never be retrieved into a prompt. Each stage of a night commits through one method here, in one transaction that writes the stage's outputs and its stages_done token together, which is what makes a preempted stage leave nothing behind.

// The stages_done tokens for the five dream stages, in the order a night runs them. internal/dream reads these same constants to decide which stages a night has already committed, so the token spelled here and the token checked there can never drift apart.
const (
	StageHyp        = "hyp"
	StageUnd        = "und"
	StageCompact    = "compact"
	StageReplay     = "replay"
	StageProcedures = "procedures"
	StageLessons    = "lessons"
)

// DreamRun is one night's bookkeeping row: which stages have committed, the one-line report, and whether the night finished.
type DreamRun struct {
	Night      string
	StagesDone string
	Report     string
	Finished   bool
}

// Hypothesis is one private guess about the user, tracked across nights until it is promoted into the understanding doc or retired. Born and LastTested are local 'YYYY-MM-DD' night keys; Evidence accumulates one appended line per test.
type Hypothesis struct {
	ID          int64
	Statement   string
	Confidence  string
	Status      string
	Born        string
	LastTested  string
	TimesTested int
	Evidence    string
	Reason      string
}

// HypothesisVerdict is the fully decided outcome of one hypothesis for one night, mechanics already applied by the runner: the new confidence, status and reason, the evidence line to append, and whether tonight counted as a test (which bumps times_tested and stamps last_tested).
type HypothesisVerdict struct {
	ID           int64
	Confidence   string
	Status       string
	LastTested   string
	EvidenceLine string
	Reason       string
	Tested       bool
}

// NewHypothesis is one statement the nightly extraction adopted for future testing.
type NewHypothesis struct {
	Statement  string
	Confidence string
}

// openHypothesesCap bounds how many open hypotheses one night works on, so the judge prompt and the write volume stay small no matter how enthusiastic past extraction was.
const openHypothesesCap = 20

// execer is the least of *sql.DB and *sql.Tx that the write helpers need, so each can run standalone or inside a stage transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// StartDreamRun creates the night's dream_runs row if none exists. The night PRIMARY KEY is the single-run-per-night guarantee: starting an already-started night is a no-op and the runner resumes whatever stages are missing.
func (s *Store) StartDreamRun(ctx context.Context, night string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dream_runs (night) VALUES (?) ON CONFLICT(night) DO NOTHING`, night); err != nil {
		return fmt.Errorf("start dream run: %w", err)
	}
	return nil
}

// DreamRun returns the night's run row, with ok false when that night has never started.
func (s *Store) DreamRun(ctx context.Context, night string) (run DreamRun, ok bool, err error) {
	run.Night = night
	err = s.db.QueryRowContext(ctx,
		`SELECT stages_done, report, finished_at IS NOT NULL FROM dream_runs WHERE night = ?`, night).
		Scan(&run.StagesDone, &run.Report, &run.Finished)
	if err == sql.ErrNoRows {
		return run, false, nil
	}
	if err != nil {
		return run, false, fmt.Errorf("read dream run: %w", err)
	}
	return run, true, nil
}

// DreamRunsSince returns the dream_runs rows whose night is on or after sinceNight (a local 'YYYY-MM-DD' string), oldest first — the weekly system log's window into how many nights actually ran and how far each got.
func (s *Store) DreamRunsSince(ctx context.Context, sinceNight string) ([]DreamRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT night, stages_done, report, finished_at IS NOT NULL FROM dream_runs WHERE night >= ? ORDER BY night`, sinceNight)
	if err != nil {
		return nil, fmt.Errorf("query dream runs since: %w", err)
	}
	defer rows.Close()

	var out []DreamRun
	for rows.Next() {
		var r DreamRun
		if err := rows.Scan(&r.Night, &r.StagesDone, &r.Report, &r.Finished); err != nil {
			return nil, fmt.Errorf("scan dream run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dream runs: %w", err)
	}
	return out, nil
}

// OpenHypotheses returns up to openHypothesesCap status='open' hypotheses, oldest born first so long-standing guesses keep getting their nights in front of the judge.
func (s *Store) OpenHypotheses(ctx context.Context) ([]Hypothesis, error) {
	return s.queryHypotheses(ctx,
		`SELECT id, statement, confidence, status, born, last_tested, times_tested, evidence, reason
		 FROM hypotheses WHERE status = 'open' ORDER BY born ASC, id ASC LIMIT ?`, openHypothesesCap)
}

// StrongHypotheses returns the promoted hypotheses plus the open ones already judged high-confidence — the set the understanding rewrite is allowed to lean on.
func (s *Store) StrongHypotheses(ctx context.Context) ([]Hypothesis, error) {
	return s.queryHypotheses(ctx,
		`SELECT id, statement, confidence, status, born, last_tested, times_tested, evidence, reason
		 FROM hypotheses WHERE status = 'promoted' OR (status = 'open' AND confidence = 'high')
		 ORDER BY born ASC, id ASC LIMIT ?`, openHypothesesCap)
}

func (s *Store) queryHypotheses(ctx context.Context, query string, args ...any) ([]Hypothesis, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query hypotheses: %w", err)
	}
	defer rows.Close()

	var out []Hypothesis
	for rows.Next() {
		var h Hypothesis
		var lastTested sql.NullString
		if err := rows.Scan(&h.ID, &h.Statement, &h.Confidence, &h.Status, &h.Born, &lastTested, &h.TimesTested, &h.Evidence, &h.Reason); err != nil {
			return nil, fmt.Errorf("scan hypothesis: %w", err)
		}
		h.LastTested = lastTested.String
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hypotheses: %w", err)
	}
	return out, nil
}

// InsertHypothesis adds a new open hypothesis born on the given night. A statement already on file is left untouched — the UNIQUE index makes re-extraction of the same idea across nights idempotent.
func (s *Store) InsertHypothesis(ctx context.Context, statement, confidence, born string) error {
	return insertHypothesis(ctx, s.db, statement, confidence, born)
}

func insertHypothesis(ctx context.Context, e execer, statement, confidence, born string) error {
	if _, err := e.ExecContext(ctx,
		`INSERT INTO hypotheses (statement, confidence, born) VALUES (?, ?, ?) ON CONFLICT(statement) DO NOTHING`,
		statement, confidence, born); err != nil {
		return fmt.Errorf("insert hypothesis: %w", err)
	}
	return nil
}

// ApplyHypothesisVerdict writes one decided verdict onto its hypothesis row: confidence, status and reason are replaced, the evidence line (when non-empty) is appended, and a tested verdict bumps times_tested and stamps last_tested.
func (s *Store) ApplyHypothesisVerdict(ctx context.Context, v HypothesisVerdict) error {
	return applyHypothesisVerdict(ctx, s.db, v)
}

func applyHypothesisVerdict(ctx context.Context, e execer, v HypothesisVerdict) error {
	bump := 0
	if v.Tested {
		bump = 1
	}
	if _, err := e.ExecContext(ctx, `
		UPDATE hypotheses SET
			confidence = ?, status = ?, reason = ?,
			times_tested = times_tested + ?,
			last_tested = CASE WHEN ? > 0 THEN ? ELSE last_tested END,
			evidence = CASE WHEN ? = '' THEN evidence WHEN evidence = '' THEN ? ELSE evidence || char(10) || ? END
		WHERE id = ?`,
		v.Confidence, v.Status, v.Reason, bump, bump, v.LastTested,
		v.EvidenceLine, v.EvidenceLine, v.EvidenceLine, v.ID); err != nil {
		return fmt.Errorf("apply hypothesis verdict: %w", err)
	}
	return nil
}

// markStageDone appends the stage's token to the night's stages_done inside the caller's transaction.
func markStageDone(ctx context.Context, e execer, night, token string) error {
	if _, err := e.ExecContext(ctx,
		`UPDATE dream_runs SET stages_done = TRIM(stages_done || ' ' || ?) WHERE night = ?`, token, night); err != nil {
		return fmt.Errorf("mark dream stage done: %w", err)
	}
	return nil
}

// CommitHypothesisStage writes one night's hypothesis work in a single transaction: every verdict, every adopted new hypothesis, and the 'hyp' token in stages_done. All or nothing — a cancelled stage leaves no partial verdicts, and the token's absence is what tells the next wake to redo the stage.
func (s *Store) CommitHypothesisStage(ctx context.Context, night string, verdicts []HypothesisVerdict, adopted []NewHypothesis) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.CommitHypothesisStage")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, v := range verdicts {
			if err := applyHypothesisVerdict(ctx, tx, v); err != nil {
				return err
			}
		}
		for _, a := range adopted {
			if err := insertHypothesis(ctx, tx, a.Statement, a.Confidence, night); err != nil {
				return err
			}
		}
		return markStageDone(ctx, tx, night, StageHyp)
	})
}

// CommitUnderstandingStage upserts the rewritten understanding doc and the 'und' token in one transaction.
func (s *Store) CommitUnderstandingStage(ctx context.Context, night, understanding string) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.CommitUnderstandingStage")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := upsertDiary(ctx, tx, "", "understanding", understanding); err != nil {
			return err
		}
		return markStageDone(ctx, tx, night, StageUnd)
	})
}

// DiaryCompaction is one coarse diary entry summarising a run of finer ones: the (day, kind) to upsert with its content, and the finer-kind constituent days to reparent under it in the same transaction.
type DiaryCompaction struct {
	Day             string
	Kind            string
	Content         string
	ConstituentKind string
	ConstituentDays []string
}

// CommitCompactStage writes one tier of the night's diary compaction in a single transaction: every coarse entry upserted, its constituents reparented under it, and — when done is set — the 'compact' token in stages_done. The runner calls this once per tier and sets done only on the last call, so the token lands exactly once; a night with nothing to compact is one call with no compactions that still commits the token.
// The constituents are kept, not deleted. The coarse entry is a model rewrite of seven day pages and there is no other copy of what those days said, so this follows ReplaceSummariesWithDigest and ReplaceAllNotes in keeping the source of a compaction. DiaryEntriesThrough skips a reparented row, which is what stops the next night rolling the same week up again.
func (s *Store) CommitCompactStage(ctx context.Context, night string, comps []DiaryCompaction, done bool) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.CommitCompactStage")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, c := range comps {
			if err := upsertDiary(ctx, tx, c.Day, c.Kind, c.Content); err != nil {
				return err
			}
			var coarseID int64
			if err := tx.QueryRowContext(ctx,
				`SELECT id FROM diary WHERE day = ? AND kind = ?`, c.Day, c.Kind).Scan(&coarseID); err != nil {
				return fmt.Errorf("read compacted diary parent: %w", err)
			}
			for _, day := range c.ConstituentDays {
				if _, err := tx.ExecContext(ctx,
					`UPDATE diary SET parent_id = ? WHERE kind = ? AND day = ?`, coarseID, c.ConstituentKind, day); err != nil {
					return fmt.Errorf("reparent compacted diary row: %w", err)
				}
			}
		}
		if done {
			return markStageDone(ctx, tx, night, StageCompact)
		}
		return nil
	})
}

// CommitReplayStage marks the night's 'replay' token done in its own transaction. The replay stage's deliverable is a markdown artifact on disk, not a database row, so unlike the other stages there is nothing else to write here — the token alone is what tells the next wake the night's replay (full or partial) is not to be redone.
func (s *Store) CommitReplayStage(ctx context.Context, night string) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.CommitReplayStage")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		return markStageDone(ctx, tx, night, StageReplay)
	})
}

// CommitProceduresStage marks the night's procedures token done so the stage runs once a night. The procedures stage's deliverable is the "How I did X" notes it writes directly through LogNote, not a row this transaction owns, so like CommitReplayStage there is nothing else to write here — the token alone is what tells the next wake the night's procedures stage is not to be redone.
func (s *Store) CommitProceduresStage(ctx context.Context, night string) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.CommitProceduresStage")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		return markStageDone(ctx, tx, night, StageProcedures)
	})
}

// FinishDreamRun closes the night in one transaction: the morning report becomes the diary kind='dream' row (FTS-indexed via the diary triggers on purpose — "what did you dream last night" must find it), and the run row gets its one-line report and finished_at stamp.
func (s *Store) FinishDreamRun(ctx context.Context, night, entry, line string) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.FinishDreamRun")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := upsertDiary(ctx, tx, night, "dream", entry); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE dream_runs SET finished_at = CURRENT_TIMESTAMP, report = ? WHERE night = ?`, line, night); err != nil {
			return fmt.Errorf("finish dream run: %w", err)
		}
		return nil
	})
}

// inTx runs fn inside one transaction, rolling back on any error — a cancelled ctx aborts the transaction before commit, which is the dreaming loop's preemption guarantee.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dream tx: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dream tx: %w", err)
	}
	return nil
}
