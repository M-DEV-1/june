// This file is the storage behind the "watch me once" replay learning: a record of every ask that used a screen tool (observe_screen, show_marks, point_at, click, scroll_to, type_text), so a later pass can study what worked. What is kept per step is bounded and redacted on the way in (see AddActRun and storableArgs): only the screen hops of a turn are stored, every argument's text and every result is capped at stepArgRuneCap/stepResultRuneCap runes, and type_text's own text argument — whatever the user dictated, a passphrase, a card number, a private message — is dropped outright (see droppedArgs) rather than merely capped.
// An act run does reach a model, two ways. The nightly procedures stage (internal/dream/procedures.go) renders a successful run's steps into a "How I did X" note, which is ordinary memory from there on: embedded, searched, and read back on a later question like any other note. And the act-reference block (internal/agent/act_reference.go) renders the runs closest in meaning to a brand-new screen ask straight into that ask's own prompt, without ever becoming a note — the lookup behind it, SimilarActRuns in act_reference.go, is also the one search this table is put through; nothing else here is ever searched or read back on its own. Both paths render through the shared RenderActStep/RenderActSteps in steps.go, so what a note says and what the reference block says about the same run are worded identically.
package db

import (
	"context"
	"encoding/json"
	"fmt"

	"ora/internal/util"
)

// stepResultRuneCap bounds how many runes of a tool's result are kept per step, so a chatty tool (observe_screen listing a long page) cannot make one run's steps_json unbounded.
const stepResultRuneCap = 300

// stepArgRuneCap bounds how many runes of any one string argument are kept per step, the same guard stepResultRuneCap puts on a result: an argument can carry a pasted page just as a result can.
const stepArgRuneCap = 300

// screenTools names the tools that touch the screen. Only these hops are stored, because an act run is a record of screen work: the same turn also searches memory, reads files and runs commands, and those hops carry the contents of what they read, which has no place in a row kept to teach the replay learning how a screen goal was reached.
var screenTools = map[string]bool{
	"observe_screen": true,
	"show_marks":     true,
	"point_at":       true,
	"click":          true,
	"scroll_to":      true,
	"type_text":      true,
}

// ScreenTool reports whether a tool touches the screen and so belongs in an act run. Input: a tool's name. Output: true for observe_screen, show_marks, point_at, click, scroll_to and type_text.
func ScreenTool(name string) bool { return screenTools[name] }

// droppedArgs names, per tool, the arguments that are never stored because they carry the user's own content rather than a description of the step. type_text's text is whatever they dictated — a passphrase, a card number, a private message — and a stored step is rendered either into a note that gets embedded and searched, or straight into another ask's prompt (see this file's header), so a string kept here comes back on a later, unrelated question.
var droppedArgs = map[string]map[string]bool{
	"type_text": {"text": true},
}

// storableArgs copies one step's arguments into the form kept on disk: the content arguments dropped, every string cut to stepArgRuneCap runes, every other value as it was. Input: the tool's name and the arguments the model called it with. Output: a new map, or nil when the step had none.
func storableArgs(name string, args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	dropped := droppedArgs[name]
	out := make(map[string]any, len(args))
	for k, v := range args {
		if dropped[k] {
			continue
		}
		if s, ok := v.(string); ok {
			out[k] = util.Runes(s, stepArgRuneCap)
			continue
		}
		out[k] = v
	}
	return out
}

// ActStep is one screen-tool call inside an act run: its name, the arguments the model called it with as storableArgs left them, and the first stepResultRuneCap runes of what the tool returned.
type ActStep struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Result string         `json:"result"`
}

// ActRun is one ask whose trace included a screen tool. Outcome is "ok" or "error"; Error carries the failure message when Outcome is "error" and is empty otherwise; Answer is the model's final reply, empty on error.
type ActRun struct {
	ID         int64
	Question   string
	Model      string
	Outcome    string
	Answer     string
	Error      string
	DurationMS int64
	Steps      []ActStep
}

// AddActRun stores one act run. Input: the run, with Steps in call order. Output: the new run's id, or an error from the store. Steps that named a tool other than a screen tool are left out, and the ones kept are stored with their results capped and their arguments capped and redacted (see storableArgs).
func (s *Store) AddActRun(ctx context.Context, run ActRun) (int64, error) {
	steps := make([]ActStep, 0, len(run.Steps))
	for _, step := range run.Steps {
		if !ScreenTool(step.Name) {
			continue
		}
		steps = append(steps, ActStep{Name: step.Name, Args: storableArgs(step.Name, step.Args), Result: util.Runes(step.Result, stepResultRuneCap)})
	}
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return 0, fmt.Errorf("add act run: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO act_runs (question, model, outcome, answer, error, duration_ms, steps_json) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run.Question, run.Model, run.Outcome, run.Answer, run.Error, run.DurationMS, string(stepsJSON))
	if err != nil {
		return 0, fmt.Errorf("add act run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add act run: %w", err)
	}
	// The question is embedded here, once, rather than on every lookup that scores this run: SimilarActRuns compares a new question against every stored one, so embedding at lookup time would cost one round trip per stored run per ask. Only a run that could ever be offered as reference is embedded — a failed run and one that never touched the screen are both filtered out of that lookup, so a vector for either is one nothing would read.
	if run.Outcome == "ok" && len(steps) > 0 {
		s.embedActRunQuestionAsync(id, run.Question)
	}
	return id, nil
}

// scanActRun reads one act_runs row in the "id, question, model, outcome, answer, error, duration_ms, steps_json" column order and decodes its steps, the shared body behind both ActRuns and act_reference.go's single-row actRun. Input: anything a *sql.Row or *sql.Rows can Scan into that order. Output: the run, or an error from the scan or from steps that will not decode.
func scanActRun(row rowScanner) (ActRun, error) {
	var r ActRun
	var stepsJSON string
	if err := row.Scan(&r.ID, &r.Question, &r.Model, &r.Outcome, &r.Answer, &r.Error, &r.DurationMS, &stepsJSON); err != nil {
		return ActRun{}, err
	}
	if stepsJSON != "" {
		if err := json.Unmarshal([]byte(stepsJSON), &r.Steps); err != nil {
			return ActRun{}, fmt.Errorf("act run %d: decode steps: %w", r.ID, err)
		}
	}
	return r, nil
}

// ActRuns returns the most recently started act runs, newest first. Input: how many to return. Output: each run with its steps decoded. A long-running job's checkpoint row (see act_jobs.go) is skipped: it carries a plan and a budget rather than one turn's screen hops, and offering it as a worked example of a finished turn would teach the replay learning the wrong shape.
func (s *Store) ActRuns(ctx context.Context, limit int) ([]ActRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, question, model, outcome, answer, error, duration_ms, steps_json FROM act_runs WHERE outcome != ? ORDER BY id DESC LIMIT ?`, actJobOutcome, limit)
	if err != nil {
		return nil, fmt.Errorf("act runs: %w", err)
	}
	defer rows.Close()

	var out []ActRun
	for rows.Next() {
		r, err := scanActRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan act run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
