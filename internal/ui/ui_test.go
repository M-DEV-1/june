package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ora/internal/agent"
	"ora/internal/ipctoken"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// newTestModel gives Update() a fully-initialized model (textarea/viewport included) backed by a channel-only Agent — mirrors agent.NewAgent(nil, nil, nil, nil, "") already used throughout internal/agent's own tests.
func newTestModel() model {
	a := agent.NewAgent(nil, nil, nil, nil, "")
	return NewModel(a, "connected", "")
}

// TestNewModel_BuildMismatch_ShowsWarningLine is WP13: a non-empty buildMismatch string (the daemon is running an older build than this client — see cmd/root.go's checkDaemonBuildMismatch) must surface as a visible system line at startup, not silently logged and dropped.
func TestNewModel_BuildMismatch_ShowsWarningLine(t *testing.T) {
	a := agent.NewAgent(nil, nil, nil, nil, "")
	m := NewModel(a, "connected", "daemon is running an older build — quit it from the tray or `pkill ora`, then relaunch")

	joined := strings.Join(func() []string {
		var out []string
		for _, msg := range m.messages {
			out = append(out, msg.Content)
		}
		return out
	}(), "\n")

	if !strings.Contains(joined, "older build") {
		t.Errorf("expected the build-mismatch warning in the intro messages, got: %q", joined)
	}
}

// TestNewModel_NoBuildMismatch_NoWarningLine verifies the common case (empty buildMismatch) doesn't inject any warning text — the vast majority of startups where daemon and client match.
func TestNewModel_NoBuildMismatch_NoWarningLine(t *testing.T) {
	a := agent.NewAgent(nil, nil, nil, nil, "")
	m := NewModel(a, "connected", "")

	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "older build") {
			t.Errorf("expected no build-mismatch warning when buildMismatch is empty, got: %q", msg.Content)
		}
	}
}

// --- daemon status polling ---

func TestPollDaemonHTTP_RespondsOK_ReturnsTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !pollDaemonHTTP(http.DefaultClient, srv.URL, "") {
		t.Error("expected true for a 200 OK response")
	}
}

func TestPollDaemonHTTP_NonOKStatus_ReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if pollDaemonHTTP(http.DefaultClient, srv.URL, "") {
		t.Error("expected false for a non-200 response")
	}
}

func TestPollDaemonHTTP_Unreachable_ReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	if pollDaemonHTTP(http.DefaultClient, url, "") {
		t.Error("expected false for an unreachable server")
	}
}

// TestPollDaemonHTTP_AttachesIPCToken verifies the poll carries the daemon's IPC auth token — /status now requires it like every other daemon IPC endpoint except /ping (see internal/ipctoken).
func TestPollDaemonHTTP_AttachesIPCToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "ipc-token")
	token, err := ipctoken.Generate(tokenPath)
	if err != nil {
		t.Fatalf("ipctoken.Generate: %v", err)
	}

	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(ipctoken.HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !pollDaemonHTTP(http.DefaultClient, srv.URL, tokenPath) {
		t.Fatal("expected true for a 200 OK response")
	}
	if gotToken != token {
		t.Errorf("expected the %s header to carry %q, got %q", ipctoken.HeaderName, token, gotToken)
	}
}

func TestUpdate_DaemonStatusMsg_FlipsDaemonOKBothDirections(t *testing.T) {
	m := model{daemonOK: false}
	next, _ := m.Update(daemonStatusMsg(true))
	nm := next.(model)
	if !nm.daemonOK {
		t.Error("expected daemonOK to flip to true on daemonStatusMsg(true)")
	}

	m2 := model{daemonOK: true}
	next2, _ := m2.Update(daemonStatusMsg(false))
	nm2 := next2.(model)
	if nm2.daemonOK {
		t.Error("expected daemonOK to flip to false on daemonStatusMsg(false)")
	}
}

func TestUpdate_DaemonPollMsg_ReturnsBatchOfTickAndPoll(t *testing.T) {
	m := model{}
	_, cmd := m.Update(daemonPollMsg{})
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (should re-arm the poll tick + fire an HTTP check)")
	}
	// tea.Batch resolves to a tea.BatchMsg carrying both sub-commands' results — just prove it's non-nil and produces a message, without depending on tea's internal batch representation.
	msg := cmd()
	if msg == nil {
		t.Fatal("expected the batched command to produce a message")
	}
	if _, ok := msg.(tea.BatchMsg); !ok {
		t.Fatalf("expected a tea.BatchMsg from daemonPollMsg's Update, got %T", msg)
	}
}

// --- tool-activity / thinking status ---

func TestUpdate_ToolActivityStarted_SetsLiveStatus(t *testing.T) {
	m := newTestModel()

	next, cmd := m.Update(agent.ToolActivity{
		ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolStarted,
	})
	nm := next.(model)

	if nm.activity == nil {
		t.Fatal("expected m.activity to be set on ToolStarted")
	}
	if nm.activity.kind != statusTool {
		t.Errorf("expected kind statusTool, got %v", nm.activity.kind)
	}
	if nm.activity.id != "call-1" {
		t.Errorf("expected id %q, got %q", "call-1", nm.activity.id)
	}
	if nm.activity.label != `query_memory("Riddler")` {
		t.Errorf("unexpected label %q", nm.activity.label)
	}
	if cmd == nil {
		t.Error("expected a non-nil cmd (re-arm wait + start the spinner ticking)")
	}
}

func TestUpdate_ToolActivityFinished_AppendsTranscriptAndRevertsToThinking(t *testing.T) {
	m := newTestModel()
	startBefore := len(m.messages)

	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolStarted})
	m = started.(model)

	finished, _ := m.Update(agent.ToolActivity{
		ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolFinished, ResultSummary: "3 hits",
	})
	nm := finished.(model)

	if len(nm.messages) != startBefore+1 {
		t.Fatalf("expected exactly one new transcript message, got %d new", len(nm.messages)-startBefore)
	}
	last := nm.messages[len(nm.messages)-1]
	if !last.IsToolLog {
		t.Error("expected the new message to have IsToolLog set")
	}
	if last.Content != `query_memory("Riddler") → 3 hits` {
		t.Errorf("unexpected transcript content: %q", last.Content)
	}
	if nm.activity == nil || nm.activity.kind != statusThinking {
		t.Errorf("expected activity to revert to statusThinking after its own call finishes, got %+v", nm.activity)
	}
}

func TestUpdate_ToolActivityFinished_Failed_MarksTranscriptEntryFailed(t *testing.T) {
	m := newTestModel()
	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "shell_exec", ArgsSummary: `"rm x"`, Phase: agent.ToolStarted})
	m = started.(model)

	finished, _ := m.Update(agent.ToolActivity{
		ID: "call-1", Name: "shell_exec", ArgsSummary: `"rm x"`, Phase: agent.ToolFinished, ResultSummary: "failed", Err: true,
	})
	nm := finished.(model)

	last := nm.messages[len(nm.messages)-1]
	if !last.ToolLogFailed {
		t.Error("expected ToolLogFailed to be set on a Finished event with Err=true")
	}
}

func TestUpdate_ToolActivityFinished_Success_DoesNotMarkFailed(t *testing.T) {
	m := newTestModel()
	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "list_files", Phase: agent.ToolStarted})
	m = started.(model)

	finished, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "list_files", Phase: agent.ToolFinished, ResultSummary: "done", Err: false})
	nm := finished.(model)

	last := nm.messages[len(nm.messages)-1]
	if last.ToolLogFailed {
		t.Error("expected ToolLogFailed to stay false on a successful Finished event")
	}
}

// Covers the concurrent-tool-calls edge case: if call A finishes after call B has already become the live status, A's Finished event must still append A's transcript line, but must not stomp on B's still-active live status.
func TestUpdate_ToolActivityFinished_StaleID_DoesNotClobberNewerActivity(t *testing.T) {
	m := newTestModel()

	afterA, _ := m.Update(agent.ToolActivity{ID: "call-A", Name: "read_file", Phase: agent.ToolStarted})
	m = afterA.(model)
	afterB, _ := m.Update(agent.ToolActivity{ID: "call-B", Name: "list_files", Phase: agent.ToolStarted})
	m = afterB.(model)

	if m.activity.id != "call-B" {
		t.Fatalf("expected call-B to be the live status (last-started-wins), got %q", m.activity.id)
	}

	finishedA, _ := m.Update(agent.ToolActivity{ID: "call-A", Name: "read_file", Phase: agent.ToolFinished, ResultSummary: "done"})
	nm := finishedA.(model)

	if nm.activity == nil || nm.activity.id != "call-B" {
		t.Errorf("call-A finishing must not clobber call-B's still-active live status, got %+v", nm.activity)
	}
	last := nm.messages[len(nm.messages)-1]
	if !last.IsToolLog || last.Content != "read_file() → done" {
		t.Errorf("expected call-A's transcript entry to still be appended, got %+v", last)
	}
}

// The approval menu (ModeToolConfirm) already says "paused, waiting on you" — a spinner ticking behind it would misleadingly read as "still running".
func TestUpdate_ToolRequest_ClearsLiveStatus(t *testing.T) {
	m := newTestModel()
	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "shell_exec", ArgsSummary: `"rm -rf /tmp/x"`, Phase: agent.ToolStarted})
	m = started.(model)
	if m.activity == nil {
		t.Fatal("setup: expected activity to be set")
	}

	resChan := make(chan string, 1)
	next, _ := m.Update(agent.ToolRequest{Description: "shell: rm -rf /tmp/x", ResultChan: resChan})
	nm := next.(model)

	if nm.activity != nil {
		t.Errorf("expected the live status to be cleared once HITL approval takes over, got %+v", nm.activity)
	}
}

// --- generalized HITL plumbing (F1a) ---

// TestUpdate_ToolRequest_NonEditable_HidesSuggestChanges verifies a request with no EditableCommand (e.g. read_clipboard, a sensitive read_file) doesn't offer "Suggest changes" — there's no command text to edit.
func TestUpdate_ToolRequest_NonEditable_HidesSuggestChanges(t *testing.T) {
	m := newTestModel()

	next, _ := m.Update(agent.ToolRequest{Description: "read the clipboard", ResultChan: make(chan string, 1)})
	nm := next.(model)

	for _, it := range nm.hitlList.Items() {
		if ci, ok := it.(commandItem); ok && ci.title == "Suggest changes" {
			t.Error("expected \"Suggest changes\" to be hidden for a non-editable request")
		}
	}
}

// TestUpdate_ToolRequest_Editable_ShowsSuggestChanges verifies a shell-backed request (EditableCommand set) still offers "Suggest changes".
func TestUpdate_ToolRequest_Editable_ShowsSuggestChanges(t *testing.T) {
	m := newTestModel()

	next, _ := m.Update(agent.ToolRequest{Description: "shell: ls -la", EditableCommand: "ls -la", ResultChan: make(chan string, 1)})
	nm := next.(model)

	found := false
	for _, it := range nm.hitlList.Items() {
		if ci, ok := it.(commandItem); ok && ci.title == "Suggest changes" {
			found = true
		}
	}
	if !found {
		t.Error("expected \"Suggest changes\" to be offered for a shell-backed (editable) request")
	}
}

// TestUpdate_ToolConfirm_AllowOnce_CallsExecuteAndDeliversResult verifies selecting "Allow once" calls the request's Execute func (not a hardcoded shell command) and delivers its result on ResultChan — the generalized plumbing F1a introduces.
func TestUpdate_ToolConfirm_AllowOnce_CallsExecuteAndDeliversResult(t *testing.T) {
	m := newTestModel()
	m.mode = ModeToolConfirm
	resChan := make(chan string, 1)
	executeCalled := make(chan struct{}, 1)
	m.activeToolReq = &agent.ToolRequest{
		Description: "read the clipboard",
		Execute: func() string {
			executeCalled <- struct{}{}
			return "clipboard contents"
		},
		ResultChan: resChan,
	}
	m.hitlList.Select(0) // "Allow once" is always index 0

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case <-executeCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Execute to be called")
	}
	select {
	case got := <-resChan:
		if got != "clipboard contents" {
			t.Errorf("expected Execute's result delivered on ResultChan, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the result on ResultChan")
	}
}

// TestUpdate_ToolConfirm_AllowForSession_StoresAllowKey verifies "Allow for session" stores the request's AllowKey (not a hardcoded Command field) into AllowedCmds.
func TestUpdate_ToolConfirm_AllowForSession_StoresAllowKey(t *testing.T) {
	m := newTestModel()
	m.mode = ModeToolConfirm
	m.activeToolReq = &agent.ToolRequest{
		Description: "read the clipboard",
		Execute:     func() string { return "ok" },
		ResultChan:  make(chan string, 1),
		AllowKey:    "read_clipboard",
	}
	m.hitlList.Select(1) // "Allow for session" is always index 1

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if _, allowed := m.agent.AllowedCmds.Load("read_clipboard"); !allowed {
		t.Error("expected AllowKey to be stored in AllowedCmds after \"Allow for session\"")
	}
}

// TestUpdate_ToolConfirm_AllowForSession_EmptyAllowKey_DoesNotStore verifies a request with no AllowKey doesn't store a bogus empty-string key.
func TestUpdate_ToolConfirm_AllowForSession_EmptyAllowKey_DoesNotStore(t *testing.T) {
	m := newTestModel()
	m.mode = ModeToolConfirm
	m.activeToolReq = &agent.ToolRequest{
		Description: "something ungated",
		Execute:     func() string { return "ok" },
		ResultChan:  make(chan string, 1),
	}
	m.hitlList.Select(1)

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if _, stored := m.agent.AllowedCmds.Load(""); stored {
		t.Error("expected no empty-string key stored in AllowedCmds")
	}
}

// --- concurrent tool-request queueing (FINDING 3) ---

// TestUpdate_ToolRequest_SecondArrivesWhileFirstPending_QueuesInsteadOfOverwriting verifies a second
// agent.ToolRequest that arrives while the first is still awaiting a user decision does not clobber
// m.activeToolReq — it queues, and only becomes active once the first request is resolved. Under the old
// overwrite behaviour the first request's ResultChan never received a value because activeToolReq no longer
// pointed at it, which wedges the agent goroutine blocked on that channel forever.
func TestUpdate_ToolRequest_SecondArrivesWhileFirstPending_QueuesInsteadOfOverwriting(t *testing.T) {
	m := newTestModel()
	res1 := make(chan string, 1)
	res2 := make(chan string, 1)

	next, _ := m.Update(agent.ToolRequest{Description: "shell: ls -la", ResultChan: res1})
	m = next.(model)

	next, _ = m.Update(agent.ToolRequest{Description: "shell: rm -rf /tmp/x", ResultChan: res2})
	m = next.(model)

	if m.activeToolReq == nil || m.activeToolReq.Description != "shell: ls -la" {
		t.Fatalf("expected the first request to stay active while the second queues, got %+v", m.activeToolReq)
	}

	// Resolve the first request (Esc rejects it, same as TestUpdate_Esc_InToolConfirm_RejectsCommand).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)

	select {
	case got := <-res1:
		if !strings.Contains(got, "rejected") {
			t.Errorf("expected a rejection on the first request's ResultChan, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first request's result")
	}

	if m.activeToolReq == nil || m.activeToolReq.Description != "shell: rm -rf /tmp/x" {
		t.Fatalf("expected the queued second request to become active once the first resolved, got %+v", m.activeToolReq)
	}
	if m.mode != ModeToolConfirm {
		t.Errorf("expected mode to stay ModeToolConfirm for the promoted request, got %q", m.mode)
	}

	// Resolve the second (now-active) request too, so both channels end up with a value.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)

	select {
	case got := <-res2:
		if !strings.Contains(got, "rejected") {
			t.Errorf("expected a rejection on the second request's ResultChan, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the second request's result")
	}
}

func TestUpdate_ResponseMsg_ClearsActivity(t *testing.T) {
	m := newTestModel()
	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", Phase: agent.ToolStarted})
	m = started.(model)
	if m.activity == nil {
		t.Fatal("setup: expected activity to be set")
	}

	next, _ := m.Update(responseMsg{Text: "hi there"})
	nm := next.(model)
	if nm.activity != nil {
		t.Errorf("expected activity to be cleared on responseMsg, got %+v", nm.activity)
	}
}

// TestUpdate_ResponseMsg_SystemChunk_DoesNotFlipIsConnectedTrue verifies a Sender=system chunk (e.g. the "connection lost — reconnecting…" notice) does not mark isConnected true — only real ora/you server traffic proves the link is up; otherwise the "⚠ reconnecting" hint-bar chip would go quiet precisely while the link is down.
func TestUpdate_ResponseMsg_SystemChunk_DoesNotFlipIsConnectedTrue(t *testing.T) {
	m := newTestModel()
	m.isConnected = false

	next, _ := m.Update(responseMsg{Text: "connection lost — reconnecting…", Sender: agent.SenderSystem})
	nm := next.(model)

	if nm.isConnected {
		t.Error("expected isConnected to stay false for a system chunk")
	}
}

// TestUpdate_ResponseMsg_OraOrYouChunk_SetsIsConnectedTrue verifies real server traffic (ora text or a you transcription) does mark the link as connected.
func TestUpdate_ResponseMsg_OraOrYouChunk_SetsIsConnectedTrue(t *testing.T) {
	for _, sender := range []string{"", agent.SenderYou} {
		m := newTestModel()
		m.isConnected = false

		next, _ := m.Update(responseMsg{Text: "hi", Sender: sender})
		nm := next.(model)

		if !nm.isConnected {
			t.Errorf("expected isConnected to become true for sender %q", sender)
		}
	}
}

// TestUpdate_ResponseMsg_NonOraChunk_DoesNotClearActivity verifies a "you" transcription or a "[ora stopped]" system chunk doesn't clear a pending thinking spinner — only an actual ora reply should, since no reply has arrived yet for either of those.
func TestUpdate_ResponseMsg_NonOraChunk_DoesNotClearActivity(t *testing.T) {
	for _, sender := range []string{agent.SenderYou, agent.SenderSystem} {
		m := newTestModel()
		m.activity = &liveStatus{kind: statusThinking, label: "thinking", started: time.Now()}

		next, _ := m.Update(responseMsg{Text: "chunk", Sender: sender})
		nm := next.(model)

		if nm.activity == nil {
			t.Errorf("expected activity to survive a %q-sender chunk, got cleared", sender)
		}
	}
}

// TestUpdate_ResponseMsg_OraChunk_ClearsActivity verifies an actual ora reply chunk still clears the thinking spinner (the original behavior).
func TestUpdate_ResponseMsg_OraChunk_ClearsActivity(t *testing.T) {
	m := newTestModel()
	m.activity = &liveStatus{kind: statusThinking, label: "thinking", started: time.Now()}

	next, _ := m.Update(responseMsg{Text: "the answer is 4"})
	nm := next.(model)

	if nm.activity != nil {
		t.Error("expected activity to be cleared on an ora chunk")
	}
}

// TestUpdate_ResponseMsg_RoutesSenderToMessage verifies responseMsg's Sender field ("" / agent.SenderYou / agent.SenderSystem) picks the right transcript speaker instead of always rendering as "ora".
func TestUpdate_ResponseMsg_RoutesSenderToMessage(t *testing.T) {
	cases := []struct {
		name        string
		chunkSender string
		wantSender  string
	}{
		{"empty sender is ora", "", "ora"},
		{"SenderYou is you", agent.SenderYou, "you"},
		{"SenderSystem is system", agent.SenderSystem, "system"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()

			next, _ := m.Update(responseMsg{Text: "hello", Sender: tc.chunkSender})
			nm := next.(model)

			// streamLine merges into the prior message when sender/thought-state match (see its own doc comment) — the intro banner is itself sender "system", so assert on the suffix rather than assuming a brand-new message.
			got := nm.messages[len(nm.messages)-1]
			if got.Sender != tc.wantSender || !strings.HasSuffix(got.Content, "hello") {
				t.Errorf("expected {Sender: %q, Content ending in %q}, got %+v", tc.wantSender, "hello", got)
			}
		})
	}
}

// TestStreamLine_ThoughtFlagTaggedOnMessage verifies streamLine tags the appended Message's IsThought directly from its explicit isThought parameter — not by sniffing the content for markdown "**", which desyncs permanently the first time a real reply contains a complete bold span.
func TestStreamLine_ThoughtFlagTaggedOnMessage(t *testing.T) {
	m := newTestModel()
	startBefore := len(m.messages)

	m.streamLine("ora", "**Planning the reply**", true)

	if len(m.messages) != startBefore+1 {
		t.Fatalf("expected 1 new message, got %d", len(m.messages)-startBefore)
	}
	got := m.messages[len(m.messages)-1]
	if !got.IsThought {
		t.Error("expected IsThought=true")
	}
	if got.Content != "**Planning the reply**" {
		t.Errorf("expected content to pass through unmodified (no ** stripping), got %q", got.Content)
	}
}

// TestStreamLine_YouSender_ConsecutiveCallsNeverMerge verifies two consecutive "you" utterances stay as two separate messages instead of running together into one ("what's the weatherand tomorrow?") — "you" and "system" chunks are complete discrete units, unlike ora's streaming text.
func TestStreamLine_YouSender_ConsecutiveCallsNeverMerge(t *testing.T) {
	m := newTestModel()
	startBefore := len(m.messages)

	m.streamLine("you", "what's the weather", false)
	m.streamLine("you", "and tomorrow?", false)

	if len(m.messages) != startBefore+2 {
		t.Fatalf("expected 2 separate messages, got %d: %+v", len(m.messages)-startBefore, m.messages)
	}
	last := m.messages[len(m.messages)-1]
	if last.Content != "and tomorrow?" {
		t.Errorf("expected the second message to be its own unmerged block, got %q", last.Content)
	}
}

// TestStreamLine_SystemSender_ConsecutiveCallsNeverMerge verifies two consecutive system chunks (e.g. two barge-ins) stay separate instead of merging into "[ora stopped][ora stopped]".
func TestStreamLine_SystemSender_ConsecutiveCallsNeverMerge(t *testing.T) {
	m := newTestModel()
	startBefore := len(m.messages)

	m.streamLine("system", "[ora stopped]", false)
	m.streamLine("system", "[ora stopped]", false)

	if len(m.messages) != startBefore+2 {
		t.Fatalf("expected 2 separate messages, got %d: %+v", len(m.messages)-startBefore, m.messages)
	}
}

// TestStreamLine_OraSender_ConsecutiveCallsStillMerge verifies ora's own streaming text still merges across calls (unlike you/system) — this is the normal token-by-token transcription stream, not discrete units.
func TestStreamLine_OraSender_ConsecutiveCallsStillMerge(t *testing.T) {
	m := newTestModel()
	m.streamLine("you", "priming", false) // ensure the prior message isn't itself sender "ora"
	startBefore := len(m.messages)

	m.streamLine("ora", "Hello", false)
	m.streamLine("ora", " there.", false)

	if len(m.messages) != startBefore+1 {
		t.Fatalf("expected the two ora chunks to merge into 1 message, got %d: %+v", len(m.messages)-startBefore, m.messages)
	}
	if m.messages[len(m.messages)-1].Content != "Hello there." {
		t.Errorf("expected merged content %q, got %q", "Hello there.", m.messages[len(m.messages)-1].Content)
	}
}

// TestStreamLine_FinalTextAfterThought_StartsNewMessageNotThought verifies a non-thought chunk arriving right after a thought chunk starts a new, non-thought message rather than appending to (or misclassifying as) the thought block — the old "**"-toggle heuristic could desync this permanently.
func TestStreamLine_FinalTextAfterThought_StartsNewMessageNotThought(t *testing.T) {
	m := newTestModel()
	startBefore := len(m.messages)

	m.streamLine("ora", "**Planning the reply**", true)
	m.streamLine("ora", "Here's your answer.", false)

	if len(m.messages) != startBefore+2 {
		t.Fatalf("expected 2 new separate messages, got %d: %+v", len(m.messages)-startBefore, m.messages)
	}
	last := m.messages[len(m.messages)-1]
	if last.IsThought {
		t.Error("expected the second message to have IsThought=false")
	}
	if last.Content != "Here's your answer." {
		t.Errorf("unexpected content %q", last.Content)
	}
}

// TestStreamLine_VoiceMode_ShowsFinalOraTextAndThoughts verifies voice mode shows both final ora text and thoughts in the transcript. Final ora text now arrives via OutputTranscription (the complete-text form of what Ora actually said, forwarded through the same responseMsg path as any other ora chunk) rather than the old ModelTurn fragments, so there's no longer a reason to hide it in voice mode.
func TestStreamLine_VoiceMode_ShowsFinalOraTextAndThoughts(t *testing.T) {
	m := newTestModel()
	m.mode = ModeVoice
	startBefore := len(m.messages)

	m.streamLine("ora", "Here's your answer.", false)
	if len(m.messages) != startBefore+1 {
		t.Fatalf("expected final ora text to be shown in voice mode, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}

	m.streamLine("ora", "**thinking about it**", true)
	if len(m.messages) != startBefore+2 {
		t.Fatalf("expected thought text to still be shown in voice mode, got %d new messages", len(m.messages)-startBefore)
	}
}

// TestStreamLine_CloseOraBlockFlagSet_ForcesNewBlockInsteadOfMerging verifies a pending turn boundary (m.closeOraBlock, set when a TurnBoundary chunk arrives — see TestUpdate_ResponseMsg_TurnBoundary_SetsCloseOraBlockWithoutAppending) makes the next ora chunk start a fresh message block instead of merging into whatever the prior turn left behind, even though sender and thought-state both match.
func TestStreamLine_CloseOraBlockFlagSet_ForcesNewBlockInsteadOfMerging(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "Hello there.", false)
	startBefore := len(m.messages)

	m.closeOraBlock = true
	m.streamLine("ora", "Namaste again.", false)

	if len(m.messages) != startBefore+1 {
		t.Fatalf("expected the turn boundary to force a new block, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[len(m.messages)-1].Content; got != "Namaste again." {
		t.Errorf("expected the new block's content to be exactly the post-boundary chunk, got %q", got)
	}
}

// TestStreamLine_CloseOraBlockFlagConsumedByFirstPostBoundaryChunk verifies the boundary is a one-shot signal: once the first ora chunk after it has started its own new block, subsequent ora chunks merge normally again.
func TestStreamLine_CloseOraBlockFlagConsumedByFirstPostBoundaryChunk(t *testing.T) {
	m := newTestModel()
	m.closeOraBlock = true
	m.streamLine("ora", "First.", false)
	startBefore := len(m.messages)

	m.streamLine("ora", " Second.", false)

	if len(m.messages) != startBefore {
		t.Fatalf("expected the post-boundary chunk to merge normally, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
}

// TestStreamLine_RestartedUtterance_ResetsBlockInsteadOfAppending verifies the VAD-restart guard: when OutputTranscription's next chunk is itself a PREFIX of what the block already has (the model got interrupted mid-utterance, commonly by its own voice echoing into the mic, and restarted from the beginning — a documented Live API behavior), the block resets to the new chunk instead of appending it, which is what collapses a restart loop into just its final, longest attempt.
func TestStreamLine_RestartedUtterance_ResetsBlockInsteadOfAppending(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "Hello there, how can I help", false)
	startBefore := len(m.messages)

	m.streamLine("ora", "Hello there, h", false) // restart, only got this far before restarting again

	if len(m.messages) != startBefore {
		t.Fatalf("expected the restart to still merge into the same block, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[len(m.messages)-1].Content; got != "Hello there, h" {
		t.Errorf("expected the block content reset to the restarted chunk, got %q", got)
	}
}

// TestStreamLine_ShortPrefixOverlap_BelowRestartThreshold_StillAppends verifies the restart guard only fires above its rune-count threshold — a short chunk that happens to be a coincidental prefix of the block so far (e.g. a legitimately repeated short word) must not falsely reset the block and lose everything already streamed.
func TestStreamLine_ShortPrefixOverlap_BelowRestartThreshold_StillAppends(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "the weather today is nice", false)
	startBefore := len(m.messages)

	m.streamLine("ora", "the", false) // short coincidental prefix match, below the restart threshold

	if len(m.messages) != startBefore {
		t.Fatalf("expected still one block, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[len(m.messages)-1].Content; got != "the weather today is nicethe" {
		t.Errorf("expected the short chunk appended (not treated as a restart), got %q", got)
	}
}

// TestStreamLine_ExactResendChunk_DroppedNotDuplicated verifies an exact resend of a chunk already at the block's tail (an echo, per the Live API's documented server-side truncation/resend behavior) is dropped instead of duplicating it.
func TestStreamLine_ExactResendChunk_DroppedNotDuplicated(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "Hello there", false)
	startBefore := len(m.messages)

	m.streamLine("ora", " there", false) // exact resend of the existing tail

	if len(m.messages) != startBefore {
		t.Fatalf("expected still one block, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[len(m.messages)-1].Content; got != "Hello there" {
		t.Errorf("expected the resent tail dropped (content unchanged), got %q", got)
	}
}

// TestStreamLine_CumulativeSnapshotChunk_ReplacesBlock verifies a chunk that is a superset of the block so far (a full cumulative snapshot rather than a delta — kept as defense even though OutputTranscription is documented as incremental fragments in practice) replaces the block's content instead of duplicating the overlap.
func TestStreamLine_CumulativeSnapshotChunk_ReplacesBlock(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "Hello", false)
	startBefore := len(m.messages)

	m.streamLine("ora", "Hello there", false)

	if len(m.messages) != startBefore {
		t.Fatalf("expected still one block, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[len(m.messages)-1].Content; got != "Hello there" {
		t.Errorf("expected the block replaced with the full snapshot (not \"HelloHello there\"), got %q", got)
	}
}

// TestUpdate_ResponseMsg_TurnBoundary_SetsCloseOraBlockWithoutAppending verifies a TurnBoundary chunk (empty Text) sets the pending-boundary flag but never appends an empty message to the transcript.
func TestUpdate_ResponseMsg_TurnBoundary_SetsCloseOraBlockWithoutAppending(t *testing.T) {
	m := newTestModel()
	startBefore := len(m.messages)

	next, _ := m.Update(responseMsg{TurnBoundary: true})
	nm := next.(model)

	if len(nm.messages) != startBefore {
		t.Fatalf("expected no message appended for a boundary marker, got %d new messages: %+v", len(nm.messages)-startBefore, nm.messages)
	}
	if !nm.closeOraBlock {
		t.Error("expected closeOraBlock to be set")
	}
}

func TestUpdate_ErrorMsg_ClearsActivity(t *testing.T) {
	m := newTestModel()
	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", Phase: agent.ToolStarted})
	m = started.(model)

	next, _ := m.Update(errorMsg(errTest))
	nm := next.(model)
	if nm.activity != nil {
		t.Errorf("expected activity to be cleared on errorMsg, got %+v", nm.activity)
	}
}

func TestUpdate_TextSend_SetsThinkingActivity(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("what did I do yesterday")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(model)

	if nm.activity == nil || nm.activity.kind != statusThinking {
		t.Fatalf("expected a statusThinking activity after sending text, got %+v", nm.activity)
	}
	if cmd == nil {
		t.Error("expected a non-nil cmd to start the spinner ticking")
	}
}

func TestUpdate_SpinnerTickMsg_ReArmsOnlyWhenActivityIsSet(t *testing.T) {
	m := newTestModel()
	m.activity = nil

	_, cmd := m.Update(spinner.TickMsg{})
	if cmd != nil {
		t.Error("expected no re-arm when there's no active status")
	}

	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", Phase: agent.ToolStarted})
	m = started.(model)

	_, cmd2 := m.Update(spinner.TickMsg{})
	if cmd2 == nil {
		t.Error("expected the spinner to re-arm while an activity is active")
	}
}

// --- quit / esc semantics ---

// TestUpdate_CtrlC_RequiresDoublePressToQuit_AcrossEveryMode verifies Ctrl+C no longer quits on the first press — it arms a ~1.5s quit-confirm window (see quitConfirmWindow) — and only quits on a second press within that window, across every mode including ModeToolConfirm and ModeToolEdit (previously swallowed there because those modes' own KeyMsg switches returned before reaching the global hotkey switch — the double-press check runs even earlier than that, so it's unaffected by the same issue).
func TestUpdate_CtrlC_RequiresDoublePressToQuit_AcrossEveryMode(t *testing.T) {
	for _, mode := range []AgentMode{ModeBoth, ModeVoice, ModeText, ModeToolConfirm, ModeToolEdit} {
		t.Run(string(mode), func(t *testing.T) {
			m := newTestModel()
			m.mode = mode
			if mode == ModeToolConfirm || mode == ModeToolEdit {
				m.activeToolReq = &agent.ToolRequest{Description: "shell: echo hi", ResultChan: make(chan string, 1)}
			}

			first, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			nm := first.(model)
			if cmd != nil {
				if _, ok := cmd().(tea.QuitMsg); ok {
					t.Fatal("expected the first Ctrl+C not to quit immediately")
				}
			}
			if !nm.quitConfirmArmed {
				t.Fatal("expected the first Ctrl+C to arm the quit confirmation")
			}

			second, cmd2 := nm.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			if cmd2 == nil {
				t.Fatal("expected a quit cmd on the second Ctrl+C within the window")
			}
			if _, ok := cmd2().(tea.QuitMsg); !ok {
				t.Errorf("expected tea.QuitMsg on the second Ctrl+C, got %T", cmd2())
			}
			_ = second
		})
	}
}

// TestUpdate_CtrlC_ArmedPastWindow_RearmsInsteadOfQuitting verifies a second Ctrl+C arriving after the confirm window has lapsed re-arms instead of quitting — the confirmation shouldn't still fire from a press left over from a minute ago.
func TestUpdate_CtrlC_ArmedPastWindow_RearmsInsteadOfQuitting(t *testing.T) {
	m := newTestModel()
	m.quitConfirmArmed = true
	m.quitConfirmArmedAt = time.Now().Add(-2 * quitConfirmWindow)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	nm := next.(model)

	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("expected a stale arm past the window not to quit on this press")
		}
	}
	if !nm.quitConfirmArmed {
		t.Error("expected this press to re-arm the confirmation")
	}
}

// TestUpdate_TickMsg_ClearsExpiredQuitConfirmArm verifies the confirm-armed hint doesn't linger forever — the existing 50ms tickMsg handler clears it once the window lapses, same auto-clear shape staleActivityTimeout already has for the tool-activity spinner.
func TestUpdate_TickMsg_ClearsExpiredQuitConfirmArm(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.quitConfirmArmed = true
	m.quitConfirmArmedAt = time.Now().Add(-2 * quitConfirmWindow)

	next, _ := m.Update(tickMsg(time.Now()))
	nm := next.(model)

	if nm.quitConfirmArmed {
		t.Error("expected an expired quit-confirm arm to be cleared on tick")
	}
}

// TestExecuteCommand_Quit_ReturnsQuitCmd verifies /quit quits immediately, no confirmation — it's explicit, unlike Ctrl+C.
func TestExecuteCommand_Quit_ReturnsQuitCmd(t *testing.T) {
	m := newTestModel()

	cmd := m.executeCommand("/quit")

	if cmd == nil {
		t.Fatal("expected a non-nil quit cmd from /quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", cmd())
	}
}

// TestFilterCommands_MatchesSubstringNotJustPrefix verifies the "/" command menu filters by substring, not just prefix — "otes" should still find "notes".
func TestFilterCommands_MatchesSubstringNotJustPrefix(t *testing.T) {
	l := newCommandList(DefaultStyles())
	FilterCommands(&l, "otes")

	items := l.Items()
	found := false
	for _, it := range items {
		if ci, ok := it.(commandItem); ok && ci.title == "notes" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected substring query %q to match \"notes\", got items: %+v", "otes", items)
	}
}

// TestFilterCommands_StillMatchesPrefix is the regression check: prefix queries (the common case — typing from the start of a command name) must keep working under substring matching.
func TestFilterCommands_StillMatchesPrefix(t *testing.T) {
	l := newCommandList(DefaultStyles())
	FilterCommands(&l, "vo")

	items := l.Items()
	if len(items) == 0 {
		t.Fatal("expected prefix query \"vo\" to match at least the voice commands")
	}
	for _, it := range items {
		ci, ok := it.(commandItem)
		if !ok || !strings.Contains(ci.title, "vo") {
			t.Errorf("unexpected non-matching item in filtered results: %+v", it)
		}
	}
}

// TestUpdate_Esc_InToolConfirm_RejectsCommand verifies Esc in ModeToolConfirm has the same effect as selecting "Reject" — sends the rejection on ResultChan, drops back to ModeBoth, and clears the pending request — rather than quitting the app.
func TestUpdate_Esc_InToolConfirm_RejectsCommand(t *testing.T) {
	m := newTestModel()
	m.mode = ModeToolConfirm
	resChan := make(chan string, 1)
	m.activeToolReq = &agent.ToolRequest{Description: "shell: rm -rf /tmp/x", ResultChan: resChan}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(model)

	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("expected Esc not to quit the app")
		}
	}
	if nm.mode != ModeBoth {
		t.Errorf("expected mode back to ModeBoth, got %v", nm.mode)
	}
	if nm.activeToolReq != nil {
		t.Error("expected activeToolReq to be cleared")
	}
	select {
	case got := <-resChan:
		if got != "User rejected this command." {
			t.Errorf("unexpected rejection message: %q", got)
		}
	default:
		t.Error("expected a rejection sent on ResultChan")
	}
}

// TestUpdate_Esc_WithCmdListOpen_ClosesMenuWithoutQuitting verifies Esc closes the open "/" command menu instead of quitting or falling through to the textarea.
func TestUpdate_Esc_WithCmdListOpen_ClosesMenuWithoutQuitting(t *testing.T) {
	m := newTestModel()
	m.showCmdList = true
	m.textarea.SetValue("/vo")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(model)

	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("expected Esc not to quit the app")
		}
	}
	if nm.showCmdList {
		t.Error("expected showCmdList to be closed")
	}
}

// TestUpdate_Esc_ScrolledAwayFromBottom_JumpsToBottomWithoutClearingTextarea verifies the new WP3 rung: with no menu open but the viewport scrolled away from the bottom, Esc jumps back to the bottom instead of (in the same keypress) also clearing a non-empty textarea — proving the rung ordering is menu-close, then jump-to-bottom, then clear-textarea, not "do everything at once."
func TestUpdate_Esc_ScrolledAwayFromBottom_JumpsToBottomWithoutClearingTextarea(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	if m.viewport.AtBottom() {
		t.Fatal("setup: expected to have scrolled away from the bottom")
	}
	m.textarea.SetValue("still typing this")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(model)

	if !nm.viewport.AtBottom() {
		t.Error("expected Esc to jump the viewport back to the bottom")
	}
	if nm.textarea.Value() != "still typing this" {
		t.Errorf("expected the textarea to be left alone on this keypress, got %q", nm.textarea.Value())
	}
}

// TestUpdate_Esc_MenuOpenAndScrolledAway_OnlyClosesMenu verifies menu-close still takes priority over the jump-to-bottom rung — both conditions true at once, only the menu closes on this keypress.
func TestUpdate_Esc_MenuOpenAndScrolledAway_OnlyClosesMenu(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	m.showCmdList = true
	if m.viewport.AtBottom() {
		t.Fatal("setup: expected to have scrolled away from the bottom")
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(model)

	if nm.showCmdList {
		t.Error("expected the menu to close")
	}
	if nm.viewport.AtBottom() {
		t.Error("expected the viewport to stay scrolled away on this keypress (jump-to-bottom is the next rung, not this one)")
	}
}

// TestUpdate_Esc_WithNonEmptyTextarea_ClearsIt verifies Esc clears in-progress typed input (not prefixed with "/", so no menu is open) instead of quitting.
func TestUpdate_Esc_WithNonEmptyTextarea_ClearsIt(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("what did I do yesterday")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(model)

	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("expected Esc not to quit the app")
		}
	}
	if nm.textarea.Value() != "" {
		t.Errorf("expected the textarea to be cleared, got %q", nm.textarea.Value())
	}
}

// TestUpdate_Esc_WithEmptyTextareaAndNoMenu_IsNoop verifies Esc with nothing to clear and no menu open doesn't quit and doesn't error.
func TestUpdate_Esc_WithEmptyTextareaAndNoMenu_IsNoop(t *testing.T) {
	m := newTestModel()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(model)

	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("expected Esc not to quit the app")
		}
	}
	if nm.showCmdList {
		t.Error("expected showCmdList to remain false")
	}
}

var errTest = &testError{"boom"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// --- viewport height recalculation ---

func TestRecalcViewportHeight_StatusLineAddsExactlyOneLine(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.mode = ModeText // zeroes signalHeight, isolating the height delta to the status line alone

	m.activity = nil
	m.recalcViewportHeight()
	withoutStatus := m.viewport.Height

	m.activity = &liveStatus{kind: statusThinking, label: "thinking"}
	m.recalcViewportHeight()
	withStatus := m.viewport.Height

	if withoutStatus-withStatus != 1 {
		t.Errorf("expected the status line to cost exactly 1 line of viewport height, got %d (without=%d, with=%d)",
			withoutStatus-withStatus, withoutStatus, withStatus)
	}
}

// TestRecalcViewportHeight_CmdListOpen_AccountsForExtraRows verifies opening the "/" command menu shrinks the viewport by exactly the extra rows renderInput actually adds for the menu (in place of the 1-line hints row) — otherwise the layout overflows the terminal. Measures the real rendered height via lipgloss.Height rather than a hardcoded row count, so it stays correct if the menu's own styling changes.
func TestRecalcViewportHeight_CmdListOpen_AccountsForExtraRows(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.mode = ModeText // zeroes signalHeight, isolating the delta to the menu alone

	m.recalcViewportHeight()
	withoutMenuInputHeight := lipgloss.Height(m.renderInput())
	withoutMenuViewportHeight := m.viewport.Height

	m.showCmdList = true
	m.recalcViewportHeight()
	withMenuInputHeight := lipgloss.Height(m.renderInput())
	withMenuViewportHeight := m.viewport.Height

	wantDelta := withMenuInputHeight - withoutMenuInputHeight
	gotDelta := withoutMenuViewportHeight - withMenuViewportHeight
	if gotDelta != wantDelta {
		t.Errorf("renderInput grew by %d rows when the menu opened, but viewport.Height only shrank by %d (without=%d, with=%d)",
			wantDelta, gotDelta, withoutMenuViewportHeight, withMenuViewportHeight)
	}
}

// TestRecalcViewportHeight_ToolConfirmOpen_AccountsForExtraRows is the same check for the HITL confirm menu (ModeToolConfirm), which replaces the hints row with m.hitlList instead of m.cmdList. Baseline mode is ModeBoth, not ModeText: both ModeBoth and ModeToolConfirm carry the same non-zero signalHeight, so toggling between them isolates the delta to the menu term alone instead of also picking up recalcViewportHeight's unrelated signalHeight swing.
func TestRecalcViewportHeight_ToolConfirmOpen_AccountsForExtraRows(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.mode = ModeBoth

	m.recalcViewportHeight()
	withoutMenuInputHeight := lipgloss.Height(m.renderInput())
	withoutMenuViewportHeight := m.viewport.Height

	m.mode = ModeToolConfirm
	m.recalcViewportHeight()
	withMenuInputHeight := lipgloss.Height(m.renderInput())
	withMenuViewportHeight := m.viewport.Height

	wantDelta := withMenuInputHeight - withoutMenuInputHeight
	gotDelta := withoutMenuViewportHeight - withMenuViewportHeight
	if gotDelta != wantDelta {
		t.Errorf("renderInput grew by %d rows when ModeToolConfirm opened, but viewport.Height only shrank by %d (without=%d, with=%d)",
			wantDelta, gotDelta, withoutMenuViewportHeight, withMenuViewportHeight)
	}
}

// TestUpdate_TypingSlash_ShrinksViewportForMenu verifies the viewport actually shrinks as soon as typing "/" opens the command menu — through the real Update() key-handling path, not just a direct recalcViewportHeight() call — since the menu-overflow accounting is only useful if every showCmdList mutation site actually triggers it.
func TestUpdate_TypingSlash_ShrinksViewportForMenu(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.recalcViewportHeight()
	before := m.viewport.Height

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	nm := next.(model)

	if !nm.showCmdList {
		t.Fatal("setup: expected showCmdList to be true after typing \"/\"")
	}
	if nm.viewport.Height >= before {
		t.Errorf("expected the viewport to shrink once the menu opened, before=%d after=%d", before, nm.viewport.Height)
	}
}

// --- throttled viewport flush ---

// TestStreamLine_ThrottledUpdate_SetsDirtyFlag verifies a chunk arriving inside the 80ms throttle window (see streamLine's own throttle comment) marks the viewport dirty instead of just silently skipping the render — otherwise a reply's last chunk can go permanently unrendered if no further chunk ever arrives to trigger the next render.
func TestStreamLine_ThrottledUpdate_SetsDirtyFlag(t *testing.T) {
	m := newTestModel()
	m.lastUpdate = time.Now() // just updated, so the next chunk falls inside the throttle window

	m.streamLine("ora", "partial reply", false)

	if !m.viewportDirty {
		t.Error("expected viewportDirty to be set when the render was throttled")
	}
}

// TestUpdate_TickMsg_FlushesDirtyViewport verifies the existing 50ms tickMsg handler flushes a throttle-skipped render and clears the dirty flag, so a stranded final chunk still reaches the screen.
func TestUpdate_TickMsg_FlushesDirtyViewport(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.viewportDirty = true

	next, _ := m.Update(tickMsg(time.Now()))
	nm := next.(model)

	if nm.viewportDirty {
		t.Error("expected tickMsg to flush the viewport and clear the dirty flag")
	}
}

// --- stale-activity watchdog ---
//
// ToolActivityChan's non-blocking send can drop a Finished event under a burst of concurrent tool calls (see sendToolActivity in connect.go), leaving the spinner stuck forever — this is the automated check for that safety valve.

func TestUpdate_TickMsg_StaleActivity_AutoClearsAfterTimeout(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.activity = &liveStatus{kind: statusTool, id: "call-1", label: "query_memory(...)", started: time.Now().Add(-(staleActivityTimeout + time.Second))}

	next, _ := m.Update(tickMsg(time.Now()))
	nm := next.(model)

	if nm.activity != nil {
		t.Errorf("expected a stale (>%v old) activity to be auto-cleared on tick, got %+v", staleActivityTimeout, nm.activity)
	}
}

func TestUpdate_TickMsg_FreshActivity_SurvivesTick(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.activity = &liveStatus{kind: statusTool, id: "call-1", label: "query_memory(...)", started: time.Now()}

	next, _ := m.Update(tickMsg(time.Now()))
	nm := next.(model)

	if nm.activity == nil {
		t.Error("expected a freshly-started activity to survive a tick, got nil")
	}
}

// --- sticky-bottom scroll ---

// fillScrollableViewport gives the model enough messages and a small enough viewport that scroll position is meaningful, then scrolls away from the bottom.
func fillScrollableViewport(m *model) {
	m.width, m.height = 100, 40
	m.viewport.Height = 5
	for i := 0; i < 50; i++ {
		m.streamLine("system", "line of history content", false)
	}
	m.updateViewport(false)
	m.viewport.SetYOffset(0) // scroll to the top, away from the bottom
}

// TestUpdateViewport_ScrolledAwayFromBottom_NonForcedCallDoesNotYankDown verifies a non-"you" append (e.g. a tool-log line arriving while the user is reading earlier history) does not force the viewport back to the bottom.
func TestUpdateViewport_ScrolledAwayFromBottom_NonForcedCallDoesNotYankDown(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	if m.viewport.AtBottom() {
		t.Fatal("setup: expected to have scrolled away from the bottom")
	}

	m.messages = append(m.messages, Message{Sender: "tool", Content: "another line"})
	m.updateViewport(false)

	if m.viewport.AtBottom() {
		t.Error("expected a non-forced updateViewport to leave scroll position alone while reading history")
	}
}

// TestUpdateViewport_AtBottom_NonForcedCallStaysAtBottom verifies the normal chat-follows-along case: if the user was already at the bottom, new content keeps them glued there even without forcing.
func TestUpdateViewport_AtBottom_NonForcedCallStaysAtBottom(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	m.viewport.GotoBottom()

	m.messages = append(m.messages, Message{Sender: "tool", Content: "another line"})
	m.updateViewport(false)

	if !m.viewport.AtBottom() {
		t.Error("expected to stay at the bottom when already there before new content arrived")
	}
}

// TestUpdateViewport_Forced_JumpsToBottomEvenWhenScrolledAway verifies force=true (a "you" send) always scrolls to the bottom regardless of prior scroll position.
func TestUpdateViewport_Forced_JumpsToBottomEvenWhenScrolledAway(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	if m.viewport.AtBottom() {
		t.Fatal("setup: expected to have scrolled away from the bottom")
	}

	m.messages = append(m.messages, Message{Sender: "you", Content: "hi"})
	m.updateViewport(true)

	if !m.viewport.AtBottom() {
		t.Error("expected a forced updateViewport to jump to the bottom")
	}
}

// TestStreamLine_YouSender_ForcesBottomEvenWhenScrolledAway verifies streamLine itself forces the bottom for "you" sends specifically — it's the only call site that knows the sender directly.
func TestStreamLine_YouSender_ForcesBottomEvenWhenScrolledAway(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	if m.viewport.AtBottom() {
		t.Fatal("setup: expected to have scrolled away from the bottom")
	}

	m.streamLine("you", "hi", false)

	if !m.viewport.AtBottom() {
		t.Error("expected streamLine to force the bottom for a \"you\" send")
	}
}

// --- scrollback keys (WP3 item 2, textarea-empty-gating from X1) ---
//
// Before this, every KeyMsg path returned from Update() before the viewport ever saw the key (see the KeyMsg case's trailing `return m, tiCmd`) — the mouse wheel was the only way to scroll. These route PgUp/PgDn/Ctrl+U/Ctrl+D explicitly using bubbles' own paging methods rather than hand-rolling line math.
// Ctrl+U/Ctrl+D (and Ctrl+E below) only take over when the textarea is empty — bubbles' textarea binds all three to real editing actions (delete-before-cursor, delete-char-forward, line-end) via its own DefaultKeyMap, and stealing them mid-sentence broke composing (X1). PgUp/PgDn stay global since the textarea doesn't bind either.

func TestUpdate_PgDown_ScrollsViewportDown(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	before := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	nm := next.(model)

	if nm.viewport.YOffset <= before {
		t.Errorf("expected PgDown to scroll down, before=%d after=%d", before, nm.viewport.YOffset)
	}
}

func TestUpdate_PgUp_ScrollsViewportUp(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	m.viewport.GotoBottom()
	before := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	nm := next.(model)

	if nm.viewport.YOffset >= before {
		t.Errorf("expected PgUp to scroll up, before=%d after=%d", before, nm.viewport.YOffset)
	}
}

func TestUpdate_CtrlD_HalfPageDown_EmptyTextarea(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	before := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	nm := next.(model)

	if nm.viewport.YOffset <= before {
		t.Errorf("expected Ctrl+D to scroll down, before=%d after=%d", before, nm.viewport.YOffset)
	}
}

func TestUpdate_CtrlU_HalfPageUp_EmptyTextarea(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	m.viewport.GotoBottom()
	before := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	nm := next.(model)

	if nm.viewport.YOffset >= before {
		t.Errorf("expected Ctrl+U to scroll up, before=%d after=%d", before, nm.viewport.YOffset)
	}
}

// TestUpdate_CtrlU_NonEmptyTextarea_DeletesInsteadOfScrolling verifies Ctrl+U with in-progress typed input deletes to line start (bubbles' native DeleteBeforeCursor) instead of scrolling — X1.
func TestUpdate_CtrlU_NonEmptyTextarea_DeletesInsteadOfScrolling(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	m.textarea.SetValue("hello there")
	m.textarea.CursorEnd()
	before := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	nm := next.(model)

	if nm.viewport.YOffset != before {
		t.Errorf("expected Ctrl+U not to scroll while composing, before=%d after=%d", before, nm.viewport.YOffset)
	}
	if nm.textarea.Value() != "" {
		t.Errorf("expected Ctrl+U to delete before the cursor (native textarea behavior), got %q", nm.textarea.Value())
	}
}

// TestUpdate_CtrlD_NonEmptyTextarea_DeletesCharInsteadOfScrolling verifies Ctrl+D with in-progress typed input deletes the character forward (bubbles' native DeleteCharacterForward) instead of scrolling — X1.
func TestUpdate_CtrlD_NonEmptyTextarea_DeletesCharInsteadOfScrolling(t *testing.T) {
	m := newTestModel()
	fillScrollableViewport(&m)
	m.textarea.SetValue("hello")
	m.textarea.CursorStart()
	before := m.viewport.YOffset

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	nm := next.(model)

	if nm.viewport.YOffset != before {
		t.Errorf("expected Ctrl+D not to scroll while composing, before=%d after=%d", before, nm.viewport.YOffset)
	}
	if nm.textarea.Value() != "ello" {
		t.Errorf("expected Ctrl+D to delete the character forward (native textarea behavior), got %q", nm.textarea.Value())
	}
}

// --- foldable thoughts (WP3 item 4) ---

func longThoughtContent() string {
	return strings.Repeat("reasoning about the best approach here in great detail ", 5)
}

// TestRenderMessage_Thought_CollapsedByDefault verifies a thought message renders as one dim line with roughly the first 60 runes plus the "ctrl+e expands" hint, not the full content — by default (m.expandThoughts is the zero value, false).
func TestRenderMessage_Thought_CollapsedByDefault(t *testing.T) {
	m := newTestModel()
	full := longThoughtContent()
	msg := Message{Sender: "ora", IsThought: true, Content: full}

	out := m.renderMessage(msg, 100)

	if strings.Contains(out, full) {
		t.Error("expected the collapsed render to NOT contain the full thought content")
	}
	if !strings.Contains(out, "ctrl+e expands") {
		t.Errorf("expected a ctrl+e expand hint in the collapsed render, got:\n%s", out)
	}
	if !strings.Contains(out, string([]rune(full)[:20])) {
		t.Error("expected the collapsed render to still show a preview of the thought's start")
	}
}

// TestUpdate_CtrlE_TogglesExpandThoughts verifies Ctrl+E flips the global fold state, and flips back on a second press. Textarea is empty (the zero value default) — this is the fold-toggle case; see TestUpdate_CtrlE_NonEmptyTextarea_MovesToLineEndInsteadOfToggling for the composing case (X1).
func TestUpdate_CtrlE_TogglesExpandThoughts(t *testing.T) {
	m := newTestModel()
	if m.expandThoughts {
		t.Fatal("setup: expected expandThoughts to start false")
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	nm := next.(model)
	if !nm.expandThoughts {
		t.Fatal("expected the first Ctrl+E to set expandThoughts true")
	}

	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	nm2 := next2.(model)
	if nm2.expandThoughts {
		t.Error("expected the second Ctrl+E to toggle expandThoughts back to false")
	}
}

// TestUpdate_CtrlE_NonEmptyTextarea_MovesToLineEndInsteadOfToggling verifies Ctrl+E with in-progress typed input moves the cursor to line end (bubbles' native LineEnd) instead of toggling the thought fold — X1.
func TestUpdate_CtrlE_NonEmptyTextarea_MovesToLineEndInsteadOfToggling(t *testing.T) {
	m := newTestModel()
	m.textarea.SetValue("hello there")
	m.textarea.CursorStart()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	nm := next.(model)

	if nm.expandThoughts {
		t.Error("expected Ctrl+E not to toggle the thought fold while composing")
	}
	if nm.textarea.Value() != "hello there" {
		t.Errorf("expected the textarea content to be untouched by line-end movement, got %q", nm.textarea.Value())
	}
}

// TestRenderMessage_Thought_WhenExpanded_ShowsFullContent verifies toggling expandThoughts on renders the thought's full content instead of the collapsed preview.
func TestRenderMessage_Thought_WhenExpanded_ShowsFullContent(t *testing.T) {
	m := newTestModel()
	m.expandThoughts = true
	full := longThoughtContent()
	msg := Message{Sender: "ora", IsThought: true, Content: full}

	out := m.renderMessage(msg, 200)

	if !strings.Contains(strings.ReplaceAll(out, "\n", ""), strings.TrimSpace(full)[:40]) {
		t.Errorf("expected the expanded render to contain the full thought content, got:\n%s", out)
	}
	if strings.Contains(out, "ctrl+e expands") {
		t.Error("expected no fold hint once expanded")
	}
}

// TestRenderMessage_NonThoughtMessage_UnaffectedByExpandThoughts verifies a normal (non-thought) ora message's rendering doesn't depend on expandThoughts at all.
func TestRenderMessage_NonThoughtMessage_UnaffectedByExpandThoughts(t *testing.T) {
	m := newTestModel()
	msg := Message{Sender: "ora", IsThought: false, Content: "the final answer is 42"}

	collapsed := m.renderMessage(msg, 100)
	m.expandThoughts = true
	expanded := m.renderMessage(msg, 100)

	if collapsed != expanded {
		t.Errorf("expected a non-thought message to render identically regardless of expandThoughts, got:\ncollapsed=%q\nexpanded=%q", collapsed, expanded)
	}
}

// --- rendering smoke tests ---
//
// Real terminal rendering isn't unit-tested here (see waveform_test.go's TestWaveform_VisualDemo, which just prints for human inspection) — but View() panicking is a real regression, and driving bubbletea interactively needs a real TTY/daemon/API key this environment doesn't have. These tests just prove each new rendering path doesn't panic and the expected text shows up.

func TestView_WithToolActivity_DoesNotPanicAndShowsLabel(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)

	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolStarted})
	m = started.(model)

	out := m.View()
	if !strings.Contains(out, `query_memory("Riddler")`) {
		t.Errorf("expected the live status line's label in the rendered output, got:\n%s", out)
	}
}

func TestView_AfterToolFinishes_ShowsTranscriptLogLine(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)

	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolStarted})
	m = started.(model)
	finished, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolFinished, ResultSummary: "3 hits"})
	m = finished.(model)

	out := m.View()
	if !strings.Contains(out, "3 hits") {
		t.Errorf("expected the collapsed transcript entry's result in the rendered output, got:\n%s", out)
	}
}

func TestView_WithThinkingStatus_DoesNotPanic(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)
	m.textarea.SetValue("what did I do yesterday")

	sent, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = sent.(model)

	out := m.View()
	if !strings.Contains(out, "thinking") {
		t.Errorf("expected the thinking status label in the rendered output, got:\n%s", out)
	}
}

func TestView_NoActivity_OmitsStatusLineEntirely(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)

	if got := m.renderStatusLine(); got != "" {
		t.Errorf("expected renderStatusLine to return \"\" when nothing is active, got %q", got)
	}
}

// --- contextual shortcut bar (WP3 item 1) ---

// TestHintsText_ReflectsState is table-driven over every state the hint bar must distinguish: normal, slash menu open, tool confirm, tool edit, and quit-confirm-armed (which overrides all the others, since Ctrl+C works from every mode).
func TestHintsText_ReflectsState(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*model)
		want  string
	}{
		{"normal", func(m *model) {}, "↵ send · ctrl+j newline · / commands · esc clear · pgup/pgdn scroll"},
		{"slash menu open", func(m *model) { m.showCmdList = true }, "↑↓ navigate · ↵ select · esc close"},
		{"tool confirm", func(m *model) { m.mode = ModeToolConfirm }, "↑↓ choose · ↵ confirm · esc reject"},
		{"tool edit", func(m *model) { m.mode = ModeToolEdit }, "↵ run · esc cancel"},
		{"quit armed overrides menu state", func(m *model) {
			m.quitConfirmArmed = true
			m.showCmdList = true
		}, "press ctrl+c again to quit"},
		{"quit armed overrides tool confirm", func(m *model) {
			m.quitConfirmArmed = true
			m.mode = ModeToolConfirm
		}, "press ctrl+c again to quit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()
			tc.setup(&m)
			got := m.hintsText()
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected hintsText() to contain %q, got %q", tc.want, got)
			}
		})
	}
}

// TestRenderInput_NormalState_StillShowsModeChip verifies the normal-state hint row keeps the mode chip at the end — the ram chip was deleted in WP6 (ReadMemStats sampling caused a periodic GC stop-the-world for cosmetic dev trivia).
func TestRenderInput_NormalState_StillShowsModeChip(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40

	out := m.renderInput()

	if !strings.Contains(out, "mode") {
		t.Errorf("expected the normal-state input row to still show the mode chip, got:\n%s", out)
	}
}

// TestRenderInput_DegradedStateChips_OnlyShowWhenUnhealthy verifies the daemon/connection warning chips WP8 moved off the deleted header row into the hint bar are quiet when healthy and appear only in their own degraded state — the header used to show "⊙ tracking"/"● live" pills unconditionally; the replacement only speaks up when there's something to warn about.
func TestRenderInput_DegradedStateChips_OnlyShowWhenUnhealthy(t *testing.T) {
	cases := []struct {
		name        string
		daemonOK    bool
		isConnected bool
		wantTracker bool
		wantConn    bool
	}{
		{"both healthy: no chips", true, true, false, false},
		{"daemon down: tracker chip only", false, true, true, false},
		{"disconnected: reconnecting chip only", true, false, false, true},
		{"both down: both chips", false, false, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()
			m.width, m.height = 100, 40
			m.daemonOK = tc.daemonOK
			m.isConnected = tc.isConnected

			out := m.renderInput()

			if gotTracker := strings.Contains(out, "no tracker"); gotTracker != tc.wantTracker {
				t.Errorf("no-tracker chip presence = %v, want %v, got:\n%s", gotTracker, tc.wantTracker, out)
			}
			if gotConn := strings.Contains(out, "reconnecting"); gotConn != tc.wantConn {
				t.Errorf("reconnecting chip presence = %v, want %v, got:\n%s", gotConn, tc.wantConn, out)
			}
		})
	}
}

// TestView_HeaderRowRemoved_NoTrackingOrLivePillText is WP8's deletion pin: the header row (logo + "⊙ tracking"/"● live" pills) is gone entirely, not just hidden — those exact strings must never appear in View() output again, healthy or not (the replacement chips in renderInput use different text and only appear when unhealthy).
func TestView_HeaderRowRemoved_NoTrackingOrLivePillText(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)

	out := m.View()
	if strings.Contains(out, "⊙ tracking") {
		t.Error("expected the deleted header's \"⊙ tracking\" pill text to be gone, found it in View() output")
	}
	if strings.Contains(out, "● live") {
		t.Error("expected the deleted header's \"● live\" pill text to be gone, found it in View() output")
	}
}

// malayalamRepro is the exact stair-step repro string from the WP5 bug report — heavy in conjuncts/combining marks, the class of glyph where cell-width math (go-runewidth, under lipgloss) can disagree with a real terminal's rendering.
const malayalamRepro = "batches?ണ്ടില്ല, ചതിവുകളെ ഇവിടെയുണ്ട്. ഏതെങ്കിലും കണ്ടുകൂടാ ശരി"

// TestRenderMessage_MalayalamContent_EveryRenderedLineFitsWidth is the WP5 bug-2a rendering diagnostic: feed renderMessage the Malayalam repro string at a fixed width and assert every wrapped line's lipgloss.Width is exactly that width (row background fully painted, no gap a real terminal would show through) and never over it. lipgloss wraps and measures with the same internal cell-width function, so a failure here would mean lipgloss disagrees with ITSELF, not just with a real terminal's font rendering (a lipgloss-vs-terminal disagreement can't be observed headlessly at all — see this test's sibling in the WP5 report for that caveat).
func TestRenderMessage_MalayalamContent_EveryRenderedLineFitsWidth(t *testing.T) {
	m := newTestModel()
	width := 60

	rendered := m.renderMessage(Message{Sender: "ora", Content: malayalamRepro}, width)

	for i, line := range strings.Split(rendered, "\n") {
		if w := lipgloss.Width(line); w != width {
			t.Errorf("line %d %q rendered width %d, want exactly %d (background padding)", i, line, w, width)
		}
	}
}

// TestRenderMessage_ContentWrapsWithSafetyMargin_BelowFullGutterWidth is the WP5 bug-2a defensive fix: content is word-wrapped a couple of cells narrower than the full width-minus-gutter column, not exactly at it, so a minor cell-width disagreement between lipgloss/go-runewidth and a real terminal's font (the class of thing wide Malayalam conjuncts/combining marks trigger, and which can't be proven or disproven headlessly — see TestRenderMessage_MalayalamContent_EveryRenderedLineFitsWidth's own doc comment) doesn't overflow the physical line. An ASCII string exactly filling the un-margined width would fit on one line with no margin; with the margin it must wrap onto two.
func TestRenderMessage_ContentWrapsWithSafetyMargin_BelowFullGutterWidth(t *testing.T) {
	m := newTestModel()
	width := 40
	content := strings.Repeat("a", width-GutterWidth)

	rendered := m.renderMessage(Message{Sender: "ora", Content: content}, width)
	lines := strings.Split(rendered, "\n")

	if len(lines) < 2 {
		t.Fatalf("expected the safety margin to force a wrap before the full gutter-width column, got %d line(s): %+v", len(lines), lines)
	}
}

// noBackgroundCode matches any RGB background-setting SGR sequence (lipgloss's Background() always encodes as 48;2;R;G;B in a forced true-color profile).
var noBackgroundCode = regexp.MustCompile(`48;2;`)

// TestRenderMessage_OraMessage_HasNoBackgroundCode is WP10 Part B's root fix: the transcript used to paint an explicit background on every row/prefix/separator to fight terminal bleed-through, which was itself the leak's cause — any span that missed one of those Background() calls (there were many, spread across renderMessage/updateViewport/styles.go) showed the terminal's own default instead, patchy and permanent. The fix kills the approach: no message-row background at all, so the terminal's own background IS the background, like every native TUI. Forces a true-color profile since color codes are stripped entirely under go test's non-tty stdout.
func TestRenderMessage_OraMessage_HasNoBackgroundCode(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	m := newTestModel()
	out := m.renderMessage(Message{Sender: "ora", Content: "Hello there."}, 60)

	if noBackgroundCode.MatchString(out) {
		t.Errorf("expected no background ANSI code in a rendered ora message, got %q", out)
	}
}

// TestUpdateViewport_SeparatorHasNoBackgroundCode verifies the blank-line separator between messages (updateViewport) carries no background either — it used to be explicitly styled to match the viewport's background so the gap wouldn't show terminal-default color through; now there's nothing to match, the gap is just blank.
func TestUpdateViewport_SeparatorHasNoBackgroundCode(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	m := newTestModel()
	m.viewport.Width = 60
	m.messages = []Message{{Sender: "you", Content: "hi"}, {Sender: "ora", Content: "hello"}}
	m.updateViewport(false)

	if noBackgroundCode.MatchString(m.viewport.View()) {
		t.Errorf("expected no background ANSI code anywhere in the viewport content, got %q", m.viewport.View())
	}
}

// TestRenderMessage_TrailingNewlines_TrimmedAtRenderTime is WP10 Part B item 2: message content ending in blank lines (thought parts commonly end with a trailing newline) used to render as visible empty bordered rows inside the thought box. Trimmed only at render time — streamLine's own accumulation keeps the raw content untouched, this only affects the styled output.
func TestRenderMessage_TrailingNewlines_TrimmedAtRenderTime(t *testing.T) {
	m := newTestModel()
	withTrailing := m.renderMessage(Message{Sender: "ora", Content: "Hello there.\n\n\n"}, 60)
	without := m.renderMessage(Message{Sender: "ora", Content: "Hello there."}, 60)

	if got, want := lipgloss.Height(withTrailing), lipgloss.Height(without); got != want {
		t.Errorf("expected trailing newlines trimmed at render time (height %d, matching no-trailing-newline content), got height %d", want, got)
	}
}

// TestRenderInput_MultiRowTextarea_PromptAlignsTop is the WP10 addendum's ❯-alignment fix: JoinHorizontal(lipgloss.Center, ...) vertically centered the ❯ prompt against the textarea block, so on a multi-line textarea (Ctrl+J newlines, or wrapped long input) the prompt floated away from the cursor line instead of hugging the first input row — part of the "scrambled input deck" the user reported. Top alignment keeps ❯ pinned to row one regardless of textarea height.
func TestRenderInput_MultiRowTextarea_PromptAlignsTop(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 100, 40
	m.textarea.SetHeight(3)

	out := m.renderInput()
	lines := strings.Split(out, "\n")

	// Skip InputWrap's own leading vertical-padding row (Padding(1,4), unrelated to this fix) to find the input row block's actual first content line.
	firstContent := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			firstContent = i
			break
		}
	}
	if firstContent == -1 {
		t.Fatal("expected at least one non-blank line in renderInput() output")
	}
	if !strings.Contains(lines[firstContent], "❯") {
		t.Errorf("expected the input deck's first content row to carry the prompt (top-aligned against a 3-row textarea), got %q", lines[firstContent])
	}
}

// TestCommandDelegate_Render_NeverWrapsPastDeclaredHeight is a WP7 finding: commandDelegate.Height() declares 1 row per item, but Render's description text had no width bound, so a long description (e.g. hitlItems' "Execute this command and return the result") word-wrapped to 2 physical lines despite the declared height — breaking SetHeight(len(items))'s assumption that N items need exactly N rows.
func TestCommandDelegate_Render_NeverWrapsPastDeclaredHeight(t *testing.T) {
	s := DefaultStyles()
	l := newHitlList(s)
	items := hitlItems(true)
	l.SetItems(items)
	d := commandDelegate{styles: s}

	for i, item := range items {
		var buf bytes.Buffer
		d.Render(&buf, l, i, item)
		if got := lipgloss.Height(buf.String()); got != d.Height() {
			t.Errorf("item %d (%q) rendered %d lines, want exactly delegate.Height()=%d", i, item.(commandItem).title, got, d.Height())
		}
	}
}

// noUnbackgroundedGap matches a bare ANSI reset (\x1b[0m) immediately followed by a printable byte with no fresh SGR in between — a nested lipgloss Render's own reset code lands mid-row and everything after it shows the terminal's own default background (black in most terminals) instead of the row's, until the next styled span re-arms it. A reset at the very end of the row (nothing after it) never matches, since there's no unbackgrounded content left to paint.
var noUnbackgroundedGap = regexp.MustCompile("\x1b\\[0m[^\x1b]")

// TestCommandDelegate_Render_RowBackgroundIsContinuous is WP9 item 1: commandDelegate.Render's inner title/desc styles carried no explicit Background, so their own ANSI reset (emitted by every lipgloss Render call) cut the outer row's background short wherever it landed — the "patchy" leak. Forces a true-color profile since color codes are stripped entirely under go test's non-tty stdout, which would make this pass vacuously.
func TestCommandDelegate_Render_RowBackgroundIsContinuous(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	s := DefaultStyles()
	l := newCommandList(s)
	l.SetItems(commandItems)
	d := commandDelegate{styles: s}

	for i, item := range commandItems {
		var buf bytes.Buffer
		d.Render(&buf, l, 999, item) // 999: never the cursor index, exercises the normal (unselected) branch
		if noUnbackgroundedGap.MatchString(buf.String()) {
			t.Errorf("normal row %d (%q) has an unbackgrounded gap after an inner reset: %q", i, item.(commandItem).title, buf.String())
		}
	}

	var buf bytes.Buffer
	d.Render(&buf, l, 0, commandItems[0]) // index == cursor: highlighted branch
	if noUnbackgroundedGap.MatchString(buf.String()) {
		t.Errorf("highlighted row has an unbackgrounded gap after an inner reset: %q", buf.String())
	}
}

// TestNewCommandList_HeightMatchesUnfilteredItemCount is WP9 item 2: cmdList used to construct at the fixed DefaultListHeight=8 regardless of its 12-item unfiltered set, forcing bubbles to paginate. It must size to the real item count from construction.
func TestNewCommandList_HeightMatchesUnfilteredItemCount(t *testing.T) {
	l := newCommandList(DefaultStyles())
	if got, want := l.Height(), len(commandItems); got != want {
		t.Errorf("newCommandList height = %d, want %d (len(commandItems))", got, want)
	}
}

// TestFilterCommands_SetsHeightToFilteredCount verifies FilterCommands resizes the list to the filtered set's length every time, not just the unfiltered construction height — a narrower filter must shrink the deck, not leave it padded with blank rows.
func TestFilterCommands_SetsHeightToFilteredCount(t *testing.T) {
	s := DefaultStyles()
	l := newCommandList(s)

	FilterCommands(&l, "voice")

	wantCount := 0
	for _, item := range commandItems {
		if strings.Contains(item.(commandItem).title, "voice") {
			wantCount++
		}
	}
	if got := l.Height(); got != wantCount {
		t.Errorf("FilterCommands height = %d, want %d (filtered item count)", got, wantCount)
	}
}

// TestCommandListAndHitlList_ViewHasNoPaginationDotsOrFillerRows is WP9 items 2+3: with height sized to the real item count, bubbles must never paginate (no "•"/"○" dot row) or pad with blank filler rows to reach a taller fixed height — the list's rendered height should equal exactly its item count.
func TestCommandListAndHitlList_ViewHasNoPaginationDotsOrFillerRows(t *testing.T) {
	s := DefaultStyles()

	cmd := newCommandList(s)
	cmd.SetWidth(100)
	if out := cmd.View(); strings.ContainsAny(out, "•○") {
		t.Errorf("cmdList.View() contains a pagination dot, want none:\n%s", out)
	} else if got, want := lipgloss.Height(out), len(commandItems); got != want {
		t.Errorf("cmdList.View() height = %d, want %d (no filler rows)", got, want)
	}

	hitl := newHitlList(s)
	hitl.SetWidth(100)
	items := hitlItems(true)
	hitl.SetItems(items)
	hitl.SetHeight(len(items))
	if out := hitl.View(); strings.ContainsAny(out, "•○") {
		t.Errorf("hitlList.View() contains a pagination dot, want none:\n%s", out)
	} else if got, want := lipgloss.Height(out), len(items); got != want {
		t.Errorf("hitlList.View() height = %d, want %d (no filler rows)", got, want)
	}
}

// TestUpdate_WindowSizeMsg_ListsWidthedToRealDeckWidth is WP9 item 1's other half: cmdList/hitlList used to stay pinned at the fixed DefaultListWidth=60 forever, so a row's background band stopped mid-screen on any wider terminal even after the per-row Background fix above. They must track the actual input-deck content width (terminal width minus InputWrap's own horizontal padding) on every resize.
func TestUpdate_WindowSizeMsg_ListsWidthedToRealDeckWidth(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	nm := next.(model)

	wantWidth := 200 - nm.styles.InputWrap.GetHorizontalPadding()
	if got := nm.cmdList.Width(); got != wantWidth {
		t.Errorf("cmdList.Width() = %d, want %d (terminal width minus InputWrap's horizontal padding)", got, wantWidth)
	}
	if got := nm.hitlList.Width(); got != wantWidth {
		t.Errorf("hitlList.Width() = %d, want %d", got, wantWidth)
	}
}

// TestView_RenderedHeightMatchesModelHeight is the WP5 bug-2b fill/height diagnostic, table-driven per R-WP5-1: after a real WindowSizeMsg, View()'s total rendered height must stay exactly m.height across every mode/menu combination, not just the default one. The ModeBoth+multiline case is the original structural viewport-height accounting bug (see recalcViewportHeight) — content-independent (reproduces with plain ASCII, no wide glyphs needed). The ModeText case is R-WP5-1: renderSignalField() returns "" in ModeText, but View()'s JoinVertical still counted that empty string as a blank row, a 1-row dose of the same desync class. The menu-open cases at 30 AND 24 rows are WP10: the invariant is View() height == m.height with NO exceptions, even under floor pressure — a menu open lets the viewport shrink to 0 (not just MinViewportHeight) and, as a last resort, shrinks the open list itself so the deck always fits (see recalcViewportHeight). lipgloss.Place(m.width, m.height, ...) is the outermost wrapper and does NOT crop oversized content, so any internal component rendering taller than its accounted-for height pushes the input deck past the bottom of a real terminal exactly like the WP5 screenshot (input row, hints, kbd chips all gone).
func TestView_RenderedHeightMatchesModelHeight(t *testing.T) {
	openToolConfirm := func(m *model) {
		next, _ := m.Update(agent.ToolRequest{Description: "shell: ls -la", ResultChan: make(chan string, 1)})
		*m = next.(model)
	}
	openSlashMenu := func(m *model) {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
		*m = next.(model)
	}

	cases := []struct {
		name       string
		mutate     func(m *model)
		content    bool
		termHeight int
	}{
		{"ModeBoth with multiline ora content", func(m *model) { m.mode = ModeBoth }, true, 30},
		{"ModeText", func(m *model) { m.mode = ModeText }, false, 30},
		// Drives the real agent.ToolRequest Update() path (not a direct m.mode flip) so the hitlList actually gets sized by that case's SetHeight(len(items)) call (WP7).
		{"ModeToolConfirm with menu open at 30 rows", openToolConfirm, false, 30},
		{"ModeToolConfirm with menu open at 24 rows", openToolConfirm, false, 24},
		// Drives the real "/" keypress so cmdList opens through FilterCommands (not a direct showCmdList flip).
		{"ModeBoth with slash menu open at 30 rows", openSlashMenu, false, 30},
		{"ModeBoth with slash menu open at 24 rows", openSlashMenu, false, 24},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: tc.termHeight})
			m = next.(model)
			tc.mutate(&m)
			m.recalcViewportHeight()

			if tc.content {
				long := strings.Repeat("this is a filler reply line that repeats\n", 11)
				m.streamLine("ora", long, false)
			}

			out := m.View()
			if h := lipgloss.Height(out); h != m.height {
				t.Errorf("View() rendered height %d, want exactly m.height %d", h, m.height)
			}
			if !strings.Contains(out, "❯") {
				t.Error("expected the input prompt to still be present in View() output")
			}
		})
	}
}

// TestView_ShortTerminal_NoMenuOpen_InputPromptStaysVisible is FINDING 8: recalcViewportHeight only relaxes
// the viewport floor to 0 when a menu is open, so with no menu open on a short terminal the viewport stays
// clamped at MinViewportHeight no matter how negative the available space goes — pushing the ❯ input prompt
// off the bottom. This got easier to hit because renderInput adds a status-line row whenever a tool call or
// the thinking spinner is active, plus "⚠ no tracker" / "⚠ reconnecting" warning chips, on top of the
// ordinary input row and hint row. No menu is open here (mode stays ModeBoth) — only that extra chrome.
func TestView_ShortTerminal_NoMenuOpen_InputPromptStaysVisible(t *testing.T) {
	m := newTestModel()
	// 24 rows is the exact height where the baseline chrome (no live status, no warning chips) already
	// fits with nothing to spare — turning on the extra status-line row below is what tips it over.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = next.(model)

	m.activity = &liveStatus{kind: statusTool, label: `shell_exec("ls -la")`}
	m.daemonOK = false
	m.isConnected = false
	m.recalcViewportHeight()

	out := m.View()
	if h := lipgloss.Height(out); h != m.height {
		t.Errorf("View() rendered height %d, want exactly m.height %d", h, m.height)
	}
	if !strings.Contains(out, "❯") {
		t.Error("expected the input prompt to still be present in View() output on a short terminal with no menu open")
	}
}

// TestUpdate_SlashMenuOpen_NoBlankPaddingRowsBeforeFirstItem is WP9 item 3: with the list's fixed DefaultListHeight=8 against fewer visible items, bubbles padded the gap with blank rows between the hint line and the first real menu item — visible in the screenshot as a dead black strip. Dynamic height (item 2) should eliminate it: the line directly below the menu-open hint row must already be the first real menu item, not blank padding.
func TestUpdate_SlashMenuOpen_NoBlankPaddingRowsBeforeFirstItem(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	nm := next.(model)

	lines := strings.Split(nm.renderInput(), "\n")
	hintIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "navigate") { // hintsText()'s showCmdList text: "↑↓ navigate · ↵ select · esc close"
			hintIdx = i
			break
		}
	}
	if hintIdx == -1 || hintIdx+1 >= len(lines) {
		t.Fatalf("setup: expected to find the menu-open hint row with at least one line after it, got:\n%s", nm.renderInput())
	}
	if got := lines[hintIdx+1]; !strings.Contains(got, "voice") {
		t.Errorf("expected the line directly after the hint row to be the first menu item (\"voice\"), got %q", got)
	}
}

// TestUpdate_WindowSizeMsg_InputRowRendersAtFullWidth is the WP5 bug-3 diagnostic, kept as a permanent regression guard: after a WindowSizeMsg, every row of the input deck (background included) must span the terminal's full width, not some narrower fraction of it. This traced clean at the code level (bubbles textarea.SetWidth's reservedOuter/reservedInner are both zero here — no Prompt text, no border, no line numbers — so inputWidth passes straight through), which points away from a width-computation bug and toward the same viewport-height overflow TestView_RenderedHeightMatchesModelHeight caught: bubbletea's diff-based terminal renderer can desync once fed a View() taller than the screen height it's tracking, which would show up as exactly this kind of partial/stale-looking redraw rather than a genuine narrow textarea.
func TestUpdate_WindowSizeMsg_InputRowRendersAtFullWidth(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	nm := next.(model)

	lines := strings.Split(nm.renderInput(), "\n")
	for i, line := range lines {
		if w := lipgloss.Width(line); w != nm.width {
			t.Errorf("input deck line %d rendered width %d, want exactly m.width %d", i, w, nm.width)
		}
	}
}

// selectHITLItem points the approval menu at the entry titled want, so a test doesn't depend on menu ordering.
func selectHITLItem(t *testing.T, m *model, want string) {
	t.Helper()
	for i, it := range m.hitlList.Items() {
		if ci, ok := it.(commandItem); ok && ci.title == want {
			m.hitlList.Select(i)
			return
		}
	}
	t.Fatalf("no %q entry in the approval menu", want)
}

// openSuggestChanges drives the real flow a user takes to reach the edit prompt: a shell-backed tool request arrives, the approval menu opens, and "Suggest changes" is chosen. Returns the model in ModeToolEdit plus the channel the request's result is delivered on.
// Setting mode/activeToolReq by hand instead would skip the menu step that clears activeToolReq, which is the bug these tests exist for.
func openSuggestChanges(t *testing.T, cmd string) (model, chan string) {
	t.Helper()
	m := newTestModel()
	res := make(chan string, 1)
	req := agent.ToolRequest{
		Description:     "shell: " + cmd,
		Execute:         func() string { return "unedited ran" },
		ResultChan:      res,
		EditableCommand: cmd,
	}

	next, _ := m.Update(req)
	m = next.(model)
	if m.mode != ModeToolConfirm {
		t.Fatalf("expected ModeToolConfirm after a tool request, got %q", m.mode)
	}
	selectHITLItem(t, &m, "Suggest changes")

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.mode != ModeToolEdit {
		t.Fatalf("expected ModeToolEdit after Suggest changes, got %q", m.mode)
	}
	return m, res
}

// TestUpdate_SuggestChanges_KeepsActiveToolReq verifies "Suggest changes" does not clear activeToolReq. Unlike the other three menu entries it is not terminal — it hands off to ModeToolEdit, which still needs the request to deliver a result on.
func TestUpdate_SuggestChanges_KeepsActiveToolReq(t *testing.T) {
	m, _ := openSuggestChanges(t, "echo ora-test")

	if m.activeToolReq == nil {
		t.Fatal("activeToolReq was cleared by Suggest changes — the edit handler needs it to deliver a result")
	}
}

// TestUpdate_SuggestChanges_PrefillsTextarea verifies the textarea is seeded with the original command so the user edits it rather than retyping it.
func TestUpdate_SuggestChanges_PrefillsTextarea(t *testing.T) {
	m, _ := openSuggestChanges(t, "echo ora-test")

	if got := m.textarea.Value(); got != "echo ora-test" {
		t.Errorf("expected the textarea prefilled with the original command, got %q", got)
	}
}

// TestUpdate_SuggestChanges_Enter_RunsEditedCommand verifies submitting an edited command runs that command and delivers its output, rather than panicking on a nil activeToolReq.
func TestUpdate_SuggestChanges_Enter_RunsEditedCommand(t *testing.T) {
	m, res := openSuggestChanges(t, "echo ora-original")
	m.textarea.SetValue("echo ora-edited")

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case got := <-res:
		if !strings.Contains(got, "ora-edited") {
			t.Errorf("expected the edited command's output on ResultChan, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the edited command's result")
	}
}

// TestUpdate_SuggestChanges_Esc_Rejects verifies cancelling an edit rejects the request, rather than panicking on a nil activeToolReq.
func TestUpdate_SuggestChanges_Esc_Rejects(t *testing.T) {
	m, res := openSuggestChanges(t, "echo ora-test")

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	select {
	case got := <-res:
		if !strings.Contains(got, "rejected") {
			t.Errorf("expected a rejection on ResultChan, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the rejection")
	}
}

// TestStreamLine_OraChunkAfterInterleavedVoiceLine_MergesInPlace is bug 3: an ambient voice transcript (or any system notice) landing between two chunks of the same ora answer used to break the merge chain, so the partial text and the fuller text stacked as two separate transcript entries — "मैं पैसे नहीं" followed by "मैं पैसे नहीं दे सकता, लेकिन...". The answer must keep updating its own block no matter what else arrives alongside it.
func TestStreamLine_OraChunkAfterInterleavedVoiceLine_MergesInPlace(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "मैं पैसे नहीं", false)
	oraIdx := len(m.messages) - 1
	startBefore := len(m.messages)

	m.streamLine("you", "[noise]", false)
	m.streamLine("ora", "मैं पैसे नहीं दे सकता, लेकिन...", false)

	if len(m.messages) != startBefore+1 {
		t.Fatalf("expected only the voice line to be new, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[oraIdx].Content; got != "मैं पैसे नहीं दे सकता, लेकिन..." {
		t.Errorf("expected the ora block updated in place, got %q", got)
	}
}

// TestStreamLine_OraChunkAfterToolLogLine_MergesInPlace is the same defect on the tool path: a tool-log entry appended mid-answer must not split the answer into two blocks either.
func TestStreamLine_OraChunkAfterToolLogLine_MergesInPlace(t *testing.T) {
	m := newTestModel()
	m.streamLine("ora", "let me check", false)
	oraIdx := len(m.messages) - 1
	m.messages = append(m.messages, Message{Sender: "tool", Content: "recall(...) → 12 lines", IsToolLog: true})
	startBefore := len(m.messages)

	m.streamLine("ora", " — you were on the spreadsheet.", false)

	if len(m.messages) != startBefore {
		t.Fatalf("expected no new message, got %d new: %+v", len(m.messages)-startBefore, m.messages)
	}
	if got := m.messages[oraIdx].Content; got != "let me check — you were on the spreadsheet." {
		t.Errorf("expected the ora block updated in place, got %q", got)
	}
}

// mutedTestMic is an audio.Microphone whose amplitude is always loud, so a test can tell "the UI stopped reading the mic" apart from "the room went quiet".
type mutedTestMic struct{}

func (mutedTestMic) StartCapture(ctx context.Context) (<-chan []byte, error) { return nil, nil }
func (mutedTestMic) CurrentAmplitude() float64                               { return 1.0 }
func (mutedTestMic) Close() error                                            { return nil }

// TestUpdate_Tick_MutedMic_FlatlinesWaveform is bug 1's visible half: /text (and /mute) stop the mic being listened to, so the waveform must sit flat instead of dancing to a room the agent is no longer hearing.
func TestUpdate_Tick_MutedMic_FlatlinesWaveform(t *testing.T) {
	a := agent.NewAgent(mutedTestMic{}, nil, nil, nil, "")
	m := NewModel(a, "connected", "")

	next, _ := m.Update(tickMsg(time.Now()))
	m = next.(model)
	if m.micWave.smoothed == 0 {
		t.Fatal("expected the unmuted mic to drive the waveform")
	}

	a.SetMute(true)
	for i := 0; i < 40; i++ {
		next, _ = m.Update(tickMsg(time.Now()))
		m = next.(model)
	}
	if m.micWave.smoothed != 0 {
		t.Errorf("expected a muted mic to flatline the waveform, got %v", m.micWave.smoothed)
	}
}

// TestExecuteCommand_Text_MutesMic verifies /text actually stops the mic at the agent, not just the mode chip in the footer — the whole point of text-only mode.
func TestExecuteCommand_Text_MutesMic(t *testing.T) {
	m := newTestModel()

	m.executeCommand("/text")
	if !m.agent.IsMuted() {
		t.Error("expected /text to mute the mic")
	}

	m.executeCommand("/both")
	if m.agent.IsMuted() {
		t.Error("expected /both to unmute the mic")
	}
}

// TestUpdate_ToolConfirmResolved_RestoresPriorMode verifies approving a tool call returns to whatever mode the user was actually in. Forcing ModeBoth here silently left a text-only session showing "both" in the footer, and made the next /mute toggle the mic ON instead of off.
func TestUpdate_ToolConfirmResolved_RestoresPriorMode(t *testing.T) {
	m := newTestModel()
	m.executeCommand("/text")

	resCh := make(chan string, 1)
	next, _ := m.Update(agent.ToolRequest{Description: "shell: ls", Execute: func() string { return "ok" }, ResultChan: resCh})
	m = next.(model)
	if m.mode != ModeToolConfirm {
		t.Fatalf("expected ModeToolConfirm, got %v", m.mode)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.mode != ModeText {
		t.Errorf("expected the prior mode restored, got %v", m.mode)
	}
}
