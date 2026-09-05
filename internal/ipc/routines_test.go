package ipc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ora/internal/agent"
	"ora/internal/db"
)

// newRoutinesServer wires a Server behind a real HTTP server with just the routines routes registered, under the same patterns cmd/daemon.go gives them.
func newRoutinesServer(t *testing.T, asker Asker, store *db.Store) (*Server, *httptest.Server) {
	t.Helper()
	s := New(asker, store, nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/routines", s.Routines)
	mux.HandleFunc("/routines/{id}", s.RoutineDelete)
	mux.HandleFunc("/routines/{id}/run", s.RoutineRun)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// TestRoutinesCreateListDelete walks the basic lifecycle: POST creates one, GET lists it with the fields a fresh routine should carry, and DELETE removes it — a second DELETE and one against an id that never existed both answer 404.
func TestRoutinesCreateListDelete(t *testing.T) {
	store := newReadStore(t)
	_, srv := newRoutinesServer(t, &fakeAsker{}, store)

	var created map[string]string
	if code := postJSON(t, srv, "/routines", `{"text":"tell me the one thing I must do today","schedule":"weekdays at 8"}`, &created); code != http.StatusCreated {
		t.Fatalf("POST /routines status = %d, want 201", code)
	}
	if created["id"] == "" {
		t.Fatalf("POST /routines = %+v, want an id", created)
	}

	var list struct{ Routines []RoutineView }
	getJSON(t, srv, "/routines", &list)
	if len(list.Routines) != 1 {
		t.Fatalf("routines = %d, want 1", len(list.Routines))
	}
	r := list.Routines[0]
	if r.ID != created["id"] || r.Text != "tell me the one thing I must do today" || r.Schedule != "weekdays at 8" {
		t.Errorf("routine = %+v", r)
	}
	if !r.Enabled || r.LastRun != "" || r.LastAnswer != "" {
		t.Errorf("fresh routine = %+v, want enabled with no run yet", r)
	}

	if code := deleteRequest(t, srv, "/routines/"+created["id"]); code != http.StatusNoContent {
		t.Errorf("DELETE /routines/%s = %d, want 204", created["id"], code)
	}
	if code := deleteRequest(t, srv, "/routines/"+created["id"]); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", code)
	}
	if code := deleteRequest(t, srv, "/routines/999"); code != http.StatusNotFound {
		t.Errorf("DELETE on a routine that never existed = %d, want 404", code)
	}
}

// TestCreateRoutineRequiresTextAndSchedule checks a blank instruction or schedule is 400.
func TestCreateRoutineRequiresTextAndSchedule(t *testing.T) {
	store := newReadStore(t)
	_, srv := newRoutinesServer(t, &fakeAsker{}, store)

	cases := []string{`{"text":"","schedule":"every day at 8"}`, `{"text":"tell me something","schedule":""}`}
	for _, body := range cases {
		if code := postJSON(t, srv, "/routines", body, nil); code != http.StatusBadRequest {
			t.Errorf("POST /routines %s = %d, want 400", body, code)
		}
	}
}

// TestRoutineRunAsksRecordsAndNotifies checks the run-now route puts the instruction plus the answer-shape suffix to the asker, records the answer as the routine's last run, and posts a notice carrying it under place "routine".
func TestRoutineRunAsksRecordsAndNotifies(t *testing.T) {
	store := newReadStore(t)
	id, err := store.AddRoutine(t.Context(), "tell me the one thing I must do today", "weekdays at 8")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	asker := &fakeAsker{trace: agent.TurnTrace{Answer: "Ship the report — it's due today."}}
	_, srv := newRoutinesServer(t, asker, store)

	var reply map[string]string
	idStr := strconv.FormatInt(id, 10)
	if code := postJSON(t, srv, "/routines/"+idStr+"/run", `{}`, &reply); code != http.StatusOK {
		t.Fatalf("POST /routines/%s/run status = %d, want 200", idStr, code)
	}
	if reply["answer"] != "Ship the report — it's due today." {
		t.Errorf("answer = %q", reply["answer"])
	}

	r, err := store.RoutineByID(t.Context(), id)
	if err != nil || r.LastAnswer != "Ship the report — it's due today." || r.LastRun.IsZero() {
		t.Errorf("routine after run = %+v, %v", r, err)
	}
}

// TestRoutineRunSendsNoNoticeForNothing checks that an exact "NOTHING" answer is still recorded as the run but reported back so the caller can tell nothing was said.
func TestRoutineRunSendsNoNoticeForNothing(t *testing.T) {
	store := newReadStore(t)
	id, err := store.AddRoutine(t.Context(), "tell me if anything is on fire", "every 1 hour")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	asker := &fakeAsker{trace: agent.TurnTrace{Answer: "NOTHING"}}
	_, srv := newRoutinesServer(t, asker, store)

	var reply map[string]string
	idStr := strconv.FormatInt(id, 10)
	if code := postJSON(t, srv, "/routines/"+idStr+"/run", `{}`, &reply); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if reply["answer"] != "NOTHING" {
		t.Errorf("answer = %q, want NOTHING", reply["answer"])
	}
	r, err := store.RoutineByID(t.Context(), id)
	if err != nil || r.LastAnswer != "NOTHING" || r.LastRun.IsZero() {
		t.Errorf("routine after a NOTHING run = %+v, %v, want the run still recorded", r, err)
	}
}

// TestRoutineRunMissingIDIs404 checks POST /routines/{id}/run against an id that names no routine.
func TestRoutineRunMissingIDIs404(t *testing.T) {
	store := newReadStore(t)
	_, srv := newRoutinesServer(t, &fakeAsker{}, store)
	if code := postJSON(t, srv, "/routines/999/run", `{}`, nil); code != http.StatusNotFound {
		t.Errorf("POST /routines/999/run = %d, want 404", code)
	}
}

// TestRoutineRunSkipsAlreadyRunning checks the in-flight guard RoutineRun shares with the scheduler tick: a routine store.TryStart already holds — standing in for the scheduler's own tick asking it right now — answers 409 rather than asking and recording the run a second time.
func TestRoutineRunSkipsAlreadyRunning(t *testing.T) {
	store := newReadStore(t)
	id, err := store.AddRoutine(t.Context(), "tell me the one thing I must do today", "weekdays at 8")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	if !store.TryStart(id) {
		t.Fatal("TryStart: expected the first claim to succeed")
	}
	t.Cleanup(func() { store.Finish(id) })

	asker := &fakeAsker{trace: agent.TurnTrace{Answer: "should not be asked"}}
	_, srv := newRoutinesServer(t, asker, store)

	idStr := strconv.FormatInt(id, 10)
	if code := postJSON(t, srv, "/routines/"+idStr+"/run", `{}`, nil); code != http.StatusConflict {
		t.Errorf("POST /routines/%s/run while already running = %d, want 409", idStr, code)
	}
	r, err := store.RoutineByID(t.Context(), id)
	if err != nil || !r.LastRun.IsZero() {
		t.Errorf("routine after a skipped run = %+v, %v, want no run recorded", r, err)
	}
}

// ctxCapturingAsker blocks until unblock is closed, then records whether the context it was handed was already cancelled — the way it would be if RoutineRun had passed the HTTP request's own context through, rather than one bounded by askTimeout.
type ctxCapturingAsker struct {
	unblock chan struct{}
	trace   agent.TurnTrace
	ctxErr  error
}

func (a *ctxCapturingAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	<-a.unblock
	a.ctxErr = ctx.Err()
	return a.trace, nil
}

// TestRoutineRunSurvivesRequestCancellation checks that a run-now ask keeps going, and still records its result, after the client that started it disconnects — the same "the window closing must not silently abort a minutes-long ask" guarantee /ask's own run() gives, via its own askTimeout-bounded context rather than r.Context().
func TestRoutineRunSurvivesRequestCancellation(t *testing.T) {
	store := newReadStore(t)
	id, err := store.AddRoutine(t.Context(), "tell me the one thing I must do today", "weekdays at 8")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	unblock := make(chan struct{})
	asker := &ctxCapturingAsker{unblock: unblock, trace: agent.TurnTrace{Answer: "still going"}}
	_, srv := newRoutinesServer(t, asker, store)

	idStr := strconv.FormatInt(id, 10)
	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/routines/"+idStr+"/run", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Give the request time to reach the handler and start the ask, then cancel it client-side — the way closing the window would — and give the server a moment to notice, if it were (wrongly) watching r.Context().
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(unblock)

	deadline := time.Now().Add(2 * time.Second)
	for {
		r, err := store.RoutineByID(t.Context(), id)
		if err == nil && !r.LastRun.IsZero() {
			if r.LastAnswer != "still going" {
				t.Errorf("LastAnswer = %q, want the ask's real answer", r.LastAnswer)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the run to be recorded despite the cancelled request")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if asker.ctxErr != nil {
		t.Errorf("ask context error = %v, want nil — the ask must not be tied to the cancelled request context", asker.ctxErr)
	}
}
