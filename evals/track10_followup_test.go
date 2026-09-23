package main

// The night of 2026-09-05: turn 1 "ring the refresh button" ringed item [n] "Reload"; turn 2 "draw a circle around it" resolved "it" against a fresh screen read instead of turn 1's target and circled the address bar. follow-up-ring in track10_act.go is the eval task for that regression, and the tests here cover the two-turn plumbing it needed: act10RunTask asking the second question with the first answer's conversation_id and scoring it with Pass2, and never asking it after the first turn failed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

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
			return slices.Contains(steps, "draw")
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
