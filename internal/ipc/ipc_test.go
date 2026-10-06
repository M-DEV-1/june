package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"june/internal/agent"
	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/tracker"
)

// fakeAsker is a test double for Asker: before returning its canned trace or error, it reports one live tool event per hop in that trace through agent.ObserveTool, standing in for what askText's real tool loop reports live as it runs (see ObserveTool's doc comment in internal/agent/ask.go).
type fakeAsker struct {
	trace agent.TurnTrace
	err   error
}

func (f *fakeAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	for _, hop := range f.trace.ToolHops {
		agent.ObserveTool(ctx, hop.Name, hop.Result, strings.HasPrefix(hop.Result, "error"))
	}
	return f.trace, f.err
}

// subscribe registers a hub client that named no role, the way a window or a curl reading /events does, and returns its event channel. A test seam: the daemon's own subscriptions all go through Events, which passes the role the client named.
func (h *hub) subscribe() chan Event { return h.subscribeAs("") }

// newTestServer wires a Server behind a real HTTP server (httptest.NewServer, not a Recorder) because /events needs a live connection two clients can read concurrently while /ask runs in the background. Every route is registered, under the same names cmd/daemon.go gives them.
func newTestServer(t *testing.T, asker Asker, store *db.Store, screen func() []tracker.Activity, focused func(context.Context) (tracker.Activity, bool)) *httptest.Server {
	t.Helper()
	s := New(asker, store, screen, focused)
	mux := http.NewServeMux()
	mux.HandleFunc("/ask", s.Ask)
	mux.HandleFunc("/events", s.Events)
	mux.HandleFunc("/context", s.Context)
	mux.HandleFunc("/matters", s.Matters)
	mux.HandleFunc("/today", s.Today)
	mux.HandleFunc("/meetings", s.Meetings)
	mux.HandleFunc("/meetings/{id}", s.Meeting)
	mux.HandleFunc("/memory/search", s.MemorySearch)
	mux.HandleFunc("/people", s.People)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// readSSE opens /events and returns a channel of decoded Events plus a close func. Reads happen on a background goroutine so the caller can select with a timeout instead of blocking forever on a missing event.
func readSSE(t *testing.T, srv *httptest.Server) (<-chan Event, func()) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			out <- ev
		}
	}()
	return out, func() { resp.Body.Close() }
}

func mustEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("event stream closed early")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event")
	}
	return Event{}
}

func TestEvents_DeliversStatusAnswerDoneInOrder(t *testing.T) {
	trace := agent.TurnTrace{
		ToolHops: []agent.ToolHop{{Name: "search_memory"}, {Name: "query_store"}},
		Answer:   "it's 3pm",
	}
	srv := newTestServer(t, &fakeAsker{trace: trace}, dbtest.Open(t), nil, nil)

	ch, closeFn := readSSE(t, srv)
	defer closeFn()

	resp, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"what time is it"}`))
	if err != nil {
		t.Fatalf("POST /ask: %v", err)
	}
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()

	wantTypes := []string{"status", "tool", "tool", "answer", "done"}
	for i, wantType := range wantTypes {
		ev := mustEvent(t, ch)
		if ev.Type != wantType {
			t.Fatalf("event %d: type = %q, want %q", i, ev.Type, wantType)
		}
		if ev.ID != body.ID {
			t.Fatalf("event %d: id = %q, want %q", i, ev.ID, body.ID)
		}
	}
}

func TestEvents_ErrorInsteadOfAnswerOnFailure(t *testing.T) {
	srv := newTestServer(t, &fakeAsker{err: errors.New("boom")}, dbtest.Open(t), nil, nil)

	ch, closeFn := readSSE(t, srv)
	defer closeFn()

	resp, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"what time is it"}`))
	if err != nil {
		t.Fatalf("POST /ask: %v", err)
	}
	resp.Body.Close()

	statusEv := mustEvent(t, ch)
	if statusEv.Type != "status" {
		t.Fatalf("first event type = %q, want status", statusEv.Type)
	}
	// The stream carries a sentence a person can read, not the provider's own words; the raw failure goes to the log and to the stored turn.
	errEv := mustEvent(t, ch)
	if errEv.Type != "error" || errEv.Text != AskSentence(errors.New("boom")) {
		t.Fatalf("got %+v, want an error event carrying the plain sentence", errEv)
	}
	if strings.Contains(errEv.Text, "boom") {
		t.Errorf("the raw error reached the window: %q", errEv.Text)
	}
}

func TestEvents_TwoClientsBothReceiveEvents(t *testing.T) {
	trace := agent.TurnTrace{Answer: "42"}
	srv := newTestServer(t, &fakeAsker{trace: trace}, dbtest.Open(t), nil, nil)

	chA, closeA := readSSE(t, srv)
	defer closeA()
	chB, closeB := readSSE(t, srv)
	defer closeB()

	resp, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"?"}`))
	if err != nil {
		t.Fatalf("POST /ask: %v", err)
	}
	resp.Body.Close()

	wantTypes := []string{"status", "answer", "done"}
	for _, want := range wantTypes {
		if ev := mustEvent(t, chA); ev.Type != want {
			t.Fatalf("client A: type = %q, want %q", ev.Type, want)
		}
		if ev := mustEvent(t, chB); ev.Type != want {
			t.Fatalf("client B: type = %q, want %q", ev.Type, want)
		}
	}
}

// hangingAsker blocks until its context ends, the way a stuck model call would, and reports the context's error.
type hangingAsker struct{}

func (hangingAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	<-ctx.Done()
	return agent.TurnTrace{}, ctx.Err()
}

// An ask that never returns must not hold its goroutine forever: run bounds the model call with askTimeout and the client sees an error event instead of silence.
func TestAsk_HungModelCallEndsWithAnErrorEvent(t *testing.T) {
	old := askTimeout
	askTimeout = 50 * time.Millisecond
	t.Cleanup(func() { askTimeout = old })
	srv := newTestServer(t, hangingAsker{}, dbtest.Open(t), nil, nil)
	events, closeEvents := readSSE(t, srv)
	defer closeEvents()

	resp, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"hi"}`))
	if err != nil {
		t.Fatalf("POST /ask: %v", err)
	}
	resp.Body.Close()

	if ev := mustEvent(t, events); ev.Type != "status" {
		t.Fatalf("first event = %q, want status", ev.Type)
	}
	// A timeout reads as the sentence for one, rather than the Go runtime's own wording.
	if ev := mustEvent(t, events); ev.Type != "error" || !strings.Contains(ev.Text, "took too long") {
		t.Errorf("second event = %+v, want an error saying it took too long", ev)
	}
}

type codexFake struct{ got string }

func (f *codexFake) AskText(ctx context.Context, q string) (agent.TurnTrace, error) {
	f.got = q
	return agent.TurnTrace{Answer: "from codex"}, nil
}

// POST /ask carries the brain the chat was opened with: a name the daemon has an asker for routes there, and an empty name means the default asker. A name the daemon does not have is refused instead (see TestAsk_UnknownBrainIsRefused), so it is not in this table.
func TestAsk_RoutesToTheNamedBrain(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	codex := &codexFake{}
	s := New(&fakeAsker{trace: agent.TurnTrace{Answer: "from gemini"}}, store, nil, nil)
	s.AddBrain("codex", codex)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	for brain, want := range map[string]string{"codex": "from codex", "": "from gemini"} {
		rec := httptest.NewRecorder()
		s.Ask(rec, httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(`{"question":"q","brain":"`+brain+`"}`)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("brain %q: status %d", brain, rec.Code)
		}
		got := ""
		deadline := time.After(2 * time.Second)
	wait:
		for {
			select {
			case ev := <-ch:
				if ev.Type == "answer" {
					got = ev.Text
					break wait
				}
			case <-deadline:
				t.Fatalf("brain %q: no answer event", brain)
			}
		}
		if got != want {
			t.Errorf("brain %q answered %q, want %q", brain, got, want)
		}
	}
	if codex.got != "q" {
		t.Errorf("codex asker got %q", codex.got)
	}
}

// historyAsker records the conversation it was handed, so a test can prove the thread reaches the model rather than only being stored.
type historyAsker struct {
	mu   sync.Mutex
	got  []agent.History
	last string
}

func (h *historyAsker) AskText(ctx context.Context, question string) (agent.TurnTrace, error) {
	return h.AskTextWith(ctx, nil, question)
}

func (h *historyAsker) AskTextWith(ctx context.Context, history agent.History, question string) (agent.TurnTrace, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, history)
	h.last = question
	return agent.TurnTrace{Answer: "answer to " + question}, nil
}

// A follow-up question is given the thread it belongs to. On 2026-09-04 the user asked June to ring a button, then typed "do it again" and was asked "What do you want repeated?", because the turns were stored and shown but never sent to the model.
func TestAsk_HandsTheConversationSoFarToTheModel(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	asker := &historyAsker{}
	s := New(asker, store, nil, nil)

	ask := func(body string) string {
		rec := httptest.NewRecorder()
		s.Ask(rec, httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status %d", rec.Code)
		}
		var out map[string]string
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out["conversation_id"]
	}

	convID := ask(`{"question":"ring the Pause button"}`)
	if convID == "" {
		t.Fatal("no conversation was opened")
	}
	waitFor(t, func() bool { asker.mu.Lock(); defer asker.mu.Unlock(); return len(asker.got) == 1 })
	if h := asker.got[0]; len(h) != 0 {
		t.Errorf("the first question must carry no history, got %d entries", len(h))
	}
	// The window asks a follow-up only after "done", which comes after the answer is stored; asking the moment the model was called raced that store and read an empty thread.
	id, _ := strconv.ParseInt(convID, 10, 64)
	waitFor(t, func() bool { turns, _ := store.ConversationTurns(context.Background(), id); return len(turns) >= 2 })

	ask(`{"question":"do it again","conversation_id":"` + convID + `"}`)
	waitFor(t, func() bool { asker.mu.Lock(); defer asker.mu.Unlock(); return len(asker.got) == 2 })

	asker.mu.Lock()
	defer asker.mu.Unlock()
	second := asker.got[1]
	if len(second) != 2 {
		t.Fatalf("the follow-up carried %d history entries, want the first question and its answer", len(second))
	}
	if !strings.Contains(historyText(second[0]), "ring the Pause button") {
		t.Errorf("first entry = %q", historyText(second[0]))
	}
	if !strings.Contains(historyText(second[1]), "answer to ring the Pause button") {
		t.Errorf("second entry = %q", historyText(second[1]))
	}
	// The question being asked right now must not appear in its own history.
	for i, c := range second {
		if strings.Contains(historyText(c), "do it again") {
			t.Errorf("entry %d repeats the question being asked: %q", i, historyText(c))
		}
	}
}

// historyText joins the text parts of one history entry, so a test can assert on what the model would read.
func historyText(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// waitFor blocks until cond is true or the test's patience runs out, since an ask runs on its own goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the ask to reach the asker")
}

// The event stream opens with enough bytes to get past a client that buffers a small response body, and keeps a quiet connection warm. A webview held the first ring for seconds and sometimes dropped it entirely, because it counts bytes rather than events before it delivers anything.
func TestEvents_OpensWithEnoughBytesToGetPastAClientThatBuffers(t *testing.T) {
	store := dbtest.Open(t)
	srv := newTestServer(t, &fakeAsker{}, store, nil, nil)

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("the stream must ask proxies not to buffer it, got %q", got)
	}

	// The preamble must arrive on its own, before any event has been broadcast.
	buf := make([]byte, 4096)
	done := make(chan int, 1)
	go func() {
		n, _ := io.ReadFull(resp.Body, buf[:2048])
		done <- n
	}()
	select {
	case n := <-done:
		if n < 2048 {
			t.Fatalf("only %d bytes arrived before any event; a buffering client would still be waiting", n)
		}
		if !strings.HasPrefix(string(buf[:n]), ":") {
			t.Errorf("the preamble must be a comment the event-stream format ignores, got %q", string(buf[:60]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no preamble arrived; the first event would be held by a client that buffers")
	}
}
