package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ora/internal/agent"
	"ora/internal/db"
	"ora/internal/db/dbtest"
)

// newWindowServer wires a Server behind a real HTTP server with the conversation, task and day routes registered under the same patterns cmd/daemon.go gives them, so the path values ({id}, {date}) resolve exactly as they do in the daemon.
func newWindowServer(t *testing.T, asker Asker, store *db.Store) (*Server, *httptest.Server) {
	t.Helper()
	s := New(asker, store, nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/ask", s.Ask)
	mux.HandleFunc("/events", s.Events)
	mux.HandleFunc("/conversations", s.Conversations)
	mux.HandleFunc("/conversations/{id}", s.Conversation)
	mux.HandleFunc("/conversations/{id}/title", s.ConversationTitle)
	mux.HandleFunc("/tasks", s.Tasks)
	mux.HandleFunc("/tasks/{id}/done", s.TaskDone)
	mux.HandleFunc("/tasks/{id}", s.TaskOwner)
	mux.HandleFunc("/days", s.Days)
	mux.HandleFunc("/days/{date}", s.Day)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// postJSON posts body to path and returns the status and the decoded reply, failing the test only when the request itself could not be made.
func postJSON(t *testing.T, srv *httptest.Server, path, body string, out any) int {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out) //nolint:errcheck — an empty body on an error status is expected
	}
	return resp.StatusCode
}

// patchJSON sends a PATCH with body to path and returns the status, failing the test only when the request itself could not be made.
func patchJSON(t *testing.T, srv *httptest.Server, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build PATCH %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestConversationsEmptyListIsNotNull(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var raw json.RawMessage
	getJSON(t, srv, "/conversations", &raw)
	if !strings.Contains(string(raw), `"conversations":[]`) {
		t.Errorf("GET /conversations on an empty store = %s, want an empty list", raw)
	}
}

func TestCreateAndReadConversation(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct{ ID string }
	if code := postJSON(t, srv, "/conversations", `{"title":"the flight","brain":"claude"}`, &created); code != http.StatusCreated {
		t.Fatalf("POST /conversations status = %d, want 201", code)
	}
	if created.ID == "" {
		t.Fatalf("POST /conversations returned no id")
	}

	var list struct {
		Conversations []ConversationSummary
	}
	getJSON(t, srv, "/conversations", &list)
	if len(list.Conversations) != 1 {
		t.Fatalf("GET /conversations returned %d, want 1", len(list.Conversations))
	}
	got := list.Conversations[0]
	if got.ID != created.ID || got.Title != "the flight" || got.Brain != "claude" {
		t.Errorf("conversation = %+v, want the one just created", got)
	}
	if got.Updated == "" {
		t.Errorf("conversation has no updated time")
	}

	var one ConversationView
	getJSON(t, srv, "/conversations/"+created.ID, &one)
	if one.ID != created.ID || one.Title != "the flight" {
		t.Errorf("GET /conversations/{id} = %+v", one)
	}
	if one.Turns == nil {
		t.Errorf("turns came back null, want an empty list")
	}
}

func TestConversationMissingIs404(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)
	resp, err := http.Get(srv.URL + "/conversations/999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET a missing conversation = %d, want 404", resp.StatusCode)
	}
}

// TestAskStoresTurns checks the whole contract /ask gained: an ask with no conversation opens one titled from the question's first eight words, both turns are stored with the answer's evidence and tool names, and the answer event carries the conversation id.
func TestAskStoresTurns(t *testing.T) {
	store := dbtest.Open(t)
	asker := &fakeAsker{trace: agent.TurnTrace{
		Answer:   "at half past four",
		ToolHops: []agent.ToolHop{{Name: "query_memory"}},
		Evidence: []agent.Evidence{{Kind: "note", Title: "the flight", When: "2026-09-04", Excerpt: "leaves at half past four"}},
	}}
	_, srv := newWindowServer(t, asker, store)

	events, closeSSE := readSSE(t, srv)
	defer closeSSE()

	var accepted struct {
		ID             string
		ConversationID string `json:"conversation_id"`
	}
	if code := postJSON(t, srv, "/ask", `{"question":"when exactly does the flight to bengaluru leave on friday"}`, &accepted); code != http.StatusAccepted {
		t.Fatalf("POST /ask status = %d, want 202", code)
	}
	if accepted.ConversationID == "" {
		t.Fatalf("POST /ask returned no conversation_id")
	}

	var answer Event
	deadline := time.After(3 * time.Second)
	for answer.Type != "answer" {
		select {
		case ev := <-events:
			answer = ev
		case <-deadline:
			t.Fatalf("no answer event arrived")
		}
	}
	if answer.ConversationID != accepted.ConversationID {
		t.Errorf("answer event conversation_id = %q, want %q", answer.ConversationID, accepted.ConversationID)
	}

	var one ConversationView
	getJSON(t, srv, "/conversations/"+accepted.ConversationID, &one)
	if one.Title != "when exactly does the flight to bengaluru leave" {
		t.Errorf("title = %q, want the question's first eight words", one.Title)
	}
	if len(one.Turns) != 2 {
		t.Fatalf("turns = %d, want the question and the answer", len(one.Turns))
	}
	if one.Turns[0].Role != "you" || one.Turns[0].Kind != "ask" {
		t.Errorf("first turn = %+v, want your question", one.Turns[0])
	}
	if one.Turns[1].Role != "ora" || one.Turns[1].Text != "at half past four" {
		t.Errorf("second turn = %+v, want Ora's answer", one.Turns[1])
	}
	if len(one.Turns[1].Tools) != 1 || one.Turns[1].Tools[0] != "query_memory" {
		t.Errorf("answer tools = %v, want the one tool the agent called", one.Turns[1].Tools)
	}
	if len(one.Turns[1].Evidence) != 1 || one.Turns[1].Evidence[0].Title != "the flight" {
		t.Errorf("answer evidence = %+v", one.Turns[1].Evidence)
	}
	if one.Turns[0].Evidence == nil || one.Turns[0].Tools == nil {
		t.Errorf("a turn with no evidence or tools came back null, want empty lists")
	}
}

// TestAskJoinsAnExistingConversation checks that an ask naming a conversation appends to it instead of opening another.
func TestAskJoinsAnExistingConversation(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{trace: agent.TurnTrace{Answer: "yes"}}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/conversations", `{"title":"the flight"}`, &created)
	if code := postJSON(t, srv, "/ask", `{"question":"is it still on","conversation_id":"`+created.ID+`"}`, nil); code != http.StatusAccepted {
		t.Fatalf("POST /ask into a conversation was not accepted")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		var one ConversationView
		getJSON(t, srv, "/conversations/"+created.ID, &one)
		if len(one.Turns) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer turn never landed in the conversation")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var list struct{ Conversations []ConversationSummary }
	getJSON(t, srv, "/conversations", &list)
	if len(list.Conversations) != 1 {
		t.Errorf("conversations = %d, want the one that was asked in", len(list.Conversations))
	}
	if list.Conversations[0].Last != "yes" {
		t.Errorf("last = %q, want the newest turn", list.Conversations[0].Last)
	}
}

// TestConversationErrorTurnCarriesAReason checks that a failed turn's stored error text is summarised as a plain reason on GET /conversations/{id}, and that the same reason stands in for the raw error on GET /conversations' "last".
func TestConversationErrorTurnCarriesAReason(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	id, err := store.CreateConversation(ctx, "flaky", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "you", "are you there", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn you: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "ora", "Error 503, high demand", "error", nil, nil); err != nil {
		t.Fatalf("AddTurn error: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var one ConversationView
	getJSON(t, srv, "/conversations/"+strconv.FormatInt(id, 10), &one)
	if len(one.Turns) != 2 {
		t.Fatalf("turns = %d, want the question and the failure", len(one.Turns))
	}
	if one.Turns[0].Reason != "" {
		t.Errorf("a question turn carries a reason = %q, want none", one.Turns[0].Reason)
	}
	if one.Turns[1].Reason != "The model was overloaded (503)." {
		t.Errorf("error turn reason = %q, want the overloaded sentence", one.Turns[1].Reason)
	}

	var list struct{ Conversations []ConversationSummary }
	getJSON(t, srv, "/conversations", &list)
	if len(list.Conversations) != 1 {
		t.Fatalf("conversations = %d, want 1", len(list.Conversations))
	}
	if list.Conversations[0].Last != "The model was overloaded (503)." {
		t.Errorf("last = %q, want the reason instead of the raw error", list.Conversations[0].Last)
	}
}

// TestErrorReason is a table test on the pure function GET /conversations/{id} uses to turn a failed turn's stored error text into one plain sentence.
func TestErrorReason(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"a 429 status", "Error 429, Message: You exceeded your current quota", "The model's quota is spent (429)."},
		{"resource exhausted without the code", "Status: RESOURCE_EXHAUSTED", "The model's quota is spent (429)."},
		{"a 503 status", "Error 503, high demand", "The model was overloaded (503)."},
		{"unavailable without the code", "Status: UNAVAILABLE", "The model was overloaded (503)."},
		{"a timeout", "context deadline exceeded: the call timed out", "The model took too long."},
		{"anything else", "tool loop: the store is locked", "The model could not answer."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errorReason(c.text); got != c.want {
				t.Errorf("errorReason(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

// TestConversationTitle checks the rename route: a good title sticks and answers 204, a blank one is refused, and renaming a conversation that does not exist is 404.
func TestConversationTitle(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/conversations", `{"title":"old title"}`, &created)

	if code := postJSON(t, srv, "/conversations/"+created.ID+"/title", `{"title":"new title"}`, nil); code != http.StatusNoContent {
		t.Fatalf("POST title status = %d, want 204", code)
	}
	var one ConversationView
	getJSON(t, srv, "/conversations/"+created.ID, &one)
	if one.Title != "new title" {
		t.Errorf("title = %q, want the renamed one", one.Title)
	}

	if code := postJSON(t, srv, "/conversations/"+created.ID+"/title", `{"title":"   "}`, nil); code != http.StatusBadRequest {
		t.Errorf("blank title status = %d, want 400", code)
	}
	if code := postJSON(t, srv, "/conversations/999/title", `{"title":"x"}`, nil); code != http.StatusNotFound {
		t.Errorf("missing conversation status = %d, want 404", code)
	}
}

// TestConversationDelete checks the DELETE route: 204 on success, the conversation is then unreadable, and deleting it again (or one that never existed) is 404.
func TestConversationDelete(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/conversations", `{"title":"gone soon"}`, &created)

	if code := deleteRequest(t, srv, "/conversations/"+created.ID); code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", code)
	}

	resp, err := http.Get(srv.URL + "/conversations/" + created.ID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", resp.StatusCode)
	}

	if code := deleteRequest(t, srv, "/conversations/"+created.ID); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", code)
	}
	if code := deleteRequest(t, srv, "/conversations/999"); code != http.StatusNotFound {
		t.Errorf("DELETE on a conversation that never existed = %d, want 404", code)
	}
}

// deleteRequest sends a DELETE to path and returns the status, failing the test only when the request itself could not be made.
func deleteRequest(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build DELETE %s: %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTitleFromQuestion(t *testing.T) {
	cases := []struct{ question, want string }{
		{"when exactly does the flight to bengaluru leave on friday", "when exactly does the flight to bengaluru leave"},
		{"what now", "what now"},
		{"   ", "New conversation"},
	}
	for _, c := range cases {
		if got := titleFromQuestion(c.question); got != c.want {
			t.Errorf("titleFromQuestion(%q) = %q, want %q", c.question, got, c.want)
		}
	}
}

// TestAskWithAStaleConversationIs404 checks that an ask naming a conversation that no longer exists is refused rather than accepted: AddTurn refuses the orphan turn, so answering 202 would draw an answer the window loses the moment it reloads.
func TestAskWithAStaleConversationIs404(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{trace: agent.TurnTrace{Answer: "yes"}}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/conversations", `{"title":"the flight"}`, &created)
	if code := deleteRequest(t, srv, "/conversations/"+created.ID); code != http.StatusNoContent {
		t.Fatalf("DELETE /conversations/%s did not remove it", created.ID)
	}
	if code := postJSON(t, srv, "/ask", `{"question":"is it still on","conversation_id":"`+created.ID+`"}`, nil); code != http.StatusNotFound {
		t.Errorf("POST /ask into a deleted conversation = %d, want 404", code)
	}
	if code := postJSON(t, srv, "/ask", `{"question":"is it still on","conversation_id":"424242"}`, nil); code != http.StatusNotFound {
		t.Errorf("POST /ask into a conversation that never existed = %d, want 404", code)
	}
}
