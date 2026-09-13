package actjob

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/act"
	"ora/internal/db"
	"ora/internal/db/dbtest"
)

// fakeExec is a tool executor that answers from a script instead of touching the screen: every call is recorded, observe_screen and wait_for get canned answers, and anything else returns what the script says.
type fakeExec struct {
	mu    sync.Mutex
	calls []string
	// verdicts is the answer wait_for gives, one per call, the last one repeating once the list runs out.
	verdicts []bool
	// block, when non-nil, is closed by the test to release a tool call that is standing in for a slow action.
	block chan struct{}
	// blockTool, when set, narrows block to that one tool rather than every tool but observe_screen.
	blockTool string
	// result, when set, is what every tool but observe_screen and wait_for answers with.
	result string
	// heldBefore is what the pre-reading of a step's check answers: the step loop takes its check once before the action, with a one-poll timeout, and true here says the check was already true before anything happened.
	heldBefore bool
	// preChecks records the "value" of each of those pre-readings, kept apart from calls so the tool sequence a test asserts on is still the actions and their checks.
	preChecks []string
}

// CheckHolds is the PreChecker half of the seam: the step loop's one reading of a check before it acts, which never goes through wait_for and so never shows up among the tool calls a test asserts on.
func (f *fakeExec) CheckHolds(ctx context.Context, check act.Check) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preChecks = append(f.preChecks, check.Value)
	return f.heldBefore
}

func (f *fakeExec) ExecuteAskTool(ctx context.Context, name string, args map[string]any) string {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	n := 0
	for _, c := range f.calls {
		if c == "wait_for" {
			n++
		}
	}
	verdict := true
	if len(f.verdicts) > 0 {
		i := n - 1
		if i >= len(f.verdicts) {
			i = len(f.verdicts) - 1
		}
		if i >= 0 {
			verdict = f.verdicts[i]
		}
	}
	block, blockTool, result := f.block, f.blockTool, f.result
	f.mu.Unlock()

	if blockTool != "" && name != blockTool {
		block = nil
	}
	if block != nil && name != "observe_screen" {
		select {
		case <-block:
		case <-ctx.Done():
			return "error: the job stopped before this finished"
		}
	}
	switch name {
	case "observe_screen":
		return "Brave · Netflix\n[1] push button \"Play\" (10,10)"
	case "wait_for":
		if verdict {
			return act.WaitPassPrefix + "the title is \"S16 E8\""
		}
		return act.WaitFailPrefix + "5s: the title is \"Netflix\""
	}
	if result != "" {
		return result
	}
	return "did " + name
}

// preChecksSeen returns the value of each pre-reading the step loop took, in order.
func (f *fakeExec) preChecksSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.preChecks...)
}

func (f *fakeExec) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// script is a Model that answers with the replies given, in order, and repeats the last one for ever after.
func script(replies ...string) Model {
	var i int
	var mu sync.Mutex
	return func(ctx context.Context, prompt string) (string, Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		reply := replies[len(replies)-1]
		if i < len(replies) {
			reply = replies[i]
		}
		i++
		return reply, Usage{Model: "fake", Input: 1000, Cached: 100, Output: 50}, nil
	}
}

// stepReply is one decision in the shape the prompt asks the model for.
func stepReply(tool, expect string) string {
	b, _ := json.Marshal(map[string]any{
		"plan":   "click play, then check the title",
		"next":   "click " + tool,
		"tool":   tool,
		"args":   map[string]any{"n": 1},
		"expect": map[string]string{"kind": act.TitleContains, "value": expect},
	})
	return string(b)
}

// doneReply is the decision that ends a job.
func doneReply(say string) string {
	b, _ := json.Marshal(map[string]any{"done": true, "say": say})
	return string(b)
}

// newRunner builds a Runner over an in-memory store with the given executor and model, and collects every event it emits.
func newRunner(t *testing.T, exec Executor, m Model) (*Runner, *db.Store, func() []Event) {
	t.Helper()
	store := dbtest.Open(t)
	var mu sync.Mutex
	var events []Event
	r := New(store, exec, map[string]Model{"fake": m}, "fake", func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	return r, store, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), events...)
	}
}

// waitState blocks until the job reaches one of the states named, or fails the test after a second.
func waitState(t *testing.T, r *Runner, id string, states ...State) Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := r.Job(context.Background(), id)
		if err == nil {
			for _, s := range states {
				if job.State == s {
					return job
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	job, _ := r.Job(context.Background(), id)
	t.Fatalf("job %s stayed in state %q, waiting for one of %v", id, job.State, states)
	return Job{}
}

// TestRunner_HappyPath checks the whole loop on a goal that works first time: one step is decided, acted on, verified, and the job ends done with what it said, its steps recorded (with a check that did not already hold) and its token spend added up. It also checks the pre-reading of a step's check is one screen walk rather than a wait_for poll, so a job does not pay for verification before it has acted at all.
func TestRunner_HappyPath(t *testing.T) {
	exec := &fakeExec{}
	r, store, events := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("It is playing S16 E8.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}
	if job.Say != "It is playing S16 E8." {
		t.Errorf("Say = %q, want the model's closing words", job.Say)
	}
	if len(job.Steps) != 1 || job.Steps[0].Outcome != "pass" || job.Steps[0].HeldBefore {
		t.Fatalf("steps = %+v, want one plain verified step whose check did not already hold", job.Steps)
	}
	if job.Spend.Rounds != 2 || job.Spend.Input != 2000 || job.Spend.Cached != 200 || job.Spend.Output != 100 {
		t.Errorf("spend = %+v, want two rounds summed", job.Spend)
	}
	if job.Spend.ByModel["fake"].Input != 2000 {
		t.Errorf("per-model split = %+v, want the fake model's own 2000 input", job.Spend.ByModel)
	}
	calls := exec.names()
	want := []string{"observe_screen", "click", "wait_for", "observe_screen"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("tool calls = %v, want %v", calls, want)
	}
	// The pre-reading is one walk of the screen matched against the check, not a wait_for: wait_for polls for up to five seconds and, on a list check, walks the window a second time to see whether another one came to the front, and a job used to pay both on every step before it had acted at all.
	waits := 0
	for _, name := range calls {
		if name == "wait_for" {
			waits++
		}
	}
	if waits != 1 {
		t.Errorf("wait_for was called %d time(s) for one step, want only the verification", waits)
	}
	if got := exec.preChecksSeen(); len(got) != 1 || got[0] != "S16 E8" {
		t.Errorf("pre-readings = %v, want one, of the step's own check", got)
	}

	// The job is on disk under its own id, in its end state, so a restart never picks it up again.
	row, err := store.ActJob(context.Background(), id)
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if row.State != string(Done) || row.Answer != job.Say {
		t.Errorf("stored row = %+v, want the done state and the answer", row)
	}
	var kinds []string
	for _, ev := range events() {
		kinds = append(kinds, ev.Kind)
	}
	if strings.Join(kinds, ",") != "started,plan,step,verified,done" {
		t.Errorf("events = %v, want started,plan,step,verified,done", kinds)
	}
	if last := events()[len(events())-1]; last.Spend == nil || last.Spend.Rounds != 2 {
		t.Errorf("the done event carries %+v, want the token accounting", last.Spend)
	}
}

// TestRunner_StuckThenAnswered checks three failed verifications on one step stop the job with one plain question, and that the answer the user sends is added to the goal context and lets the loop carry on.
func TestRunner_StuckThenAnswered(t *testing.T) {
	exec := &fakeExec{verdicts: []bool{false, false, false, true}}
	r, _, events := newRunner(t, exec, script(
		stepReply("click", "S16 E8"),
		stepReply("click", "S16 E8"),
		stepReply("click", "S16 E8"),
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Stuck, Done, Failed)
	if job.State != Stuck {
		t.Fatalf("state = %q (%s), want stuck after three failed checks", job.State, job.Err)
	}
	if job.Question == "" {
		t.Fatal("a stuck job asked the user nothing")
	}
	var asked bool
	for _, ev := range events() {
		if ev.Kind == "question" && ev.Text == job.Question {
			asked = true
		}
	}
	if !asked {
		t.Error("the stuck question never went out as an event")
	}

	if err := r.Answer(id, "it is under season 16, scroll further"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	job = waitState(t, r, id, Done, Failed)
	if job.State != Done {
		t.Fatalf("state after the answer = %q (%s), want done", job.State, job.Err)
	}
	if len(job.Answers) != 1 || !strings.Contains(job.Answers[0], "season 16") {
		t.Errorf("answers = %v, want the user's own words kept with the goal", job.Answers)
	}
	if !strings.Contains(BuildPrompt(job), "season 16") {
		t.Error("the answer never reached the prompt")
	}
}

// TestRunner_StopMidStep checks a stop that lands while a tool call is still running ends the job stopped, with the step it was in recorded, and that no further tool runs.
func TestRunner_StopMidStep(t *testing.T) {
	exec := &fakeExec{block: make(chan struct{})}
	r, store, _ := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("done")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait until the click is actually in flight, then stop and release it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(exec.names()) >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := r.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(exec.block)

	job := waitState(t, r, id, Stopped, Done, Failed)
	if job.State != Stopped {
		t.Fatalf("state = %q, want stopped", job.State)
	}
	for _, name := range exec.names() {
		if name == "wait_for" {
			t.Error("the job verified a step after it was stopped")
		}
	}
	row, err := store.ActJob(context.Background(), id)
	if err != nil || row.State != string(Stopped) {
		t.Errorf("stored row = %+v (%v), want the stopped state on disk", row, err)
	}
}

// TestRunner_ResumeFromCheckpoint checks a job a fresh Runner has never seen — the shape a daemon restart leaves behind — is picked up from its stored checkpoint, keeping the steps already done rather than starting the goal again.
func TestRunner_ResumeFromCheckpoint(t *testing.T) {
	store := dbtest.Open(t)

	// The checkpoint a daemon that died mid-job would have left: one step done and verified, the plan and the progress summary written.
	saved := Job{
		ID: "act-7", Goal: "play S16 E8", Brain: "fake", State: Stepping,
		Plan: "open the show, then click episode 8", Summary: "The show page is open.",
		Steps:  []Step{{N: 1, Tool: "click", Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Show"}}},
		Budget: DefaultBudget(), Spend: Spend{Rounds: 3, Input: 9000},
	}
	blob, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.SaveActJob(context.Background(), db.ActJobRow{ID: saved.ID, Goal: saved.Goal, Brain: "fake", State: string(Stepping), Checkpoint: blob}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}

	exec := &fakeExec{}
	r := New(store, exec, map[string]Model{"fake": script(stepReply("click", "S16 E8"), doneReply("Playing."))}, "fake", func(Event) {})
	if err := r.Resume(context.Background(), "act-7", Budget{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	job := waitState(t, r, "act-7", Done, Failed, Stuck)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}
	if len(job.Steps) != 2 || job.Steps[0].N != 1 {
		t.Fatalf("steps = %d, want the checkpointed one kept and the new one added", len(job.Steps))
	}
	if job.Plan != saved.Plan {
		t.Errorf("plan = %q, want the checkpointed plan", job.Plan)
	}
	if job.Spend.Rounds < 4 {
		t.Errorf("rounds = %d, want the checkpointed spend carried forward", job.Spend.Rounds)
	}
}

// TestRunner_BudgetExhaustionOnTokens checks the input-token budget ends a job the same way the step budget does.
func TestRunner_BudgetExhaustionOnTokens(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(stepReply("click", "S16 E8")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Wall: time.Minute, InputTokens: 2500, Steps: 40}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Failed, Done, Stuck)
	if job.State != Failed || !strings.Contains(job.Err, "token") {
		t.Fatalf("state = %q, err = %q, want failed on the token budget", job.State, job.Err)
	}
}

// TestRunner_UnknownBrainIsRefused checks a job naming a brain the daemon has no model for never starts, so a goal is never quietly answered by a different model under the wrong name.
func TestRunner_UnknownBrainIsRefused(t *testing.T) {
	r, _, _ := newRunner(t, &fakeExec{}, script(doneReply("done")))
	if _, err := r.Start(context.Background(), "play S16 E8", Opts{Brain: "nope"}); err == nil {
		t.Fatal("Start with an unknown brain returned no error")
	}
}

// TestRunner_PauseAndResume checks a paused job takes no further step until it is resumed.
func TestRunner_PauseAndResume(t *testing.T) {
	exec := &fakeExec{}
	// The model takes its time, so the pause below lands while the job is still on its first round rather than after it has already finished.
	fast := script(stepReply("click", "S16 E8"), stepReply("click", "S16 E8"), doneReply("Playing."))
	slow := func(ctx context.Context, prompt string) (string, Usage, error) {
		time.Sleep(20 * time.Millisecond)
		return fast(ctx, prompt)
	}
	r, _, _ := newRunner(t, exec, slow)
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Pause(id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitState(t, r, id, Paused, Done)
	before := len(exec.names())
	time.Sleep(30 * time.Millisecond)
	// The check is not conditioned on the job still reading as paused: a job that has left the state should be making no calls either, and a condition there is a check that quietly stops testing anything when the scheduler is slow.
	if len(exec.names()) > before+2 {
		t.Errorf("a paused job made %d more tool calls", len(exec.names())-before)
	}
	if err := r.Resume(context.Background(), id, Budget{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if job := waitState(t, r, id, Done, Failed, Stuck); job.State != Done {
		t.Fatalf("state = %q, want done once resumed", job.State)
	}
}

// TestRunner_NewJobIdsCarryOnFromTheStore checks a fresh Runner numbers its first job past the highest id already on disk, so a daemon restart never hands a new job an id an older one still holds and rewrites that job's row under it.
func TestRunner_NewJobIdsCarryOnFromTheStore(t *testing.T) {
	store := dbtest.Open(t)
	if err := store.SaveActJob(context.Background(), db.ActJobRow{ID: "act-9", Goal: "an older job", State: string(Done), Checkpoint: []byte(`{"id":"act-9"}`)}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}

	r := New(store, &fakeExec{}, map[string]Model{"fake": script(stepReply("click", "S16 E8"), doneReply("Playing."))}, "fake", nil)
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "act-10" {
		t.Fatalf("the first job of a restarted daemon is %q, want act-10", id)
	}
	waitState(t, r, id, Done, Failed, Stuck)
	older, err := store.ActJob(context.Background(), "act-9")
	if err != nil || older.Goal != "an older job" {
		t.Fatalf("the older job's row = %+v (%v), want it untouched", older, err)
	}
}

// TestRunner_ResumeClaimsTheJobBeforeReadingTheCheckpoint checks the live slot is taken under one lock before the store read, so a second Resume landing while the first is still reading is refused rather than launching the same job twice against the same screen.
func TestRunner_ResumeClaimsTheJobBeforeReadingTheCheckpoint(t *testing.T) {
	store := dbtest.Open(t)
	saved := Job{ID: "act-7", Goal: "play S16 E8", Brain: "fake", State: Stepping, Budget: DefaultBudget()}
	blob, _ := json.Marshal(saved)
	if err := store.SaveActJob(context.Background(), db.ActJobRow{ID: saved.ID, Goal: saved.Goal, Brain: "fake", State: string(Stepping), Checkpoint: blob}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}
	r := New(store, &fakeExec{}, map[string]Model{"fake": script(doneReply("Playing."))}, "fake", nil)

	// The slot a first Resume has claimed and not yet launched: a second Resume must not read the checkpoint and launch its own loop.
	l, claimed := r.claim("act-7")
	if !claimed || l == nil {
		t.Fatal("the first claim of an idle job did not take the slot")
	}
	if err := r.Resume(context.Background(), "act-7", Budget{}); err == nil {
		t.Fatal("a second Resume launched a job another Resume had already claimed")
	}
	r.release("act-7")
	if _, claimed := r.claim("act-7"); !claimed {
		t.Fatal("a released slot was not free again")
	}
}

// TestRunner_DoneNeedsAStepThatCheckedOut checks a model that claims the goal is reached before anything has been verified is handed back a nudge rather than ending the job done, which is the failure the whole checked-step design exists to stop.
func TestRunner_DoneNeedsAStepThatCheckedOut(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		doneReply("Now playing S16 E8."),
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done once a step had checked out", job.State, job.Err)
	}
	if len(job.Steps) != 1 || job.Steps[0].Outcome != "pass" {
		t.Fatalf("steps = %+v, want the one verified step the job had to take first", job.Steps)
	}
	if job.Say != "Playing now." {
		t.Errorf("Say = %q, want the closing words of the round that was allowed to end it", job.Say)
	}
}

// peekExec reads the job back off disk while wait_for runs, so a test can see what the checkpoint held between the action and its check.
type peekExec struct {
	fakeExec
	store *db.Store
	id    string
	mu    sync.Mutex
	seen  []Step
}

func (p *peekExec) ExecuteAskTool(ctx context.Context, name string, args map[string]any) string {
	if name == "wait_for" {
		if row, err := p.store.ActJob(context.Background(), p.id); err == nil {
			var j Job
			if json.Unmarshal(row.Checkpoint, &j) == nil {
				p.mu.Lock()
				p.seen = j.Steps
				p.mu.Unlock()
			}
		}
	}
	return p.fakeExec.ExecuteAskTool(ctx, name, args)
}

// TestRunner_ActionIsCheckpointedBeforeItIsChecked checks the step is on disk the moment its action has run, before the check: a daemon that dies between the two comes back knowing the action happened rather than taking it a second time on the real screen.
func TestRunner_ActionIsCheckpointedBeforeItIsChecked(t *testing.T) {
	store := dbtest.Open(t)
	exec := &peekExec{store: store, id: "act-1"}
	r := New(store, exec, map[string]Model{"fake": script(stepReply("click", "S16 E8"), doneReply("Playing."))}, "fake", nil)
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Done, Failed, Stuck)

	exec.mu.Lock()
	seen := exec.seen
	exec.mu.Unlock()
	if len(seen) != 1 || seen[0].Tool != "click" {
		t.Fatalf("the checkpoint held %+v while the check ran, want the click already recorded", seen)
	}
	if seen[0].Outcome != "" {
		t.Errorf("the marker step already claims outcome %q before the check answered", seen[0].Outcome)
	}
}

// TestRunner_AnswerIsRefusedUnlessTheJobIsStuck checks an answer to a job that asked nothing is refused rather than buffered, which would otherwise be drained as the answer to a different question steps later.
func TestRunner_AnswerIsRefusedUnlessTheJobIsStuck(t *testing.T) {
	exec := &fakeExec{block: make(chan struct{})}
	r, _, _ := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("done")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(exec.names()) < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if err := r.Answer(id, "it is under season 16"); err == nil {
		t.Fatal("an answer to a job that is not waiting on a question was accepted")
	}
	r.Stop(id)
	close(exec.block)
	waitState(t, r, id, Stopped, Done, Failed)
}

// TestRunner_TimeoutWhileWaitingOnTheUserIsABudgetFailure checks a job whose time runs out while it waits on an answer says so — failed, on its wall-clock budget — instead of reporting that the user stopped it, and keeps the question it asked.
func TestRunner_TimeoutWhileWaitingOnTheUserIsABudgetFailure(t *testing.T) {
	r, store, _ := newRunner(t, &fakeExec{}, script(doneReply("done")))
	l, _ := r.claim("act-1")
	l.started, l.job = time.Now(), Job{ID: "act-1", Goal: "play S16 E8", State: Stuck, Question: "Which season is it under?", Budget: DefaultBudget()}
	l.cancel()

	r.endedWaiting(l.ctx, l)
	job := l.snapshot()
	if job.State != Failed {
		t.Fatalf("state = %q, want failed: nobody stopped this job, its time ran out", job.State)
	}
	if !strings.Contains(job.Err, "budget") {
		t.Errorf("err = %q, want the wall-clock budget named", job.Err)
	}
	if job.Question != "Which season is it under?" {
		t.Errorf("question = %q, want the question kept so the user can still answer it", job.Question)
	}
	row, err := store.ActJob(context.Background(), "act-1")
	if err != nil || row.State != string(Failed) {
		t.Errorf("stored row = %+v (%v), want the failed state on disk", row, err)
	}
}

// TestRunner_TimeWaitingOnTheUserDoesNotSpendTheWallBudget checks a job stuck on a question outlives its own wall budget while it waits, because a job holding still for a person is not a job running away with the screen.
func TestRunner_TimeWaitingOnTheUserDoesNotSpendTheWallBudget(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		`{"ask":"Which season is it under?"}`,
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Wall: 300 * time.Millisecond}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Stuck, Done, Failed)
	time.Sleep(600 * time.Millisecond)
	if job, err := r.Job(context.Background(), id); err != nil || job.State != Stuck {
		t.Fatalf("after waiting past the wall budget the job is %q (%v), want it still stuck on its question", job.State, err)
	}
	if err := r.Answer(id, "season 16"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if job := waitState(t, r, id, Done, Failed, Stopped); job.State != Done {
		t.Fatalf("state after the answer = %q (%s), want done", job.State, job.Err)
	}
}

// TestRunner_ElapsedIsCheckpointedOnEverySave checks the wall time a job has spent is on disk before it ends, so a daemon that crashes mid-job resumes with the budget it had left rather than a fresh five minutes.
func TestRunner_ElapsedIsCheckpointedOnEverySave(t *testing.T) {
	slow := func(ctx context.Context, prompt string) (string, Usage, error) {
		time.Sleep(30 * time.Millisecond)
		return `{"ask":"Which season is it under?"}`, Usage{Model: "fake", Input: 100}, nil
	}
	r, store, _ := newRunner(t, &fakeExec{}, slow)
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Stuck, Done, Failed)
	// The state reaches memory just before the checkpoint reaches disk, so the read waits for the write the stuck state promised.
	var row db.ActJobRow
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if row, err = store.ActJob(context.Background(), id); err == nil && row.State == string(Stuck) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if row.DurationMS < 20 {
		t.Fatalf("the checkpoint of a running job says %d ms spent, want the round it has already taken", row.DurationMS)
	}
	var job Job
	if err := json.Unmarshal(row.Checkpoint, &job); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if job.ElapsedMS < 20 {
		t.Errorf("the checkpoint's ElapsedMS = %d, want the wall time this run has spent", job.ElapsedMS)
	}
	r.Stop(id)
}

// TestRunner_TypedTextIsNeverCheckpointed checks what the user dictated into type_text is dropped from the step before it is written to disk or shown on the event stream, the same redaction the "watch me once" act runs have always applied.
func TestRunner_TypedTextIsNeverCheckpointed(t *testing.T) {
	typeReply, _ := json.Marshal(map[string]any{
		"tool":   "type_text",
		"args":   map[string]any{"text": "hunter2correcthorse", "n": 1},
		"expect": map[string]string{"kind": act.TitleContains, "value": "S16 E8"},
	})
	r, store, _ := newRunner(t, &fakeExec{}, script(string(typeReply), doneReply("Typed it.")))
	id, err := r.Start(context.Background(), "type the passphrase", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if len(job.Steps) != 1 {
		t.Fatalf("steps = %+v, want the one type_text step", job.Steps)
	}
	if _, ok := job.Steps[0].Args["text"]; ok {
		t.Errorf("the step kept what was typed: %v", job.Steps[0].Args)
	}
	row, err := store.ActJob(context.Background(), id)
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if strings.Contains(string(row.Checkpoint), "hunter2correcthorse") {
		t.Errorf("the typed text reached the checkpoint on disk: %s", row.Checkpoint)
	}
	line := describeAction(decision{Tool: "type_text", Args: map[string]any{"text": "hunter2correcthorse"}})
	if strings.Contains(line, "hunter2correcthorse") {
		t.Errorf("the hover line carries what was typed: %q", line)
	}
}

// TestRedactedExpect checks the rule for when a step's Expect.Value must be swapped for db.RedactedValue before it is stored or described: a type_text step redacts regardless of what kind of check it wrote down, and a field_holds check redacts regardless of which tool the step named, since either one can carry back out what StorableArgs already dropped from the step's own arguments. Any other combination is left alone.
func TestRedactedExpect(t *testing.T) {
	cases := []struct {
		name string
		tool string
		kind string
		want bool
	}{
		{"type_text with title_contains", "type_text", act.TitleContains, true},
		{"click with field_holds", "click", act.FieldHolds, true},
		{"type_text with field_holds", "type_text", act.FieldHolds, true},
		{"click with title_contains", "click", act.TitleContains, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactedExpect(c.tool, act.Check{Kind: c.kind, Value: "hunter2correcthorse"})
			redacted := got.Value == db.RedactedValue
			if redacted != c.want {
				t.Errorf("redactedExpect(%q, {%q, ...}) = %+v, want redacted=%v", c.tool, c.kind, got, c.want)
			}
			if got.Kind != c.kind {
				t.Errorf("redactedExpect changed Kind to %q, want it left at %q", got.Kind, c.kind)
			}
			if !c.want && got.Value != "hunter2correcthorse" {
				t.Errorf("redactedExpect changed a value it should have left alone: %q", got.Value)
			}
		})
	}
	// A type_text step that named no check at all stays empty rather than gaining a redacted value for a check that never existed.
	if got := redactedExpect("type_text", act.Check{}); got.Value != "" || got.Kind != "" {
		t.Errorf("redactedExpect(type_text, empty) = %+v, want it untouched", got)
	}
}

// TestRunner_FieldHoldsExpectOfTypedTextIsRedacted checks the case the plain type_text redaction above cannot reach on its own: a step's Expect naming field_holds with the same text just typed, which otherwise lands in the checkpoint and on the event stream even though StorableArgs already dropped it from the step's own arguments. The live wait_for check must still see the real text, since only that lets the step actually pass.
func TestRunner_FieldHoldsExpectOfTypedTextIsRedacted(t *testing.T) {
	const secret string = "hunter2correcthorse"
	typeReply, _ := json.Marshal(map[string]any{
		"tool":   "type_text",
		"args":   map[string]any{"text": secret, "n": 1},
		"expect": map[string]string{"kind": act.FieldHolds, "value": secret},
	})
	r, store, events := newRunner(t, &fakeExec{}, script(string(typeReply), doneReply("Typed it.")))
	id, err := r.Start(context.Background(), "type the passphrase", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if len(job.Steps) != 1 {
		t.Fatalf("steps = %+v, want the one type_text step", job.Steps)
	}
	if job.Steps[0].Expect.Value != db.RedactedValue {
		t.Errorf("job.Steps[0].Expect.Value = %q, want the redaction marker", job.Steps[0].Expect.Value)
	}
	row, err := store.ActJob(context.Background(), id)
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if strings.Contains(string(row.Checkpoint), secret) {
		t.Errorf("the typed text reached the checkpoint on disk via Expect: %s", row.Checkpoint)
	}
	for _, ev := range events() {
		if strings.Contains(ev.Expect, secret) {
			t.Errorf("event %+v carries the typed text in its Expect", ev)
		}
	}
}

// TestRunner_ResumeAfterRedactedFieldHoldsStepDoesNotReverify checks a checkpointed step whose Expect was already redacted comes back through resume the same way any other finished step does: never checked again, only described. A daemon that saved this checkpoint has already recorded pass or fail for it, so a resumed run must ask the model for a fresh decision rather than re-running wait_for against the marker text.
func TestRunner_ResumeAfterRedactedFieldHoldsStepDoesNotReverify(t *testing.T) {
	store := dbtest.Open(t)

	saved := Job{
		ID: "act-9", Goal: "type the passphrase", Brain: "fake", State: Stepping,
		Steps:  []Step{{N: 1, Tool: "type_text", Outcome: "pass", Expect: act.Check{Kind: act.FieldHolds, Value: db.RedactedValue}}},
		Budget: DefaultBudget(),
	}
	blob, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.SaveActJob(context.Background(), db.ActJobRow{ID: saved.ID, Goal: saved.Goal, Brain: "fake", State: string(Stepping), Checkpoint: blob}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}

	exec := &fakeExec{}
	r := New(store, exec, map[string]Model{"fake": script(doneReply("Playing."))}, "fake", func(Event) {})
	if err := r.Resume(context.Background(), "act-9", Budget{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	job := waitState(t, r, "act-9", Done, Failed, Stuck)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}
	if len(job.Steps) != 1 || job.Steps[0].Outcome != "pass" {
		t.Fatalf("steps = %+v, want the checkpointed step kept as is, still passed", job.Steps)
	}
	if calls := exec.names(); strings.Join(calls, ",") != "observe_screen" {
		t.Errorf("tool calls = %v, want only the fresh observe before the model's own decision, no re-verification of the checkpointed step", calls)
	}
}

// TestRunner_SaveRefusesACheckpointThatWillNotMarshal checks a job whose checkpoint cannot be rendered is left alone on disk and the failure is logged, rather than an empty object being written over the whole history.
func TestRunner_SaveRefusesACheckpointThatWillNotMarshal(t *testing.T) {
	r, store, _ := newRunner(t, &fakeExec{}, script(doneReply("done")))
	l, _ := r.claim("act-1")
	l.started = time.Now()
	good := Job{ID: "act-1", Goal: "play S16 E8", State: Stepping, Steps: []Step{{N: 1, Tool: "click", Outcome: "pass"}}}
	r.save(l, good)

	bad := good
	bad.Steps = []Step{{N: 1, Tool: "click", Args: map[string]any{"n": math.NaN()}}}
	if _, err := row(bad); err == nil {
		t.Fatal("row rendered a job that cannot be marshaled without an error")
	}
	r.save(l, bad)

	stored, err := store.ActJob(context.Background(), "act-1")
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	var back Job
	if err := json.Unmarshal(stored.Checkpoint, &back); err != nil {
		t.Fatalf("the stored checkpoint will not read back: %v", err)
	}
	if len(back.Steps) != 1 || back.Steps[0].Tool != "click" {
		t.Fatalf("the stored checkpoint = %+v, want the last good one still standing", back)
	}
}

// TestRunner_ResumeRaisesASpentBudget checks POST-style resume takes a budget and applies it over the spent one, so a job that ended on its budget can be sent back in with more, and the fields the caller left at zero keep what the checkpoint had. It also checks that ending on a budget says how far the job got in plain words rather than claiming the goal was reached.
//
// It is the token budget that ends this job, not the step budget: running out of steps is only a bad guess at the size of the task and now asks the user rather than ending (see outOfRoomQuestion), so tokens are the budget that still ends a job outright.
func TestRunner_ResumeRaisesASpentBudget(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		stepReply("click", "S16 E8"),
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	// Each round of the test model costs a thousand input tokens, so this runs out on the third round's check, before the reply that would have said done.
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Wall: time.Minute, InputTokens: 1500, Steps: 40}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Failed, Done, Stuck)
	if job.State != Failed || !strings.Contains(job.Err, "token") {
		t.Fatalf("job = %q (%s), want failed on the token budget", job.State, job.Err)
	}
	for _, want := range []string{"step", "play S16 E8"} {
		if !strings.Contains(job.Say, want) {
			t.Errorf("the closing words %q do not mention %q", job.Say, want)
		}
	}
	if strings.Contains(strings.ToLower(job.Say), "done") {
		t.Errorf("the closing words %q claim the job finished", job.Say)
	}
	if err := r.Resume(context.Background(), id, Budget{InputTokens: 500000}); err != nil {
		t.Fatalf("Resume with a raised budget: %v", err)
	}
	job = waitState(t, r, id, Done, Stopped, Failed)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done on the raised budget", job.State, job.Err)
	}
	if job.Budget.InputTokens != 500000 || job.Budget.Steps != 40 {
		t.Errorf("budget = %+v, want the raised tokens and the checkpoint's own step budget", job.Budget)
	}
}

// TestRunner_ResumeStillRefusesAJobTheUserEnded checks a raised budget does not reopen a job that finished or that the user stopped: only the one that ran out of budget comes back.
func TestRunner_ResumeStillRefusesAJobTheUserEnded(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("Playing now.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Done, Failed, Stuck)
	if err := r.Resume(context.Background(), id, Budget{Steps: 12}); err == nil {
		t.Fatal("a finished job was resumed")
	}
}

// TestRunner_SummaryIsRewrittenEveryFiveRounds checks the progress summary goes to the cheap summary model rather than growing the trail in the prompt.
func TestRunner_SummaryIsRewrittenEveryFiveRounds(t *testing.T) {
	exec := &fakeExec{}
	var summaries int
	var mu sync.Mutex
	cheap := func(ctx context.Context, prompt string) (string, Usage, error) {
		mu.Lock()
		summaries++
		mu.Unlock()
		return "Two episodes tried. The show page is open.", Usage{Model: "cheap", Input: 300, Output: 20}, nil
	}
	store := dbtest.Open(t)
	r := New(store, exec, map[string]Model{
		"fake":  script(stepReply("click", "S16 E8")),
		"cheap": cheap,
	}, "fake", func(Event) {})
	id, err := r.Start(context.Background(), "play S16 E8", Opts{SummaryBrain: "cheap", Budget: Budget{Wall: time.Minute, InputTokens: 200000, Steps: 6}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Failed, Done, Stuck)
	mu.Lock()
	n := summaries
	mu.Unlock()
	if n == 0 {
		t.Fatal("the summary was never rewritten over six rounds")
	}
	if !strings.Contains(job.Summary, "Two episodes tried") {
		t.Errorf("summary = %q, want the cheap model's words", job.Summary)
	}
	if job.Spend.ByModel["cheap"].Input == 0 {
		t.Errorf("per-model split = %+v, want the summary model's own cost", job.Spend.ByModel)
	}
}

// TestBuildPrompt_StaysSmallWithALongTrail checks the round prompt of a job forty steps in is still under six thousand tokens, because the trail lives in the checkpoint and only the plan, the summary, the last two observations and the last three results go to the model.
func TestBuildPrompt_StaysSmallWithALongTrail(t *testing.T) {
	listing := "Brave · Netflix\n" + strings.Repeat("[1] push button \"Some fairly long control label here\" (100,200)\n", 100)
	job := Job{
		ID: "act-1", Goal: "play season 16 episode 8 of the show", Window: "Brave", Brain: "codex",
		Plan:    strings.Repeat("first open the show page, then find the season picker, ", 20),
		Summary: strings.Repeat("The show page is open and season 16 is picked. ", 10),
		Next:    "click episode 8",
		Budget:  DefaultBudget(),
	}
	for i := 1; i <= 40; i++ {
		job.Steps = append(job.Steps, Step{N: i, Tool: "click", Args: map[string]any{"n": i}, Expect: act.Check{Kind: act.TitleContains, Value: fmt.Sprintf("thing %d", i)}, Result: listing, Outcome: "pass", Why: listing})
		job.Observations = pushCapped(job.Observations, listing, keptObservations, observationCap)
		job.Results = pushCapped(job.Results, listing, keptResults, resultCap)
	}
	for i := 0; i < 5; i++ {
		job.Answers = append(job.Answers, strings.Repeat("scroll further down the list, it is under season sixteen. ", 10))
	}
	prompt := BuildPrompt(job)
	if got := EstimateTokens(prompt); got >= 6000 {
		t.Fatalf("the round prompt is about %d tokens (%d chars), want under 6000", got, len(prompt))
	}
	for _, want := range []string{"play season 16 episode 8", "The show page is open", "40 steps", act.TitleContains} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	t.Logf("prompt with a 40-step trail: %d chars, about %d tokens", len(prompt), EstimateTokens(prompt))
}

// TestParseDecision reads the shapes a model actually replies in: bare JSON, JSON in a fenced block with words around it, and a reply that is not JSON at all.
func TestParseDecision(t *testing.T) {
	d, err := parseDecision("```json\n{\"tool\":\"click\",\"args\":{\"n\":3},\"expect\":{\"kind\":\"title_contains\",\"value\":\"S16 E8\"}}\n```")
	if err != nil {
		t.Fatalf("parseDecision: %v", err)
	}
	if d.Tool != "click" || d.Expect.Value != "S16 E8" {
		t.Errorf("decision = %+v, want the click and its expected change", d)
	}
	if _, err := parseDecision("I think we should click play."); err == nil {
		t.Error("prose parsed as a decision")
	}
}

// TestFromPromptFunc_EstimatesWhatTheSeamDoesNotReport checks the adapter over the daemon's plain prompt-in, text-out brain files its round under the brain's name and estimates both sides at four characters to the token, so the input budget still binds on a login that reports nothing.
func TestFromPromptFunc_EstimatesWhatTheSeamDoesNotReport(t *testing.T) {
	m := FromPromptFunc("claude-cli", func(ctx context.Context, prompt string) (string, error) {
		return strings.Repeat("b", 40), nil
	})
	reply, usage, err := m(context.Background(), strings.Repeat("a", 400))
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if len(reply) != 40 {
		t.Fatalf("reply = %d chars, want the brain's own answer", len(reply))
	}
	if usage.Model != "claude-cli" || usage.Input != 100 || usage.Output != 10 {
		t.Errorf("usage = %+v, want claude-cli with 100 in and 10 out", usage)
	}
}

// TestRunner_ARunningJobCanBeReadWhileItWorks checks the copy a GET is handed can be encoded while the loop goes on working. The job it is copied from has two fields the loop writes in place — the per-model spend map and the last element of the steps slice — and handing those out unguarded is a concurrent map read and write, which is not a panic but a fatal error that takes the whole daemon down. Run under -race.
func TestRunner_ARunningJobCanBeReadWhileItWorks(t *testing.T) {
	exec := &fakeExec{}
	steps := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		steps = append(steps, stepReply("click", "S16 E8"))
	}
	r, _, _ := newRunner(t, exec, script(append(steps, doneReply("Playing."))...))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stop := make(chan struct{})
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			select {
			case <-stop:
				return
			default:
			}
			job, err := r.Job(context.Background(), id)
			if err != nil {
				continue
			}
			if err := json.NewEncoder(io.Discard).Encode(job); err != nil {
				t.Errorf("encoding a running job: %v", err)
				return
			}
		}
	}()
	job := waitState(t, r, id, Done, Failed, Stuck)
	close(stop)
	<-read
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}
}

// TestRunner_StopDuringTheModelRoundLeavesTheToolUncalled checks a stop that lands while the model is still thinking takes effect before the action it decided is run. The input drivers take no context, so a keystroke or a click started after the stop really lands on the user's screen.
func TestRunner_StopDuringTheModelRoundLeavesTheToolUncalled(t *testing.T) {
	exec := &fakeExec{}
	thinking, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	model := func(ctx context.Context, prompt string) (string, Usage, error) {
		once.Do(func() { close(thinking) })
		<-release
		return stepReply("click", "S16 E8"), Usage{Model: "fake", Input: 100}, nil
	}
	r, _, _ := newRunner(t, exec, model)
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-thinking
	if err := r.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)

	job := waitState(t, r, id, Stopped, Done, Failed)
	if job.State != Stopped {
		t.Fatalf("state = %q, want stopped", job.State)
	}
	for _, name := range exec.names() {
		if name == "click" {
			t.Errorf("the job clicked after it was stopped; calls = %v", exec.names())
		}
	}
}

// TestRunner_APausedStuckJobStillTakesItsAnswer checks pausing a job that is waiting on a question does not make that question unanswerable: the answer still lands, and the job carries on once it is resumed.
func TestRunner_APausedStuckJobStillTakesItsAnswer(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		`{"ask":"Which season is it under?"}`,
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Stuck, Done, Failed)
	if err := r.Pause(id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := r.Answer(id, "season 16"); err != nil {
		t.Fatalf("Answer to a paused job that is waiting on a question: %v", err)
	}
	if err := r.Resume(context.Background(), id, Budget{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if job := waitState(t, r, id, Done, Failed, Stopped); job.State != Done {
		t.Fatalf("state = %q (%s), want done once answered and resumed", job.State, job.Err)
	}
}

// scopeKey is the value scopedExec stamps on a context to tell one screen scope from another.
type scopeKey struct{}

// scopedExec is an executor that keeps per-caller screen state the way the agent does: NewScreenScope stamps a fresh scope on the context, and every tool call records which scope it was made with.
type scopedExec struct {
	fakeExec
	smu    sync.Mutex
	scopes int
	seen   []int
	rounds int
}

func (s *scopedExec) EndScreenRound(ctx context.Context) {
	s.smu.Lock()
	s.rounds++
	s.smu.Unlock()
}

func (s *scopedExec) NewScreenScope(ctx context.Context) context.Context {
	s.smu.Lock()
	s.scopes++
	n := s.scopes
	s.smu.Unlock()
	return context.WithValue(ctx, scopeKey{}, n)
}

func (s *scopedExec) ExecuteAskTool(ctx context.Context, name string, args map[string]any) string {
	n, _ := ctx.Value(scopeKey{}).(int)
	s.smu.Lock()
	s.seen = append(s.seen, n)
	s.smu.Unlock()
	return s.fakeExec.ExecuteAskTool(ctx, name, args)
}

// scopesSeen returns the scope each tool call carried, in call order.
func (s *scopedExec) scopesSeen() []int {
	s.smu.Lock()
	defer s.smu.Unlock()
	return append([]int(nil), s.seen...)
}

// TestRunner_AJobRunsInItsOwnScreenScope checks the loop asks the executor for a screen scope once and makes every tool call of the job inside it. Without it every job shares one numbered list, so a click by number can land in another job's window, and the screenshots a job takes are recorded against a state nobody reads.
func TestRunner_AJobRunsInItsOwnScreenScope(t *testing.T) {
	exec := &scopedExec{}
	r, _, _ := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("Playing.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job := waitState(t, r, id, Done, Failed, Stuck); job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}
	if exec.scopes != 1 {
		t.Errorf("the job asked for %d screen scopes, want exactly one for the whole job", exec.scopes)
	}
	for i, n := range exec.scopesSeen() {
		if n != 1 {
			t.Fatalf("tool call %d ran in scope %d, want every call of the job in its own scope 1", i, n)
		}
	}
}

// TestRunner_TheCheckpointDoesNotCarryTheScreenListing checks the stored job keeps a screen reading as its window line and an item count rather than the listing itself, and cuts a step's result to the length the ordinary act runs keep. The listing is the live text of somebody's window — a half-written message, whatever is on the page — and the checkpoint is a lasting record.
func TestRunner_TheCheckpointDoesNotCarryTheScreenListing(t *testing.T) {
	exec := &fakeExec{result: "did click, and then " + strings.Repeat("x", 600)}
	r, store, _ := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("Playing.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job := waitState(t, r, id, Done, Failed, Stuck); job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}
	row, err := store.ActJob(context.Background(), id)
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if strings.Contains(string(row.Checkpoint), "push button") {
		t.Errorf("the checkpoint carries the screen listing itself:\n%s", row.Checkpoint)
	}
	var stored Job
	if err := json.Unmarshal(row.Checkpoint, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(stored.Observations) == 0 {
		t.Fatal("the stored job kept no screen readings at all")
	}
	for _, obs := range stored.Observations {
		if !strings.Contains(obs, "Brave · Netflix") || !strings.Contains(obs, "1 item") {
			t.Errorf("stored observation = %q, want the window line and how many items were listed", obs)
		}
	}
	if len(stored.Steps) != 1 {
		t.Fatalf("stored steps = %d, want one", len(stored.Steps))
	}
	if n := len([]rune(stored.Steps[0].Result)); n != storedResultCap {
		t.Errorf("the stored step result is %d runes, want it cut to %d", n, storedResultCap)
	}
}

// TestRow_LeavesTheJobItWasGivenAlone checks rendering a row for the store does not edit the running job's own observations or steps, since the loop goes on using them for the prompt.
func TestRow_LeavesTheJobItWasGivenAlone(t *testing.T) {
	job := Job{
		ID:           "act-1",
		Observations: []string{"Brave · Netflix\n[1] push button \"Play\" (10,10)"},
		Steps:        []Step{{N: 1, Tool: "click", Result: strings.Repeat("y", 700)}},
	}
	if _, err := row(job); err != nil {
		t.Fatalf("row: %v", err)
	}
	if !strings.Contains(job.Observations[0], "push button") {
		t.Errorf("observations = %q, want the full listing still in memory for the prompt", job.Observations)
	}
	if n := len([]rune(job.Steps[0].Result)); n != 700 {
		t.Errorf("the in-memory step result is %d runes, want the full %d", n, 700)
	}
}

// TestRunner_AStopLineRefusalIsPutToTheUser checks a tool the stop line refused becomes the job's one question rather than a result the model is invited to work around. Nothing was clicked, and no rewording of the same action gets past the stop line.
func TestRunner_AStopLineRefusalIsPutToTheUser(t *testing.T) {
	refusal := `Stopped before clicking [7] push button "Send" in "Slack". Say "yes, go ahead" if you want me to.`
	exec := &fakeExec{result: refusal}
	r, _, events := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("Playing.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Stuck, Done, Failed)
	if job.State != Stuck || job.Question != refusal {
		t.Fatalf("state = %q with question %q, want it stuck on the stop line's own words", job.State, job.Question)
	}
	for _, name := range exec.names() {
		if name == "wait_for" {
			t.Errorf("the job verified an action the stop line refused; calls = %v", exec.names())
		}
	}
	var asked bool
	for _, ev := range events() {
		if ev.Kind == "question" && ev.Text == refusal {
			asked = true
		}
	}
	if !asked {
		t.Error("the refusal never went out as a question event")
	}
}

// TestRunner_AVerifiedStepDoesNotUndoAPause checks a check that comes back after the user pressed Pause leaves the job reading as paused. Both front ends take the state word off every event, so a "verified" that says stepping puts a Pause button back on a job that is already held and leaves the user with no Resume to press. It also checks the pause reaches disk, so a daemon that stops while a job is held comes back knowing it was held rather than resuming it into the screen the user had just asked it to leave alone.
func TestRunner_AVerifiedStepDoesNotUndoAPause(t *testing.T) {
	exec := &fakeExec{block: make(chan struct{}), blockTool: "wait_for"}
	r, store, events := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("Playing.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait until the check is in flight, then pause and let it answer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(exec.names()) >= 3 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := r.Pause(id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	close(exec.block)

	// Wait for the check that was in flight to report, since that is the event that used to undo the pause.
	var verified Event
	for deadline = time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		for _, ev := range events() {
			if ev.Kind == "verified" {
				verified = ev
			}
		}
		if verified.Kind != "" {
			break
		}
	}
	if verified.Kind == "" {
		t.Fatal("the check that was in flight never reported")
	}
	if verified.State == Stepping {
		t.Errorf("the verified event says %q, want the pause the user pressed", verified.State)
	}
	if job, err := r.Job(context.Background(), id); err != nil || job.State != Paused {
		t.Fatalf("state = %q (%v), want it still paused after the check came back", job.State, err)
	}
	row, err := store.ActJob(context.Background(), id)
	if err != nil {
		t.Fatalf("ActJob: %v", err)
	}
	if row.State != string(Paused) {
		t.Errorf("the stored row says %q, want the pause on disk", row.State)
	}
	r.Stop(id)
	waitState(t, r, id, Stopped, Done, Failed)
}

// TestRunner_ResumeAsksAStuckJobsQuestionAgain checks a job read back from a checkpoint it was stuck on asks its question again rather than starting to drive the screen while the user is still deciding what to answer.
func TestRunner_ResumeAsksAStuckJobsQuestionAgain(t *testing.T) {
	store := dbtest.Open(t)
	saved := Job{
		ID: "act-4", Goal: "play S16 E8", Brain: "fake", State: Stuck,
		Question: "Which season is it under?", Budget: DefaultBudget(),
	}
	blob, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.SaveActJob(context.Background(), db.ActJobRow{ID: saved.ID, Goal: saved.Goal, Brain: "fake", State: string(Stuck), Checkpoint: blob}); err != nil {
		t.Fatalf("SaveActJob: %v", err)
	}

	r := New(store, &fakeExec{}, map[string]Model{"fake": script(stepReply("click", "S16 E8"), doneReply("Playing."))}, "fake", func(Event) {})
	if err := r.Resume(context.Background(), "act-4", Budget{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	job := waitState(t, r, "act-4", Stuck, Done, Failed)
	if job.State != Stuck || job.Question != saved.Question {
		t.Fatalf("the resumed job is %q asking %q, want it stuck on the question it was waiting on", job.State, job.Question)
	}
	if err := r.Answer("act-4", "season 16"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if job := waitState(t, r, "act-4", Done, Failed, Stopped); job.State != Done {
		t.Fatalf("state after the answer = %q (%s), want done", job.State, job.Err)
	}
}

// gateStore is a store whose write of a row in one state is held until the test lets it through, so a test can look at a job in the moment between its last checkpoint starting and its live slot being given up.
type gateStore struct {
	*db.Store
	state string
	gate  chan struct{}
	once  sync.Once
	hit   chan struct{}
}

func (g *gateStore) SaveActJob(ctx context.Context, row db.ActJobRow) error {
	if row.State == g.state {
		g.once.Do(func() { close(g.hit) })
		<-g.gate
	}
	return g.Store.SaveActJob(ctx, row)
}

// TestRunner_AnswerIsRefusedOnceTheJobHasEnded checks an answer sent to a job that has already been stopped is refused rather than reported as landed. The live slot outlives the stop by as long as the last checkpoint takes to write, and an answer dropped into it is never read by anyone.
func TestRunner_AnswerIsRefusedOnceTheJobHasEnded(t *testing.T) {
	inner := dbtest.Open(t)
	store := &gateStore{Store: inner, state: string(Stopped), gate: make(chan struct{}), hit: make(chan struct{})}
	r := New(store, &fakeExec{}, map[string]Model{"fake": script(`{"ask":"Which season is it under?"}`)}, "fake", func(Event) {})
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Stuck, Done, Failed)
	if err := r.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-store.hit
	if err := r.Answer(id, "season 16"); err == nil {
		t.Error("an answer to a job that had already been stopped was accepted")
	}
	close(store.gate)
	waitState(t, r, id, Stopped, Done, Failed)
}

// TestRunner_TheWallBudgetStillEndsAJobAfterItHasWaited checks the wall-clock guard is still watching after a job has waited on the user: the time spent waiting is the user's and does not count, but the work done after the answer does, and it must still be able to end the job.
func TestRunner_TheWallBudgetStillEndsAJobAfterItHasWaited(t *testing.T) {
	var rounds atomic.Int64
	model := func(ctx context.Context, prompt string) (string, Usage, error) {
		if rounds.Add(1) == 1 {
			return `{"ask":"Which season is it under?"}`, Usage{Model: "fake", Input: 10}, nil
		}
		time.Sleep(40 * time.Millisecond)
		return stepReply("click", "S16 E8"), Usage{Model: "fake", Input: 10}, nil
	}
	r, _, _ := newRunner(t, &fakeExec{}, model)
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Wall: 200 * time.Millisecond}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Stuck, Done, Failed)
	time.Sleep(400 * time.Millisecond)
	if err := r.Answer(id, "season 16"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	job := waitState(t, r, id, Failed, Done, Stopped)
	if job.State != Failed || !strings.Contains(job.Err, "wall-clock") {
		t.Fatalf("state = %q (%s), want it failed on the wall-clock budget once it went back to work", job.State, job.Err)
	}
}

// A check that was already true before the action says nothing about what the action did: a title_contains "Netflix" written after a click passes on a window that was already called that. The step loop takes the check once before it acts and records that pre-reading beside the outcome, so the model is told and done cannot ride on it — while the outcome word itself stays "pass", because the wire carries two outcomes and a flag, not three outcomes.
func TestRunner_ACheckThatAlreadyHeldCannotCarryADone(t *testing.T) {
	exec := &fakeExec{heldBefore: true}
	r, _, events := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("It is playing S16 E8."), stepReply("click", "S16 E8")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Stuck, Done, Failed)

	if job.State == Done {
		t.Fatalf("state = done, want the job refused to end on a step whose check already held")
	}
	if len(job.Steps) == 0 {
		t.Fatal("no steps recorded")
	}
	first := job.Steps[0]
	if !first.HeldBefore {
		t.Errorf("step = %+v, want the pre-reading recorded on it", first)
	}
	if first.Outcome != "pass" {
		t.Errorf("outcome = %q, want the plain pass the check itself gave, with held_before saying what it is worth", first.Outcome)
	}
	if !strings.Contains(first.Why, "already held before the action") {
		t.Errorf("why = %q, want it to say the check already held before the action", first.Why)
	}
	if got := exec.preChecksSeen(); len(got) == 0 || got[0] != "S16 E8" {
		t.Errorf("pre-readings = %v, want the step's own check taken before the action", got)
	}
	var verified []Event
	for _, ev := range events() {
		if ev.Kind == "verified" {
			verified = append(verified, ev)
		}
	}
	if len(verified) == 0 {
		t.Fatal("no verified event went out")
	}
	if verified[0].Outcome != "pass" || !verified[0].HeldBefore {
		t.Errorf("verified event = %+v, want outcome pass with held_before set, which is what the window renders", verified[0])
	}
}

// A check that already held told the job nothing, so it is neither progress nor a failure: it must not clear the stuck counter and must not move it on. Counting it as a failure is what used to stop a job three ordinary steps into a window whose title its checks all named.
func TestRunner_ACheckThatAlreadyHeldIsNotCountedAsAFailure(t *testing.T) {
	exec := &fakeExec{heldBefore: true}
	r, _, _ := newRunner(t, exec, script(stepReply("click", "S16 E8")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Steps: 4}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Failed, Done, Stuck, Stopped)

	// Spending the steps is now a question rather than an end, and the question it asks is the out-of-room one, not the three-failed-checks one — which is the whole point: none of those checks counted as a failure.
	if job.State != Stuck || !strings.Contains(job.Question, "not there yet") {
		t.Fatalf("state = %q, question = %q, want it to run out of room rather than get stuck on failed checks", job.State, job.Question)
	}
	if job.FailsInARow != 0 {
		t.Errorf("fails in a row = %d after %d steps whose checks already held, want 0", job.FailsInARow, len(job.Steps))
	}
	if !strings.Contains(BuildPrompt(job), "0 of which checked out") {
		t.Error("the progress line counted a check that already held as a step that checked out")
	}
}

// A look or an observe_screen changes nothing on the screen, so a check written for it can only fail, and on 2026-09-09 a job spent the first of its three tries on a look that came back fine. A read carries no check, calls no wait_for, and does not move the stuck counter either way.
func TestRunner_AReadIsNotCheckedAndDoesNotCountTowardStuck(t *testing.T) {
	exec := &fakeExec{verdicts: []bool{false, false, true}}
	r, _, _ := newRunner(t, exec, script(
		stepReply("look", "Spotify"),
		stepReply("click", "S16 E8"),
		stepReply("observe_screen", "Spotify"),
		stepReply("click", "S16 E8"),
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Stuck, Done, Failed)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done: two failed clicks with reads between them are not three failures in a row", job.State, job.Err)
	}
	waits := 0
	for _, c := range exec.calls {
		if c == "wait_for" {
			waits++
		}
	}
	if waits != 3 {
		t.Errorf("wait_for ran %d times, want 3: once per click and never for a read", waits)
	}
	if job.Steps[0].Outcome != "read" || job.Steps[0].Expect.Kind != "" {
		t.Errorf("the look step = %+v, want outcome read and no check", job.Steps[0])
	}
}

// The stuck question used to be stitched from the check's description and the verifier's reason: "I tried 3 times to make it so that the title contains Screenshot, and each time 5s: the title is Claude". It is written for a person, and carries what the last tool answered, which is what the person needs to help.
func TestStuckQuestion_IsPlainAndCarriesTheLastResult(t *testing.T) {
	j := Job{Next: "open a screen recorder", Steps: []Step{{Tool: "open_app", Result: `error: no installed application is named "OBS Studio"`, Expect: act.Check{Kind: act.TitleContains, Value: "OBS"}, Why: "5s: the title is \"Claude\""}}}
	q := stuckQuestion(j)
	for _, want := range []string{"open a screen recorder", "open_app", "OBS Studio", "What should I do"} {
		if !strings.Contains(q, want) {
			t.Errorf("question %q lacks %q", q, want)
		}
	}
	if strings.Contains(q, "make it so that") {
		t.Errorf("question %q still reads like the old stitched sentence", q)
	}
}

// The model is told what machine it is on: on 2026-09-09 it hunted for a recording application because nothing said GNOME on Wayland, where recording is a built-in shortcut. The line is read from the session, not written in.
func TestBuildPrompt_NamesTheDesktop(t *testing.T) {
	t.Setenv("XDG_CURRENT_DESKTOP", "ubuntu:GNOME")
	t.Setenv("XDG_SESSION_TYPE", "wayland")
	p := BuildPrompt(Job{Goal: "record the screen"})
	if !strings.Contains(p, "ubuntu:GNOME") || !strings.Contains(p, "wayland") {
		t.Errorf("prompt does not name the desktop: %q", p[:min(len(p), 200)])
	}
}

// A job shares one screen scope across all its rounds, and the look allowance on that scope is meant for one question, so on 2026-09-09 a job went blind at its third look and pressed keys it could not see the effect of. The runner hands the allowance back at the start of every round; the picture the last look took stays, since click_at aims by it in the round after.
func TestRunner_GivesTheLookAllowanceBackEveryRound(t *testing.T) {
	exec := &scopedExec{}
	r, _, _ := newRunner(t, exec, script(stepReply("look", ""), stepReply("click", "S16 E8"), doneReply("done")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Done, Failed)
	exec.smu.Lock()
	rounds := exec.rounds
	exec.smu.Unlock()
	if rounds < 3 {
		t.Errorf("EndScreenRound ran %d times, want once per round (three rounds: look, click, done)", rounds)
	}
}

// A job could only ever poke the screen. Its prompt named eleven tools, all of them screen or app actions, so nothing in a forty-step job could look anything up or remember anything: Orient had one input, the pixels in front of it. The tools were reachable through the Executor seam the whole time — ExecuteAskTool handles branch, query_memory and recall — so the only thing stopping a job from using them was that the prompt never said they existed.
func TestSystemPromptOffersSearchAndMemory(t *testing.T) {
	for _, tool := range []string{"branch", "query_memory", "recall"} {
		if !strings.Contains(systemPrompt, tool) {
			t.Errorf("the job's system prompt never names %q, so a job can never call it", tool)
		}
	}
}

// Searching and remembering change nothing on the screen, so the verify step has nothing to wait for. They have to count as reads, or every lookup would be checked against a screen change that was never coming and would fail, three of them in a row would trip the stuck counter, and the job would stop to ask the user why its own research did not move the page.
func TestSearchAndMemoryCountAsReads(t *testing.T) {
	for _, tool := range []string{"branch", "query_memory", "recall"} {
		if !isRead(tool) {
			t.Errorf("isRead(%q) = false; a lookup changes no screen and must not be checked against one", tool)
		}
	}
	// The actions must still be checked. A click that is treated as a read is a click nothing verifies.
	for _, tool := range []string{"click", "type_text", "press_key", "click_at", "open_app", "open_url", "switch_window", "scroll_to"} {
		if isRead(tool) {
			t.Errorf("isRead(%q) = true; an action has to be verified", tool)
		}
	}
}

// wallGuard took the wall budget by value when the job started, so a job that raised its own budget mid-flight kept the clock it was born with and got killed on the old one. It has to read the budget the job holds now.
func TestWallGuard_HonorsABudgetRaisedMidFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &live{
		job:     Job{Budget: Budget{Wall: 150 * time.Millisecond}},
		ctx:     ctx,
		cancel:  cancel,
		started: time.Now(),
		working: make(chan struct{}, 1),
	}
	go wallGuard(l, l.job.Budget.Wall)

	// Raise it well past what the guard was started with, before the original would have fired.
	l.set(func(j *Job) { j.Budget.Wall = 10 * time.Second })

	select {
	case <-ctx.Done():
		t.Fatal("the job was cancelled on the wall budget it started with, not the one it now holds")
	case <-time.After(500 * time.Millisecond):
	}
}

// estimateReply is a step reply that also carries the model's own guess at how many steps the whole task needs.
func estimateReply(tool, expect string, estimate int) string {
	b, _ := json.Marshal(map[string]any{
		"plan":     "click play, then check the title",
		"next":     "click " + tool,
		"tool":     tool,
		"args":     map[string]any{"n": 1},
		"expect":   map[string]string{"kind": act.TitleContains, "value": expect},
		"estimate": estimate,
	})
	return string(b)
}

// The model is the only thing that knows roughly how big a task is, and looking at the screen and switching windows are steps too, so its own guess is the honest starting point. It gets twice what it asks for, because the whole point is room to get things wrong and still finish.
func TestRunner_DoublesTheModelsOwnEstimate(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		estimateReply("click", "S16 E8", 20),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if job.Estimate != 20 {
		t.Errorf("estimate = %d, want the 20 the model asked for", job.Estimate)
	}
	if job.Budget.Steps != 40 {
		t.Errorf("step budget = %d, want twice the estimate", job.Budget.Steps)
	}
}

// An estimate only ever adds room. A model that lowballs a task must not talk its own budget down below what it was given, or a bad guess on round one becomes a task that cannot finish.
func TestRunner_ASmallEstimateNeverShrinksTheBudget(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		estimateReply("click", "S16 E8", 2),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Steps: 30}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if job.Budget.Steps != 30 {
		t.Errorf("step budget = %d, want the 30 it was given rather than twice a two-step guess", job.Budget.Steps)
	}
}

// There are two honest ways for a task to end: it finishes, or it admits it cannot and asks for help. Running out of room is the second of those, not a silent failure, so it asks — and saying yes has to actually buy it more room, or it would ask again on the very next round forever.
func TestRunner_OutOfRoomAsksRatherThanFailing(t *testing.T) {
	exec := &fakeExec{}
	r, _, _ := newRunner(t, exec, script(
		estimateReply("click", "S16 E8", 1),
		stepReply("click", "S16 E8"),
		stepReply("click", "S16 E8"),
		doneReply("Playing now."),
	))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{Budget: Budget{Wall: time.Minute, InputTokens: 200000, Steps: 1}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Stuck, Failed, Done)
	if job.State != Stuck {
		t.Fatalf("state = %q (%s), want it to ask for help rather than fail", job.State, job.Err)
	}
	// The question has to say what it has spent and that it has not got there, so the answer is an informed one.
	if !strings.Contains(job.Question, "step") {
		t.Errorf("the question %q does not mention the steps it has taken", job.Question)
	}
	before := job.Budget.Steps
	if err := r.Answer(id, "yes, keep going"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	job = waitState(t, r, id, Done, Failed)
	if job.State != Done {
		t.Fatalf("state = %q (%s) after being told to carry on, want done", job.State, job.Err)
	}
	if job.Budget.Steps <= before {
		t.Errorf("step budget = %d after a yes, want more than the %d it had", job.Budget.Steps, before)
	}
}

// Chaining taps in one call is the efficiency a task earns by getting good, and it was reachable from the voice path only. A job that cannot chain pays a model round trip for every tap of a menu that has already closed by the time the round comes back.
func TestSystemPromptOffersClickChaining(t *testing.T) {
	if !strings.Contains(systemPrompt, "then") {
		t.Error("the job's system prompt never mentions chaining taps, so a job pays a round trip per tap")
	}
}

// What he likes about watching an agent work is that it says what it is going to do and then does it, so you can tell a deviation from a plan you were told. A job writes a plan on its first round and carries it in every later prompt, but for its whole life it never left the job: the event stream opened with the goal, which is the user's own words handed back, and then went straight to "Step 1". Nothing said what the thirty steps about to arrive were for.
func TestRunner_SaysThePlanBeforeItStarts(t *testing.T) {
	first, _ := json.Marshal(map[string]any{
		"plan":     "open Netflix, find the episode, press play",
		"estimate": 6,
		"next":     "click play",
		"tool":     "click",
		"args":     map[string]any{"n": 1},
		"expect":   map[string]string{"kind": act.TitleContains, "value": "S16 E8"},
	})
	exec := &fakeExec{}
	r, _, events := newRunner(t, exec, script(string(first), doneReply("It is playing S16 E8.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Done, Failed, Stuck)

	all := events()
	planAt, stepAt := -1, -1
	for i, ev := range all {
		if ev.Kind == "plan" && planAt < 0 {
			planAt = i
		}
		if ev.Kind == "step" && stepAt < 0 {
			stepAt = i
		}
	}
	if planAt < 0 {
		t.Fatal("no plan event, so the plan the model wrote never reached the user")
	}
	if plan := all[planAt]; plan.Text != "open Netflix, find the episode, press play" {
		t.Errorf("plan event says %q, want the plan the model actually wrote", plan.Text)
	}
	// The estimate rides the plan event: it is written in the same breath, it is the model's own guess at the size of the job, and it is what makes the step count arriving afterwards mean anything.
	if plan := all[planAt]; plan.Step != 6 {
		t.Errorf("plan event carries estimate %d, want the model's own estimate of 6 steps", plan.Step)
	}
	if stepAt >= 0 && planAt > stepAt {
		t.Errorf("the plan arrived at event %d, after the first step at %d — it has to come before the work", planAt, stepAt)
	}
	// One plan per job: a plan restated every round is not a plan, and the job already refuses to rewrite the one it is working to.
	planned := 0
	for _, ev := range all {
		if ev.Kind == "plan" {
			planned++
		}
	}
	if planned != 1 {
		t.Errorf("%d plan events, want exactly one", planned)
	}
}

// refExec is an executor that also supplies the past-run reference block, the way the daemon's agent does.
type refExec struct {
	fakeExec
	block string
	asked []string
}

func (e *refExec) ActReferenceFor(ctx context.Context, goal string, now time.Time) string {
	e.asked = append(e.asked, goal)
	return e.block
}

// A job planned from nothing every time. The block that shows a model its own past runs, step by step, with the lessons it drew from them, has been built and populated since 2026-09-09 and had exactly one caller: the ask path, at agent/ask.go. The long-horizon path never read it, so a task done yesterday was re-derived from scratch today while a one-shot screen question got the benefit of history.
func TestRunner_PlansAgainstWhatItDidBefore(t *testing.T) {
	exec := &refExec{block: "WHAT YOU DID LAST TIME\n- 2 days ago, \"play the next episode\": clicked Netflix, clicked play"}
	r, _, _ := newRunner(t, exec, script(stepReply("click", "S16 E8"), doneReply("Playing.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, r, id, Done, Failed, Stuck)

	if len(exec.asked) == 0 {
		t.Fatal("the job never asked for the reference block, so it planned against nothing")
	}
	if exec.asked[0] != "play S16 E8" {
		t.Errorf("looked up %q, want the job's own goal", exec.asked[0])
	}
	// Looked up once for the job, not once a round: the block is about the goal, which does not change, and a lookup every round would pay an embedding search forty times over for the same answer.
	if len(exec.asked) != 1 {
		t.Errorf("looked it up %d times, want once for the whole job", len(exec.asked))
	}
	if !strings.Contains(BuildPrompt(mustJob(t, r, id)), "WHAT YOU DID LAST TIME") {
		t.Error("the reference block is not in the job's prompt")
	}
}

// mustJob reads a job back or fails the test.
func mustJob(t *testing.T, r *Runner, id string) Job {
	t.Helper()
	job, err := r.Job(context.Background(), id)
	if err != nil {
		t.Fatalf("Job(%s): %v", id, err)
	}
	return job
}

// burstReply is a decision that takes several actions in one round, checked once at the end.
func burstReply(expect string, tools ...string) string {
	var then []map[string]any
	for _, t := range tools[1:] {
		then = append(then, map[string]any{"tool": t, "args": map[string]any{"n": 1}})
	}
	b, _ := json.Marshal(map[string]any{
		"plan":   "do the sequence that worked last time",
		"next":   "the whole burst",
		"tool":   tools[0],
		"args":   map[string]any{"n": 1},
		"then":   then,
		"expect": map[string]string{"kind": act.TitleContains, "value": expect},
	})
	return string(b)
}

// One action a round is what a job had to pay whether or not it had done the task before, so a sequence of five known taps cost five model rounds, five screen readings and five checks. The frontier's answer is to dispatch a predicted sequence in one go and check the end state once, which is also the cheapest thing: four of those five rounds were the cost, not the taps.
func TestRunner_TakesAKnownSequenceInOneRound(t *testing.T) {
	exec := &fakeExec{}
	r, _, events := newRunner(t, exec, script(burstReply("S16 E8", "click", "click", "press_key"), doneReply("Playing.")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Done, Failed, Stuck)
	if job.State != Done {
		t.Fatalf("state = %q (%s), want done", job.State, job.Err)
	}

	// Every action in order, then one check for the whole burst — not one check each.
	calls := exec.names()
	want := []string{"observe_screen", "click", "click", "press_key", "wait_for", "observe_screen"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("tool calls = %v, want %v", calls, want)
	}
	// One round, so one step: the budget counts model rounds, which is what a job's cost actually is, and a burst that buys three actions for one round should buy room as well as time.
	if len(job.Steps) != 1 {
		t.Fatalf("steps = %d, want the burst recorded as one step", len(job.Steps))
	}
	// The checkpoint has to say every action the burst took. A daemon that dies mid-burst and comes back believing one click happened would take the other two again on the user's real screen, which is the guarantee the one-action loop gave and a burst must not quietly drop.
	if got := job.Steps[0].Burst; len(got) != 3 {
		t.Fatalf("the step records %d actions, want all 3 the burst took", len(got))
	}
	if job.Steps[0].Burst[2].Tool != "press_key" {
		t.Errorf("the last recorded action is %q, want press_key", job.Steps[0].Burst[2].Tool)
	}
	if job.Spend.Rounds != 2 {
		t.Errorf("rounds = %d, want two: the burst and the done", job.Spend.Rounds)
	}
	// The trail has to say what actually happened on the screen, or a user watching sees one click where three landed.
	var stepLine string
	for _, ev := range events() {
		if ev.Kind == "step" {
			stepLine = ev.Text
		}
	}
	if !strings.Contains(stepLine, "3") {
		t.Errorf("the step line says %q, want it to say how many actions the burst took", stepLine)
	}
}

// A burst whose check fails says nothing about which action in it went wrong, so the job must not credit any of them and must not be left thinking one particular tap failed.
func TestRunner_ABurstThatFailsItsCheckCountsAsOneFailure(t *testing.T) {
	exec := &fakeExec{verdicts: []bool{false}}
	r, _, _ := newRunner(t, exec, script(burstReply("S16 E8", "click", "click")))
	id, err := r.Start(context.Background(), "play S16 E8", Opts{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitState(t, r, id, Stuck, Failed, Done)
	if job.FailsInARow != stuckAfter {
		t.Errorf("fails in a row = %d, want the burst to count once each time, reaching %d", job.FailsInARow, stuckAfter)
	}
	if job.State != Stuck {
		t.Errorf("state = %q, want stuck once the same burst has failed %d times", job.State, stuckAfter)
	}
}
