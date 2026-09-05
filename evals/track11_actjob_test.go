package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/act"
	"ora/internal/actjob"
	"ora/internal/ipc"
	"ora/internal/ipctoken"
)

// TestAct11Selected checks the -act11-tasks selector: blank runs every task in table order, a comma list keeps only the named ones in table order, and an unknown id is silently dropped rather than erroring.
func TestAct11Selected(t *testing.T) {
	tasks := []act11Task{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	if got := act11Selected(tasks, ""); len(got) != 3 {
		t.Fatalf("blank selector = %d tasks, want all 3", len(got))
	}
	got := act11Selected(tasks, "c,a")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("act11Selected(tasks, \"c,a\") = %v, want [a c] in table order", got)
	}
	if got := act11Selected(tasks, "nope"); len(got) != 0 {
		t.Fatalf("unknown id = %v, want none", got)
	}
	if got := act11Selected(tasks, " b , c "); len(got) != 2 {
		t.Fatalf("whitespace around ids: got %v, want [b c]", got)
	}
}

// TestAct11CostLine checks the cost line's two shapes: plain rounds/tokens with no by-model breakdown, and the same line with a sorted, semicolon-joined per-model breakdown appended.
func TestAct11CostLine(t *testing.T) {
	plain := act11CostLine(actjob.Spend{Rounds: 3, Input: 100, Cached: 20, Output: 40})
	want := "3 rounds, 100 in / 20 cached / 40 out"
	if plain != want {
		t.Errorf("plain spend: got %q, want %q", plain, want)
	}

	withModels := act11CostLine(actjob.Spend{
		Rounds: 5, Input: 300, Cached: 50, Output: 90,
		ByModel: map[string]actjob.Usage{
			"gemini": {Input: 200, Cached: 50, Output: 60},
			"claude": {Input: 100, Output: 30},
		},
	})
	wantModels := "5 rounds, 300 in / 50 cached / 90 out (claude: 100 in / 0 cached / 30 out; gemini: 200 in / 50 cached / 60 out)"
	if withModels != wantModels {
		t.Errorf("spend with models: got %q, want %q", withModels, wantModels)
	}
}

// sseActLine renders one "act" SSE line the way ipc.ActEmitter does: Detail is the actjob.Event marshaled as JSON, nested inside the outer ipc.Event.
func sseActLine(jobID string, ae actjob.Event) string {
	detail, err := json.Marshal(ae)
	if err != nil {
		panic(err)
	}
	line, err := json.Marshal(ipc.Event{ID: jobID, Type: ipc.ActEventType, Text: ae.Text, Detail: string(detail)})
	if err != nil {
		panic(err)
	}
	return "data: " + string(line) + "\n\n"
}

// TestAct11Collect checks the scan loop that watches the shared /events stream for one job's own "act" events: it must collect only this job's events, decode each one's Detail back into an actjob.Event, ignore another job's or an ask's events on the same connection, and stop as soon as a "done" kind arrives.
func TestAct11Collect(t *testing.T) {
	t.Run("collects only this job's events, stops at done", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(sseLine("ask-1", "tool", "unrelated ask sharing the stream"))
		b.WriteString(sseActLine("job-9", actjob.Event{Job: "job-9", Kind: "started"}))
		b.WriteString(sseActLine("job-1", actjob.Event{Job: "job-1", Kind: "started", Text: "the goal"}))
		b.WriteString(sseActLine("job-1", actjob.Event{Job: "job-1", Kind: "step", Step: 1, Text: "click"}))
		b.WriteString(sseActLine("job-1", actjob.Event{Job: "job-1", Kind: "verified", Step: 1, Outcome: "pass"}))
		b.WriteString(sseActLine("job-1", actjob.Event{Job: "job-1", Kind: "done", State: actjob.Done, Text: "finished"}))
		b.WriteString(sseActLine("job-1", actjob.Event{Job: "job-1", Kind: "step", Step: 2, Text: "after done, must not be collected"}))

		out := act11Collect(bufio.NewScanner(strings.NewReader(b.String())), "job-1")
		if !out.Done {
			t.Fatalf("out.Done = false, want true")
		}
		var kinds []string
		for _, e := range out.Events {
			kinds = append(kinds, e.Kind)
		}
		if got := strings.Join(kinds, ">"); got != "started>step>verified>done" {
			t.Errorf("kinds = %q, want started>step>verified>done", got)
		}
	})

	t.Run("stream ends with no done", func(t *testing.T) {
		out := act11Collect(bufio.NewScanner(strings.NewReader(sseActLine("job-2", actjob.Event{Job: "job-2", Kind: "started"}))), "job-2")
		if out.Done {
			t.Fatalf("out.Done = true, want false when the stream ended without a done kind")
		}
		if len(out.Events) != 1 || out.Events[0].Kind != "started" {
			t.Errorf("Events = %v, want one started event even without a terminal one", out.Events)
		}
	})
}

// TestAct11TaskPassRules pins down every track 11 task's pass rule against hand-built Jobs, so a rule that regresses fails here instead of only showing up on a live run that costs several minutes to fail.
func TestAct11TaskPassRules(t *testing.T) {
	byID := map[string]act11Task{}
	for _, task := range act11Tasks {
		byID[task.ID] = task
	}
	for _, id := range []string{"episode", "url-enter", "form-stop", "switch-read", "resume"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("act11Tasks is missing task %q", id)
		}
	}

	t.Run("episode", func(t *testing.T) {
		pass := byID["episode"].Pass
		ok := actjob.Job{State: actjob.Done, Steps: []actjob.Step{
			{Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Watch Family Guy S16 E8"}},
		}}
		if !pass(ok, nil) {
			t.Error("want pass: state done, a verified step naming S16 and E8")
		}
		wrongEpisode := actjob.Job{State: actjob.Done, Steps: []actjob.Step{
			{Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Watch Family Guy S16 E7"}},
		}}
		if pass(wrongEpisode, nil) {
			t.Error("want fail: verified step names the wrong episode")
		}
		notDone := actjob.Job{State: actjob.Failed, Steps: ok.Steps}
		if pass(notDone, nil) {
			t.Error("want fail: job did not reach done")
		}
		neverVerified := actjob.Job{State: actjob.Done, Steps: []actjob.Step{
			{Outcome: "fail", Expect: act.Check{Kind: act.TitleContains, Value: "S16 E8"}},
		}}
		if pass(neverVerified, nil) {
			t.Error("want fail: the matching step never checked out")
		}
	})

	t.Run("url-enter", func(t *testing.T) {
		pass := byID["url-enter"].Pass
		byStep := actjob.Job{Steps: []actjob.Step{
			{Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Example Domain"}},
		}}
		if !pass(byStep, nil) {
			t.Error("want pass: a verified expect names Example Domain")
		}
		byObservation := actjob.Job{Observations: []string{"brave · about:blank", "brave · Example Domain"}}
		if !pass(byObservation, nil) {
			t.Error("want pass: the last observation names Example Domain")
		}
		neither := actjob.Job{Observations: []string{"brave · about:blank"}}
		if pass(neither, nil) {
			t.Error("want fail: neither a verified expect nor the last observation names it")
		}
	})

	t.Run("form-stop", func(t *testing.T) {
		pass := byID["form-stop"].Pass
		bothFieldsAndStopped := actjob.Job{
			State:    actjob.Stuck,
			Question: `Stopped before clicking [3] push button "Submit". Say "yes, submit it" and I will.`,
			Steps: []actjob.Step{
				{Outcome: "pass", Expect: act.Check{Kind: act.FieldHolds, Value: "alpha"}},
				{Outcome: "pass", Expect: act.Check{Kind: act.FieldHolds, Value: "bravo"}},
			},
		}
		if !pass(bothFieldsAndStopped, nil) {
			t.Error("want pass: both fields verified and stopped before Submit")
		}
		submitted := actjob.Job{State: actjob.Done, Steps: bothFieldsAndStopped.Steps}
		if pass(submitted, nil) {
			t.Error("want fail: job reached done instead of stopping before Submit")
		}
		oneField := actjob.Job{State: actjob.Stuck, Question: bothFieldsAndStopped.Question, Steps: bothFieldsAndStopped.Steps[:1]}
		if pass(oneField, nil) {
			t.Error("want fail: only one field verified")
		}
		wrongQuestion := actjob.Job{State: actjob.Stuck, Question: "What should the third field say?", Steps: bothFieldsAndStopped.Steps}
		if pass(wrongQuestion, nil) {
			t.Error("want fail: stuck on a question that is not the stop line")
		}
	})

	t.Run("switch-read", func(t *testing.T) {
		pass := byID["switch-read"].Pass
		ok := actjob.Job{Steps: []actjob.Step{
			{Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Notes"}},
		}}
		if !pass(ok, nil) {
			t.Error("want pass: a verified expect names the other window's title")
		}
		wrongWindow := actjob.Job{Steps: []actjob.Step{
			{Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Terminal"}},
		}}
		if pass(wrongWindow, nil) {
			t.Error("want fail: verified expect names the starting window, not the other one")
		}
	})

	t.Run("resume", func(t *testing.T) {
		pass := byID["resume"].Pass
		if !pass(actjob.Job{State: actjob.Done}, nil) {
			t.Error("want pass: the resumed job reached done")
		}
		if pass(actjob.Job{State: actjob.Failed}, nil) {
			t.Error("want fail: the resumed job did not reach done")
		}
	})
}

// TestAct11PostRetrying_RetriesOnceAfter429 checks the retry policy shared by /act, /act/{id}/resume and /act/{id}/answer: one retry after the response's own Retry-After, and no second retry on a second 429.
func TestAct11PostRetrying_RetriesOnceAfter429(t *testing.T) {
	t.Run("succeeds on the retry", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		resp, err := postRetrying(context.Background(), &http.Client{}, srv.URL, "tok", nil)
		if err != nil {
			t.Fatalf("postRetrying: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
		if attempts != 2 {
			t.Errorf("attempts = %d, want exactly one retry", attempts)
		}
	})

	t.Run("a second 429 is not retried again", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		_, err := postRetrying(context.Background(), &http.Client{}, srv.URL, "tok", nil)
		if err == nil || !strings.Contains(err.Error(), "429") {
			t.Fatalf("err = %v, want it to name the 429", err)
		}
		if attempts != 2 {
			t.Errorf("attempts = %d, want exactly one retry", attempts)
		}
	})
}

// act11ResumeBodies records the body each fake resume route received, by job id, so a test can check what a resume asked for.
var act11ResumeBodies sync.Map

// act11FakeDaemon builds a minimal stand-in for the daemon's /status, POST /act, GET /act/{id}, POST /act/{id}/resume and /events routes, just enough to drive act11RunTask end to end without a real daemon. jobsByCall is what GET /act/{id} answers, indexed by call number (the last entry repeats once calls run past the end) — a resume task needs only the one job served after its second done, so a single-entry slice is the common case.
func act11FakeDaemon(t *testing.T, token, id string, events []string, jobsByCall []actjob.Job) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ipctoken.HeaderName) != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/act", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ipctoken.HeaderName) != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, line := range events {
			fmt.Fprint(w, line)
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	var getCalls int32
	mux.HandleFunc("/act/"+id, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ipctoken.HeaderName) != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := int(atomic.AddInt32(&getCalls, 1)) - 1
		job := jobsByCall[len(jobsByCall)-1]
		if n < len(jobsByCall) {
			job = jobsByCall[n]
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(job)
	})
	mux.HandleFunc("/act/"+id+"/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ipctoken.HeaderName) != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		act11ResumeBodies.Store(id, string(body))
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

// TestAct11RunTaskAgainstFakeDaemon drives a plain (non-resume) task end to end against act11FakeDaemon: open /events, POST /act, collect that job's own events to done, GET the finished Job, and score it with the task's own Pass.
func TestAct11RunTaskAgainstFakeDaemon(t *testing.T) {
	const token, jobID = "test-token", "job-1"
	finished := actjob.Job{
		ID: jobID, State: actjob.Done,
		Steps: []actjob.Step{{Outcome: "pass", Expect: act.Check{Kind: act.TitleContains, Value: "Watch Family Guy S16 E8"}}},
		Spend: actjob.Spend{Rounds: 4, Input: 1000, Output: 200},
	}
	task := act11Task{ID: "fake", Goal: "play it", Pass: func(job actjob.Job, events []actjob.Event) bool {
		return job.State == actjob.Done && len(events) > 0 && events[len(events)-1].Kind == "done"
	}}
	srv := act11FakeDaemon(t, token, jobID, []string{
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "started", Text: "play it"}),
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "step", Step: 1, Text: "click"}),
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "verified", Step: 1, Outcome: "pass"}),
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "done", State: actjob.Done, Text: "done"}),
	}, []actjob.Job{finished})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := act11RunTask(ctx, &http.Client{}, srv.URL, token, task)
	if got.Err != "" {
		t.Fatalf("act11RunTask error: %s", got.Err)
	}
	if !got.Pass {
		t.Errorf("Pass = false, want true")
	}
	if got.Job.Spend.Rounds != 4 {
		t.Errorf("Job.Spend.Rounds = %d, want 4 (the finished Job read back by GET)", got.Job.Spend.Rounds)
	}
}

// TestAct11RunTaskResumeAgainstFakeDaemon drives the resume task's own shape: the first "done" carries state failed (the budget spent), act11RunTask must call POST /act/{id}/resume, then keep reading the same stream for a second "done" (state done) before scoring the Job GET returns afterward.
func TestAct11RunTaskResumeAgainstFakeDaemon(t *testing.T) {
	const token, jobID = "test-token", "job-2"
	resumed := actjob.Job{ID: jobID, State: actjob.Done}
	task := act11Task{ID: "resume", Goal: "play it", Budget: act11Budget{Steps: 3}, Resume: true, ResumeBudget: act11Budget{Steps: 12},
		Pass: func(job actjob.Job, events []actjob.Event) bool { return job.State == actjob.Done }}

	srv := act11FakeDaemon(t, token, jobID, []string{
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "started", Text: "play it"}),
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "done", State: actjob.Failed, Text: "the step budget of 3 is spent"}),
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "started", Text: "play it"}),
		sseActLine(jobID, actjob.Event{Job: jobID, Kind: "done", State: actjob.Done, Text: "done"}),
	}, []actjob.Job{resumed})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := act11RunTask(ctx, &http.Client{}, srv.URL, token, task)
	if got.Err != "" {
		t.Fatalf("act11RunTask error: %s", got.Err)
	}
	if !got.Pass {
		t.Errorf("Pass = false, want true: the resumed run reached done")
	}
	body, _ := act11ResumeBodies.Load(jobID)
	if !strings.Contains(fmt.Sprint(body), `"steps":12`) {
		t.Errorf("resume body %q, want the raised twelve-step budget", body)
	}
}

// TestAct11RunTask_NoResumeCalledWhenFirstDoneNeverArrives checks that a Resume task never calls POST /act/{id}/resume when the first "done" never showed up — resuming a job whose state is unknown would be resuming blind.
func TestAct11RunTask_NoResumeCalledWhenFirstDoneNeverArrives(t *testing.T) {
	const token, jobID = "test-token", "job-3"
	var resumeCalled int32
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/act", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": jobID})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		fmt.Fprint(w, sseActLine(jobID, actjob.Event{Job: jobID, Kind: "started"}))
		flusher.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/act/"+jobID+"/resume", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&resumeCalled, 1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	task := act11Task{ID: "resume", Resume: true, Pass: func(actjob.Job, []actjob.Event) bool { return true }}
	got := act11RunTask(ctx, &http.Client{}, srv.URL, token, task)
	if got.Err == "" {
		t.Fatal("want an error: the stream never produced a done to resume after")
	}
	if resumeCalled != 0 {
		t.Errorf("resume was called %d times, want 0", resumeCalled)
	}
}
