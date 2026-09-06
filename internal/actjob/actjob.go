// Package actjob runs one long computer-use goal as a job inside the daemon rather than inside an HTTP request: it plans, then takes one checked step at a time until the goal is reached, the budget is spent, or it has to ask the user something. Every step is written to the store as it happens (see db.SaveActJob), so a daemon that stops mid-task picks the job up from the last step it finished instead of starting the goal again.
// What it deliberately does not own: the tools, the tool gate and the stop line all belong to the ask path (see agent.Agent.ExecuteAskTool), and this package reaches them through the Executor seam so there is one copy of the rules about what may be clicked and what may not. Verification is pure string matching (see act.Match), so checking a step costs no model call at all — which is most of why a forty-step job is affordable on a subscription.
package actjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ora/internal/act"
	"ora/internal/db"
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

// ScreenScope is the half of the Executor seam a job needs to have a screen of its own: an executor that keeps per-caller screen state — the numbered list observe_screen produced, the picture look took, what the last click focused — hands this job a context carrying its own, so a click by number cannot land in another job's window and the pictures a job takes count against its own allowance. An executor that does not implement it (a scripted one in a test) runs on whatever state it already has. Input: the job's context. Output: a context to make every tool call of that job with.
type ScreenScope interface {
	NewScreenScope(ctx context.Context) context.Context
}

// PreChecker is the half of the Executor seam a job uses for the reading it takes before it acts: one walk of the screen matched against the step's check, with no polling and no second opinion about which window is in front. An executor that does not implement it leaves every pre-reading unanswered, which reads as "the check did not already hold" and puts the step back where it was before this reading existed. Input: the job's context and the change the model wrote down. Output: true when the screen already satisfies it.
type PreChecker interface {
	CheckHolds(ctx context.Context, check act.Check) bool
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

// live is one job actually running: its state under a lock, the context and cancel that stop it, and the two channels the user's answers and resumes arrive on.
type live struct {
	mu      sync.Mutex
	job     Job
	ctx     context.Context
	cancel  context.CancelFunc
	answers chan string
	resumes chan struct{}
	paused  atomic.Bool
	stopped atomic.Bool

	// started is when this run of the job began and baseMS is what earlier runs of it had already spent, so the wall budget is one budget across a restart rather than a fresh one each time.
	started time.Time
	baseMS  int64
	// waitedMS is how long this run has already spent waiting on the user, and waitingSince is when the wait it is in now began, zero when it is not waiting. Neither counts against the wall budget: a job holding still for a person is not a job running away with the screen.
	waitedMS     atomic.Int64
	waitingSince atomic.Int64
	// working is how endWait tells the wall-clock guard that the job has gone back to work, so the guard can sleep through a wait instead of polling a frozen clock. It holds one token, and a token left over from a wait the guard never saw costs one extra wakeup and nothing else.
	working chan struct{}
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

// snapshot copies the job out from under its lock, deeply enough to be encoded or marshalled while the loop goes on working (see cloned). Input: none. Output: the copy.
func (l *live) snapshot() Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloned(l.job)
}

// set edits the job under its lock and returns the edited copy, deep enough to hand out (see cloned).
func (l *live) set(edit func(*Job)) Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	edit(&l.job)
	return cloned(l.job)
}

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
		out[i].Result = capRunes(out[i].Result, storedResultCap)
	}
	return out
}

// stopLineRefusal is what every stop-line refusal begins with (see agent's stopBeforeClick and the type_text and press_key stop lines): the action did not happen and the user has to say go before it can.
const stopLineRefusal = "Stopped before "

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

// elapsed is the wall time this job has spent working, over every run of it, with the stretches it spent waiting on the user taken out. Input: none. Output: the milliseconds, which is what the checkpoint carries as ElapsedMS.
func (l *live) elapsed() int64 {
	spent := time.Since(l.started) - time.Duration(l.waitedMS.Load())*time.Millisecond
	if since := l.waitingSince.Load(); since != 0 {
		spent -= time.Since(time.Unix(0, since))
	}
	return l.baseMS + spent.Milliseconds()
}

// beginWait marks the job as waiting on the user from now, and endWait folds the stretch it just waited into the time that does not count against the wall budget. endWait may be called twice; the second call does nothing.
func (l *live) beginWait() { l.waitingSince.Store(time.Now().UnixNano()) }

func (l *live) endWait() {
	if since := l.waitingSince.Swap(0); since != 0 {
		l.waitedMS.Add(time.Since(time.Unix(0, since)).Milliseconds())
	}
	select {
	case l.working <- struct{}{}:
	default:
	}
}

// wallGuard ends a job once the time it has spent actually working reaches its wall budget. It does that instead of a deadline on the job's own context because the time a job spends stuck on a question or held paused is the user's, not the job's, and a job must not be timed out for how long someone took to answer it. Input: the live job and the whole wall budget, what earlier runs of the job spent included. Output: none; it returns when the job's context is done.
func wallGuard(l *live, wall time.Duration) {
	t := time.NewTicker(wallTick)
	defer t.Stop()
	for {
		if l.waitingSince.Load() != 0 {
			// The job is waiting on the user, so elapsed() is frozen and no amount of ticking can bring it nearer the budget: sleep until the wait ends or the job does, rather than waking ten times a second for as long as the question goes unanswered.
			select {
			case <-l.ctx.Done():
				return
			case <-l.working:
			}
			continue
		}
		if time.Duration(l.elapsed())*time.Millisecond >= wall {
			l.cancel()
			return
		}
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// wallTick is how often the wall-clock guard looks at how long a job has been working: fine enough grain against a five-minute budget, and one wakeup every tenth of a second while a job runs.
const wallTick = 100 * time.Millisecond

// saveTimeout bounds one checkpoint write so a wedged store cannot hold a job's own goroutine.
const saveTimeout = 10 * time.Second

// storedResultCap is how much of a tool result the checkpoint keeps, in runes: the same 300 the ordinary act runs keep theirs at (see db/act_runs.go). The prompt still sees the fuller resultCap copy the job holds in memory; what goes on disk is only what a later reader needs to see what happened.
const storedResultCap = 300

// waitTimeoutMS is how long wait_for polls for the expected change before calling it a failure, in milliseconds — the tool's own default, passed explicitly so a job's verification time is on record rather than implied.
const waitTimeoutMS = 5000

// alreadyHeldNote is appended to what the check found, so the round after it reads the verdict as saying nothing about the action rather than as proof it worked.
const alreadyHeldNote = " — but this already held before the action, so it says nothing about what the action did"

// summaryEvery is how many rounds pass between rewrites of the progress summary. Five, because a summary rewritten every round costs a round's worth of tokens for nothing, and one rewritten every twenty is describing a screen the job has long left.
const summaryEvery = 5

// stuckAfter is how many failed verifications on the same step turn into a question for the user.
const stuckAfter = 3

// loop is the job: observe, decide one action with the change it should produce, act, check, record — until the goal is reached, the budget is spent, or the user is asked something.
func (r *Runner) loop(ctx context.Context, l *live) {
	defer l.cancel()
	defer r.release(l.snapshot().ID)

	// Every tool call of this job runs in its own screen scope, so two jobs never share one numbered list, one screenshot or one record of what has keyboard focus.
	if scoped, ok := r.exec.(ScreenScope); ok {
		ctx = scoped.NewScreenScope(ctx)
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
			r.end(l, Failed, why, summarise(job, why))
			return
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
		job = l.set(func(j *Job) {
			// The plan is written once, on the first round that offers one, and left alone after: a plan rewritten every round is not a plan, and a job resumed from a checkpoint would lose the one it was already working to.
			if j.Plan == "" && d.Plan != "" {
				j.Plan = d.Plan
			}
			if d.Next != "" {
				j.Next = d.Next
			}
		})

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
		storedExpect := redactedExpect(d.Tool, d.Expect)
		step := Step{N: len(job.Steps) + 1, Tool: d.Tool, Args: db.StorableArgs(d.Tool, d.Args), Expect: storedExpect}

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
		step.HeldBefore = r.checkHolds(ctx, d.Expect)
		if r.ended(ctx, l) {
			return
		}

		result := r.exec.ExecuteAskTool(ctx, d.Tool, d.Args)
		step.Result = capRunes(result, resultCap)
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

		verdict := r.exec.ExecuteAskTool(ctx, "wait_for", map[string]any{"kind": d.Expect.Kind, "value": d.Expect.Value, "timeout_ms": float64(waitTimeoutMS)})
		if r.ended(ctx, l) {
			return
		}
		step.Outcome, step.Why = readVerdict(verdict)
		toldNothing := step.Outcome == "pass" && step.HeldBefore
		if toldNothing {
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

// waitWhilePaused holds the loop while the job is paused. Output: true when it may carry on, false when the job was stopped or its time ran out while paused.
func (l *live) waitWhilePaused(ctx context.Context) bool {
	for l.paused.Load() {
		l.beginWait()
		select {
		case <-l.resumes:
			l.endWait()
		case <-ctx.Done():
			l.endWait()
			return false
		}
	}
	return ctx.Err() == nil
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

// overBudget reports whether a job has spent what it was given, and which budget it was. Input: the job. Output: true and the plain reason, or false and "".
func overBudget(j Job) (bool, string) {
	if len(j.Steps) >= j.Budget.Steps {
		return true, fmt.Sprintf("the step budget of %d is spent", j.Budget.Steps)
	}
	if j.Spend.Input >= j.Budget.InputTokens {
		return true, fmt.Sprintf("the input-token budget of %d is spent (%d used)", j.Budget.InputTokens, j.Spend.Input)
	}
	return false, ""
}

// summarise is what a job says when it ends without reaching the goal: how far it got, what it last did, and what it was about to do — never a claim that the goal was met. Input: the job and the plain reason it stopped. Output: the sentence.
func summarise(j Job, why string) string {
	verified := 0
	for _, s := range j.Steps {
		if checkedOut(s) {
			verified++
		}
	}
	out := fmt.Sprintf("I stopped short of %q: %s. I took %d steps, %d of which checked out.", j.Goal, why, len(j.Steps), verified)
	if n := len(j.Steps); n > 0 {
		out += fmt.Sprintf(" The last thing I did was %s, and %s.", j.Steps[n-1].Tool, j.Steps[n-1].Why)
	}
	if j.Next != "" {
		out += " Next would have been: " + j.Next + "."
	}
	return out
}

// stuckQuestion is the one plain question a job asks when three checks in a row on the same step have failed. Input: the job. Output: the question, in the model's own words for what it was trying when it has them.
func stuckQuestion(j Job) string {
	last := j.Steps[len(j.Steps)-1]
	return fmt.Sprintf("I tried %d times to make it so that %s, and each time %s. What should I do instead?", stuckAfter, last.Expect.Describe(), last.Why)
}

// checkedOut reports whether a step is one that actually showed the goal moving: a check that passed and was not already true before the action. It is what the done guard, the progress line and the closing sentence all count, so a job cannot end on, or claim progress from, a check that told it nothing.
func checkedOut(s Step) bool { return s.Outcome == "pass" && !s.HeldBefore }

// checkHolds reads a step's check once, before the action runs, off a single reading of the screen. Input: the job's context and the change the model wrote down. Output: true when the screen already satisfies it, false when it does not, when the step named no check at all, or when this executor cannot take the reading.
// It does not go through wait_for: that tool polls for up to five seconds and, on a list check that matches, walks the window a second time to see whether another window came to the front. Neither is worth anything here — nothing has acted yet, so there is nothing to wait for and nothing for the front window to have changed under — and both were paid on every step of every job.
func (r *Runner) checkHolds(ctx context.Context, expect act.Check) bool {
	pre, ok := r.exec.(PreChecker)
	if !ok || expect.Value == "" {
		return false
	}
	return pre.CheckHolds(ctx, expect)
}

// readVerdict turns wait_for's own answer into a step's outcome. Input: the tool result. Output: "pass" or "fail", and what the check found in plain words.
func readVerdict(result string) (string, string) {
	if rest, ok := strings.CutPrefix(result, act.WaitPassPrefix); ok {
		return "pass", rest
	}
	if rest, ok := strings.CutPrefix(result, act.WaitFailPrefix); ok {
		return "fail", rest
	}
	return "fail", strings.TrimSpace(result)
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

// describeAction renders one decision as the line a hover shows while the step runs. The fallback for a decision that wrote no words for itself is the tool and its arguments, redacted as the checkpoint redacts them, so what the user dictated into type_text never goes out on the event stream either.
func describeAction(d decision) string {
	if d.Next != "" {
		return d.Next
	}
	if len(d.Args) == 0 {
		return d.Tool
	}
	args, err := json.Marshal(db.StorableArgs(d.Tool, d.Args))
	if err != nil {
		return d.Tool
	}
	return d.Tool + " " + string(args)
}

// rewriteSummary asks the cheap brain to restate where the job has got to in at most two sentences, so the trail never has to be re-sent. A failure leaves the old summary standing: a job must not end because its own note-taking failed.
func (r *Runner) rewriteSummary(ctx context.Context, l *live) {
	job := l.snapshot()
	name := job.SummaryBrain
	if name == "" {
		name = job.Brain
	}
	model, ok := r.models[name]
	if !ok {
		return
	}
	text, usage, err := model(ctx, SummaryPrompt(job))
	if err != nil {
		slog.Warn("act job: could not rewrite the progress summary", "job", job.ID, "error", err)
		return
	}
	l.set(func(j *Job) {
		j.Summary = capRunes(strings.TrimSpace(text), summaryCap)
		j.Spend.add(usage, name)
	})
}

// decision is what the model is asked to reply with each round: at most one action, the change it should produce, or an end.
type decision struct {
	Plan   string         `json:"plan"`
	Next   string         `json:"next"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Expect act.Check      `json:"expect"`
	Done   bool           `json:"done"`
	Say    string         `json:"say"`
	Ask    string         `json:"ask"`
}

// parseDecision reads one round's reply. Input: whatever the model wrote, which in practice is bare JSON, JSON in a fenced block, or JSON with a sentence around it. Output: the decision, or an error when there is no JSON object in it at all or it says nothing to do.
func parseDecision(reply string) (decision, error) {
	open := strings.Index(reply, "{")
	shut := strings.LastIndex(reply, "}")
	if open < 0 || shut <= open {
		return decision{}, fmt.Errorf("no JSON object in the reply")
	}
	var d decision
	if err := json.Unmarshal([]byte(reply[open:shut+1]), &d); err != nil {
		return decision{}, fmt.Errorf("the JSON would not read: %w", err)
	}
	if !d.Done && d.Tool == "" && strings.TrimSpace(d.Ask) == "" {
		return decision{}, fmt.Errorf("the reply named no tool, asked nothing and did not say it was done")
	}
	return d, nil
}
