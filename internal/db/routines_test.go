package db

import (
	"context"
	"testing"
	"time"
)

// TestRoutineRoundTrip writes a routine, reads it back both as a list and by id, records a run, deletes it, and checks every step landed.
func TestRoutineRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.AddRoutine(ctx, "every weekday at 8, tell me the one thing I must do today", "weekdays at 8")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}

	list, err := store.Routines(ctx)
	if err != nil {
		t.Fatalf("Routines: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("Routines = %+v, want one row with id %d", list, id)
	}
	r := list[0]
	if r.Schedule != "weekdays at 8" || !r.Enabled || !r.LastRun.IsZero() || r.LastAnswer != "" {
		t.Errorf("new routine = %+v, want enabled, never run, no answer", r)
	}

	byID, err := store.RoutineByID(ctx, id)
	if err != nil || byID.Text != r.Text {
		t.Errorf("RoutineByID(%d) = %+v, %v", id, byID, err)
	}

	ran := time.Now()
	if err := store.SetRoutineRun(ctx, id, ran, "Ship the report — it's due today."); err != nil {
		t.Fatalf("SetRoutineRun: %v", err)
	}
	byID, err = store.RoutineByID(ctx, id)
	if err != nil {
		t.Fatalf("RoutineByID after run: %v", err)
	}
	if byID.LastAnswer != "Ship the report — it's due today." {
		t.Errorf("LastAnswer = %q", byID.LastAnswer)
	}
	if byID.LastRun.IsZero() || byID.LastRun.Sub(ran).Abs() > time.Second {
		t.Errorf("LastRun = %v, want close to %v", byID.LastRun, ran)
	}

	if err := store.DeleteRoutine(ctx, id); err != nil {
		t.Fatalf("DeleteRoutine: %v", err)
	}
	list, err = store.Routines(ctx)
	if err != nil || len(list) != 0 {
		t.Errorf("Routines after delete = %+v, %v, want none", list, err)
	}
}

// TestTryStartFinish checks the in-flight guard both the scheduler tick and POST /routines/{id}/run share: a second TryStart for the same id fails while the first is still running, a different id is unaffected, and Finish clears the mark so a later TryStart for the same id succeeds again.
func TestTryStartFinish(t *testing.T) {
	store := newStore(t)

	if !store.TryStart(1) {
		t.Fatal("TryStart(1) first call = false, want true")
	}
	if store.TryStart(1) {
		t.Error("TryStart(1) while already running = true, want false")
	}
	if !store.TryStart(2) {
		t.Error("TryStart(2), a different routine, = false, want true")
	}
	store.Finish(1)
	if !store.TryStart(1) {
		t.Error("TryStart(1) after Finish = false, want true")
	}
	// Finish on an id never started, or already finished, must not panic.
	store.Finish(999)
}
