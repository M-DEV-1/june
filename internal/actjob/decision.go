package actjob

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"ora/internal/act"
	"ora/internal/db"
)

// stopLineRefusal is what every stop-line refusal begins with (see agent's stopBeforeClick and the type_text and press_key stop lines): the action did not happen and the user has to say go before it can.
const stopLineRefusal = "Stopped before "

// waitTimeoutMS is how long wait_for polls for the expected change before calling it a failure, in milliseconds — the tool's own default, passed explicitly so a job's verification time is on record rather than implied.
const waitTimeoutMS = 5000

// alreadyHeldNote is appended to what the check found, so the round after it reads the verdict as saying nothing about the action rather than as proof it worked.
const alreadyHeldNote = " — but this already held before the action, so it says nothing about what the action did"

// summaryEvery is how many rounds pass between rewrites of the progress summary. Five, because a summary rewritten every round costs a round's worth of tokens for nothing, and one rewritten every twenty is describing a screen the job has long left.
const summaryEvery = 5

// stuckAfter is how many failed verifications on the same step turn into a question for the user.
const stuckAfter = 3

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
