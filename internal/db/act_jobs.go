// This file is the checkpoint behind a long-running computer-use job (see internal/actjob): one act_runs row per job, rewritten in place after every step, so a daemon that stops mid-task can pick the job up from the last step it finished rather than starting the goal again. The job's own trail — its plan, every step with what it expected and whether that came true, the last observations and what it has spent — lives in the job_json column as the runner's own JSON, because nothing but the runner ever reads it back.
// A job row is not an act run in the "watch me once" sense: it carries outcome 'job' and a non-empty job_id, and ActRuns, SimilarActRuns and the nightly procedures pass all skip it. What a finished job leaves for that learning is the ordinary act run its own steps were recorded under, not this row.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// actJobOutcome is what a job's row carries in the act_runs outcome column, which is NOT NULL and means "ok" or "error" for every other row in the table. A job's real state is job_state; this only keeps it out of the ok-only lookups.
const actJobOutcome = "job"

// ActJobRow is one long-running computer-use job as it is stored: the id the routes address it by, the goal it was given, the brain it runs on, the state it reached, the runner's own checkpoint JSON, and — once it ends — what it said and how long it took.
type ActJobRow struct {
	ID         string
	Goal       string
	Brain      string
	State      string
	Answer     string
	Error      string
	DurationMS int64
	Checkpoint []byte
}

// SaveActJob writes a job's checkpoint, rewriting the row this job already has rather than adding one. Input: the job as it stands now, with Checkpoint holding the runner's JSON. Output: an error from the store.
// The row it rewrites is matched on the id alone, and the goal is written along with the checkpoint. Matching the goal as well meant a goal text that differed from the stored one by a character updated nothing, the insert behind it was refused by the id already on disk, and every later save failed the same way — so the job ran on with no checkpoint at all and could not be resumed. A goal that has changed is logged once, when it changes, rather than ending the checkpointing. The insert still refuses an id already on disk, so a second job that somehow takes a live job's id cannot overwrite its history.
func (s *Store) SaveActJob(ctx context.Context, job ActJobRow) error {
	if job.ID == "" {
		return fmt.Errorf("save act job: the job has no id")
	}
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT question FROM act_runs WHERE job_id = ?`, job.ID).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("save act job: %w", err)
	}
	if err == nil && stored != job.Goal {
		slog.Warn("a job's goal is not the one it was checkpointed under, checkpointing it under the new one", "job_id", job.ID, "stored", stored, "now", job.Goal)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE act_runs SET question = ?, model = ?, job_state = ?, answer = ?, error = ?, duration_ms = ?, job_json = ? WHERE job_id = ?`,
		job.Goal, job.Brain, job.State, job.Answer, job.Error, job.DurationMS, string(job.Checkpoint), job.ID)
	if err != nil {
		return fmt.Errorf("save act job: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	ins, err := s.db.ExecContext(ctx,
		`INSERT INTO act_runs (question, model, outcome, answer, error, duration_ms, steps_json, job_id, job_state, job_json)
		 SELECT ?, ?, ?, ?, ?, ?, '[]', ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM act_runs WHERE job_id = ?)`,
		job.Goal, job.Brain, actJobOutcome, job.Answer, job.Error, job.DurationMS, job.ID, job.State, string(job.Checkpoint), job.ID)
	if err != nil {
		return fmt.Errorf("save act job: %w", err)
	}
	if n, err := ins.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("save act job: %s is already the id of another job", job.ID)
	}
	return nil
}

// MaxActJobNumber is the largest number any stored job id ends in ("act-7" gives 7), which is what a starting daemon numbers its next job from so a restart never hands a new job an id an older one already holds. Input: a context. Output: the largest suffix, and zero when no job has ever been saved or none of the ids end in a number.
func (s *Store) MaxActJobNumber(ctx context.Context) (uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM act_runs WHERE job_id != ''`)
	if err != nil {
		return 0, fmt.Errorf("max act job number: %w", err)
	}
	defer rows.Close()
	var most uint64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("max act job number: %w", err)
		}
		n, err := strconv.ParseUint(id[strings.LastIndex(id, "-")+1:], 10, 64)
		if err == nil && n > most {
			most = n
		}
	}
	return most, rows.Err()
}

// StorableArgs is this package's own argument redaction (see storableArgs in act_runs.go) for a caller outside it: a long-running job checkpoints its steps through the same rule an act run's steps go through, so type_text's typed text is dropped rather than written to disk in the clear. Input: the tool's name and the arguments the model called it with. Output: a new map, or nil when the step had none.
func StorableArgs(name string, args map[string]any) map[string]any { return storableArgs(name, args) }

// RedactedValue is what a caller outside this package puts in place of a value StorableArgs would have dropped outright, for the one spot that cannot simply omit a key the way StorableArgs omits an argument: a job step's own Expect, which the UI still has to describe as some kind of check on some value. A job checkpoints Expect.Value verbatim, so when a step is type_text, or its Expect is a field_holds check, the value can just be whatever the same typed text StorableArgs already dropped from that step's own arguments was, and the caller swaps it for this marker before it reaches disk or an event.
const RedactedValue = "[redacted]"

// ActJob reads one job's row back. Input: the job id. Output: the row, or sql.ErrNoRows when nothing was ever saved under that id.
func (s *Store) ActJob(ctx context.Context, id string) (ActJobRow, error) {
	var job ActJobRow
	var checkpoint string
	err := s.db.QueryRowContext(ctx,
		`SELECT job_id, question, model, job_state, answer, error, duration_ms, job_json FROM act_runs WHERE job_id = ?`, id).
		Scan(&job.ID, &job.Goal, &job.Brain, &job.State, &job.Answer, &job.Error, &job.DurationMS, &checkpoint)
	if err != nil {
		return ActJobRow{}, err
	}
	job.Checkpoint = []byte(checkpoint)
	return job, nil
}

// finishedActJobStates are the states a job never comes back from, so a daemon starting up leaves them alone.
var finishedActJobStates = []any{"done", "stopped", "failed"}

// finishedActJobStatesSQL renders finishedActJobStates as a quoted, comma-separated SQL IN-list, for callers such as PruneActRuns that build a literal IN (...) clause rather than binding placeholders.
func finishedActJobStatesSQL() string {
	quoted := make([]string, len(finishedActJobStates))
	for i, s := range finishedActJobStates {
		quoted[i] = "'" + s.(string) + "'"
	}
	return strings.Join(quoted, ", ")
}

// UnfinishedActJobs lists the jobs that had not reached an end state when they were last checkpointed, newest first — what a daemon offers to resume after a restart. Input: none. Output: the rows with their checkpoints.
func (s *Store) UnfinishedActJobs(ctx context.Context) ([]ActJobRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id, question, model, job_state, answer, error, duration_ms, job_json FROM act_runs
		 WHERE job_id != '' AND job_state NOT IN (?, ?, ?) ORDER BY id DESC`, finishedActJobStates...)
	if err != nil {
		return nil, fmt.Errorf("unfinished act jobs: %w", err)
	}
	defer rows.Close()
	var out []ActJobRow
	for rows.Next() {
		var job ActJobRow
		var checkpoint string
		if err := rows.Scan(&job.ID, &job.Goal, &job.Brain, &job.State, &job.Answer, &job.Error, &job.DurationMS, &checkpoint); err != nil {
			return nil, fmt.Errorf("scan act job: %w", err)
		}
		job.Checkpoint = []byte(checkpoint)
		out = append(out, job)
	}
	return out, rows.Err()
}

// NoActJob reports whether an error from ActJob means there is no job under that id, rather than the store having failed. It is here so a caller outside this package can tell the two apart without importing database/sql. Input: the error. Output: true for the no-rows case.
func NoActJob(err error) bool { return errors.Is(err, sql.ErrNoRows) }
