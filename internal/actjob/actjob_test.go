package actjob

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"june/internal/act"
	"june/internal/db"
	"june/internal/db/dbtest"
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
