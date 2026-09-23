package actjob

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"ora/internal/act"
	"ora/internal/db"
	"ora/internal/util"
)

// saveTimeout bounds one checkpoint write so a wedged store cannot hold a job's own goroutine.
const saveTimeout = 10 * time.Second

// storedResultCap is how much of a tool result the checkpoint keeps, in runes: the same 300 the ordinary act runs keep theirs at (see db/act_runs.go). The prompt still sees the fuller resultCap copy the job holds in memory; what goes on disk is only what a later reader needs to see what happened.
const storedResultCap = 300

// cloned copies the two fields of a job that go on being written after a copy of it has been handed out: the per-model spend map, which every round writes an entry into, and the steps slice, whose last element is rewritten when its check comes back. A shallow copy shares both, so a GET that encodes one while the loop writes the other is a concurrent map read and write — which is not a panic but a fatal error that takes the daemon down. The other slices are only ever replaced wholesale or appended to, so the header alone is enough for them. Input: the job under the lock. Output: the copy.
func cloned(j Job) Job {
	j.Steps = slices.Clone(j.Steps)
	j.Spend.ByModel = maps.Clone(j.Spend.ByModel)
	return j
}

// row renders a job as the database row that holds it. Input: the job. Output: the row, or an error when the checkpoint will not marshal — which the caller must not paper over, because writing an empty object in its place would replace the job's whole history with nothing.
func row(job Job) (db.ActJobRow, error) {
	job.Observations, job.Steps = digestedObservations(job.Observations), storedSteps(job.Steps)
	blob, err := json.Marshal(job)
	if err != nil {
		return db.ActJobRow{}, fmt.Errorf("act job %s: its checkpoint will not marshal: %w", job.ID, err)
	}
	return db.ActJobRow{ID: job.ID, Goal: job.Goal, Brain: job.Brain, State: string(job.State), Answer: job.Say, Error: job.Err, DurationMS: job.ElapsedMS, Checkpoint: blob}, nil
}

// digestedObservations is what a screen reading is kept as on disk: its first line, which names the app and the window, and how many items were listed under it. The listing itself is the live text of somebody's window — the messages on screen, the half-typed reply in the compose box — and the checkpoint is a lasting record, so the body stays in memory where the prompt reads it and never reaches the row. Input: the readings the job is holding. Output: one line each.
func digestedObservations(obs []string) []string {
	if len(obs) == 0 {
		return nil
	}
	out := make([]string, len(obs))
	for i, o := range obs {
		lines := strings.Split(o, "\n")
		items := 0
		for _, line := range lines[1:] {
			if strings.HasPrefix(line, "[") {
				items++
			}
		}
		out[i] = fmt.Sprintf("%s (%d items listed)", lines[0], items)
	}
	return out
}

// storedSteps is what the steps are kept as on disk: the same steps with each result cut to storedResultCap runes. The fuller copy stays in memory for the prompt. Input: the job's steps. Output: a copy of them, since the originals are the loop's own and must not be edited under it.
func storedSteps(steps []Step) []Step {
	out := slices.Clone(steps)
	for i := range out {
		out[i].Result = util.Runes(out[i].Result, storedResultCap)
	}
	return out
}

// save writes the job's checkpoint, with the wall time it has spent brought up to date first, so a daemon that crashes mid-job resumes on what is left of the budget rather than on a fresh one. A failed write is logged, never returned: losing the ability to resume must not stop the job that is working.
func (r *Runner) save(l *live, job Job) {
	job.ElapsedMS = l.elapsed()
	l.set(func(j *Job) { j.ElapsedMS = job.ElapsedMS })
	stored, err := row(job)
	if err != nil {
		slog.Error("act job: could not checkpoint", "job", job.ID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	if err := r.store.SaveActJob(ctx, stored); err != nil {
		slog.Error("act job: could not checkpoint", "job", job.ID, "error", err)
	}
}

// redactedExpect is what a step's Expect is stored and described as, in place of what the model actually wrote down, wherever it could otherwise repeat text StorableArgs already dropped from the same step's own arguments. A type_text step redacts its Value regardless of what kind of check it names, since the box just typed into is exactly what a check right after it is about. A field_holds check redacts its Value regardless of which tool the step named, since it can only be asking about a field something was just typed into. Kind is left alone either way, so the stored and emitted check still says what kind of thing was being verified. Input: the step's tool name and the expected change the model wrote down for it. Output: the expect to store and to describe; the live wait_for check must keep using the real one this came from.
func redactedExpect(tool string, e act.Check) act.Check {
	// A step with no check named has nothing to hide, and stamping the marker on it would record a check that never existed.
	if e.Value == "" {
		return e
	}
	if tool == "type_text" || e.Kind == act.FieldHolds {
		e.Value = db.RedactedValue
	}
	return e
}
