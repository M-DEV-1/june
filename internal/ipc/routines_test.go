package ipc

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"june/internal/agent"
	"june/internal/db"
	"june/internal/db/dbtest"
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
	store := dbtest.Open(t)
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

// waitForRoutineAnswer polls the routine until its last run holds want, which is how a caller sees the result of a run started in the background. Input: the store, the routine's id and the answer expected. Output: none; the test fails if it has not been recorded within two seconds.
func waitForRoutineAnswer(t *testing.T, store *db.Store, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r, err := store.RoutineByID(t.Context(), id)
		if err == nil && !r.LastRun.IsZero() {
			if r.LastAnswer != want {
				t.Fatalf("LastAnswer = %q, want %q", r.LastAnswer, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the run to be recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRoutineRunAsksRecordsAndNotifies checks the run-now route answers 202 with the routine's id straight away, then puts the instruction plus the answer-shape suffix to the asker in the background, records the answer as the routine's last run, and delivers it through the same say path a scheduled run uses — under place "routine" with the routine's id.
func TestRoutineRunAsksRecordsAndNotifies(t *testing.T) {
	store := dbtest.Open(t)
	id, err := store.AddRoutine(t.Context(), "tell me the one thing I must do today", "weekdays at 8")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	asker := &fakeAsker{trace: agent.TurnTrace{Answer: "Ship the report — it's due today."}}
	s, srv := newRoutinesServer(t, asker, store)
	said := make(chan Notice, 1)
	s.SetSay(func(n Notice) { said <- n })

	var reply map[string]string
	idStr := strconv.FormatInt(id, 10)
	if code := postJSON(t, srv, "/routines/"+idStr+"/run", `{}`, &reply); code != http.StatusAccepted {
		t.Fatalf("POST /routines/%s/run status = %d, want 202", idStr, code)
	}
	if reply["id"] != idStr {
		t.Errorf("reply = %+v, want the routine's id", reply)
	}
	if _, ok := reply["answer"]; ok {
		t.Errorf("reply = %+v, want no answer — the run has not finished yet", reply)
	}

	waitForRoutineAnswer(t, store, id, "Ship the report — it's due today.")

	select {
	case n := <-said:
		if n.Body != "Ship the report — it's due today." || n.Place != "routine" || n.ID != idStr || n.Kind != "routine" {
			t.Errorf("notice = %+v, want the answer under place routine with the routine's id", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the say path")
	}
}

// TestRoutineRunSendsNoNoticeForNothing checks that an exact "NOTHING" answer is still recorded as the run but says nothing to the user.
func TestRoutineRunSendsNoNoticeForNothing(t *testing.T) {
	store := dbtest.Open(t)
	id, err := store.AddRoutine(t.Context(), "tell me if anything is on fire", "every 1 hour")
	if err != nil {
		t.Fatalf("AddRoutine: %v", err)
	}
	asker := &fakeAsker{trace: agent.TurnTrace{Answer: "NOTHING"}}
	s, srv := newRoutinesServer(t, asker, store)
	said := make(chan Notice, 1)
	s.SetSay(func(n Notice) { said <- n })

	idStr := strconv.FormatInt(id, 10)
	if code := postJSON(t, srv, "/routines/"+idStr+"/run", `{}`, nil); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitForRoutineAnswer(t, store, id, "NOTHING")
	select {
	case n := <-said:
		t.Errorf("a NOTHING run said %+v, want nothing said", n)
	case <-time.After(100 * time.Millisecond):
	}
}
