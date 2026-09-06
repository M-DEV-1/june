// This file tests the two retention passes in prune.go against a file-backed store, since both are deletes and a delete is where the foreign keys added to the DSN start cascading.
package db

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// testFailedGrace is the failed-run grace these tests pass in, matching config.DefaultActRunFailedKeepDays. The number lives in the config now, so every test that is not about the grace itself hands over the default and gets on with what it is testing.
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

	spokenIn, err := store.CreateConversation(ctx, "what did priya ask about", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, spokenIn, "you", "what did priya ask about", "ask", nil, nil); err != nil {
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

// TestPruneEmptyConversationsLeavesTurnsAlone checks the delete cannot reach a turn by cascade: every conversation the pass touches is empty by definition, so the count of turns in the store is the same before and after.
func TestPruneEmptyConversationsLeavesTurnsAlone(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	spokenIn, err := store.CreateConversation(ctx, "when does it leave", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.AddTurn(ctx, spokenIn, "you", "when does it leave", "ask", nil, nil); err != nil {
			t.Fatalf("AddTurn: %v", err)
		}
	}
	backdateConversation(t, store, spokenIn, 90*24*time.Hour)

	empty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	backdateConversation(t, store, empty, 90*24*time.Hour)

	if _, err := store.PruneEmptyConversations(ctx, EmptyConversationAge); err != nil {
		t.Fatalf("PruneEmptyConversations: %v", err)
	}

	var turns int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_turns`).Scan(&turns); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turns != 3 {
		t.Errorf("after pruning there are %d turns, want the 3 that were said", turns)
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

// TestPruneActRunsCapCountsOnlyTheUnprotectedRuns checks the cap is a cap on what is left over: runs held by a note do not eat into it, so a store where every run produced a note keeps them all under a cap of one.
func TestPruneActRunsCapCountsOnlyTheUnprotectedRuns(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	for _, q := range []string{"first goal", "second goal", "third goal"} {
		addRun(t, store, q, "ok", 48*time.Hour)
		if _, err := store.LogNote(ctx, "How I did "+q+": looked at the screen.", "procedure"); err != nil {
			t.Fatalf("LogNote: %v", err)
		}
	}

	removed, err := store.PruneActRuns(ctx, 1, testFailedGrace)
	if err != nil {
		t.Fatalf("PruneActRuns: %v", err)
	}
	if removed != 0 {
		t.Errorf("PruneActRuns removed %d runs that all have notes, want 0", removed)
	}
	if got := len(runQuestions(t, store)); got != 3 {
		t.Errorf("%d runs left, want all 3", got)
	}
}

// TestPruneActRunsMatchesTheNoteHeadLoosely checks the note that protects a run is matched the way the procedures stage writes and finds it: on the "How I did <question>:" head, with the question trimmed and case ignored, and only at the start of the note.
func TestPruneActRunsMatchesTheNoteHeadLoosely(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	addRun(t, store, "  Open The Pricing Page  ", "ok", 48*time.Hour)
	addRun(t, store, "mentioned in passing", "ok", 48*time.Hour)
	addRun(t, store, "the newest ordinary run", "ok", time.Hour)

	if _, err := store.LogNote(ctx, "how i did open the pricing page: looked at the screen.", "procedure"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	// A procedure note that merely contains the head partway through is about a different goal and protects nothing.
	if _, err := store.LogNote(ctx, "How I did open the settings: looked at the screen, which is not How I did mentioned in passing: nothing.", "procedure"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	if _, err := store.PruneActRuns(ctx, 1, testFailedGrace); err != nil {
		t.Fatalf("PruneActRuns: %v", err)
	}

	left := runQuestions(t, store)
	if !has(left, "  Open The Pricing Page  ") {
		t.Errorf("the run its note is about was pruned; kept %v", left)
	}
	if has(left, "mentioned in passing") {
		t.Errorf("a run named only in the middle of an unrelated note was kept; kept %v", left)
	}
}

// TestPruneActRunsNonPositiveCapKeepsEverything checks the escape hatch and the guard together: a negative cap is the config's "no count cap", and a zero cap is what a caller passing an unset number would hand over. Neither may empty the table.
func TestPruneActRunsNonPositiveCapKeepsEverything(t *testing.T) {
	for _, keep := range []int{-1, 0} {
		store := newFileStore(t)
		ctx := context.Background()

		for _, q := range []string{"one", "two", "three"} {
			addRun(t, store, q, "ok", 400*24*time.Hour)
		}

		removed, err := store.PruneActRuns(ctx, keep, testFailedGrace)
		if err != nil {
			t.Fatalf("PruneActRuns(%d): %v", keep, err)
		}
		if removed != 0 {
			t.Errorf("PruneActRuns(%d) removed %d runs, want 0", keep, removed)
		}
		if got := len(runQuestions(t, store)); got != 3 {
			t.Errorf("PruneActRuns(%d) left %d runs, want all 3", keep, got)
		}
	}
}

// TestPruneActRunsTakesItsFailedGraceFromTheCaller checks the grace is the caller's number and not a constant in this package: the same forty-day-old failure survives a ninety-day grace and is taken by a seven-day one, with everything else about the two runs identical.
func TestPruneActRunsTakesItsFailedGraceFromTheCaller(t *testing.T) {
	cases := []struct {
		name  string
		grace time.Duration
		kept  bool
	}{
		{"a grace wider than the failure's age keeps it", 90 * 24 * time.Hour, true},
		{"a grace narrower than the failure's age lets the cap take it", 7 * 24 * time.Hour, false},
		{"a grace of zero protects no failure at all", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFileStore(t)
			ctx := context.Background()

			addRun(t, store, "the failure", "error", 40*24*time.Hour)
			addRun(t, store, "the newest ordinary run", "ok", time.Hour)

			if _, err := store.PruneActRuns(ctx, 1, c.grace); err != nil {
				t.Fatalf("PruneActRuns: %v", err)
			}
			if got := has(runQuestions(t, store), "the failure"); got != c.kept {
				t.Errorf("with a %v grace the forty-day-old failure kept = %v, want %v", c.grace, got, c.kept)
			}
		})
	}
}

// TestProtectedConversationsCountsWhatThePassRefusedToTake checks the number the nightly stage logs beside its deletions: conversations old enough for the cutoff that the policy holds back, which is the ones with a turn in them and the empty ones a task points at. A conversation too young to be eligible is not protected and is not counted.
func TestProtectedConversationsCountsWhatThePassRefusedToTake(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	spokenIn, err := store.CreateConversation(ctx, "what did priya ask about", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, spokenIn, "you", "what did priya ask about", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	backdateConversation(t, store, spokenIn, 48*time.Hour)

	taskedEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	backdateConversation(t, store, taskedEmpty, 48*time.Hour)
	if _, err := store.AddUserTask(ctx, "book the flight", taskedEmpty); err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}

	oldEmpty, err := store.CreateConversation(ctx, "", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	backdateConversation(t, store, oldEmpty, 48*time.Hour)

	// A young conversation with a turn in it: safe, but not held back by the policy, because the cutoff would not have reached it anyway.
	young, err := store.CreateConversation(ctx, "asked just now", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, young, "you", "asked just now", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	protected, err := store.ProtectedConversations(ctx, EmptyConversationAge)
	if err != nil {
		t.Fatalf("ProtectedConversations: %v", err)
	}
	if protected != 2 {
		t.Errorf("ProtectedConversations = %d, want 2 (the one with a turn and the one a task points at)", protected)
	}

	removed, err := store.PruneEmptyConversations(ctx, EmptyConversationAge)
	if err != nil {
		t.Fatalf("PruneEmptyConversations: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneEmptyConversations removed %d, want 1 (only the old empty one, id %d)", removed, oldEmpty)
	}
}

// TestProtectedActRunsCountsTheTwoExemptionsApart checks the two numbers the nightly stage logs beside its act run deletions: runs a "How I did X" note was written from, and failed runs still inside the grace. A run in both groups is counted only as the note's, so the two numbers can be added up without double counting.
func TestProtectedActRunsCountsTheTwoExemptionsApart(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	addRun(t, store, "open the pricing page", "ok", 365*24*time.Hour)
	if _, err := store.LogNote(ctx, "How I did open the pricing page: looked at the screen.", "procedure"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	// A failure that also carries a note, which is the one row that could be counted twice.
	addRun(t, store, "half worked", "error", time.Hour)
	if _, err := store.LogNote(ctx, "How I did half worked: looked at the screen.", "procedure"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	addRun(t, store, "click the broken button", "error", time.Hour)
	addRun(t, store, "an old failure nobody looked at", "error", 400*24*time.Hour)
	addRun(t, store, "ordinary one", "ok", time.Hour)

	withNotes, failedYoung, err := store.ProtectedActRuns(ctx, testFailedGrace)
	if err != nil {
		t.Fatalf("ProtectedActRuns: %v", err)
	}
	if withNotes != 2 {
		t.Errorf("ProtectedActRuns reported %d runs held by a note, want 2", withNotes)
	}
	if failedYoung != 1 {
		t.Errorf("ProtectedActRuns reported %d failures inside the grace, want 1 (the note's failure is counted as the note's)", failedYoung)
	}
}

// TestCommitPruneStage records the 'prune' token the way the other nightly stages record theirs, and calling it again on an already-marked night is idempotent: the token stays discoverable by the same field-membership check the dreaming loop uses to decide whether the stage still needs to run, and a second commit never errors.
func TestCommitPruneStage(t *testing.T) {
	ctx := context.Background()
	store := newFileStore(t)
	if err := store.StartDreamRun(ctx, "2026-08-30"); err != nil {
		t.Fatalf("StartDreamRun: %v", err)
	}

	run, _, _ := store.DreamRun(ctx, "2026-08-30")
	if strings.Contains(run.StagesDone, "prune") {
		t.Fatalf("stages_done = %q before any commit, want no prune token", run.StagesDone)
	}

	if err := store.CommitPruneStage(ctx, "2026-08-30"); err != nil {
		t.Fatalf("CommitPruneStage: %v", err)
	}
	run, _, _ = store.DreamRun(ctx, "2026-08-30")
	if !slices.Contains(strings.Fields(run.StagesDone), "prune") {
		t.Errorf("stages_done = %q, want it to carry the prune token", run.StagesDone)
	}

	if err := store.CommitPruneStage(ctx, "2026-08-30"); err != nil {
		t.Fatalf("second CommitPruneStage: %v", err)
	}
	run, _, _ = store.DreamRun(ctx, "2026-08-30")
	if !slices.Contains(strings.Fields(run.StagesDone), "prune") {
		t.Errorf("stages_done = %q after a second commit, still want the prune token", run.StagesDone)
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
