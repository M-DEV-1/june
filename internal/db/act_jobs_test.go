package db

import (
	"context"
	"testing"
)

// TestSaveActJob_WritesThenOverwritesOneRow checks a job's checkpoint is one act_runs row that later saves rewrite in place, so a job of forty steps leaves one row and not forty.
func TestSaveActJob_WritesThenOverwritesOneRow(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.SaveActJob(ctx, ActJobRow{ID: "act-1", Goal: "play S16 E8", Brain: "codex", State: "stepping", Checkpoint: []byte(`{"step":1}`)}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}
	if err := store.SaveActJob(ctx, ActJobRow{ID: "act-1", Goal: "play S16 E8", Brain: "codex", State: "done", Checkpoint: []byte(`{"step":2}`), Answer: "it is playing", DurationMS: 1200}); err != nil {
		t.Fatalf("SaveActJob again: %v", err)
	}

	got, err := store.ActJob(ctx, "act-1")
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if got.State != "done" || string(got.Checkpoint) != `{"step":2}` || got.Answer != "it is playing" {
		t.Fatalf("ActJob = %+v, want the second save's state, checkpoint and answer", got)
	}

	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM act_runs WHERE job_id = 'act-1'`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("act_runs holds %d rows for the job, want 1", rows)
	}
}

// TestMaxActJobNumber_IsTheHighestIdOnDisk checks the seed a restarted daemon numbers its next job from: the largest numeric suffix among the job ids already stored, zero when none have been.
func TestMaxActJobNumber_IsTheHighestIdOnDisk(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if n, err := store.MaxActJobNumber(ctx); err != nil || n != 0 {
		t.Fatalf("MaxActJobNumber on an empty store = %d (%v), want 0", n, err)
	}
	if _, err := store.AddActRun(ctx, ActRun{Question: "an ask", Outcome: "ok"}); err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	for _, id := range []string{"act-1", "act-7", "act-3"} {
		if err := store.SaveActJob(ctx, ActJobRow{ID: id, Goal: "goal " + id, State: "done"}); err != nil {
			t.Fatalf("SaveActJob %s: %v", id, err)
		}
	}
	n, err := store.MaxActJobNumber(ctx)
	if err != nil {
		t.Fatalf("MaxActJobNumber: %v", err)
	}
	if n != 7 {
		t.Fatalf("MaxActJobNumber = %d, want 7", n)
	}
}

// TestUnfinishedActJobs_AreTheOnesADaemonRestartShouldResume checks the listing carries the jobs still in flight and leaves out the ones that reached an end state.
func TestUnfinishedActJobs_AreTheOnesADaemonRestartShouldResume(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, j := range []ActJobRow{
		{ID: "act-1", Goal: "one", State: "done"},
		{ID: "act-2", Goal: "two", State: "stepping"},
		{ID: "act-3", Goal: "three", State: "stuck"},
		{ID: "act-4", Goal: "four", State: "failed"},
	} {
		if err := store.SaveActJob(ctx, j); err != nil {
			t.Fatalf("SaveActJob %s: %v", j.ID, err)
		}
	}

	jobs, err := store.UnfinishedActJobs(ctx)
	if err != nil {
		t.Fatalf("UnfinishedActJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("UnfinishedActJobs returned %d jobs, want 2 (stepping and stuck)", len(jobs))
	}
	if jobs[0].ID != "act-3" || jobs[1].ID != "act-2" {
		t.Fatalf("UnfinishedActJobs = %s, %s, want act-3 then act-2 (newest first)", jobs[0].ID, jobs[1].ID)
	}
}
