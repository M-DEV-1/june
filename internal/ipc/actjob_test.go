package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/act"
	"ora/internal/actjob"
	"ora/internal/db"
)

// jobExec answers every tool from a script, so the routes can be driven end to end without a screen.
type jobExec struct {
	mu   sync.Mutex
	hold chan struct{}
}

func (e *jobExec) ExecuteAskTool(ctx context.Context, name string, args map[string]any) string {
	e.mu.Lock()
	hold := e.hold
	e.mu.Unlock()
	if hold != nil && name != "observe_screen" {
		select {
		case <-hold:
		case <-ctx.Done():
			return "error: stopped"
		}
	}
	switch name {
	case "observe_screen":
		return "Brave · Netflix\n[1] push button \"Play\" (10,10)"
	case "wait_for":
		// The step loop takes each check once before it acts, with a one-poll timeout, to see whether the check already held. Here it never did, so the reading taken after the action is the one that counts and every scripted step verifies as it did before.
		if ms, ok := args["timeout_ms"].(float64); ok && ms <= 1 {
			return act.WaitFailPrefix + `0s: the title is "Netflix"`
		}
		return act.WaitPassPrefix + `the title is "S16 E8"`
	}
	return "did " + name
}

// jobServer builds a Server with a job runner wired to its own event hub, and returns the routes, the runner and a reader of the events that went out.
func jobServer(t *testing.T, exec actjob.Executor, replies ...string) (*ActJobs, *actjob.Runner, func() []Event) {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(nil, store, nil, nil)

	var i int
	var mu sync.Mutex
	model := func(ctx context.Context, prompt string) (string, actjob.Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		reply := replies[len(replies)-1]
		if i < len(replies) {
			reply = replies[i]
		}
		i++
		return reply, actjob.Usage{Model: "fake", Input: 500}, nil
	}
	runner := actjob.New(store, exec, map[string]actjob.Model{"fake": model}, "fake", ActEmitter(s))

	ch := s.hub.subscribe()
	var evMu sync.Mutex
	var events []Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			evMu.Lock()
			events = append(events, ev)
			evMu.Unlock()
		}
	}()
	t.Cleanup(func() { s.hub.unsubscribe(ch); <-done })

	return NewActJobs(runner), runner, func() []Event {
		evMu.Lock()
		defer evMu.Unlock()
		return append([]Event(nil), events...)
	}
}

// post drives one route through a mux with the same patterns the daemon registers, so the {id} path values resolve the way they do in production.
func post(t *testing.T, j *ActJobs, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /act", j.Start)
	mux.HandleFunc("GET /act/{id}", j.Get)
	mux.HandleFunc("POST /act/{id}/stop", j.Stop)
	mux.HandleFunc("POST /act/{id}/pause", j.Pause)
	mux.HandleFunc("POST /act/{id}/resume", j.Resume)
	mux.HandleFunc("POST /act/{id}/answer", j.Answer)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

// stepJSON is a decision in the shape the round prompt asks for.
const stepJSON = `{"plan":"press play","next":"click play","tool":"click","args":{"n":1},"expect":{"kind":"title_contains","value":"S16 E8"}}`

// TestActRoutes_StartThenReadThenFinish checks POST /act opens a job and answers with its id, that GET /act/{id} carries the whole record, and that the job's progress went out on the shared event stream as "act" events with the job's id and a readable line.
func TestActRoutes_StartThenReadThenFinish(t *testing.T) {
	j, runner, events := jobServer(t, &jobExec{}, stepJSON, `{"done":true,"say":"It is playing S16 E8."}`)

	w := post(t, j, "POST", "/act", `{"goal":"play S16 E8","window":"Brave"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /act = %d (%s), want 202", w.Code, w.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("POST /act body = %q, want an id", w.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	var job actjob.Job
	for time.Now().Before(deadline) {
		job, _ = runner.Job(context.Background(), started.ID)
		if job.State == actjob.Done {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if job.State != actjob.Done {
		t.Fatalf("job state = %q, want done", job.State)
	}

	got := post(t, j, "GET", "/act/"+started.ID, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET /act/{id} = %d, want 200", got.Code)
	}
	var read actjob.Job
	if err := json.Unmarshal(got.Body.Bytes(), &read); err != nil {
		t.Fatalf("GET /act/{id} body: %v", err)
	}
	if read.Goal != "play S16 E8" || read.Say == "" || len(read.Steps) != 1 || read.Spend.Rounds != 2 {
		t.Errorf("GET /act/{id} = %+v, want the goal, the closing words, the step and the spend", read)
	}

	if missing := post(t, j, "GET", "/act/act-404", ""); missing.Code != http.StatusNotFound {
		t.Errorf("GET an unknown job = %d, want 404", missing.Code)
	}

	var kinds []string
	for _, ev := range events() {
		if ev.Type != ActEventType {
			t.Fatalf("event type %q on the stream, want %q", ev.Type, ActEventType)
		}
		if ev.ID != started.ID {
			t.Errorf("event id %q, want the job's id %q", ev.ID, started.ID)
		}
		var parsed actjob.Event
		if err := json.Unmarshal([]byte(ev.Detail), &parsed); err != nil {
			t.Fatalf("event detail is not a job event: %v", err)
		}
		kinds = append(kinds, parsed.Kind)
	}
	if strings.Join(kinds, ",") != "started,step,verified,done" {
		t.Fatalf("event kinds = %v, want started,step,verified,done", kinds)
	}
	for _, ev := range events() {
		if ev.Text == "" {
			t.Errorf("an act event carried no line for the hover to show: %+v", ev)
		}
	}
}

// TestActRoutes_StopPauseResumeAndAnswer checks each control route reaches the runner, and that one addressed to a job that is not running is a 404 rather than a silent success.
func TestActRoutes_StopPauseResumeAndAnswer(t *testing.T) {
	exec := &jobExec{hold: make(chan struct{})}
	j, runner, _ := jobServer(t, exec, stepJSON, `{"done":true,"say":"done"}`)

	w := post(t, j, "POST", "/act", `{"goal":"play S16 E8"}`)
	var started struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &started)

	if got := post(t, j, "POST", "/act/"+started.ID+"/pause", ""); got.Code != http.StatusNoContent {
		t.Errorf("pause = %d (%s), want 204", got.Code, got.Body.String())
	}
	if got := post(t, j, "POST", "/act/"+started.ID+"/resume", ""); got.Code != http.StatusNoContent {
		t.Errorf("resume = %d (%s), want 204", got.Code, got.Body.String())
	}
	// The job is mid-step, not stuck on a question, so the answer is refused: buffering it would hand it to whatever the job asks next, about something else entirely.
	if got := post(t, j, "POST", "/act/"+started.ID+"/answer", `{"text":"it is under season 16"}`); got.Code != http.StatusNotFound {
		t.Errorf("answer to a job that asked nothing = %d (%s), want 404", got.Code, got.Body.String())
	}
	if got := post(t, j, "POST", "/act/"+started.ID+"/answer", `{"text":"  "}`); got.Code != http.StatusBadRequest {
		t.Errorf("an empty answer = %d, want 400", got.Code)
	}
	if got := post(t, j, "POST", "/act/"+started.ID+"/stop", ""); got.Code != http.StatusNoContent {
		t.Errorf("stop = %d (%s), want 204", got.Code, got.Body.String())
	}
	close(exec.hold)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job, _ := runner.Job(context.Background(), started.ID); job.State == actjob.Stopped {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if job, _ := runner.Job(context.Background(), started.ID); job.State != actjob.Stopped {
		t.Fatalf("job state = %q, want stopped", job.State)
	}
	for _, path := range []string{"/stop", "/pause"} {
		if got := post(t, j, "POST", "/act/"+started.ID+path, ""); got.Code != http.StatusNotFound {
			t.Errorf("%s on a finished job = %d, want 404", path, got.Code)
		}
	}
}

// TestActRoutes_ResumeCarriesARaisedBudget checks POST /act/{id}/resume takes the same budget shape POST /act does and applies it over the spent one, so a job that ran out of steps can be sent back in with more of them.
func TestActRoutes_ResumeCarriesARaisedBudget(t *testing.T) {
	j, runner, _ := jobServer(t, &jobExec{}, stepJSON, stepJSON, `{"done":true,"say":"It is playing."}`)

	w := post(t, j, "POST", "/act", `{"goal":"play S16 E8","budget":{"steps":1}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /act = %d (%s), want 202", w.Code, w.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &started)
	waitJobState(t, runner, started.ID, actjob.Failed)

	if got := post(t, j, "POST", "/act/"+started.ID+"/resume", `{"budget":{"steps":5}}`); got.Code != http.StatusNoContent {
		t.Fatalf("resume with a raised budget = %d (%s), want 204", got.Code, got.Body.String())
	}
	job := waitJobState(t, runner, started.ID, actjob.Done)
	if job.Budget.Steps != 5 {
		t.Errorf("budget = %+v, want the five steps the resume asked for", job.Budget)
	}
	if len(job.Steps) != 2 {
		t.Errorf("steps = %d, want the one taken on the first budget and one more", len(job.Steps))
	}
	if got := post(t, j, "POST", "/act/"+started.ID+"/resume", `not json`); got.Code != http.StatusBadRequest {
		t.Errorf("a resume body that will not decode = %d, want 400", got.Code)
	}
}

// waitJobState blocks until the job reaches the state named, or fails the test after two seconds.
func waitJobState(t *testing.T, runner *actjob.Runner, id string, want actjob.State) actjob.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := runner.Job(context.Background(), id)
		if err == nil && job.State == want {
			return job
		}
		time.Sleep(2 * time.Millisecond)
	}
	job, _ := runner.Job(context.Background(), id)
	t.Fatalf("job %s is %q (%s), want %q", id, job.State, job.Err, want)
	return actjob.Job{}
}

// TestActRoutes_RefusesAGoallessOrUnknownBrainJob checks the two ways a job is refused before it is written anywhere.
func TestActRoutes_RefusesAGoallessOrUnknownBrainJob(t *testing.T) {
	j, _, _ := jobServer(t, &jobExec{}, `{"done":true,"say":"done"}`)
	if got := post(t, j, "POST", "/act", `{"goal":"   "}`); got.Code != http.StatusBadRequest {
		t.Errorf("a job with no goal = %d, want 400", got.Code)
	}
	if got := post(t, j, "POST", "/act", `{"goal":"play it","brain":"nope"}`); got.Code != http.StatusBadRequest {
		t.Errorf("a job naming an unknown brain = %d, want 400", got.Code)
	}
	if got := post(t, j, "POST", "/act", `not json`); got.Code != http.StatusBadRequest {
		t.Errorf("a body that will not decode = %d, want 400", got.Code)
	}
}

// TestActStart_BadRequestOnlyForValidationErrors checks POST /act separates what the caller got wrong from what the daemon could not do: a blank goal and a brain this daemon does not have are 400, while a store that cannot take the job is 500 — reporting a wedged store as "you sent a bad goal" sends the user looking in the wrong place.
func TestActStart_BadRequestOnlyForValidationErrors(t *testing.T) {
	j, _, _ := jobServer(t, &jobExec{}, stepJSON)

	if w := post(t, j, "POST", "/act", `{"goal":"   "}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST /act with a blank goal = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := post(t, j, "POST", "/act", `{"goal":"play S16 E8","brain":"nosuchbrain"}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST /act with an unknown brain = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := post(t, j, "POST", "/act", `{"goal":"play S16 E8","summary_brain":"nosuchbrain"}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST /act with an unknown summary brain = %d (%s), want 400", w.Code, w.Body.String())
	}

	// A store that has already been closed stands in for one that cannot take the write: everything the caller sent is fine, so this is the daemon's failure to report.
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	store.Close()
	model := func(ctx context.Context, prompt string) (string, actjob.Usage, error) {
		return stepJSON, actjob.Usage{}, nil
	}
	broken := NewActJobs(actjob.New(store, &jobExec{}, map[string]actjob.Model{"fake": model}, "fake", nil))
	if w := post(t, broken, "POST", "/act", `{"goal":"play S16 E8"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("POST /act against a closed store = %d (%s), want 500", w.Code, w.Body.String())
	}
}
