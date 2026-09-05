package main

// The night of 2026-09-05: turn 1 "ring the refresh button" ringed item [n] "Reload"; turn 2 "draw a circle around it" resolved "it" against a fresh screen read instead of turn 1's target and circled the address bar. follow-up-ring in track10_act.go is the eval task for that regression, and the tests here cover the two-turn plumbing it needed: a task's Question2/Pass2, act10AskFollowUp carrying the first question's conversation_id into the second POST /ask, and act10DrawHopTargetsLabel reading the item draw actually landed on off the tool Detail internal/agent's resultSummary now carries for it ("drew around %q").

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAct10DrawHopTargetsLabel(t *testing.T) {
	steps := []string{"observe_screen", "draw"}
	cases := []struct {
		name    string
		details []string
		label   string
		want    bool
	}{
		{"matches the item drawn around", []string{"brave · Family Guy – Brave", `drew around "Reload"`}, "Reload", true},
		{"wrong item drawn around", []string{"brave · Family Guy – Brave", `drew around "Address bar"`}, "Reload", false},
		{"draw named no item at all", []string{"brave · Family Guy – Brave", "done"}, "Reload", false},
		{"no draw hop in the steps", nil, "Reload", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := steps
			if c.details == nil {
				s = []string{"observe_screen"}
			}
			if got := act10DrawHopTargetsLabel(s, c.details, c.label); got != c.want {
				t.Errorf("act10DrawHopTargetsLabel(%v, %v, %q) = %v, want %v", s, c.details, c.label, got, c.want)
			}
		})
	}
}

// follow-up-ring must actually be wired as a two-turn task: Question2 set and scored by Pass2, never by Pass — see act10Task's own doc on what turns a task into a two-turn one.
func TestAct10Tasks_FollowUpRingIsATwoTurnTierOneTask(t *testing.T) {
	var task *act10Task
	for i := range act10Tasks {
		if act10Tasks[i].ID == "follow-up-ring" {
			task = &act10Tasks[i]
		}
	}
	if task == nil {
		t.Fatal("no follow-up-ring task in act10Tasks")
	}
	if task.Tier != 1 {
		t.Errorf("Tier = %d, want 1", task.Tier)
	}
	if task.Question2 == "" {
		t.Fatal("Question2 must be set for a two-turn task")
	}
	if task.Pass2 == nil {
		t.Fatal("Pass2 must be set for a two-turn task")
	}
	if !task.Pass2([]string{"draw"}, false, "Drew a circle.", []string{`drew around "Reload button"`}) {
		t.Error("Pass2 should pass a draw hop whose detail names the Reload item")
	}
	if task.Pass2([]string{"draw"}, false, "Drew a circle.", []string{`drew around "Address bar"`}) {
		t.Error("Pass2 should fail a draw hop that landed on a different item")
	}
}

// act10TwoTurnFakeDaemon is act10FakeDaemon's shape, but for a task with a follow-up: it answers two different /ask calls with two different ids, records the conversation_id each /ask request carried, and streams both asks' events off one /events connection, in order — exactly what act10RunTask needs since it reuses the same connection and scanner for the second collect.
func act10TwoTurnFakeDaemon(t *testing.T, token string, ids [2]string, convID string, events []string) (srv *httptest.Server, gotConvIDs *[]string) {
	t.Helper()
	var seen []string
	call := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ConversationID string `json:"conversation_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req.ConversationID)
		id := ids[0]
		if call == 1 {
			id = ids[1]
		}
		call++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": id, "conversation_id": convID})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, line := range events {
			w.Write([]byte(line))
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/conversations/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux), &seen
}

// The core regression check: act10RunTask on a two-turn task must fire the second question with the first answer's own conversation_id (the same thread the user's real turn 2 landed in), fold the second turn's own events, and score Pass2 against them rather than Pass.
func TestAct10RunTask_FollowUpQuestionReusesTheConversation(t *testing.T) {
	const token = "test-token"
	srv, convIDsSeen := act10TwoTurnFakeDaemon(t, token, [2]string{"ask-1", "ask-2"}, "conv-42", []string{
		sseLine("ask-1", "tool", "observe_screen"),
		sseLine("ask-1", "tool", "point_at"),
		sseAnswerLine("ask-1", "Ringed the reload button.", "conv-42"),
		sseLine("ask-1", "done", ""),
		sseLine("ask-2", "tool", "draw"),
		sseAnswerLine("ask-2", "Drew a circle.", "conv-42"),
		sseLine("ask-2", "done", ""),
	})
	defer srv.Close()

	task := act10Task{
		ID:        "follow-up-ring",
		Tier:      1,
		Question:  "ring the reload button",
		Question2: "draw a circle around it",
		Pass2: func(steps []string, ring bool, answer string, details []string) bool {
			return act10Has(steps, "draw")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "", task)

	if got.Err != "" {
		t.Fatalf("act10RunTask error: %s", got.Err)
	}
	if !got.Pass {
		t.Errorf("Pass = false, want true; steps=%v answer=%q", got.Steps, got.Answer)
	}
	if got.Answer != "Drew a circle." {
		t.Errorf("Answer = %q, want the second turn's own answer", got.Answer)
	}
	if strings.Join(got.Steps, ">") != "draw" {
		t.Errorf("Steps = %v, want only the second turn's own steps", got.Steps)
	}
	if len(*convIDsSeen) != 2 || (*convIDsSeen)[0] != "" || (*convIDsSeen)[1] != "conv-42" {
		t.Fatalf("conversation ids the daemon saw = %v, want [\"\" \"conv-42\"] — the first ask opens its own thread, the second must land in it", *convIDsSeen)
	}
}

// A task's follow-up must never be asked when the first turn itself failed: there is nothing to be a follow-up to, and firing it anyway would silently score a broken run against the wrong question.
func TestAct10RunTask_SkipsTheFollowUpWhenTheFirstTurnErrors(t *testing.T) {
	const token = "test-token"
	srv, convIDsSeen := act10TwoTurnFakeDaemon(t, token, [2]string{"ask-1", "ask-2"}, "conv-1", []string{
		sseLine("ask-1", "error", "the model call failed"),
	})
	defer srv.Close()

	task := act10Task{
		ID:        "follow-up-ring",
		Tier:      1,
		Question:  "ring the reload button",
		Question2: "draw a circle around it",
		Pass2:     func(steps []string, ring bool, answer string, details []string) bool { return true },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "", task)

	if got.Err == "" {
		t.Fatal("expected the first turn's error to be reported")
	}
	if got.Pass {
		t.Error("a task whose first turn errored must not pass")
	}
	if len(*convIDsSeen) != 1 {
		t.Errorf("the daemon saw %d /ask calls, want 1 — the follow-up must never fire after a failed first turn", len(*convIDsSeen))
	}
}

// act10AskFollowUp must actually send the conversation_id it was given, the one field that tells /ask this question belongs to an existing thread rather than opening a new one.
func TestAct10AskFollowUp_SendsTheConversationID(t *testing.T) {
	var body struct {
		Question       string `json:"question"`
		ConversationID string `json:"conversation_id"`
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": "ask-2", "conversation_id": "conv-7"})
	}))
	defer backend.Close()

	id, err := act10AskFollowUp(context.Background(), &http.Client{}, backend.URL, "tok", "draw a circle around it", "", "conv-7")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ask-2" {
		t.Errorf("id = %q, want ask-2", id)
	}
	if body.ConversationID != "conv-7" {
		t.Errorf("conversation_id sent = %q, want conv-7", body.ConversationID)
	}
	if body.Question != "draw a circle around it" {
		t.Errorf("question sent = %q", body.Question)
	}
}
