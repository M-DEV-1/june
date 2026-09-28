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

	"june/internal/act"
	"june/internal/actjob"
	"june/internal/db/dbtest"
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
		return act.WaitPassPrefix + `the title is "S16 E8"`
	}
	return "did " + name
}

// heldExec is jobExec with the reading a step takes before it acts answering that the check was already satisfied, which is what a title check written for the window the job is already in looks like.
type heldExec struct{ jobExec }

func (e *heldExec) CheckHolds(ctx context.Context, check act.Check) bool { return true }

// jobServer builds a Server with a job runner wired to its own event hub, and returns the routes, the runner and a reader of the events that went out.
func jobServer(t *testing.T, exec actjob.Executor, replies ...string) (*ActJobs, *actjob.Runner, func() []Event) {
	t.Helper()
	store := dbtest.Open(t)
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
	if strings.Join(kinds, ",") != "started,plan,step,verified,done" {
		t.Fatalf("event kinds = %v, want started,plan,step,verified,done", kinds)
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
//
// It is the token budget that is exhausted here, not the step budget: running out of steps is only a bad guess at the size of the task and now asks the user rather than ending the job, so tokens are the budget that still leaves a job in the failed state Resume reopens.
func TestActRoutes_ResumeCarriesARaisedBudget(t *testing.T) {
	// Each round of this test's model costs 500 input tokens, so a budget of 1000 runs out on the third round's check, before the reply that would have said done.
	j, runner, _ := jobServer(t, &jobExec{}, stepJSON, stepJSON, `{"done":true,"say":"It is playing."}`)

	w := post(t, j, "POST", "/act", `{"goal":"play S16 E8","budget":{"input_tokens":1000}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /act = %d (%s), want 202", w.Code, w.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &started)
	waitJobState(t, runner, started.ID, actjob.Failed)

	if got := post(t, j, "POST", "/act/"+started.ID+"/resume", `{"budget":{"input_tokens":500000}}`); got.Code != http.StatusNoContent {
		t.Fatalf("resume with a raised budget = %d (%s), want 204", got.Code, got.Body.String())
	}
	job := waitJobState(t, runner, started.ID, actjob.Done)
	if job.Budget.InputTokens != 500000 {
		t.Errorf("budget = %+v, want the tokens the resume asked for", job.Budget)
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
	store := dbtest.Open(t)
	store.Close()
	model := func(ctx context.Context, prompt string) (string, actjob.Usage, error) {
		return stepJSON, actjob.Usage{}, nil
	}
	broken := NewActJobs(actjob.New(store, &jobExec{}, map[string]actjob.Model{"fake": model}, "fake", nil))
	if w := post(t, broken, "POST", "/act", `{"goal":"play S16 E8"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("POST /act against a closed store = %d (%s), want 500", w.Code, w.Body.String())
	}
}

// The window renders a step as passed or failed and nothing else, so a check that was already true before the action goes out as a pass with held_before beside it rather than as a third outcome word the window would draw as a red cross. Both the event and the job record say it the same way.
func TestActEvents_ACheckThatAlreadyHeldIsAPassWithTheFlag(t *testing.T) {
	j, runner, events := jobServer(t, &heldExec{}, stepJSON)

	w := post(t, j, "POST", "/act", `{"goal":"play S16 E8"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST /act = %d, want 202", w.Code)
	}
	var started struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &started)
	waitForStep(t, runner, started.ID)
	runner.Stop(started.ID)

	var verified actjob.Event
	var line string
	for _, ev := range events() {
		var got actjob.Event
		if json.Unmarshal([]byte(ev.Detail), &got) == nil && got.Kind == "verified" {
			verified, line = got, ev.Text
		}
	}
	if verified.Kind != "verified" {
		t.Fatal("no verified event went out on the stream")
	}
	if verified.Outcome != "pass" {
		t.Errorf("outcome = %q, want \"pass\": the wire carries two outcomes and a flag, not three outcomes", verified.Outcome)
	}
	if !verified.HeldBefore {
		t.Error("held_before is not on the event, so the window would draw a plain pass on a check that proved nothing")
	}
	if !strings.Contains(line, "already true") {
		t.Errorf("hover line = %q, want it to say the check was already true", line)
	}

	w = post(t, j, "GET", "/act/"+started.ID, "")
	var record struct {
		Steps []struct {
			Outcome    string `json:"outcome"`
			HeldBefore bool   `json:"held_before"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &record); err != nil {
		t.Fatalf("GET /act/{id}: %v", err)
	}
	if len(record.Steps) == 0 || record.Steps[0].Outcome != "pass" || !record.Steps[0].HeldBefore {
		t.Errorf("step JSON = %+v, want outcome pass with held_before true", record.Steps)
	}
}

// waitForStep blocks until a job has recorded its first verified step.
func waitForStep(t *testing.T, r *actjob.Runner, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := r.Job(context.Background(), id)
		if err == nil && len(job.Steps) > 0 && job.Steps[0].Outcome != "" {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s recorded no verified step", id)
}

// TestSpokenJob_WaitsForTheJobThenHandsBackWhatItSaid covers the way in for the voice session's do tool. The live agent's RunJob is this, so what it returns is what June reads out when the chain lands; before it existed a spoken chain of actions never reached the runner at all and was driven one raw tool call at a time inside the conversation.
func TestSpokenJob_WaitsForTheJobThenHandsBackWhatItSaid(t *testing.T) {
	j, _, _ := jobServer(t, &jobExec{}, stepJSON, `{"done":true,"say":"It is playing S16 E8."}`)
	said, err := j.Spoken(context.Background(), "open netflix and press play")
	if err != nil {
		t.Fatalf("Spoken: %v", err)
	}
	if !strings.Contains(said, "It is playing S16 E8.") {
		t.Errorf("Spoken returned %q, want the job's own closing words", said)
	}
}
