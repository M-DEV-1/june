// Package ipc implements the daemon's HTTP routes that let the desktop window ask a question and stream back its progress and answer.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"june/internal/agent"
	"june/internal/db"
	"june/internal/tracker"
)

// Asker is the one method /ask needs from an *agent.Agent, narrowed so this package's tests run against a fake instead of the real model and store.
type Asker interface {
	AskText(ctx context.Context, question string) (agent.TurnTrace, error)
}

// EvidenceItem is one supporting fact behind an answer.
type EvidenceItem struct {
	Title string `json:"title"`
	Meta  string `json:"meta"`
	Body  string `json:"body"`
}

// ActionItem is one follow-up button offered alongside an answer.
type ActionItem struct {
	Label string `json:"label"`
	Key   string `json:"key"`
}

// Event is one line of the /events SSE stream. Type is one of "status", "tool", "answer", "done", "error". Text carries that type's payload: "Checking." for status, a tool name for tool, the final answer for answer, an error message for error, and is empty for done. Detail is only populated on tool events: a short summary of what that tool call is doing (its args before it runs) or found (its result after), so the window can show live progress while a multi-step turn is still going. Failed is only meaningful on the after-call tool event: true when that tool's result was an error, so the window can show a failed step instead of a done one. Evidence and Actions are only populated on answer events, and so is ConversationID: it names the conversation the answer was stored in, so the window knows which thread to append it to.
type Event struct {
	ID             string         `json:"id"`
	Type           string         `json:"type"`
	Text           string         `json:"text"`
	Detail         string         `json:"detail"`
	Failed         bool           `json:"failed,omitempty"`
	Evidence       []EvidenceItem `json:"evidence"`
	Actions        []ActionItem   `json:"actions"`
	ConversationID string         `json:"conversation_id"`
	// Notice is only populated on "notice" events, and is left out of every other event's JSON so nothing already reading this stream sees a new field. See notice.go.
	Notice *Notice `json:"notice,omitempty"`
}

// askTimeout bounds one /ask's model call so a hung call ends with an error event instead of holding its goroutine for ever; a variable so tests can shorten it. It matches the twelve minutes each brain allows itself (askWallClock, claudeAskTimeout, codexAskTimeout, agyAskTimeout): this is the deadline every brain inherits, so a shorter value here cut a screen task off with "took too long" before it reached its step checkpoint, and lost the report of where it got to.
var askTimeout = 12 * time.Minute

// storeTurnTimeout bounds the write that files a finished answer as a turn, so a wedged store cannot hold the ask goroutine open for ever.
const storeTurnTimeout = 10 * time.Second

// storeActRunTimeout bounds the write that files a finished screen-tool trace as an act run, on its own context like storeTurnTimeout.
const storeActRunTimeout = 10 * time.Second

// storeTokenUseTimeout bounds the write that files what a finished turn cost in tokens, on its own context like storeTurnTimeout.
const storeTokenUseTimeout = 10 * time.Second

// screenTools names every tool that touches the screen (see internal/agent/tools.go): the ones the later "watch me once" replay learning needs runs recorded for.
var screenTools = map[string]bool{
	"observe_screen": true,
	"point_at":       true,
	"show_marks":     true,
	"click":          true,
	"scroll_to":      true,
	"type_text":      true,
}

// usedScreenTool reports whether a finished trace called any screen tool. Input: the trace. Output: true when at least one of its tool hops named a screen tool.
func usedScreenTool(trace agent.TurnTrace) bool {
	for _, hop := range trace.ToolHops {
		if screenTools[hop.Name] {
			return true
		}
	}
	return false
}

// afterScreenRun kicks the end-of-ask lesson hook (agent.Agent.AfterScreenRun) for a screen-tool trace, in the background so it can never hold up the ask's own response — the hook may make a reflective model call of its own. Input: the asker this ask used, the finished trace, and its outcome ("ok" or "error"). Output: none; a trace that never touched the screen, or an asker whose brain does not implement the hook (a test fake, or a brain with no lesson store wired), is silently skipped.
func (s *Server) afterScreenRun(asker Asker, trace agent.TurnTrace, outcome string) {
	if !usedScreenTool(trace) {
		return
	}
	hook, ok := asker.(interface {
		AfterScreenRun(ctx context.Context, trace agent.TurnTrace, outcome string)
	})
	if !ok {
		return
	}
	go hook.AfterScreenRun(context.Background(), trace, outcome)
}

// recordActRun files trace as an act run when it used a screen tool, on its own context so a wedged store cannot hold the ask goroutine open. Input: the finished trace, the bare question the user asked, its outcome ("ok" or "error") and the error message ("" for ok). The question is passed in rather than read off the trace because the trace carries the question as the model saw it, which on a window ask is prefixed with up to 600 runes of whatever was on screen; that text is not the question and has no business sitting in a table nothing ages out. Failures are logged, never returned: recording a run must never fail the ask itself.
func (s *Server) recordActRun(trace agent.TurnTrace, question, outcome, errMsg string) {
	if !usedScreenTool(trace) {
		return
	}
	steps := make([]db.ActStep, 0, len(trace.ToolHops))
	for _, hop := range trace.ToolHops {
		steps = append(steps, db.ActStep{Name: hop.Name, Args: hop.Args, Result: hop.Result})
	}
	storeCtx, storeCancel := context.WithTimeout(context.Background(), storeActRunTimeout)
	defer storeCancel()
	if _, err := s.store.AddActRun(storeCtx, db.ActRun{
		Question:   question,
		Model:      trace.Model,
		Outcome:    outcome,
		Answer:     trace.Answer,
		Error:      errMsg,
		DurationMS: trace.Duration.Milliseconds(),
		Steps:      steps,
	}); err != nil {
		slog.Error("ask: could not record the act run", "error", err)
	}
}

// splitModelSlug splits a trace's model into the provider that served it and the model itself. Input: the slug, which is "codex/gpt-5.5" on the Codex path and a bare model name like "gemini-3-flash" on the Gemini ones. Output: the provider and the model, with an empty provider when the slug names none.
func splitModelSlug(slug string) (provider, model string) {
	if before, after, ok := strings.Cut(slug, "/"); ok {
		return before, after
	}
	return "", slug
}

// recordTokenUse files what a finished turn cost in tokens, so the user can see what each model provider is costing them. Input: the finished trace, the bare question the user asked, and the channel the question came in on ("text" for /ask). The question is passed in rather than read off the trace for the same reason recordActRun does it: GET /usage shows this field, and the screen context the trace's own question is prefixed with is not the question. The provider is the one named in front of the model slug, falling back to the provider the trace's own usage names when the slug is a bare model. The counts are the provider's own, summed over the turn's rounds: a call that reported none is filed as zeroes rather than a guess, so it still shows up as a call that happened. The write runs on its own bounded context, and a failure is logged rather than returned: recording usage must never fail the ask.
func (s *Server) recordTokenUse(trace agent.TurnTrace, question, channel string) {
	provider, model := splitModelSlug(trace.Model)
	if provider == "" {
		provider = trace.Usage.Provider
	}
	storeCtx, storeCancel := context.WithTimeout(context.Background(), storeTokenUseTimeout)
	defer storeCancel()
	if _, err := s.store.AddTokenUse(storeCtx, db.TokenUse{
		Provider:     provider,
		Model:        model,
		Channel:      channel,
		InputTokens:  trace.Usage.InputTokens,
		OutputTokens: trace.Usage.OutputTokens,
		TotalTokens:  trace.Usage.TotalTokens,
		CachedTokens: trace.Usage.CachedInputTokens,
		Rounds:       trace.Usage.Rounds,
		DurationMS:   trace.Duration.Milliseconds(),
		Question:     question,
	}); err != nil {
		slog.Error("ask: could not record the token use", "error", err)
	}
}

// clientBufferSize bounds how many unread events a single /events client may queue before it is dropped, so one stalled listener never backs up the others.
// It is sized from the largest burst one action can produce rather than picked: a single draw call broadcasts up to agent.MaxDrawShapes shapes, each as its own event, inside the same millisecond. At eight, a nine-shape octopus overflowed the overlay window's buffer and the hub dropped and closed it mid-drawing, so the drawing landed half-finished and the window lost every later event until it reconnected ("dropping a client that is not keeping up, buffered: 8", twice, 2026-09-07 06:08:48.7524). Doubled so a drawing arriving while an ask is already streaming still fits.
const clientBufferSize = 2 * agent.MaxDrawShapes

// levelEventType is the type of the voice waveform's amplitude event, which a live session broadcasts every 50ms for as long as it runs (see levels in voice.go). It is the one event stream that keeps arriving whether or not anything on screen has changed, so broadcast treats it differently from everything else.
const levelEventType = "level"

// levelSkipQueueDepth is how many events of any kind may already be waiting in one client's buffer before broadcast skips a "level" event instead of queueing it. At one, a level is queued only for a client that has read everything so far: a client mid-way through a tool or notice event, or holding an unread level, is skipped rather than loaded with an amplitude it would draw late, so it neither fills its buffer nor loses its place in the stream. The window breathes on its own clock after 150ms without a level (see syncBreath in app/src/main.ts), which covers the stretch a burst of other events keeps levels out.
const levelSkipQueueDepth = 1

// hub fans out events from ongoing asks to every connected /events client.
type hub struct {
	mu      sync.Mutex
	clients map[chan Event]struct{}
	// lastSeen is when a client last joined or left, which is what tells a caller a window was here a moment ago even though none is connected right this second. See Subscribed in notice.go.
	lastSeen time.Time
}

func newHub() *hub {
	return &hub{clients: make(map[chan Event]struct{})}
}

// subscribe registers a new client and returns its event channel.
func (h *hub) subscribe() chan Event {
	ch := make(chan Event, clientBufferSize)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.lastSeen = time.Now()
	h.mu.Unlock()
	return ch
}

// unsubscribe removes and closes a client's channel. Safe to call more than once, or after the hub has already dropped the same client for being slow.
func (h *hub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
		h.lastSeen = time.Now()
	}
}

// closeAll drops every subscribed client, which ends each one's Events handler at once. The HTTP server's own shutdown waits for handlers to return but never cancels their requests, so without this a daemon with the window open sits for the whole of its shutdown timeout holding its port, and the next daemon cannot bind.
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		delete(h.clients, ch)
		close(ch)
	}
}

// CloseStreams ends every open event stream, so the daemon's HTTP server has no long-lived handler left to wait for when it shuts down.
func (s *Server) CloseStreams() {
	s.hub.closeAll()
}

// broadcast sends ev to every subscribed client. A client whose buffer is full is dropped (removed and closed) instead of blocking this call, so one slow client cannot stall the rest. A "level" event is the exception: it is skipped for a client that already has levelSkipQueueDepth of them waiting, and never drops anyone. A voice session emits twenty a second for its whole length, so without that a client that stalled its read for a few hundred milliseconds would be dropped mid-conversation over amplitudes it no longer needs, ending its /events stream and making it reconnect and refetch.
func (h *hub) broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		if ev.Type == levelEventType && len(ch) >= levelSkipQueueDepth {
			continue
		}
		select {
		case ch <- ev:
		default:
			// A client dropped here loses every event from this one on until it reconnects, which for the drawing layer means a drawing that simply never appears, so the drop is said out loud rather than left to be inferred from the silence.
			slog.Warn("events: dropping a client that is not keeping up", "type", ev.Type, "id", ev.ID, "buffered", len(ch))
			delete(h.clients, ch)
			close(ch)
		}
	}
}

// clientCount is how many clients are subscribed to the hub right now. Input: none. Output: that count, which is zero when no window is reading the event stream and whatever is broadcast next reaches nobody.
func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Server holds the state every route shares: the asker that answers questions, the hub that broadcasts their progress, the store the read-only screens are drawn from, the tracker's live activity buffer, and a synchronous read of the window in focus right now.
type Server struct {
	asker Asker
	// brains are the askers a POST /ask can name in its "brain" field (codex, for one); a name not here is refused, and an empty name uses asker.
	brains  map[string]Asker
	store   *db.Store
	screen  func() []tracker.Activity
	focused func(context.Context) (tracker.Activity, bool)
	hub     *hub
	nextID  atomic.Uint64
	// askMu guards asking.
	askMu sync.Mutex
	// say delivers a notice the way the scheduler's own moments go out: the window's card when a window is reading the event stream, and the desktop notification when none is. Nil until the daemon wires it (see SetSay), which is why sayNotice falls back to the plain broadcast.
	say func(Notice)
	// asking holds the id of every ask running right now. The daemon wires one shared agent's point_at and show_marks to Ring and Marks (see cmd/daemon.go), and that agent's drawing callbacks say nothing about which question is being answered, so this is what DrawingAsk reads to stamp a drawing with the ask that caused it.
	asking map[string]struct{}
}

// New builds a Server for every route this package serves. Input: the asker that answers /ask, the store the read routes query, screen, which returns the tracker's live activity buffer (the same one /buffer serves) and may be nil when no tracker is wired, and focused, which reads the window in focus at the moment it is called (rather than the last sampled one), takes the request's context so a caller that has given up stops waiting on it, and may also be nil. Output: the server; register its methods on a mux (see cmd/daemon.go for the route names). /context tries focused first, falls back to screen, then to the newest stored episode.
func New(asker Asker, store *db.Store, screen func() []tracker.Activity, focused func(context.Context) (tracker.Activity, bool)) *Server {
	return &Server{asker: asker, store: store, screen: screen, focused: focused, hub: newHub(), brains: map[string]Asker{}, asking: map[string]struct{}{}}
}

// SetSay wires the one surface a notice this package raises should go to. Input: a function that delivers one notice, which the daemon fills in with the scheduler's own say (see proactive.Scheduler.Say) so a moment falls back to a desktop notification when no window is listening. Output: none. Left unset, sayNotice broadcasts on the event stream and a notice raised while no window is there is simply lost.
func (s *Server) SetSay(say func(Notice)) { s.say = say }

// Announce tells every window one thing about what the daemon is doing that belongs to no ask: a meeting recording starting or stopping ("recording", "on" or "off"), or the nightly dream run beginning or ending ("dreaming", "on" or "off"). Input: the event type and its text. Output: none; the windows read it as a face to show.
func (s *Server) Announce(typ, text string) {
	s.hub.broadcast(Event{Type: typ, Text: text, Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
}

// sayNotice delivers one notice through whatever SetSay wired, or straight onto the event stream when nothing was wired. Input: the notice. Output: none.
func (s *Server) sayNotice(n Notice) {
	if s.say != nil {
		s.say(n)
		return
	}
	s.Notice(n)
}

// AddBrain registers an asker a POST /ask can pick by naming it in "brain". Input: the brain id the window uses (see brains.go) and the asker.
func (s *Server) AddBrain(name string, a Asker) { s.brains[name] = a }

// brainNames lists the brains a POST /ask may name, in alphabetical order, for the message an unrecognised name is refused with. Output: the registered names joined by commas, or "none" when this daemon has no named brain at all. An empty brain field is not one of them: it always means the default asker.
func (s *Server) brainNames() string {
	names := make([]string, 0, len(s.brains))
	for name := range s.brains {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// startAsk records that the ask with this id is running, so a drawing made while it runs can be traced back to it.
func (s *Server) startAsk(id string) {
	s.askMu.Lock()
	s.asking[id] = struct{}{}
	s.askMu.Unlock()
}

// endAsk records that the ask with this id has finished.
func (s *Server) endAsk(id string) {
	s.askMu.Lock()
	delete(s.asking, id)
	s.askMu.Unlock()
}

// DrawingAsk names the ask a drawing made right now belongs to, which is what a caller wiring an agent's point_at and show_marks to Ring and Marks stamps the drawing with (see cmd/daemon.go). Input: none. Output: the id of the one ask running, or overlayNoAsk when none is running — nothing asked for this drawing — and also when more than one is, since the shared agent's callbacks carry nothing that would say which of them drew it, and naming the wrong question is worse than naming none.
func (s *Server) DrawingAsk() string {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if len(s.asking) != 1 {
		return overlayNoAsk
	}
	for id := range s.asking {
		return id
	}
	return overlayNoAsk
}

func (s *Server) newID() string {
	return fmt.Sprintf("ask-%d", s.nextID.Add(1))
}

// evidenceFor maps a finished turn's evidence rows to the items carried on its answer event, in the agent's order. Input: the turn trace. Output: one item per evidence row with the title (or the kind when the row has no title), a meta line of kind and date, and the excerpt as the body; an empty non-nil list when there is none so the JSON stays [].
func evidenceFor(trace agent.TurnTrace) []EvidenceItem {
	items := make([]EvidenceItem, 0, len(trace.Evidence))
	for _, e := range trace.Evidence {
		title := e.Title
		if title == "" {
			title = e.Kind
		}
		meta := e.Kind
		if e.When != "" {
			meta += " · " + e.When
		}
		items = append(items, EvidenceItem{Title: title, Meta: meta, Body: e.Excerpt})
	}
	return items
}

// Ask handles POST /ask. Input: JSON body {"question": string, "context": string, "conversation_id": string, "brain": string, "go": bool}, everything but the question optional. go carries the user's explicit go-ahead for the one guarded step (a send, a delete, a purchase — see agent.WithGo) this exact question is asking for; it applies to this turn only. Output: 202 with JSON {"id": string, "conversation_id": string} written immediately; the question then runs in the background and its progress and answer arrive on /events tagged with that id. A conversation_id appends this question and its answer to that conversation; without one a conversation is opened, titled from the question's first eight words, and its id comes back in the body. A body that fails to decode as JSON gets 400, and so does one naming a brain this daemon has no asker for — the message names the brains it does have — because a question meant for one model answered by another, under the other's name, is not something the window or an eval can see. An empty brain field is not a name and always means the default asker.
func (s *Server) Ask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Question       string `json:"question"`
		Context        string `json:"context"`
		ConversationID string `json:"conversation_id"`
		Brain          string `json:"brain"`
		Go             bool   `json:"go"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}

	// Refused before a conversation is opened or the question is stored, so a question meant for a brain this daemon does not have leaves no trace of having been asked.
	asker := s.asker
	if req.Brain != "" {
		named, ok := s.brains[req.Brain]
		if !ok {
			http.Error(w, "unknown brain "+req.Brain+"; this daemon has "+s.brainNames()+", and an empty brain means the default", http.StatusBadRequest)
			return
		}
		asker = named
	}

	convID, ok := s.conversationFor(r.Context(), req.ConversationID, req.Question, req.Brain)
	if !ok {
		http.Error(w, "no conversation with id "+req.ConversationID, http.StatusNotFound)
		return
	}
	// The thread so far is read before this question is stored, so the model is given what was said before rather than a copy of what is being asked right now.
	history := s.historyFor(r.Context(), convID)
	if convID != 0 {
		if _, err := s.store.AddTurn(r.Context(), convID, "you", req.Question, "ask", nil, nil); err != nil {
			slog.Error("ask: could not store the question", "conversation_id", convID, "error", err)
		}
	}

	id := s.newID()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "conversation_id": conversationIDString(convID)})

	go s.run(asker, id, convID, req.Question, req.Context, req.Go, history)
}

// conversationFor decides which conversation this ask belongs to. Input: the request context, the conversation id the caller named (empty when it named none), the question, and the brain the caller asked for. Output: the conversation's row id and true, or 0 and false when the caller named a conversation this daemon does not have — an id that is not a number, or one whose row has since been deleted. A named id is read back from the store before it is used, because AddTurn refuses a turn on a conversation that is not there: without this check the handler would answer 202 with an id the window then draws an answer under and loses on its next reload.
// A 0 with true means no conversation could be opened for an ask that named none: that ask still runs and still streams its answer, it just leaves no record.
func (s *Server) conversationFor(ctx context.Context, named, question, brain string) (int64, bool) {
	if named != "" {
		id, err := strconv.ParseInt(named, 10, 64)
		if err != nil {
			slog.Error("ask: conversation_id is not an id", "conversation_id", named)
			return 0, false
		}
		if _, err := s.store.Conversation(ctx, id); err != nil {
			slog.Error("ask: no such conversation", "conversation_id", id, "error", err)
			return 0, false
		}
		return id, true
	}
	id, err := s.store.CreateConversation(ctx, titleFromQuestion(question), brain)
	if err != nil {
		slog.Error("ask: could not open a conversation", "error", err)
		return 0, true
	}
	return id, true
}

// conversationIDString renders a conversation id for the window, or "" for the 0 that means no conversation was opened.
func conversationIDString(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// run asks the question through asker (the default, or the brain the request named) and broadcasts its progress under id: a status event first (AskText reports no earlier progress hook, so this is the only point one can fire), then one tool event per tool call as askText's own loop makes it — via the ToolObserver wrapped onto the ask context below, which fires live, twice per hop (once with its argument summary before it runs, once with its result summary after), rather than only after AskText returns — then the answer, then done. allowGo carries the request's "go" flag onto the context (see agent.WithGo) so a guarded click or Enter-with-a-message this question asks for can actually run. A failed AskText stores the failure as an "june" turn of kind "error" (when convID is not 0) and broadcasts error instead of answer and done. When convID is not 0 the answer is also stored as a turn in that conversation, with its evidence and the names of the tools behind it, and the answer event carries the conversation's id. Either way, a trace whose tool hops include a screen tool is also filed as an act run (see recordActRun), win or lose, so the later "watch me once" replay learning has data to study, and what the turn cost in tokens is filed too (see recordTokenUse), win or lose, so the user can see what each provider is costing them. The ask is registered as running for the whole call (see startAsk and DrawingAsk), so any ring or marks its screen tools draw are broadcast under this same id.
// askerWithHistory is an Asker that can also be given the conversation so far. Both production askers have it; the test fakes need not, so it is asked for rather than required.
type askerWithHistory interface {
	AskTextWith(ctx context.Context, history agent.History, question string) (agent.TurnTrace, error)
}

// historyFor reads the thread a question belongs to and renders it as the history the model is given. Input: the request context and the conversation's row id, 0 when there is none. Output: the prior turns oldest first, or nil when there is no conversation or the store could not be read, since an unreadable thread must lose the context rather than fail the ask.
func (s *Server) historyFor(ctx context.Context, convID int64) agent.History {
	if convID == 0 {
		return nil
	}
	turns, err := s.store.ConversationTurns(ctx, convID)
	if err != nil {
		slog.Error("ask: could not read the conversation so far", "conversation_id", convID, "error", err)
		return nil
	}
	return agent.HistoryFromTurns(turns)
}

func (s *Server) run(asker Asker, id string, convID int64, question, screenContext string, allowGo bool, history agent.History) {
	// Registered for as long as the turn lasts, so a ring or a set of marks drawn by its point_at or show_marks is stamped with this question's id (see DrawingAsk).
	s.startAsk(id)
	defer s.endAsk(id)

	s.hub.broadcast(Event{ID: id, Type: "status", Text: "Checking.", Evidence: []EvidenceItem{}, Actions: []ActionItem{}})

	q := question
	if screenContext != "" {
		q = fmt.Sprintf("On screen: %s\n\n%s", screenContext, question)
	}

	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	ctx = agent.WithToolObserver(ctx, func(name, summary string, failed bool) {
		s.hub.broadcast(Event{ID: id, Type: "tool", Text: name, Detail: summary, Failed: failed, Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
	})
	// Every tool this question runs is filed under the conversation it belongs to, so a later pass can read what the model reached for and what it was offered — which is the only record of it, since the activity feed is thrown away and act runs only ever covered the screen tools.
	ctx = agent.WithToolRecorder(ctx, toolRecorder(s.store, "ask", convID))
	// The ask's own id is also what ties its tool calls together as one turn — there is exactly one model turn per ask, so no extra id needs minting here.
	ctx = agent.WithTurnID(ctx, id)
	if allowGo {
		ctx = agent.WithGo(ctx)
	}
	// An asker that can read the thread is given it, so a follow-up like "do it again" knows what it refers to.
	var trace agent.TurnTrace
	var err error
	if withHistory, ok := asker.(askerWithHistory); ok {
		trace, err = withHistory.AskTextWith(ctx, history, q)
	} else {
		trace, err = asker.AskText(ctx, q)
	}
	outcome, errMsg := "ok", ""
	// Running out of steps is where the work got to, not a failure to answer: it is filed as June's answer so a "continue" after it has the task in its history (error turns are left out of history), while the act run still records it as a run that did not finish.
	var capped *agent.StepCapError
	if errors.As(err, &capped) {
		trace.Answer = capped.Msg
		outcome, errMsg, err = "error", capped.Msg, nil
	}
	if err != nil {
		s.recordActRun(trace, question, "error", err.Error())
		s.recordTokenUse(trace, question, "text")
		s.afterScreenRun(asker, trace, "error")
		if convID != 0 {
			// Filed as June's turn of kind "error" so the thread never shows a question with nothing under it.
			storeCtx, storeCancel := context.WithTimeout(context.Background(), storeTurnTimeout)
			if _, storeErr := s.store.AddTurn(storeCtx, convID, "june", err.Error(), "error", nil, nil); storeErr != nil {
				slog.Error("ask: could not store the failure", "conversation_id", convID, "error", storeErr)
			}
			storeCancel()
		}
		// The window is told one plain sentence, because a provider's raw failure is a thousand characters of JSON that pushes the whole panel off screen; the full message is kept in the log and under the failed turn.
		slog.Error("ask: the model call failed", "id", id, "detail", err.Error())
		s.hub.broadcast(Event{ID: id, Type: "error", Text: AskSentence(err), Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
		return
	}

	s.recordActRun(trace, question, outcome, errMsg)
	s.recordTokenUse(trace, question, "text")
	s.afterScreenRun(asker, trace, outcome)
	// The tool events themselves already went out live, via the ToolObserver wired onto ctx above — this only rebuilds the list of names for storage, not for broadcast, so they are not shown twice.
	tools := make([]string, 0, len(trace.ToolHops))
	for _, hop := range trace.ToolHops {
		tools = append(tools, hop.Name)
	}
	evidence := evidenceFor(trace)
	if convID != 0 {
		// The answer is stored on its own context: the request that asked for it returned at 202, long before the model finished.
		storeCtx, storeCancel := context.WithTimeout(context.Background(), storeTurnTimeout)
		body, err := json.Marshal(evidence)
		if err != nil {
			body = nil
		}
		if _, err := s.store.AddTurn(storeCtx, convID, "june", trace.Answer, "ask", body, tools); err != nil {
			slog.Error("ask: could not store the answer", "conversation_id", convID, "error", err)
		}
		storeCancel()
	}
	s.hub.broadcast(Event{ID: id, Type: "answer", Text: trace.Answer, Evidence: evidence, Actions: []ActionItem{}, ConversationID: conversationIDString(convID)})
	s.hub.broadcast(Event{ID: id, Type: "done", Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
}

// Events handles GET /events: a Server-Sent Events stream of every event from every /ask call, one JSON object per "data:" line, flushed as soon as it is sent. Each connection gets its own buffered queue (see clientBufferSize); a client that falls behind is dropped so it never stalls the others.
func (s *Server) Events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The browser is told not to buffer the stream, and is then given enough bytes to make it stop anyway: WebKit holds a small response body in its network process, which delayed the first event by seconds in the desktop window and lost overlay drawings outright.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, streamPreamble)
	flusher.Flush()

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// A comment line every so often keeps the connection warm and gives a reader that is still buffering something to push the first real event through.
	beat := time.NewTicker(streamHeartbeat)
	defer beat.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-beat.C:
			fmt.Fprint(w, ": beat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// streamPreamble is sent before any event, as a comment the event-stream format says to ignore. It exists only for its size: a webview that holds a small response body in its network process delivers nothing until enough bytes have arrived, which made the first ring after opening the window disappear.
var streamPreamble = ": " + strings.Repeat("padding to defeat client-side buffering, which is measured in bytes and not in events. ", 30) + "\n\n"

// streamHeartbeat is how often a comment line goes out on an idle stream, short enough to hold a connection open through anything that drops a quiet one, long enough to cost nothing.
const streamHeartbeat = 15 * time.Second
