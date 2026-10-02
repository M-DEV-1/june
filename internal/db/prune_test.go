// This file tests the two retention passes in prune.go against a file-backed store, since both are deletes and a delete is where the foreign keys added to the DSN start cascading.
package db

import (
	"context"

	"testing"
	"time"
)

// testFailedGrace is the failed-run grace these tests pass in, matching config.DefaultActRunFailedKeepDays. The number lives in the config, so every test hands over the default.
const testFailedGrace = 30 * 24 * time.Hour

// backdateConversation moves one conversation's two timestamps into the past, which is the only way to test an age rule without waiting.
func backdateConversation(t *testing.T, s *Store, id int64, age time.Duration) {
	t.Helper()
	when := sqliteUTC(time.Now().Add(-age))
	if _, err := s.db.Exec(`UPDATE conversations SET created_at = ?, updated_at = ? WHERE id = ?`, when, when, id); err != nil {
		t.Fatalf("backdate conversation %d: %v", id, err)
	}
}

// conversationIDs returns the ids still in the conversations table, oldest first.
func conversationIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM conversations ORDER BY id`)
	if err != nil {
		t.Fatalf("read conversations: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan conversation id: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// TestPruneEmptyConversationsKeepsAnythingSaid is the whole of the conversation policy in one test: a conversation with a turn in it is never pruned however old it is, an empty one is pruned only once it is older than the cutoff, and an empty one a task points at stays because the task's link to it is something the user can still see.
func TestPruneEmptyConversationsKeepsAnythingSaid(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	spokenIn, err := store.CreateConversation(ctx, "what did vexil ask about", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, spokenIn, "you", "what did vexil ask about", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	backdateConversation(t, store, spokenIn, 30*24*time.Hour)

	oldEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	backdateConversation(t, store, oldEmpty, 48*time.Hour)

	freshEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	backdateConversation(t, store, freshEmpty, 12*time.Hour)

	taskedEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	backdateConversation(t, store, taskedEmpty, 48*time.Hour)
	if _, err := store.AddUserTask(ctx, "book the flight", taskedEmpty); err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}

	removed, err := store.PruneEmptyConversations(ctx, EmptyConversationAge)
	if err != nil {
		t.Fatalf("PruneEmptyConversations: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneEmptyConversations removed %d conversations, want 1", removed)
	}

	left := conversationIDs(t, store)
	want := map[int64]string{spokenIn: "a conversation with a turn in it", freshEmpty: "an empty conversation not yet as old as the cutoff", taskedEmpty: "an empty conversation a task points at"}
	for id, why := range want {
		found := false
		for _, got := range left {
			if got == id {
				found = true
			}
		}
		if !found {
			t.Errorf("%s (id %d) was pruned", why, id)
		}
	}
	for _, id := range left {
		if id == oldEmpty {
			t.Errorf("the empty conversation older than the cutoff (id %d) was kept", id)
		}
	}
}

// addRun writes one act run with a chosen question, outcome and age, and returns its id. It writes the row directly rather than through AddActRun so the test can set started_at, which AddActRun leaves to the database.
func addRun(t *testing.T, s *Store, question, outcome string, age time.Duration) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO act_runs (started_at, question, model, outcome, answer, error, duration_ms, steps_json) VALUES (?, ?, 'sonnet', ?, '', '', 10, '[]')`,
		sqliteUTC(time.Now().Add(-age)), question, outcome)
	if err != nil {
		t.Fatalf("insert act run %q: %v", question, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("act run id: %v", err)
	}
	return id
}

// runQuestions returns the questions of the act runs still stored, newest first.
func runQuestions(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT question FROM act_runs ORDER BY id DESC`)
	if err != nil {
		t.Fatalf("read act runs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan question: %v", err)
		}
		out = append(out, q)
	}
	return out
}

// has reports whether questions contains want.
func has(questions []string, want string) bool {
	for _, q := range questions {
		if q == want {
			return true
		}
	}
	return false
}

// TestPruneActRunsKeepsWhatWasLearnedFrom is the act run policy in one test: a run the nightly procedures stage already wrote a note from is kept however old it is, a failed run is kept while it is still inside the grace window, and the rest are capped by count, newest kept.
func TestPruneActRunsKeepsWhatWasLearnedFrom(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	// The oldest run of all, but its steps became "How I did open the pricing page:" on some past night, so it is the record behind a note the user can read.
	addRun(t, store, "open the pricing page", "ok", 365*24*time.Hour)
	if _, err := store.LogNote(ctx, "How I did open the pricing page: looked at the screen, clicked item 3 (Pricing).", "procedure"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	addRun(t, store, "click the broken button", "error", time.Hour)
	addRun(t, store, "an old failure nobody looked at", "error", 400*24*time.Hour)

	// Four ordinary runs nothing was written from, newest last.
	for _, q := range []string{"ordinary one", "ordinary two", "ordinary three", "ordinary four"} {
		addRun(t, store, q, "ok", time.Hour)
	}

	removed, err := store.PruneActRuns(ctx, 2, testFailedGrace)
	if err != nil {
		t.Fatalf("PruneActRuns: %v", err)
	}
	if removed != 3 {
		t.Errorf("PruneActRuns removed %d runs, want 3 (two ordinary runs over the cap and one stale failure)", removed)
	}

	left := runQuestions(t, store)
	for _, want := range []string{"open the pricing page", "click the broken button", "ordinary three", "ordinary four"} {
		if !has(left, want) {
			t.Errorf("run %q was pruned; kept %v", want, left)
		}
	}
	for _, gone := range []string{"ordinary one", "ordinary two", "an old failure nobody looked at"} {
		if has(left, gone) {
			t.Errorf("run %q should have been pruned; kept %v", gone, left)
		}
	}
}

// TestPruneActRunsKeepsALiveJobsCheckpoint pins the third exemption: a long-running computer-use job's checkpoint is an act_runs row like any other, but it is the only record of a job the user can still resume, and nothing ever rewrites the row of a job that is paused or stuck. It is neither held by a note nor a young failure, so the count cap used to delete it and take the whole trail in job_json with it.
func TestPruneActRunsKeepsALiveJobsCheckpoint(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	if err := store.SaveActJob(ctx, ActJobRow{ID: "act-1", Goal: "play the next episode", Brain: "codex", State: "stepping", Checkpoint: []byte(`{"step":7}`)}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE act_runs SET started_at = ? WHERE job_id = 'act-1'`, sqliteUTC(time.Now().Add(-400*24*time.Hour))); err != nil {
		t.Fatalf("backdate the job row: %v", err)
	}
	for _, q := range []string{"ordinary one", "ordinary two", "ordinary three"} {
		addRun(t, store, q, "ok", time.Hour)
	}

	removed, err := store.PruneActRuns(ctx, 2, testFailedGrace)
	if err != nil {
		t.Fatalf("PruneActRuns: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneActRuns removed %d runs, want 1 (the oldest ordinary run, with the job's checkpoint exempt and not counted against the cap)", removed)
	}
	if left := runQuestions(t, store); !has(left, "play the next episode") {
		t.Errorf("the job's checkpoint was pruned; kept %v", left)
	}
	unfinished, err := store.UnfinishedActJobs(ctx)
	if err != nil {
		t.Fatalf("UnfinishedActJobs: %v", err)
	}
	if len(unfinished) != 1 {
		t.Errorf("%d unfinished jobs after the prune, want the one job still resumable", len(unfinished))
	}
}

// TestPruneToolCallsRemovesOnlyRowsOlderThanTheWindow writes one old and one fresh tool call and checks the pass takes only the old one, and that a non-positive window prunes nothing at all.
func TestPruneToolCallsRemovesOnlyRowsOlderThanTheWindow(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	oldID, err := store.AddToolCall(ctx, ToolCall{Path: "ask", Name: "look", Outcome: "ok"})
	if err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE tool_calls SET created_at = ? WHERE id = ?`, sqliteUTC(time.Now().Add(-60*24*time.Hour)), oldID); err != nil {
		t.Fatalf("backdate tool call: %v", err)
	}
	freshID, err := store.AddToolCall(ctx, ToolCall{Path: "ask", Name: "look", Outcome: "ok"})
	if err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}

	removed, err := store.PruneToolCalls(ctx, 0)
	if err != nil {
		t.Fatalf("PruneToolCalls(0): %v", err)
	}
	if removed != 0 {
		t.Errorf("PruneToolCalls with a non-positive window removed %d rows, want 0", removed)
	}

	removed, err = store.PruneToolCalls(ctx, testFailedGrace)
	if err != nil {
		t.Fatalf("PruneToolCalls: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneToolCalls removed %d rows, want 1", removed)
	}

	rows, err := store.ToolCallsSince(ctx, time.Now().Add(-365*24*time.Hour))
	if err != nil {
		t.Fatalf("ToolCallsSince: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != freshID {
		t.Errorf("tool calls left after pruning = %+v, want only the fresh row %d", rows, freshID)
	}
}
