// This file serves the routes behind a long-running computer-use job (see internal/actjob) and puts its progress on the /events stream the window is already listening to, so a task that takes four minutes shows up in the hover step by step instead of needing a window of its own.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ora/internal/actjob"
)

// ActEventType is the Event.Type every job event carries, so a client can pick them out of the same stream that carries asks. Its Event.ID is the job's id, and Event.Detail is the whole actjob.Event as JSON — the step number, the state, the expected change, whether it checked out, and, on the last one, what the job cost.
const ActEventType = "act"

// ActEmitter builds the sink a Runner broadcasts its progress through. Input: the server whose /events hub the window is listening to. Output: the function to hand actjob.New, which turns each job event into one stream event: Text is the line a hover shows, Detail is the same event as JSON for a client that wants the parts.
func ActEmitter(s *Server) func(actjob.Event) {
	return func(ev actjob.Event) {
		detail, err := json.Marshal(ev)
		if err != nil {
			detail = nil
		}
		s.hub.broadcast(Event{
			ID:       ev.Job,
			Type:     ActEventType,
			Text:     actEventLine(ev),
			Detail:   string(detail),
			Evidence: []EvidenceItem{},
			Actions:  []ActionItem{},
		})
	}
}

// actEventLine is the one line a hover shows for a job event, in plain words. Input: the event. Output: the line.
func actEventLine(ev actjob.Event) string {
	switch ev.Kind {
	case "started":
		return "Started: " + ev.Text
	case "step":
		return fmt.Sprintf("Step %d: %s — expecting %s", ev.Step, ev.Text, ev.Expect)
	case "verified":
		if ev.Outcome == "pass" {
			return fmt.Sprintf("Step %d checked out: %s", ev.Step, ev.Text)
		}
		return fmt.Sprintf("Step %d did not check out: %s", ev.Step, ev.Text)
	case "question":
		return ev.Text
	case "answered":
		return "You said: " + ev.Text
	case "paused":
		return "Paused."
	case "resumed":
		return "Carrying on."
	case "done":
		if ev.Text != "" {
			return ev.Text
		}
		return "Finished: " + string(ev.State)
	}
	return ev.Text
}

// ActJobs serves the job routes. It holds the runner alone: the events go out through the emitter the runner was built with (see ActEmitter).
type ActJobs struct {
	runner *actjob.Runner
}

// NewActJobs wires the routes to a runner. Input: the runner. Output: the handler set; register its methods on a mux (see cmd/daemon.go).
func NewActJobs(runner *actjob.Runner) *ActJobs { return &ActJobs{runner: runner} }

// actJobReadTimeout bounds the store reads a route makes, so a wedged store answers with an error rather than holding the request open.
const actJobReadTimeout = 5 * time.Second

// Start handles POST /act. Input: JSON body {"goal": string, "window": string, "brain": string, "summary_brain": string, "budget": {"wall_seconds": number, "input_tokens": number, "steps": number}} — everything but the goal optional, and an omitted part of the budget taking the default (five minutes, 200k input tokens, 40 steps). Output: 202 with {"id": string} as soon as the job is on disk; the job then runs in the daemon and its progress arrives on /events as "act" events tagged with that id. A body that will not decode, an empty goal, or a brain this daemon has no model for gets 400; a failure of the daemon's own — the store refusing the first checkpoint — gets 500.
func (j *ActJobs) Start(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Goal         string `json:"goal"`
		Window       string `json:"window"`
		Brain        string `json:"brain"`
		SummaryBrain string `json:"summary_brain"`
		Budget       struct {
			WallSeconds int `json:"wall_seconds"`
			InputTokens int `json:"input_tokens"`
			Steps       int `json:"steps"`
		} `json:"budget"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	id, err := j.runner.Start(r.Context(), req.Goal, actjob.Opts{
		Window:       req.Window,
		Brain:        req.Brain,
		SummaryBrain: req.SummaryBrain,
		Budget: actjob.Budget{
			Wall:        time.Duration(req.Budget.WallSeconds) * time.Second,
			InputTokens: req.Budget.InputTokens,
			Steps:       req.Budget.Steps,
		},
	})
	switch {
	case errors.Is(err, actjob.ErrNoGoal), errors.Is(err, actjob.ErrUnknownBrain):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		// Everything else Start can fail on is the daemon's own: the store would not take the first checkpoint, or the id clashed with a job already running. Reporting those as 400 sends the user looking at the goal they typed.
		fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

// Get handles GET /act/{id}: the job's whole record as JSON — its state, plan, progress summary, every step with what it expected and whether that came, what it has spent, and the question it is waiting on when it is stuck. Read from memory while the job runs and from its last checkpoint once it has ended. Output: 200 with the job, or 404 when there is no such job.
func (j *ActJobs) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), actJobReadTimeout)
	defer cancel()
	job, err := j.runner.Job(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

// Stop handles POST /act/{id}/stop: the job ends as soon as the tool it is in returns, and nothing further runs. Output: 204, or 404 when no job with that id is running.
func (j *ActJobs) Stop(w http.ResponseWriter, r *http.Request) {
	actJobControl(w, j.runner.Stop(r.PathValue("id")))
}

// Pause handles POST /act/{id}/pause: the job takes no further step until POST /act/{id}/resume. Output: 204, or 404 when no job with that id is running.
func (j *ActJobs) Pause(w http.ResponseWriter, r *http.Request) {
	actJobControl(w, j.runner.Pause(r.PathValue("id")))
}

// Resume handles POST /act/{id}/resume: a paused job carries on, and a job this daemon has never seen — what a restart leaves behind — is read back from its last checkpoint and started again from there. Input: an optional body {"budget": {"wall_seconds": number, "input_tokens": number, "steps": number}}, in the same shape POST /act takes, applied over the budget the job already has field by field; an omitted or zero field leaves that part of the job's own budget alone. Raising the budget is also what brings back a job that ran out of one — a job the user stopped, or that reached its goal, stays ended. Output: 204, 400 for a body that will not decode, or 404 when there is no such job or it may not be resumed.
func (j *ActJobs) Resume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Budget struct {
			WallSeconds int `json:"wall_seconds"`
			InputTokens int `json:"input_tokens"`
			Steps       int `json:"steps"`
		} `json:"budget"`
	}
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actJobReadTimeout)
	defer cancel()
	actJobControl(w, j.runner.Resume(ctx, r.PathValue("id"), actjob.Budget{
		Wall:        time.Duration(req.Budget.WallSeconds) * time.Second,
		InputTokens: req.Budget.InputTokens,
		Steps:       req.Budget.Steps,
	}))
}

// Answer handles POST /act/{id}/answer with body {"text": string}: the user's reply to the one question a stuck job asked, which is kept with the goal from then on and lets the loop carry on. Output: 204, 400 for a body that will not decode or an empty answer, or 404 when no job with that id is running or it is not waiting on a question.
func (j *ActJobs) Answer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "an answer needs some text", http.StatusBadRequest)
		return
	}
	actJobControl(w, j.runner.Answer(r.PathValue("id"), req.Text))
}

// actJobControl writes the answer every control route shares: nothing at all when it worked, 404 with the runner's own sentence when it did not. Every reason the runner refuses is about the job named not being there to take the call — no such job, not running, not paused, not waiting on a question, already ended — so there is one code to give and the sentence says which it was.
func actJobControl(w http.ResponseWriter, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
