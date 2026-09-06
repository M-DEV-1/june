// This file holds the store's retention passes: the two tables that nothing ever deleted from and so grew without bound, conversations and act_runs. Ten thousand turns measured about 42 MB, and one conversation read pulled 32 MB into memory, which is what these passes are for. Each is a plain method the caller runs when it likes — nothing here schedules itself, and nothing here touches memory: notes, episodes, threads and the diary are all left alone.
// The rule both passes obey is that nothing the user can still see may vanish. A conversation that anything was said in is kept forever whatever its age; an act run whose steps the nightly procedures stage already wrote a "How I did X" note from is kept forever too, because the note is read back as memory and the run is the record behind it.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ora/internal/obs"
)

// EmptyConversationAge is how old an empty conversation must be before the pass will take it. A day: the window opens a conversation row before the first question is asked in it, so a shorter age could delete the one the user is about to speak in, and anything they did speak in is kept regardless of age anyway.
const EmptyConversationAge = 24 * time.Hour

// procedureNoteKind and procedureNotePrefix are how a run is recognised as one the nightly procedures stage already wrote a note from: it stores notes of this kind whose text starts with this word, the run's question, and a colon. They are copied here rather than imported because internal/dream imports this package and not the other way round; if the stage ever changes its wording, change it here too or runs that produced a note start being pruned.
const (
	procedureNoteKind   = "procedure"
	procedureNotePrefix = "How I did "
)

// runHasNote is the SQL that reports whether a procedure note was written from one act run r. The head is matched at position 1 of the note so that a note merely mentioning the same words partway through protects nothing, and both sides are lowercased so a note stored in different case still matches — SQLite's lower() folds ASCII only, so a question with non-ASCII capitals could fall through and lose its run's protection.
const runHasNote = `EXISTS (
	SELECT 1 FROM notes n
	WHERE n.kind = '` + procedureNoteKind + `'
	  AND instr(lower(trim(n.content)), lower('` + procedureNotePrefix + `' || trim(r.question) || ':')) = 1
)`

// PruneEmptyConversations deletes conversations that have no turns at all and have been sitting untouched for longer than olderThan. Input: ctx and the age a conversation must reach before it may go (EmptyConversationAge is the intended value). Output: how many rows were deleted.
// A conversation with even one turn in it is never deleted, whatever its age — what was said in the window is the user's, and no rule here may take it. An empty conversation that one of the user's own tasks was opened alongside is kept too, since the task still links to it. Both timestamps must be older than the cutoff, so a conversation touched recently by any path survives.
func (s *Store) PruneEmptyConversations(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := sqliteUTC(time.Now().Add(-olderThan))
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM conversations
		WHERE created_at < ? AND updated_at < ?
		  AND NOT EXISTS (SELECT 1 FROM conversation_turns t WHERE t.conversation_id = conversations.id)
		  AND NOT EXISTS (SELECT 1 FROM user_tasks u WHERE u.conversation_id = conversations.id)`,
		cutoff, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune empty conversations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune empty conversations: %w", err)
	}
	return n, nil
}

// PruneActRuns caps how many act runs are kept, newest first, and deletes the rest. Input: ctx, keep, the number of ordinary runs to keep (config.OraConfig.ActRunsKept is where that number comes from), and failedGrace, how long a failed run is kept regardless of the cap (config.OraConfig.FailedActRunsKeptDays is where that one comes from, in days). keep of zero or less prunes nothing at all, so an unset number can never empty the table, and a failedGrace of zero or less protects no failure at all. Output: how many rows were deleted.
// Three kinds of run are exempt from the cap and never counted against it. A run the nightly procedures stage already wrote a "How I did X" note from is kept for good, because the note says how a thing was done and this row is the only record of the steps behind it. A run that failed is kept while it is younger than failedGrace, since a failure is never written up and would otherwise be the first thing the cap took. A live job's checkpoint (a row with a job_id in a state it can still come back from, see act_jobs.go) is kept for good as well: it is the only record of a job the user can still resume, nothing rewrites the row of a job that is paused or stuck, and deleting it takes the whole trail in job_json with it. A job that reached done, stopped or failed is not exempt, because nothing ever clears job_id and exempting those kept one permanent row and its whole job_json trail per job ever run. Everything else is ordered newest first and everything past keep is deleted.
func (s *Store) PruneActRuns(ctx context.Context, keep int, failedGrace time.Duration) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM act_runs WHERE id IN (
			SELECT r.id FROM act_runs r
			-- The three finished states are finishedActJobStates in act_jobs.go; change them there and here together.
			WHERE (r.job_id = '' OR r.job_state IN ('done', 'stopped', 'failed'))
			  AND NOT (r.outcome <> 'ok' AND r.started_at >= datetime('now', '-' || ? || ' seconds'))
			  AND NOT `+runHasNote+`
			ORDER BY r.id DESC
			LIMIT -1 OFFSET ?
		)`, int64(failedGrace.Seconds()), keep)
	if err != nil {
		return 0, fmt.Errorf("prune act runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune act runs: %w", err)
	}
	return n, nil
}

// ProtectedConversations counts the conversations PruneEmptyConversations refused to take, so a caller can log what the policy held back beside what it removed. Input: ctx and the same olderThan the pass runs with. Output: how many conversations are old enough for the cutoff and kept anyway — the ones with at least one turn in them, and the empty ones one of the user's own tasks still points at. A conversation younger than the cutoff is not counted: the pass would not have reached it, so nothing protected it.
func (s *Store) ProtectedConversations(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := sqliteUTC(time.Now().Add(-olderThan))
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations c
		WHERE c.created_at < ? AND c.updated_at < ?
		  AND (EXISTS (SELECT 1 FROM conversation_turns t WHERE t.conversation_id = c.id)
		    OR EXISTS (SELECT 1 FROM user_tasks u WHERE u.conversation_id = c.id))`,
		cutoff, cutoff).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count protected conversations: %w", err)
	}
	return n, nil
}

// ProtectedActRuns counts the act runs the count cap may not touch, split by which rule protects them, so a caller can log the policy working rather than just the deletions. Input: ctx and the same failedGrace PruneActRuns runs with. Output: withNotes, how many runs a "How I did X" note was written from, and failedYoung, how many failed runs are still inside the grace. A failed run that also has a note is counted only in withNotes, so the two numbers never count the same row twice and can be added up.
func (s *Store) ProtectedActRuns(ctx context.Context, failedGrace time.Duration) (withNotes, failedYoung int64, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE `+runHasNote+`),
			COUNT(*) FILTER (WHERE NOT `+runHasNote+`
				AND r.outcome <> 'ok'
				AND r.started_at >= datetime('now', '-' || ? || ' seconds'))
		FROM act_runs r`, int64(failedGrace.Seconds())).Scan(&withNotes, &failedYoung)
	if err != nil {
		return 0, 0, fmt.Errorf("count protected act runs: %w", err)
	}
	return withNotes, failedYoung, nil
}

// CommitPruneStage marks the night's 'prune' token done so the retention passes run once a night. It lives beside the passes rather than with the other stage commits because the token is only meaningful next to the two deletes it guards; like the replay and procedures tokens there is nothing else to write, since the passes' deliverable is rows already gone rather than a row this transaction owns.
func (s *Store) CommitPruneStage(ctx context.Context, night string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.CommitPruneStage")
	defer span.End()

	return s.inTx(ctx, func(tx *sql.Tx) error {
		return markStageDone(ctx, tx, night, "prune")
	})
}
