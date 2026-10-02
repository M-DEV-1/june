package db

import (
	"context"
	"database/sql"
	"testing"
)

// CommitCompactStage is one transaction per tier: the coarse entry lands, its constituents (and only they) are reparented under it rather than destroyed, and the 'compact' token commits only on the call that says the stage is done.
func TestCommitCompactStage(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.StartDreamRun(ctx, "2026-08-30"); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-08-03", "2026-08-04", "2026-08-05"} {
		if err := store.SetDiaryEntry(ctx, day, "day", "A daily for "+day+"."); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetDiaryEntry(ctx, "2026-08-04", "dream", "A morning report."); err != nil {
		t.Fatal(err)
	}

	comps := []DiaryCompaction{{Day: "2026-08-03", Kind: "week", Content: "A compacted week.", ConstituentKind: "day", ConstituentDays: []string{"2026-08-03", "2026-08-04"}}}
	if err := store.CommitCompactStage(ctx, "2026-08-30", comps, false); err != nil {
		t.Fatalf("CommitCompactStage: %v", err)
	}
	if got, _ := store.DiaryEntry(ctx, "2026-08-03", "week"); got != "A compacted week." {
		t.Errorf("week entry = %q", got)
	}
	// The constituents are kept and reparented under the coarse entry, the way ReplaceSummariesWithDigest keeps its summaries: the week paragraph is a model rewrite of them and there is no other copy of what the days said.
	var weekID int64
	if err := store.DB().QueryRow(`SELECT id FROM diary WHERE day = ? AND kind = 'week'`, "2026-08-03").Scan(&weekID); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-08-03", "2026-08-04"} {
		if got, _ := store.DiaryEntry(ctx, day, "day"); got == "" {
			t.Errorf("constituent %s was destroyed by its own compaction", day)
		}
		var parent sql.NullInt64
		if err := store.DB().QueryRow(`SELECT parent_id FROM diary WHERE day = ? AND kind = 'day'`, day).Scan(&parent); err != nil {
			t.Fatal(err)
		}
		if !parent.Valid || parent.Int64 != weekID {
			t.Errorf("constituent %s parent_id = %v, want the week row %d", day, parent, weekID)
		}
	}
	for _, row := range []struct{ day, kind string }{{"2026-08-05", "day"}, {"2026-08-04", "dream"}} {
		var parent sql.NullInt64
		if err := store.DB().QueryRow(`SELECT parent_id FROM diary WHERE day = ? AND kind = ?`, row.day, row.kind).Scan(&parent); err != nil {
			t.Fatal(err)
		}
		if parent.Valid {
			t.Errorf("the reparent must match kind as well as day, but %s/%s was adopted", row.day, row.kind)
		}
	}
	run, _, _ := store.DreamRun(ctx, "2026-08-30")
	if run.StagesDone != "" {
		t.Errorf("stages_done = %q before the done call", run.StagesDone)
	}
	// Nothing was deleted, so every diary row is still searchable: the three dailies, the dream report and the new week.
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE source = 'diary'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("memory_fts holds %d diary rows, want 5 (week, three dailies, dream report)", n)
	}

	// A reparented daily is out of the compaction stage's reach, so the next night cannot roll the same week up again.
	through, err := store.DiaryEntriesThrough(ctx, "day", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(through) != 1 || through[0].Day != "2026-08-05" {
		t.Errorf("DiaryEntriesThrough returned %+v, want only the daily that has not been rolled up", through)
	}

	if err := store.CommitCompactStage(ctx, "2026-08-30", nil, true); err != nil {
		t.Fatalf("done call: %v", err)
	}
	run, _, _ = store.DreamRun(ctx, "2026-08-30")
	if run.StagesDone != "compact" {
		t.Errorf("stages_done = %q, want the token exactly once", run.StagesDone)
	}
}

// The dream_runs PK is the single-run-per-night guarantee: a second start is a no-op, stage commits append their tokens, and FinishDreamRun stamps finished_at, files the one-line report, and writes the FTS-visible diary dream row.
func TestDreamRunLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, ok, err := store.DreamRun(ctx, "2026-08-29"); err != nil || ok {
		t.Fatalf("DreamRun before start = ok %v, err %v; want absent", ok, err)
	}
	if err := store.StartDreamRun(ctx, "2026-08-29"); err != nil {
		t.Fatalf("StartDreamRun: %v", err)
	}
	if err := store.StartDreamRun(ctx, "2026-08-29"); err != nil {
		t.Fatalf("second StartDreamRun must be a no-op: %v", err)
	}

	if err := store.CommitHypothesisStage(ctx, "2026-08-29", nil, []NewHypothesis{{Statement: "He codes at night.", Confidence: "low"}}); err != nil {
		t.Fatalf("CommitHypothesisStage: %v", err)
	}
	if err := store.CommitUnderstandingStage(ctx, "2026-08-29", "A person who codes at night."); err != nil {
		t.Fatalf("CommitUnderstandingStage: %v", err)
	}
	run, ok, err := store.DreamRun(ctx, "2026-08-29")
	if err != nil || !ok {
		t.Fatalf("DreamRun: ok %v, err %v", ok, err)
	}
	if run.StagesDone != "hyp und" {
		t.Errorf("StagesDone = %q, want %q", run.StagesDone, "hyp und")
	}
	if run.Finished {
		t.Error("run reads finished before FinishDreamRun")
	}
	if u, _ := store.DiaryEntry(ctx, "", "understanding"); u != "A person who codes at night." {
		t.Errorf("understanding = %q", u)
	}

	if err := store.FinishDreamRun(ctx, "2026-08-29", "I dreamt about the user's nights.", "judge-only: 0 tested"); err != nil {
		t.Fatalf("FinishDreamRun: %v", err)
	}
	run, _, _ = store.DreamRun(ctx, "2026-08-29")
	if !run.Finished || run.Report != "judge-only: 0 tested" {
		t.Errorf("after finish: finished %v, report %q", run.Finished, run.Report)
	}
	if entry, _ := store.DiaryEntry(ctx, "2026-08-29", "dream"); entry != "I dreamt about the user's nights." {
		t.Errorf("dream diary row = %q", entry)
	}
	// The dream report is meant to be findable — the diary triggers index it, unlike the hypotheses themselves.
	hits, err := store.SearchMemory(ctx, "dreamt")
	if err != nil || len(hits) == 0 {
		t.Errorf("dream row not searchable: %d hits, err %v", len(hits), err)
	}
}
