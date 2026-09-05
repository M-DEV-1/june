package db

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// CommitCompactStage is one transaction per tier: the coarse entry lands, the constituents (and only they) are deleted with the FTS mirror following, and the 'compact' token commits only on the call that says the stage is done.
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
	for _, day := range []string{"2026-08-03", "2026-08-04"} {
		if got, _ := store.DiaryEntry(ctx, day, "day"); got != "" {
			t.Errorf("constituent %s survived: %q", day, got)
		}
	}
	if got, _ := store.DiaryEntry(ctx, "2026-08-05", "day"); got == "" {
		t.Error("a daily outside the constituents was deleted")
	}
	if got, _ := store.DiaryEntry(ctx, "2026-08-04", "dream"); got == "" {
		t.Error("the delete must match kind as well as day")
	}
	run, _, _ := store.DreamRun(ctx, "2026-08-30")
	if run.StagesDone != "" {
		t.Errorf("stages_done = %q before the done call", run.StagesDone)
	}
	// The FTS mirror follows the deletes: only the surviving rows are indexed.
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM memory_fts WHERE source = 'diary'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("memory_fts holds %d diary rows, want 3 (week, spare daily, dream report)", n)
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

// InsertHypothesis is idempotent on the statement, OpenHypotheses caps at 20 oldest-born-first, ApplyHypothesisVerdict rewrites the judged fields and appends evidence, and StrongHypotheses returns only promoted or high-confidence rows.
func TestHypothesisCRUD(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", "2026-08-20"); err != nil {
		t.Fatalf("InsertHypothesis: %v", err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "high", "2026-08-29"); err != nil {
		t.Fatalf("duplicate InsertHypothesis must be a no-op: %v", err)
	}
	open, err := store.OpenHypotheses(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenHypotheses = %d rows, err %v; want 1", len(open), err)
	}
	h := open[0]
	if h.Confidence != "low" || h.Born != "2026-08-20" || h.TimesTested != 0 {
		t.Errorf("hypothesis row = %+v", h)
	}

	for i := 0; i < 25; i++ {
		if err := store.InsertHypothesis(ctx, fmt.Sprintf("Hypothesis %02d.", i), "low", "2026-08-21"); err != nil {
			t.Fatalf("InsertHypothesis %d: %v", i, err)
		}
	}
	open, _ = store.OpenHypotheses(ctx)
	if len(open) != 20 {
		t.Errorf("OpenHypotheses cap: got %d rows, want 20", len(open))
	}
	if open[0].Statement != "He codes at night." {
		t.Errorf("oldest-born-first ordering broken: first is %q", open[0].Statement)
	}

	v := HypothesisVerdict{ID: h.ID, Confidence: "high", Status: "open", LastTested: "2026-08-29", EvidenceLine: "[2026-08-29] supported — late commits all week", Reason: "", Tested: true}
	if err := store.ApplyHypothesisVerdict(ctx, v); err != nil {
		t.Fatalf("ApplyHypothesisVerdict: %v", err)
	}
	v.EvidenceLine = "[2026-08-30] supported — again"
	if err := store.ApplyHypothesisVerdict(ctx, v); err != nil {
		t.Fatalf("second ApplyHypothesisVerdict: %v", err)
	}
	open, _ = store.OpenHypotheses(ctx)
	h = open[0]
	if h.TimesTested != 2 || h.LastTested != "2026-08-29" || h.Confidence != "high" {
		t.Errorf("after verdicts: %+v", h)
	}
	if want := "[2026-08-29] supported — late commits all week\n[2026-08-30] supported — again"; h.Evidence != want {
		t.Errorf("evidence = %q, want %q", h.Evidence, want)
	}

	strong, err := store.StrongHypotheses(ctx)
	if err != nil || len(strong) != 1 || strong[0].ID != h.ID {
		t.Errorf("StrongHypotheses = %d rows (err %v), want just the high-confidence one", len(strong), err)
	}
	if err := store.ApplyHypothesisVerdict(ctx, HypothesisVerdict{ID: h.ID, Confidence: "high", Status: "retired", Reason: "done", Tested: false}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if open, _ = store.OpenHypotheses(ctx); len(open) != 20 {
		t.Errorf("retired row still open: %d", len(open))
	}
	for _, o := range open {
		if o.ID == h.ID {
			t.Error("retired hypothesis still returned by OpenHypotheses")
		}
	}
}

// DiaryDays is an inclusive day-key range over kind='day' rows only, oldest first.
func TestDiaryDays(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	for _, day := range []string{"2026-08-25", "2026-08-27", "2026-08-29"} {
		if err := store.SetDiaryEntry(ctx, day, "day", "entry "+day); err != nil {
			t.Fatalf("SetDiaryEntry: %v", err)
		}
	}
	if err := store.SetDiaryEntry(ctx, "2026-08-27", "dream", "not a day row"); err != nil {
		t.Fatalf("SetDiaryEntry dream: %v", err)
	}
	days, err := store.DiaryDays(ctx, "2026-08-26", "2026-08-29")
	if err != nil {
		t.Fatalf("DiaryDays: %v", err)
	}
	if len(days) != 2 || days[0].Day != "2026-08-27" || days[1].Day != "2026-08-29" {
		t.Errorf("DiaryDays = %+v, want the 27th and 29th in order", days)
	}
}

// CommitProceduresStage records the 'procedures' token, and calling it again on an already-marked night is idempotent: the token stays discoverable by the same field-membership check the runner uses to decide whether the stage still needs to run, and a second commit never errors.
func TestCommitProceduresStage(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.StartDreamRun(ctx, "2026-08-30"); err != nil {
		t.Fatalf("StartDreamRun: %v", err)
	}

	run, _, _ := store.DreamRun(ctx, "2026-08-30")
	if strings.Contains(run.StagesDone, "procedures") {
		t.Fatalf("stages_done = %q before any commit, want no procedures token", run.StagesDone)
	}

	if err := store.CommitProceduresStage(ctx, "2026-08-30"); err != nil {
		t.Fatalf("CommitProceduresStage: %v", err)
	}
	run, _, _ = store.DreamRun(ctx, "2026-08-30")
	if !slices.Contains(strings.Fields(run.StagesDone), "procedures") {
		t.Errorf("stages_done = %q, want it to carry the procedures token", run.StagesDone)
	}

	if err := store.CommitProceduresStage(ctx, "2026-08-30"); err != nil {
		t.Fatalf("second CommitProceduresStage: %v", err)
	}
	run, _, _ = store.DreamRun(ctx, "2026-08-30")
	if !slices.Contains(strings.Fields(run.StagesDone), "procedures") {
		t.Errorf("stages_done = %q after a second commit, still want the procedures token", run.StagesDone)
	}
}

// A cancelled context aborts a stage commit before anything lands: no verdicts, no inserts, no stage token.
func TestCommitHypothesisStage_CancelledCommitsNothing(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.StartDreamRun(ctx, "2026-08-29"); err != nil {
		t.Fatalf("StartDreamRun: %v", err)
	}
	if err := store.InsertHypothesis(ctx, "He codes at night.", "low", "2026-08-20"); err != nil {
		t.Fatalf("InsertHypothesis: %v", err)
	}
	open, _ := store.OpenHypotheses(ctx)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := store.CommitHypothesisStage(cancelled, "2026-08-29",
		[]HypothesisVerdict{{ID: open[0].ID, Confidence: "high", Status: "promoted", Tested: true}},
		[]NewHypothesis{{Statement: "New one.", Confidence: "low"}})
	if err == nil {
		t.Fatal("commit with a cancelled context succeeded")
	}
	run, _, _ := store.DreamRun(ctx, "2026-08-29")
	if run.StagesDone != "" {
		t.Errorf("stages_done = %q after a cancelled commit, want empty", run.StagesDone)
	}
	open, _ = store.OpenHypotheses(ctx)
	if len(open) != 1 || open[0].TimesTested != 0 || open[0].Status != "open" {
		t.Errorf("hypotheses changed by a cancelled commit: %+v", open)
	}
}
