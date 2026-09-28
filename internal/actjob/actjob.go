// Package actjob runs one long computer-use goal as a job inside the daemon rather than inside an HTTP request: it plans, then takes one checked step at a time until the goal is reached, the budget is spent, or it has to ask the user something. Every step is written to the store as it happens (see db.SaveActJob), so a daemon that stops mid-task picks the job up from the last step it finished instead of starting the goal again.
// What it deliberately does not own: the tools, the tool gate and the stop line all belong to the ask path (see agent.Agent.ExecuteAskTool), and this package reaches them through the Executor seam so there is one copy of the rules about what may be clicked and what may not. Verification is pure string matching (see act.Match), so checking a step costs no model call at all — which is most of why a forty-step job is affordable on a subscription.
package actjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/act"
	"june/internal/db"
	"june/internal/util"
)

// State is where a job has got to. A job is planning until its first decision, stepping while it acts, verifying while it checks what its action did, and then reaches one of the four ends — or waits in paused or stuck for the user.
type State string

const (
	// Planning is the first round, before any action has been decided.
	Planning State = "planning"
	// Stepping is deciding and running one action.
	Stepping State = "stepping"
	// Verifying is checking that the expected change came.
	Verifying State = "verifying"
	// Paused is the user having asked it to wait; it takes no step until resumed.
	Paused State = "paused"
	// Stuck is three failed verifications on one step: the job has asked the user one question and waits for the answer.
	Stuck State = "stuck"
	// Done is the goal reached, in the model's own closing words.
	Done State = "done"
	// Stopped is the user having ended it.
	Stopped State = "stopped"
	// Failed is the budget spent or the model unreachable; Job.Say still says how far it got.
	Failed State = "failed"
)

// Budget is what one job may spend before it must stop and say how far it got. Wall time and steps are the two a user feels; the input-token budget is the one that matters on a subscription, since every round re-sends the prompt.
type Budget struct {
	Wall        time.Duration `json:"wall"`
	InputTokens int           `json:"input_tokens"`
	Steps       int           `json:"steps"`
}

// DefaultBudget is what a job gets when the caller names none: five minutes, two hundred thousand input tokens, forty steps. Measured against the 2026-09-05 runs, where a twelve-round ask cost 48k input and the long evals 150k to 165k, so 200k is about one long task and not two.
func DefaultBudget() Budget {
	return Budget{Wall: 5 * time.Minute, InputTokens: 200_000, Steps: 40}
}

// withDefaults fills in any part of a budget the caller left at zero.
func (b Budget) withDefaults() Budget {
	d := DefaultBudget()
	if b.Wall <= 0 {
		b.Wall = d.Wall
	}
	if b.InputTokens <= 0 {
		b.InputTokens = d.InputTokens
	}
	if b.Steps <= 0 {
		b.Steps = d.Steps
	}
	return b
}

// raise applies a budget a caller asked for over the one a job already has, field by field: a zero field leaves the job's own value alone, so resuming a job that ran out of steps with more of them does not also reset its wall clock or its tokens. Input: the budget asked for. Output: the merged budget.
func (b Budget) raise(over Budget) Budget {
	if over.Wall > 0 {
		b.Wall = over.Wall
	}
	if over.InputTokens > 0 {
		b.InputTokens = over.InputTokens
	}
	if over.Steps > 0 {
		b.Steps = over.Steps
	}
	return b
}

// Opts is what a caller may choose per job: which window it is about, which brain thinks for it, which cheaper brain rewrites its progress summary, and what it may spend.
type Opts struct {
	Window string `json:"window"`
	// Brain names the model this job thinks with, from the runner's own map. Empty means the daemon's configured brain, which already carries the fallback chain; nothing here ever picks an API-key path of its own.
	Brain string `json:"brain"`
	// SummaryBrain names the model that rewrites the progress summary, which is a two-sentence job a cheap model does as well as an expensive one. Empty means the job's own brain.
	SummaryBrain string `json:"summary_brain"`
	Budget       Budget `json:"budget"`
}

// Usage is what one round of thinking cost, as the model reported it. A model that reports nothing leaves the counts at zero, so the round is still on record as having happened rather than carrying a guess.
type Usage struct {
	Model  string `json:"model"`
	Input  int    `json:"input"`
	Cached int    `json:"cached"`
	Output int    `json:"output"`
}

// Spend is what a whole job has cost: the rounds it took, the tokens each side of them, and the same counts split by model so the user can compare two brains on the same task.
type Spend struct {
	Rounds  int              `json:"rounds"`
	Input   int              `json:"input"`
	Cached  int              `json:"cached"`
	Output  int              `json:"output"`
	ByModel map[string]Usage `json:"by_model"`
}

// add folds one round's usage into the running total, both overall and under the model that served it. Input: the round's usage and the brain name to file it under when the model reported no name of its own. Output: none.
func (s *Spend) add(u Usage, fallbackName string) {
	name := u.Model
	if name == "" {
		name = fallbackName
	}
	s.Rounds++
	s.Input += u.Input
	s.Cached += u.Cached
	s.Output += u.Output
	if s.ByModel == nil {
		s.ByModel = map[string]Usage{}
	}
	was := s.ByModel[name]
	s.ByModel[name] = Usage{Model: name, Input: was.Input + u.Input, Cached: was.Cached + u.Cached, Output: was.Output + u.Output}
}

// Step is one action a job took: the tool it called, what it called it with, the change it wrote down beforehand, what the tool said, and whether the change actually came.
type Step struct {
	N      int            `json:"n"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Expect act.Check      `json:"expect"`
	Result string         `json:"result"`
	// Outcome is "pass" or "fail", from the wait_for check alone. A check that was already satisfied before the action still reads "pass" here, with HeldBefore set beside it; there is no third word, so a reader that knows only the two still gets a true answer.
	Outcome string `json:"outcome"`
	// Why is what the check found, in plain words, whichever way it went.
	Why string `json:"why"`
	// HeldBefore is what the same check said when it was taken once before the action ran. True means the screen already satisfied it, so the reading taken afterwards says nothing about what the action did.
	HeldBefore bool `json:"held_before,omitempty"`
	// Burst is every action this step took, in order, when the round took more than one. Tool and Args above are the first of them, so a reader that knows nothing about bursts still reads the step correctly.
	// Recorded because the checkpoint is the only thing a restarted daemon knows: a job that died mid-burst and came back believing one click had happened would take the rest again on the user's real screen. Arguments are redacted the same way Args is.
	Burst []action `json:"burst,omitempty"`
}

// Job is the whole checkpoint: everything a fresh Runner needs to carry on where the last one stopped. It is stored as JSON in the act_runs row's job_json column and is the only place the full trail lives — the prompt sent each round carries a small part of it (see BuildPrompt).
type Job struct {
	ID           string    `json:"id"`
	Goal         string    `json:"goal"`
	Window       string    `json:"window"`
	Brain        string    `json:"brain"`
	SummaryBrain string    `json:"summary_brain"`
	State        State     `json:"state"`
	Plan         string    `json:"plan"`
	Summary      string    `json:"summary"`
	Next         string    `json:"next"`
	Steps        []Step    `json:"steps"`
	Observations []string  `json:"observations"`
	Results      []string  `json:"results"`
	Answers      []string  `json:"answers"`
	Question     string    `json:"question"`
	Say          string    `json:"say"`
	Err          string    `json:"err"`
	Budget       Budget    `json:"budget"`
	Spend        Spend     `json:"spend"`
	StartedAt    time.Time `json:"started_at"`
	ElapsedMS    int64     `json:"elapsed_ms"`
	// FailsInARow counts failed verifications since the last one that passed; three of them is what makes a job stuck.
	FailsInARow int `json:"fails_in_a_row"`
	// Estimate is what the model guessed the whole task would take, in steps, on the round it first said so. Kept on the job because the question it asks when it runs out names the guess against what it actually spent, and because a job resumed from a checkpoint should not re-guess.
	Estimate int `json:"estimate"`
	// Reference is what this machine did the last few times it was asked something like this goal, and the lessons drawn from those runs, rendered by whoever supplied it (see Referencer). Looked up once, when the run starts, because it is about the goal and the goal does not change; carried on the job so a resumed run reads the same history rather than searching again.
	Reference string `json:"reference,omitempty"`
}

// Event is one line of a job's progress, broadcast as it happens so the hover can show a running task without a window of its own. Kind is "started", "step", "verified", "question", "answered", "paused", "resumed" or "done"; the last of those carries Spend.
type Event struct {
	Job     string `json:"job"`
	Kind    string `json:"kind"`
	State   State  `json:"state"`
	Step    int    `json:"step"`
	Text    string `json:"text"`
	Expect  string `json:"expect,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	// HeldBefore says the step's check was already satisfied before the action ran, so the pass beside it says nothing about what the action did. It rides next to the outcome rather than replacing it, so a client that knows only "pass" and "fail" still reads the event.
	HeldBefore bool   `json:"held_before,omitempty"`
	Spend      *Spend `json:"spend,omitempty"`
}

// Model is one round of a job's thinking: the prompt in, the model's reply and what the round cost out. The daemon wires one per brain name; a brain that reports no token counts leaves them at zero rather than guessing.
type Model func(ctx context.Context, prompt string) (reply string, usage Usage, err error)

// FromPromptFunc adapts the daemon's one-shot text seam (internal/brain.Brain: a prompt in, an answer out) to the model a job thinks with. Input: the name to file this model's cost under, and the function that answers a prompt. Output: the Model.
// That seam carries no token counts — none of the CLI logins report any — so the counts here are the same four-characters-to-the-token estimate the rest of the codebase already applies to a provider that reports none (see internal/tally). They are an estimate on purpose: without them the input-token budget could never bind and two brains could not be compared on the same task, and a job that never counted its own prompt is a job that cannot be stopped before it is expensive.
func FromPromptFunc(name string, ask func(context.Context, string) (string, error)) Model {
	return func(ctx context.Context, prompt string) (string, Usage, error) {
		reply, err := ask(ctx, prompt)
		return reply, Usage{Model: name, Input: EstimateTokens(prompt), Output: EstimateTokens(reply)}, err
	}
}

// Executor runs one tool for a job, through the same gated path an ask's own tool loop uses (agent.Agent.ExecuteAskTool): the tool gate, the stop line and the screen tools are the ask's, not a second copy of them.
type Executor interface {
	ExecuteAskTool(ctx context.Context, name string, args map[string]any) string
}

// ScreenScope creates a fresh screen-tool namespace so concurrent jobs do not stomp on each other's numbered lists or focus state. Optional: when the executor implements it, NewScreenScope is called once per job run.
type ScreenScope interface {
	NewScreenScope(ctx context.Context) context.Context
}

// Referencer supplies what this machine did the last few times it was asked something like this goal. Optional: when the executor implements it, it is called once per run and the block goes in every round's prompt.
// The one caller of this block used to be the ask path (agent/ask.go), so a one-shot screen question planned against its own history and a forty-step job planned against nothing. A task done yesterday was re-derived from scratch today, which is the whole of why a known task was no faster the second time.
type Referencer interface {
	ActReferenceFor(ctx context.Context, goal string, now time.Time) string
}

// PreChecker tests whether an expected change already holds on the screen before the action runs, off a single reading. Optional: when missing, HeldBefore stays false.
type PreChecker interface {
	CheckHolds(ctx context.Context, expect act.Check) bool
}

// Store is what a job needs from the database: write its checkpoint after every step, read one back to resume it, and say what the highest job id already on disk is so a restarted daemon carries on numbering from there.
type Store interface {
	SaveActJob(ctx context.Context, row db.ActJobRow) error
	ActJob(ctx context.Context, id string) (db.ActJobRow, error)
	MaxActJobNumber(ctx context.Context) (uint64, error)
}

// Runner owns every job this daemon is running. One Runner is shared by every route.
type Runner struct {
	store  Store
	exec   Executor
	models map[string]Model
	// brain is the daemon's configured brain, used by a job that names none.
	brain string
	emit  func(Event)

	nextID atomic.Uint64
	mu     sync.Mutex
	live   map[string]*live
}

// New builds the runner. Input: the store checkpoints go to, the executor tools run through, the models by brain name, the name of the daemon's configured brain (used by a job that names none), and the sink progress events go to, which may be nil. Output: the runner.
// The id counter is seeded from the store: a daemon that started counting at zero again would hand a new job an id an older job still holds, and the older job's whole history would be rewritten under it.
func New(store Store, exec Executor, models map[string]Model, defaultBrain string, emit func(Event)) *Runner {
	if emit == nil {
		emit = func(Event) {}
	}
	r := &Runner{store: store, exec: exec, models: models, brain: defaultBrain, emit: emit, live: map[string]*live{}}
	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	if most, err := store.MaxActJobNumber(ctx); err != nil {
		slog.Error("act job: could not read the highest job id on disk; a new job may be refused for clashing with a stored one", "error", err)
	} else {
		r.nextID.Store(most)
	}
	return r
}

// ErrNoGoal and ErrUnknownBrain are the two ways a caller can get Start wrong, as opposed to the ways the daemon itself can fail it (a store that will not take the checkpoint, an id that clashes with a live job). POST /act tells them apart with errors.Is to answer 400 rather than 500 — see internal/ipc/actjob.go.
var (
	ErrNoGoal       = errors.New("a job needs a goal")
	ErrUnknownBrain = errors.New("no such brain")
)

// Start opens a job for a goal and runs it in the background. Input: a context used only for the first checkpoint write — the job itself outlives the request that asked for it — the goal in the user's own words, and the options. Output: the job's id, or an error: ErrNoGoal for an empty goal, ErrUnknownBrain for a brain this daemon has no model for, and anything else for a failure of the daemon's own.
func (r *Runner) Start(ctx context.Context, goal string, opts Opts) (string, error) {
	if strings.TrimSpace(goal) == "" {
		return "", ErrNoGoal
	}
	brain := opts.Brain
	if brain == "" {
		brain = r.brain
	}
	if _, ok := r.models[brain]; !ok {
		return "", fmt.Errorf("%w called %q; this daemon has %s", ErrUnknownBrain, brain, r.brainNames())
	}
	if opts.SummaryBrain != "" {
		if _, ok := r.models[opts.SummaryBrain]; !ok {
			return "", fmt.Errorf("%w for the summary: %q; this daemon has %s", ErrUnknownBrain, opts.SummaryBrain, r.brainNames())
		}
	}
	job := Job{
		ID:           fmt.Sprintf("act-%d", r.nextID.Add(1)),
		Goal:         goal,
		Window:       opts.Window,
		Brain:        brain,
		SummaryBrain: opts.SummaryBrain,
		State:        Planning,
		Budget:       opts.Budget.withDefaults(),
		StartedAt:    time.Now(),
	}
	l, claimed := r.claim(job.ID)
	if !claimed {
		return "", fmt.Errorf("job %s is already running", job.ID)
	}
	stored, err := row(job)
	if err != nil {
		r.release(job.ID)
		return "", err
	}
	if err := r.store.SaveActJob(ctx, stored); err != nil {
		r.release(job.ID)
		return "", fmt.Errorf("start job: %w", err)
	}
	r.launch(l, job)
	return job.ID, nil
}

// brainNames lists the brains a job may name, for the message an unknown one is refused with.
func (r *Runner) brainNames() string {
	names := make([]string, 0, len(r.models))
	for name := range r.models {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// claim reserves the live slot for a job id, under one lock, before anything slow — the store read a resume starts with — is done with it. Input: the id. Output: the reserved slot and true, or the job already live and false. It is what stops two Resume calls landing together from both launching the same job against the same screen.
func (r *Runner) claim(id string) (*live, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.live[id]; l != nil {
		return l, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &live{job: Job{ID: id}, ctx: ctx, cancel: cancel, answers: make(chan string, 1), resumes: make(chan struct{}, 1), working: make(chan struct{}, 1), started: time.Now()}
	r.live[id] = l
	return l, true
}

// release gives up a claimed slot when the job it was claimed for could not be started after all.
func (r *Runner) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, id)
}

// launch fills in a claimed slot with the job — new or read back from a checkpoint — and starts its loop and the guard that watches its wall clock.
func (r *Runner) launch(l *live, job Job) {
	l.mu.Lock()
	l.job, l.started, l.baseMS = job, time.Now(), job.ElapsedMS
	l.mu.Unlock()
	l.paused.Store(job.State == Paused)
	r.emit(Event{Job: job.ID, Kind: "started", State: job.State, Text: job.Goal})
	go wallGuard(l, job.Budget.Wall)
	go r.loop(l.ctx, l)
}

// Resume either lets a paused job carry on, or — when this Runner has never seen the job, which is what a daemon restart leaves behind — reads its last checkpoint and starts it again from there. Input: a context for the store read, the job id, and a budget applied over the one the job already has, field by field, so a job that ran out of steps can come back with more (a zero field leaves the job's own value alone). Output: an error when there is no such job, it is already running, or it has ended in a way a raised budget does not reopen.
func (r *Runner) Resume(ctx context.Context, id string, budget Budget) error {
	l, claimed := r.claim(id)
	if !claimed {
		if !l.paused.Load() {
			return fmt.Errorf("job %s is not paused", id)
		}
		l.paused.Store(false)
		l.set(func(j *Job) { j.State, j.Budget = Stepping, j.Budget.raise(budget) })
		select {
		case l.resumes <- struct{}{}:
		default:
		}
		r.emit(Event{Job: id, Kind: "resumed", State: Stepping})
		return nil
	}
	job, err := r.resumable(ctx, id, budget)
	if err != nil {
		r.release(id)
		return err
	}
	r.launch(l, job)
	return nil
}

// resumable reads a job's checkpoint and says whether it may be started again, and on what budget. Input: a context for the store read, the id and the budget asked for. Output: the job to launch, or the reason it may not be.
// A job the user stopped and a job that reached its goal stay ended. One that ran out of budget comes back only when the caller raises that budget, and its old failure is cleared so the resumed run does not carry the last one's words.
func (r *Runner) resumable(ctx context.Context, id string, budget Budget) (Job, error) {
	stored, err := r.store.ActJob(ctx, id)
	if err != nil {
		if db.NoActJob(err) {
			return Job{}, fmt.Errorf("there is no job called %s", id)
		}
		return Job{}, fmt.Errorf("resume job: %w", err)
	}
	var job Job
	if err := json.Unmarshal(stored.Checkpoint, &job); err != nil {
		return Job{}, fmt.Errorf("resume job %s: its checkpoint will not read back: %w", id, err)
	}
	if finished(job.State) {
		if job.State != Failed || budget == (Budget{}) {
			return Job{}, fmt.Errorf("job %s already ended (%s)", id, job.State)
		}
		job.Err, job.Say = "", ""
	}
	job.ID, job.Budget = id, job.Budget.raise(budget).withDefaults()
	if _, ok := r.models[job.Brain]; !ok {
		return Job{}, fmt.Errorf("job %s ran on brain %q, which this daemon no longer has", id, job.Brain)
	}
	// A job that was being held or was waiting on an answer keeps saying so: forcing stepping here would start driving the screen on a job the user had stopped in its tracks, and would throw away the question it was waiting on without ever asking it again.
	if job.State != Stuck && job.State != Paused {
		job.State = Stepping
	}
	return job, nil
}

// finished reports whether a state is one a job never comes back from.
func finished(s State) bool { return s == Done || s == Stopped || s == Failed }

// Job reads a job's whole record: from memory while it is running, from its last checkpoint once it is not. Input: a context for the store read and the id. Output: the job, or an error when there is no such job.
func (r *Runner) Job(ctx context.Context, id string) (Job, error) {
	if l := r.find(id); l != nil {
		return l.snapshot(), nil
	}
	stored, err := r.store.ActJob(ctx, id)
	if err != nil {
		if db.NoActJob(err) {
			return Job{}, fmt.Errorf("there is no job called %s", id)
		}
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(stored.Checkpoint, &job); err != nil {
		return Job{}, fmt.Errorf("job %s: its checkpoint will not read back: %w", id, err)
	}
	job.ID = id
	return job, nil
}

// Stop ends a job now. The step in flight is abandoned as soon as the tool it is in returns, and nothing further is run. Input: the id. Output: an error when there is no such job running.
func (r *Runner) Stop(id string) error {
	l := r.find(id)
	if l == nil {
		return fmt.Errorf("job %s is not running", id)
	}
	l.stopped.Store(true)
	l.cancel()
	return nil
}

// Pause holds a job before its next step. Input: the id. Output: an error when there is no such job running.
// A job that is already stuck on a question is shown as paused straight away, but goes on waiting for that answer: the pause only takes effect at the top of the next round, which is after the answer arrives.
func (r *Runner) Pause(id string) error {
	l := r.find(id)
	if l == nil {
		return fmt.Errorf("job %s is not running", id)
	}
	l.paused.Store(true)
	// The pause goes to disk as well as to the window: a daemon that stops here comes back knowing the job was being held, instead of resuming it into the screen the user had just asked it to leave alone.
	r.save(l, l.set(func(j *Job) { j.State = Paused }))
	r.emit(Event{Job: id, Kind: "paused", State: Paused})
	return nil
}

// Answer gives a stuck job the user's reply to the one question it asked, which is added to the goal context and lets the loop carry on. Input: the id and the answer. Output: an error when there is no such job running, or when it is not waiting on a question.
// A job that asked nothing refuses the answer rather than holding it: a buffered answer would be drained by whatever question the job asks next, steps later and about something else, as though the user had answered that one.
func (r *Runner) Answer(id, text string) error {
	l := r.find(id)
	if l == nil {
		return fmt.Errorf("job %s is not running", id)
	}
	// The live slot outlives the end of a job by as long as its last checkpoint takes to write, and an answer dropped into it then is never read by anyone, so a job whose context is done refuses it rather than reporting that it landed.
	if l.ctx.Err() != nil {
		return fmt.Errorf("job %s is not running", id)
	}
	// The gate is the question itself rather than the state word: a job that is stuck and then paused is still sitting in the same select waiting for this answer, and gating on the state would leave it unanswerable and unresumable both.
	if l.snapshot().Question == "" {
		return fmt.Errorf("job %s is not waiting on a question", id)
	}
	select {
	case l.answers <- text:
	default:
		return fmt.Errorf("job %s already has an answer waiting", id)
	}
	return nil
}

// find returns the live job with this id, or nil.
func (r *Runner) find(id string) *live {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[id]
}

// loop is the job: observe, decide one action with the change it should produce, act, check, record — until the goal is reached, the budget is spent, or the user is asked something.
func (r *Runner) loop(ctx context.Context, l *live) {
	defer l.cancel()
	defer r.release(l.snapshot().ID)

	// Every tool call of this job runs in its own screen scope, so two jobs never share one numbered list, one screenshot or one record of what has keyboard focus.
	if scoped, ok := r.exec.(ScreenScope); ok {
		ctx = scoped.NewScreenScope(ctx)
	}
	// Once per run, before the first round: a resumed job keeps the history it already had rather than searching again on a goal that has not changed.
	if ref, ok := r.exec.(Referencer); ok && l.snapshot().Reference == "" {
		if block := ref.ActReferenceFor(ctx, l.snapshot().Goal, time.Now()); block != "" {
			r.save(l, l.set(func(j *Job) { j.Reference = block }))
		}
	}
	// A job resumed while it was stuck asks its question again: the answer it was waiting for never came, and the checkpoint is the only place that question survived.
	if job := l.snapshot(); job.State == Stuck && job.Question != "" {
		if !r.askUser(ctx, l, job.Question) {
			r.endedWaiting(ctx, l)
			return
		}
	}

	for {
		if !l.waitWhilePaused(ctx) {
			r.endedWaiting(ctx, l)
			return
		}
		if over, why := overBudget(l.snapshot()); over {
			job := l.snapshot()
			// The token and wall budgets are what a runaway job actually costs, so those still end it. Running out of STEPS is different: steps are only a guess at how big the task was, and a job that has spent them has not failed so much as found out it guessed low. That one asks.
			if !strings.HasPrefix(why, "the step budget") {
				r.end(l, Failed, why, summarise(job, why))
				return
			}
			if !r.askUser(ctx, l, outOfRoomQuestion(job)) {
				r.endedWaiting(ctx, l)
				return
			}
			// A yes has to buy real room, or the very next round is over budget again and it asks forever. Another estimate's worth is what it asked for in the first place.
			job = l.set(func(j *Job) {
				j.Budget.Steps += max(j.Estimate, len(j.Steps))
			})
			slog.Info("job given more room after asking", "job", job.ID, "steps", job.Budget.Steps, "taken", len(job.Steps))
			r.save(l, job)
		}

		// Observe first, every round: the screen is the only thing that says what the last action actually did.
		observation := r.exec.ExecuteAskTool(ctx, "observe_screen", nil)
		if r.ended(ctx, l) {
			return
		}
		l.set(func(j *Job) {
			j.Observations = pushCapped(j.Observations, observation, keptObservations, observationCap)
		})

		model := r.models[l.snapshot().Brain]
		reply, usage, err := model(ctx, BuildPrompt(l.snapshot()))
		if err != nil {
			if r.ended(ctx, l) {
				return
			}
			job := l.snapshot()
			r.end(l, Failed, err.Error(), summarise(job, "the model stopped answering"))
			return
		}
		job := l.set(func(j *Job) { j.Spend.add(usage, j.Brain) })

		d, err := parseDecision(reply)
		if err != nil {
			// A reply that is not a decision costs one round and is handed straight back, rather than ending a job that may well be one sentence from finishing.
			l.set(func(j *Job) {
				j.Results = pushCapped(j.Results, "your last reply was not the JSON asked for: "+err.Error(), keptResults, resultCap)
			})
			r.save(l, l.snapshot())
			continue
		}
		planned := false
		job = l.set(func(j *Job) {
			// The plan is written once, on the first round that offers one, and left alone after: a plan rewritten every round is not a plan, and a job resumed from a checkpoint would lose the one it was already working to.
			if j.Plan == "" && d.Plan != "" {
				j.Plan = d.Plan
				planned = true
			}
			if d.Next != "" {
				j.Next = d.Next
			}
			// Written once, like the plan: the model's guess at the size of the task, and twice it as the room to get there. A later round re-guessing would move the goalposts mid-task.
			if j.Estimate == 0 && d.Estimate > 0 {
				j.Estimate = d.Estimate
				j.Budget.Steps = budgetFromEstimate(j.Budget, d.Estimate)
			}
		})

		// Said out loud the once, in the same round it was written, so the steps that follow arrive against a stated intent rather than on their own. Until 2026-09-12 the plan lived only in the job's own prompt: the stream opened with the goal, which is the user's words handed back, and went straight to "Step 1". The estimate rides along because it was written in the same breath and is what makes "step 9" mean anything.
		if planned {
			r.emit(Event{Job: job.ID, Kind: "plan", State: job.State, Step: job.Estimate, Text: job.Plan})
		}

		if d.Done {
			// The goal is only reached once something checked that it was: a model may end a job on the step it just watched come true, never on its own say-so before anything has been verified.
			// A step whose check already held before its action is not something that checked out, so it cannot carry a done either.
			if n := len(job.Steps); n == 0 || !checkedOut(job.Steps[n-1]) {
				l.set(func(j *Job) {
					j.Results = pushCapped(j.Results, "you said the goal was reached, but nothing has checked out yet; take the action that would make it true and let the check confirm it before you set done", keptResults, resultCap)
				})
				continue
			}
			r.end(l, Done, "", strings.TrimSpace(d.Say))
			return
		}
		if strings.TrimSpace(d.Ask) != "" {
			if !r.askUser(ctx, l, strings.TrimSpace(d.Ask)) {
				r.endedWaiting(ctx, l)
				return
			}
			continue
		}
		if d.Tool == "" {
			l.set(func(j *Job) {
				j.Results = pushCapped(j.Results, "your last reply named no tool, and nothing happened; name one tool or set done", keptResults, resultCap)
			})
			continue
		}

		// The arguments are kept as the act runs keep them (see db.StorableArgs): what the user dictated into type_text is dropped here, before the step is ever written to disk or described on the event stream. Expect gets the same treatment (see redactedExpect): a field_holds check right after a type_text step would otherwise repeat the typed text StorableArgs just dropped, this time under Expect.Value rather than under an argument. The tool itself is still called with what the model actually said, and the live wait_for check below still verifies against the real, unredacted Expect.
		// A read (look, observe_screen, point_at) changes nothing on the screen, so a check written for it could only fail; it carries none, is not checked, and does not move the stuck counter.
		read := isRead(d.Tool)
		if read {
			d.Expect = act.Check{}
		}
		storedExpect := redactedExpect(d.Tool, d.Expect)
		step := Step{N: len(job.Steps) + 1, Tool: d.Tool, Args: db.StorableArgs(d.Tool, d.Args), Expect: storedExpect}
		if actions := burst(d); len(actions) > 1 {
			for _, a := range actions {
				step.Burst = append(step.Burst, action{Tool: a.Tool, Args: db.StorableArgs(a.Tool, a.Args)})
			}
		}

		// A stop or a pause decided while the model was still thinking takes effect here, before the mouse or the keyboard is touched: the input drivers take no context, so a click or a keystroke started after the stop really lands on the user's screen, and the checks after the tool call would only notice it afterwards.
		if r.ended(ctx, l) {
			return
		}
		if l.paused.Load() {
			continue
		}
		l.set(func(j *Job) { j.State = Stepping })
		r.emit(Event{Job: job.ID, Kind: "step", State: Stepping, Step: step.N, Text: describeAction(d), Expect: storedExpect.Describe()})

		// The check is taken once before the action, so a verdict that was true either way cannot be counted as proof: a title_contains "Netflix" written after a click passes on a window that was already called that, and item_absent passes on an item that was never there. One poll, no waiting.
		if !read {
			step.HeldBefore = r.checkHolds(ctx, d.Expect)
		}
		if r.ended(ctx, l) {
			return
		}

		// Every action of the burst in order, with no round trip and no reading between them. The first failure ends the burst: the actions after it were predicted against a screen that is now somewhere else, and taking them anyway is how a wrong guess turns into several wrong guesses on the user's real screen.
		actions := burst(d)
		var result string
		for i, a := range actions {
			result = r.exec.ExecuteAskTool(ctx, a.Tool, a.Args)
			if strings.HasPrefix(result, stopLineRefusal) || strings.HasPrefix(result, "error") {
				if i > 0 {
					slog.Info("burst stopped early", "job", job.ID, "action", i+1, "of", len(actions), "tool", a.Tool, "result", result)
				}
				break
			}
			if r.ended(ctx, l) {
				return
			}
		}
		step.Result = util.Runes(result, resultCap)
		step.Why = "the action ran, and the check had not answered yet"

		// Nothing was clicked, typed or pressed, and no rewording of the same action gets past the stop line, so the refusal goes to the user as the job's one question rather than back to the model as a result to try around — which is what the ask loop does with the same sentinel (see ask.go).
		// ponytail: the user's answer is kept with the goal and shown to the model, but it is not put on the context the next tool call runs with, so a guarded control still cannot be clicked and the job will ask again; carrying agent.WithGo through the Executor seam is the upgrade path.
		if strings.HasPrefix(result, stopLineRefusal) {
			if !r.askUser(ctx, l, result) {
				r.endedWaiting(ctx, l)
				return
			}
			continue
		}

		// The step goes on disk the moment its action has run, before it is checked: a daemon that dies between the two comes back knowing the action happened, rather than taking it a second time on the real screen.
		job = l.set(func(j *Job) {
			j.Steps = append(j.Steps, step)
			if !l.paused.Load() {
				j.State = Verifying
			}
		})
		r.save(l, job)
		if r.ended(ctx, l) {
			return
		}

		if read {
			step.Outcome, step.Why = "read", "a read, nothing to check"
		} else {
			verdict := r.exec.ExecuteAskTool(ctx, "wait_for", map[string]any{"kind": d.Expect.Kind, "value": d.Expect.Value, "timeout_ms": float64(waitTimeoutMS)})
			if r.ended(ctx, l) {
				return
			}
			step.Outcome, step.Why = readVerdict(verdict)
		}
		toldNothing := (step.Outcome == "pass" && step.HeldBefore) || read
		if toldNothing && !read {
			step.Why += alreadyHeldNote
		}

		job = l.set(func(j *Job) {
			j.Steps[len(j.Steps)-1] = step
			j.Results = pushCapped(j.Results, step.Tool+": "+step.Result, keptResults, resultCap)
			// A check that already held is neither a pass nor a failure: it told us nothing about the action, so it neither clears the stuck counter nor moves it on. Counting it as a failure is what used to stop a job whose every check was the same window title after three ordinary steps.
			switch {
			case toldNothing:
			case step.Outcome == "pass":
				j.FailsInARow = 0
			default:
				j.FailsInARow++
			}
			// A pause the user pressed while this check was running stands: both front ends take the state word off every event, so writing stepping here would put a Pause button back on a job that is already held and leave no Resume to press.
			if !l.paused.Load() {
				j.State = Stepping
			}
		})
		r.emit(Event{Job: job.ID, Kind: "verified", State: job.State, Step: step.N, Text: step.Why, Expect: step.Expect.Describe(), Outcome: step.Outcome, HeldBefore: step.HeldBefore})
		r.save(l, job)

		if job.FailsInARow >= stuckAfter {
			if !r.askUser(ctx, l, stuckQuestion(job)) {
				r.endedWaiting(ctx, l)
				return
			}
			continue
		}
		if job.Spend.Rounds%summaryEvery == 0 {
			r.rewriteSummary(ctx, l)
		}
	}
}

// ended reports whether the job should stop now because its context is finished, recording the reason. Input: the job's context and the live job. Output: true when the loop must return.
func (r *Runner) ended(ctx context.Context, l *live) bool {
	if ctx.Err() == nil {
		return false
	}
	if l.stopped.Load() {
		r.end(l, Stopped, "", "I stopped there.")
		return true
	}
	job := l.snapshot()
	r.end(l, Failed, "the job ran out of its wall-clock budget", summarise(job, "I ran out of time"))
	return true
}

// endedWaiting ends a job whose wait on the user did not come back, telling the two reasons apart the way every other exit does: the user stopped it, or its time ran out while it waited. Input: the job's context and the live job. Output: none.
func (r *Runner) endedWaiting(ctx context.Context, l *live) {
	if r.ended(ctx, l) {
		return
	}
	r.end(l, Stopped, "", "I stopped there.")
}

// askUser puts the job in stuck with one plain question and waits for the answer, which is kept with the goal so every later round sees it. Input: the job's context, the live job and the question. Output: true when an answer came, false when the job was stopped or its time ran out while waiting.
func (r *Runner) askUser(ctx context.Context, l *live, question string) bool {
	job := l.set(func(j *Job) {
		j.State = Stuck
		j.Question = question
	})
	r.emit(Event{Job: job.ID, Kind: "question", State: Stuck, Text: question})
	r.save(l, job)
	l.beginWait()
	select {
	case answer := <-l.answers:
		l.endWait()
		job = l.set(func(j *Job) {
			j.Answers = append(j.Answers, answer)
			j.Question = ""
			j.FailsInARow = 0
			j.State = Stepping
		})
		r.emit(Event{Job: job.ID, Kind: "answered", State: Stepping, Text: answer})
		r.save(l, job)
		return true
	case <-ctx.Done():
		l.endWait()
		return false
	}
}

// end puts a job into its final state, writes the last checkpoint and says so. Input: the live job, the end state, the error text ("" when none) and the closing words. Output: none.
// The final checkpoint is written before the end state is published, so that a caller who sees the job as ended — the hover, a GET, a test — always finds the same thing on disk. Only the loop's own goroutine writes the job, so reading it out, editing the copy and putting it back is safe. A question the job was still waiting on is kept rather than wiped, so a job whose time ran out while it waited can still be seen to have been waiting, and on what.
func (r *Runner) end(l *live, state State, errText, say string) {
	job := l.snapshot()
	job.State, job.Err, job.Say = state, errText, say
	r.save(l, job)
	job.ElapsedMS = l.elapsed()
	l.set(func(j *Job) { *j = job })
	spend := job.Spend
	r.emit(Event{Job: job.ID, Kind: "done", State: state, Step: len(job.Steps), Text: say, Spend: &spend})
}

// isRead reports whether a tool only reads the screen. Input: the tool's name. Output: true for look, observe_screen and point_at.
func isRead(tool string) bool {
	switch tool {
	case "look", "observe_screen", "point_at":
		return true
	// A lookup changes nothing on the screen, so there is nothing for wait_for to wait for. Without this, every search a job ran would be checked against a screen change that was never coming, three in a row would trip the stuck counter, and the job would stop to ask the user why its own research had not moved the page.
	case "branch", "query_memory", "recall":
		return true
	}
	return false
}
