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

	"github.com/charmbracelet/bubbles/list"
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

	// The common case — daemon and client match — injects no warning at all.
	for _, msg := range newTestModel().messages {
		if strings.Contains(msg.Content, "older build") {
			t.Errorf("expected no build-mismatch warning when buildMismatch is empty, got: %q", msg.Content)
		}
	}
}

// --- daemon status polling ---

// TestPollDaemonHTTP is a table over pollDaemonHTTP: only a 200 from the daemon counts as up (any
// other status, and an unreachable daemon, read as down), and the poll carries the daemon's IPC
// auth token — /status now requires it like every other daemon IPC endpoint except /ping (see
// internal/ipctoken).
func TestPollDaemonHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		close  bool
		token  bool
		want   bool
	}{
		{name: "200 OK", status: http.StatusOK, want: true},
		{name: "500", status: http.StatusInternalServerError, want: false},
		{name: "unreachable", status: http.StatusOK, close: true, want: false},
		{name: "attaches token", status: http.StatusOK, token: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenPath, wantToken string
			var gotToken string
			if tc.token {
				tokenPath = filepath.Join(t.TempDir(), "ipc-token")
				tok, err := ipctoken.Generate(tokenPath)
				if err != nil {
					t.Fatalf("ipctoken.Generate: %v", err)
				}
				wantToken = tok
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotToken = r.Header.Get(ipctoken.HeaderName)
				w.WriteHeader(tc.status)
			}))
			if tc.close {
				srv.Close()
			} else {
				defer srv.Close()
			}
			if got := pollDaemonHTTP(http.DefaultClient, srv.URL, tokenPath); got != tc.want {
				t.Errorf("pollDaemonHTTP = %v, want %v", got, tc.want)
			}
			if tc.token && gotToken != wantToken {
				t.Errorf("expected the %s header to carry %q, got %q", ipctoken.HeaderName, wantToken, gotToken)
			}
		})
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

// --- tool-activity / thinking status ---

// TestUpdate_ToolActivity is a table over agent.ToolActivity's effect on Update(), one row per
// property: Started sets the live status (kind, id, label) and arms the spinner; Finished appends
// a transcript line and reverts to statusThinking, marks it failed only when Err is true, does not
// clobber a newer call's still-active live status when an older call finishes late, and renders
// (or omits) the elapsed-time suffix depending on whether a start time was given.
func TestUpdate_ToolActivity(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"started sets live status and arms the spinner", func(t *testing.T) {
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
		}},
		{"finished appends a transcript line and reverts to thinking", func(t *testing.T) {
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
		}},
		{"finished with Err marks the transcript entry failed", func(t *testing.T) {
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
		}},
		{"finished without Err does not mark it failed", func(t *testing.T) {
			m := newTestModel()
			started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "list_files", Phase: agent.ToolStarted})
			m = started.(model)

			finished, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "list_files", Phase: agent.ToolFinished, ResultSummary: "done", Err: false})
			nm := finished.(model)

			last := nm.messages[len(nm.messages)-1]
			if last.ToolLogFailed {
				t.Error("expected ToolLogFailed to stay false on a successful Finished event")
			}
		}},
		{"a stale finish does not clobber a newer call's live status", func(t *testing.T) {
			// Covers the concurrent-tool-calls edge case: if call A finishes after call B has already become the live status, A's Finished event must still append A's transcript line, but must not stomp on B's still-active live status.
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
		}},
		{"finished records its elapsed duration", func(t *testing.T) {
			m := newTestModel()
			started := time.Now().Add(-800 * time.Millisecond)

			next, _ := m.Update(agent.ToolActivity{
				ID: "call-1", Name: "recall", ArgsSummary: `"meeting notes"`,
				Phase: agent.ToolFinished, ResultSummary: "4 hits", Started: started,
			})
			nm := next.(model)

			last := nm.messages[len(nm.messages)-1]
			if !strings.Contains(last.Content, "0.8s") {
				t.Errorf("expected the call duration in the finished tool line, got %q", last.Content)
			}
			if !strings.Contains(last.Content, "4 hits") {
				t.Errorf("expected the result summary to survive, got %q", last.Content)
			}
		}},
		{"finished with no start time omits the duration", func(t *testing.T) {
			// Only possible from a synthetic event; must not render a nonsense duration measured from the zero time.
			m := newTestModel()

			next, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "list_files", Phase: agent.ToolFinished, ResultSummary: "done"})
			nm := next.(model)

			if last := nm.messages[len(nm.messages)-1]; last.Content != "list_files() → done" {
				t.Errorf("expected no duration suffix without a start time, got %q", last.Content)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// --- generalized HITL plumbing (F1a) ---

// TestUpdate_ToolRequest is a table over agent.ToolRequest/the HITL approval menu, one row per
// property: a live status spinner is cleared once approval takes over (it already says "paused,
// waiting on you"; a ticking spinner behind it would misleadingly read as "still running"), a
// non-editable request hides "Suggest changes" while a shell-backed one offers it, "Allow once"
// calls the request's own Execute func and delivers its result on ResultChan, and "Allow for
// session" stores the request's AllowKey — but never a bogus empty-string key when it has none.
func TestUpdate_ToolRequest(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"clears the live status once approval takes over", func(t *testing.T) {
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
		}},
		{"non-editable request hides Suggest changes", func(t *testing.T) {
			m := newTestModel()

			next, _ := m.Update(agent.ToolRequest{Description: "read the clipboard", ResultChan: make(chan string, 1)})
			nm := next.(model)

			for _, it := range nm.hitlList.Items() {
				if ci, ok := it.(commandItem); ok && ci.title == "Suggest changes" {
					t.Error("expected \"Suggest changes\" to be hidden for a non-editable request")
				}
			}
		}},
		{"editable (shell-backed) request shows Suggest changes", func(t *testing.T) {
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
		}},
		{"Allow once calls Execute and delivers its result", func(t *testing.T) {
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
		}},
		{"Allow for session stores the request's AllowKey", func(t *testing.T) {
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
		}},
		{"Allow for session with no AllowKey stores nothing", func(t *testing.T) {
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
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

// TestUpdate_ResponseMsg is a table over responseMsg's effect on Update(), one row per property:
// any chunk clears a pending live-status spinner, but only an ora/you chunk marks isConnected true
// (a system chunk like "connection lost — reconnecting…" must not, or the reconnecting hint-bar
// chip would go quiet precisely while the link is down) and only an actual ora reply — not a "you"
// transcription or a system chunk — clears a pending thinking spinner (there is no reply yet for
// either of those), the Sender field routes to the right transcript speaker, and a TurnBoundary
// chunk (empty Text) sets the pending-boundary flag without appending an empty message.
func TestUpdate_ResponseMsg(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"clears a pending live-status spinner", func(t *testing.T) {
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
		}},
		{"a system chunk does not flip isConnected true", func(t *testing.T) {
			m := newTestModel()
			m.isConnected = false

			next, _ := m.Update(responseMsg{Text: "connection lost — reconnecting…", Sender: agent.SenderSystem})
			nm := next.(model)

			if nm.isConnected {
				t.Error("expected isConnected to stay false for a system chunk")
			}
		}},
		{"an ora or you chunk sets isConnected true", func(t *testing.T) {
			for _, sender := range []string{"", agent.SenderYou} {
				m := newTestModel()
				m.isConnected = false

				next, _ := m.Update(responseMsg{Text: "hi", Sender: sender})
				nm := next.(model)

				if !nm.isConnected {
					t.Errorf("expected isConnected to become true for sender %q", sender)
				}
			}
		}},
		{"a you or system chunk does not clear a pending thinking spinner", func(t *testing.T) {
			for _, sender := range []string{agent.SenderYou, agent.SenderSystem} {
				m := newTestModel()
				m.activity = &liveStatus{kind: statusThinking, label: "thinking", started: time.Now()}

				next, _ := m.Update(responseMsg{Text: "chunk", Sender: sender})
				nm := next.(model)

				if nm.activity == nil {
					t.Errorf("expected activity to survive a %q-sender chunk, got cleared", sender)
				}
			}
		}},
		{"an ora chunk still clears a pending thinking spinner", func(t *testing.T) {
			m := newTestModel()
			m.activity = &liveStatus{kind: statusThinking, label: "thinking", started: time.Now()}

			next, _ := m.Update(responseMsg{Text: "the answer is 4"})
			nm := next.(model)

			if nm.activity != nil {
				t.Error("expected activity to be cleared on an ora chunk")
			}
		}},
		{"Sender routes to the right transcript speaker", func(t *testing.T) {
			senderCases := []struct {
				name        string
				chunkSender string
				wantSender  string
			}{
				{"empty sender is ora", "", "ora"},
				{"SenderYou is you", agent.SenderYou, "you"},
				{"SenderSystem is system", agent.SenderSystem, "system"},
			}
			for _, sc := range senderCases {
				t.Run(sc.name, func(t *testing.T) {
					m := newTestModel()

					next, _ := m.Update(responseMsg{Text: "hello", Sender: sc.chunkSender})
					nm := next.(model)

					// streamLine merges into the prior message when sender/thought-state match (see its own doc comment) — the intro banner is itself sender "system", so assert on the suffix rather than assuming a brand-new message.
					got := nm.messages[len(nm.messages)-1]
					if got.Sender != sc.wantSender || !strings.HasSuffix(got.Content, "hello") {
						t.Errorf("expected {Sender: %q, Content ending in %q}, got %+v", sc.wantSender, "hello", got)
					}
				})
			}
		}},
		{"a TurnBoundary chunk sets closeOraBlock without appending a message", func(t *testing.T) {
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// TestStreamLine is a table over streamLine's message-merging rules, one row per property: the
// IsThought flag is tagged directly from the explicit parameter, not sniffed from "**" markdown;
// "you" and "system" chunks are discrete units that never merge across calls while ora's own
// streaming text still does; a non-thought chunk right after a thought chunk starts its own new
// non-thought message; voice mode shows both final ora text and thoughts; a pending turn boundary
// forces the next ora chunk into a fresh block and is then a one-shot signal consumed by that first
// post-boundary chunk; and the Live API chunk-reconciliation cases — a restarted utterance (the
// next chunk is a prefix of the block, above the restart threshold) resets the block, a short
// coincidental prefix below that threshold still appends, an exact resend of the tail is dropped,
// and a cumulative snapshot replaces the block instead of duplicating the overlap.
func TestStreamLine(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"IsThought is tagged from the explicit parameter, not sniffed", func(t *testing.T) {
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
		}},
		{"you sender never merges consecutive calls", func(t *testing.T) {
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
		}},
		{"system sender never merges consecutive calls", func(t *testing.T) {
			m := newTestModel()
			startBefore := len(m.messages)

			m.streamLine("system", "[ora stopped]", false)
			m.streamLine("system", "[ora stopped]", false)

			if len(m.messages) != startBefore+2 {
				t.Fatalf("expected 2 separate messages, got %d: %+v", len(m.messages)-startBefore, m.messages)
			}
		}},
		{"ora sender still merges consecutive calls", func(t *testing.T) {
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
		}},
		{"final text after a thought starts a new, non-thought message", func(t *testing.T) {
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
		}},
		{"voice mode shows both final ora text and thoughts", func(t *testing.T) {
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
		}},
		{"a pending turn boundary forces a new block instead of merging", func(t *testing.T) {
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
		}},
		{"the turn-boundary flag is a one-shot signal", func(t *testing.T) {
			m := newTestModel()
			m.closeOraBlock = true
			m.streamLine("ora", "First.", false)
			startBefore := len(m.messages)

			m.streamLine("ora", " Second.", false)

			if len(m.messages) != startBefore {
				t.Fatalf("expected the post-boundary chunk to merge normally, got %d new messages: %+v", len(m.messages)-startBefore, m.messages)
			}
		}},
		{"a restarted utterance resets the block instead of appending", func(t *testing.T) {
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
		}},
		{"a short prefix overlap below the restart threshold still appends", func(t *testing.T) {
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
		}},
		{"an exact resend of the tail is dropped, not duplicated", func(t *testing.T) {
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
		}},
		{"a cumulative snapshot chunk replaces the block", func(t *testing.T) {
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
		}},
		{"a chunk inside the throttle window still marks the viewport dirty", func(t *testing.T) {
			// Otherwise a reply's last chunk can go permanently unrendered if no further chunk ever arrives to trigger the next render.
			m := newTestModel()
			m.lastUpdate = time.Now() // just updated, so the next chunk falls inside the throttle window

			m.streamLine("ora", "partial reply", false)

			if !m.viewportDirty {
				t.Error("expected viewportDirty to be set when the render was throttled")
			}
		}},
		{"an ora chunk after an interleaved voice line still merges in place", func(t *testing.T) {
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
		}},
		{"an ora chunk after a tool-log line still merges in place", func(t *testing.T) {
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// TestUpdate_ActivityLifecycle is a table over three activity-related Update paths: an errorMsg
// clears any in-flight activity, sending text sets a statusThinking activity and returns a non-nil
// cmd to start the spinner ticking, and a spinner.TickMsg only re-arms (returns a non-nil cmd)
// while an activity is actually set.
func TestUpdate_ActivityLifecycle(t *testing.T) {
	t.Run("errorMsg clears activity", func(t *testing.T) {
		m := newTestModel()
		started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", Phase: agent.ToolStarted})
		m = started.(model)

		next, _ := m.Update(errorMsg(errTest))
		nm := next.(model)
		if nm.activity != nil {
			t.Errorf("expected activity to be cleared on errorMsg, got %+v", nm.activity)
		}
	})

	t.Run("sending text sets a thinking activity", func(t *testing.T) {
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
	})

	t.Run("spinner tick re-arms only when activity is set", func(t *testing.T) {
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
	})
}

// --- quit / esc semantics ---

// TestUpdate_CtrlC is a table over the quit-confirm double-press rule: Ctrl+C no longer quits on
// the first press — it arms a ~1.5s quit-confirm window (see quitConfirmWindow) — and only quits
// on a second press within that window, across every mode including ModeToolConfirm and
// ModeToolEdit (previously swallowed there because those modes' own KeyMsg switches returned
// before reaching the global hotkey switch — the double-press check runs even earlier than that,
// so it's unaffected by the same issue); and a press arriving after the window has lapsed re-arms
// instead of quitting, since the confirmation shouldn't fire from a press left over from a minute
// ago.
func TestUpdate_CtrlC(t *testing.T) {
	for _, mode := range []AgentMode{ModeBoth, ModeVoice, ModeText, ModeToolConfirm, ModeToolEdit} {
		t.Run("requires a double press to quit in "+string(mode), func(t *testing.T) {
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

	t.Run("armed past the window re-arms instead of quitting", func(t *testing.T) {
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
	})
}

// TestUpdate_TickMsg is a table over the 50ms tickMsg handler's auto-clear/flush duties, one row
// per watchdog: the confirm-armed hint doesn't linger forever once its window lapses, a
// throttle-skipped render is flushed and its dirty flag cleared, a tool-activity spinner stuck
// (ToolActivityChan's non-blocking send can drop a Finished event under a burst of concurrent
// tool calls) is auto-cleared once it is stale, and a freshly-started one survives the tick.
func TestUpdate_TickMsg(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"clears an expired quit-confirm arm", func(t *testing.T) {
			m := newTestModel()
			m.width, m.height = 100, 40
			m.quitConfirmArmed = true
			m.quitConfirmArmedAt = time.Now().Add(-2 * quitConfirmWindow)

			next, _ := m.Update(tickMsg(time.Now()))
			nm := next.(model)

			if nm.quitConfirmArmed {
				t.Error("expected an expired quit-confirm arm to be cleared on tick")
			}
		}},
		{"flushes a dirty (throttle-skipped) viewport", func(t *testing.T) {
			m := newTestModel()
			m.width, m.height = 100, 40
			m.viewportDirty = true

			next, _ := m.Update(tickMsg(time.Now()))
			nm := next.(model)

			if nm.viewportDirty {
				t.Error("expected tickMsg to flush the viewport and clear the dirty flag")
			}
		}},
		{"auto-clears a stale tool-activity spinner", func(t *testing.T) {
			m := newTestModel()
			m.width, m.height = 100, 40
			m.activity = &liveStatus{kind: statusTool, id: "call-1", label: "query_memory(...)", started: time.Now().Add(-(staleActivityTimeout + time.Second))}

			next, _ := m.Update(tickMsg(time.Now()))
			nm := next.(model)

			if nm.activity != nil {
				t.Errorf("expected a stale (>%v old) activity to be auto-cleared on tick, got %+v", staleActivityTimeout, nm.activity)
			}
		}},
		{"a fresh activity survives the tick", func(t *testing.T) {
			m := newTestModel()
			m.width, m.height = 100, 40
			m.activity = &liveStatus{kind: statusTool, id: "call-1", label: "query_memory(...)", started: time.Now()}

			next, _ := m.Update(tickMsg(time.Now()))
			nm := next.(model)

			if nm.activity == nil {
				t.Error("expected a freshly-started activity to survive a tick, got nil")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
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

// TestFilterCommands_MatchesSubstringAndPrefix verifies the "/" command menu filters by substring,
// not just prefix — "otes" still finds "notes" — while the common case of a prefix query (typing
// from the start of a command name, e.g. "vo" for the voice commands) keeps matching too.
func TestFilterCommands_MatchesSubstringAndPrefix(t *testing.T) {
	cases := []struct {
		name  string
		query string
		check func(t *testing.T, items []list.Item)
	}{
		{"substring query matches mid-word", "otes", func(t *testing.T, items []list.Item) {
			found := false
			for _, it := range items {
				if ci, ok := it.(commandItem); ok && ci.title == "notes" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected substring query %q to match \"notes\", got items: %+v", "otes", items)
			}
		}},
		{"prefix query still matches", "vo", func(t *testing.T, items []list.Item) {
			if len(items) == 0 {
				t.Fatal("expected prefix query \"vo\" to match at least the voice commands")
			}
			for _, it := range items {
				ci, ok := it.(commandItem)
				if !ok || !strings.Contains(ci.title, "vo") {
					t.Errorf("unexpected non-matching item in filtered results: %+v", it)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newCommandList(DefaultStyles())
			FilterCommands(&l, tc.query)
			tc.check(t, l.Items())
		})
	}
}

// TestUpdate_Esc is a table over Esc's priority rungs, one row per rung and never quitting the
// app: in ModeToolConfirm it has the same effect as Reject (rejects on ResultChan, drops back to
// ModeBoth, clears the pending request); with the "/" command menu open it closes the menu without
// falling through to the textarea, and menu-close still wins even when the viewport is also
// scrolled away; with no menu open but the viewport scrolled away it jumps back to the bottom
// without touching a non-empty textarea; with nothing else pending it clears in-progress typed
// input; and with nothing to clear and no menu open it is a no-op.
func TestUpdate_Esc(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"in ModeToolConfirm rejects the command", func(t *testing.T) {
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
		}},
		{"with the command menu open, closes the menu without quitting", func(t *testing.T) {
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
		}},
		{"scrolled away with no menu open, jumps to bottom without clearing the textarea", func(t *testing.T) {
			// Proves the rung ordering is menu-close, then jump-to-bottom, then clear-textarea, not "do everything at once."
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
		}},
		{"menu open and scrolled away, only closes the menu", func(t *testing.T) {
			// Menu-close still takes priority over the jump-to-bottom rung — both conditions true at once, only the menu closes on this keypress.
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
		}},
		{"with a non-empty textarea and nothing else pending, clears it", func(t *testing.T) {
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
		}},
		{"with an empty textarea and no menu, is a no-op", func(t *testing.T) {
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

var errTest = &testError{"boom"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// --- viewport height recalculation ---

// TestRecalcViewportHeight is a table over recalcViewportHeight's accounting, one row per term:
// the status line costs exactly one line, and opening either menu (the "/" command list or the
// HITL confirm list) shrinks the viewport by exactly the extra rows renderInput actually grows by
// for that menu — measured via the real rendered height (lipgloss.Height) rather than a hardcoded
// row count, so it stays correct if either menu's own styling changes.
func TestRecalcViewportHeight(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"the status line adds exactly one line", func(t *testing.T) {
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
		}},
		{"the command menu opening accounts for its extra rows", func(t *testing.T) {
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
		}},
		{"the tool-confirm menu opening accounts for its extra rows", func(t *testing.T) {
			// Baseline mode is ModeBoth, not ModeText: both ModeBoth and ModeToolConfirm carry the same non-zero signalHeight, so toggling between them isolates the delta to the menu term alone instead of also picking up recalcViewportHeight's unrelated signalHeight swing.
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
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

// TestUpdateViewport_StickyBottom is a table over updateViewport's scroll-position rules, one row
// per case: a non-"you" append (e.g. a tool-log line arriving while the user is reading earlier
// history) leaves scroll position alone rather than yanking it down; if the user was already at
// the bottom, new content keeps them glued there; force=true (a "you" send) always jumps to the
// bottom regardless of prior scroll position; and streamLine itself forces the bottom for "you"
// sends specifically, since it's the only call site that knows the sender directly.
func TestUpdateViewport_StickyBottom(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a non-forced, non-you append leaves scroll position alone", func(t *testing.T) {
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
		}},
		{"already at the bottom, a non-forced append stays at the bottom", func(t *testing.T) {
			m := newTestModel()
			fillScrollableViewport(&m)
			m.viewport.GotoBottom()

			m.messages = append(m.messages, Message{Sender: "tool", Content: "another line"})
			m.updateViewport(false)

			if !m.viewport.AtBottom() {
				t.Error("expected to stay at the bottom when already there before new content arrived")
			}
		}},
		{"forced always jumps to the bottom even when scrolled away", func(t *testing.T) {
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
		}},
		{"streamLine forces the bottom for a you send", func(t *testing.T) {
			m := newTestModel()
			fillScrollableViewport(&m)
			if m.viewport.AtBottom() {
				t.Fatal("setup: expected to have scrolled away from the bottom")
			}

			m.streamLine("you", "hi", false)

			if !m.viewport.AtBottom() {
				t.Error("expected streamLine to force the bottom for a \"you\" send")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// --- scrollback keys (WP3 item 2, textarea-empty-gating from X1) ---
//
// Before this, every KeyMsg path returned from Update() before the viewport ever saw the key (see the KeyMsg case's trailing `return m, tiCmd`) — the mouse wheel was the only way to scroll. These route PgUp/PgDn/Ctrl+U/Ctrl+D explicitly using bubbles' own paging methods rather than hand-rolling line math.
// Ctrl+U/Ctrl+D (and Ctrl+E below) only take over when the textarea is empty — bubbles' textarea binds all three to real editing actions (delete-before-cursor, delete-char-forward, line-end) via its own DefaultKeyMap, and stealing them mid-sentence broke composing (X1). PgUp/PgDn stay global since the textarea doesn't bind either.

// TestUpdate_ScrollKeys is a table over the viewport scroll keys: PgDown/PgUp and Ctrl+D/Ctrl+U
// scroll the viewport with an empty textarea, but Ctrl+U and Ctrl+D fall back to bubbles' native
// textarea editing (delete to line start, delete character forward) instead of scrolling once
// there's in-progress typed input — X1.
func TestUpdate_ScrollKeys(t *testing.T) {
	cases := []struct {
		name          string
		key           tea.KeyType
		setup         func(m *model)
		wantScrollDir int    // +1 down, -1 up, 0 no scroll
		wantTextarea  string // checked only when wantScrollDir == 0
	}{
		{name: "PgDown scrolls the viewport down", key: tea.KeyPgDown, wantScrollDir: 1},
		{name: "PgUp scrolls the viewport up", key: tea.KeyPgUp, setup: func(m *model) { m.viewport.GotoBottom() }, wantScrollDir: -1},
		{name: "Ctrl+D half-page-downs an empty textarea", key: tea.KeyCtrlD, wantScrollDir: 1},
		{name: "Ctrl+U half-page-ups an empty textarea", key: tea.KeyCtrlU, setup: func(m *model) { m.viewport.GotoBottom() }, wantScrollDir: -1},
		{name: "Ctrl+U deletes instead of scrolling with a non-empty textarea", key: tea.KeyCtrlU, setup: func(m *model) {
			m.textarea.SetValue("hello there")
			m.textarea.CursorEnd()
		}, wantScrollDir: 0, wantTextarea: ""},
		{name: "Ctrl+D deletes a character instead of scrolling with a non-empty textarea", key: tea.KeyCtrlD, setup: func(m *model) {
			m.textarea.SetValue("hello")
			m.textarea.CursorStart()
		}, wantScrollDir: 0, wantTextarea: "ello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()
			fillScrollableViewport(&m)
			if tc.setup != nil {
				tc.setup(&m)
			}
			before := m.viewport.YOffset

			next, _ := m.Update(tea.KeyMsg{Type: tc.key})
			nm := next.(model)

			switch {
			case tc.wantScrollDir > 0:
				if nm.viewport.YOffset <= before {
					t.Errorf("expected the key to scroll down, before=%d after=%d", before, nm.viewport.YOffset)
				}
			case tc.wantScrollDir < 0:
				if nm.viewport.YOffset >= before {
					t.Errorf("expected the key to scroll up, before=%d after=%d", before, nm.viewport.YOffset)
				}
			default:
				if nm.viewport.YOffset != before {
					t.Errorf("expected no scrolling while composing, before=%d after=%d", before, nm.viewport.YOffset)
				}
				if nm.textarea.Value() != tc.wantTextarea {
					t.Errorf("expected the native textarea edit to leave %q, got %q", tc.wantTextarea, nm.textarea.Value())
				}
			}
		})
	}
}

// --- foldable thoughts (WP3 item 4) ---

func longThoughtContent() string {
	return strings.Repeat("reasoning about the best approach here in great detail ", 5)
}

// TestRenderMessage is a table over renderMessage's rendering-detail regressions, one row per
// property: a thought collapses to a preview plus the ctrl+e hint by default and shows its full
// content once expanded, a non-thought message renders identically either way, a wide-glyph
// (Malayalam) string never renders a line wider than the column it was given, plain content wraps
// with a safety margin below the full gutter width rather than exactly at it, an ora message
// carries no background ANSI code, and trailing blank lines are trimmed at render time. Each row
// is independent, so a failure still names exactly which rendering property broke.
func TestRenderMessage(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"thought collapsed by default", func(t *testing.T) {
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
		}},
		{"thought shows full content when expanded", func(t *testing.T) {
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
		}},
		{"non-thought message unaffected by expandThoughts", func(t *testing.T) {
			m := newTestModel()
			msg := Message{Sender: "ora", IsThought: false, Content: "the final answer is 42"}

			collapsed := m.renderMessage(msg, 100)
			m.expandThoughts = true
			expanded := m.renderMessage(msg, 100)

			if collapsed != expanded {
				t.Errorf("expected a non-thought message to render identically regardless of expandThoughts, got:\ncollapsed=%q\nexpanded=%q", collapsed, expanded)
			}
		}},
		{"wide-glyph content: every rendered line fits width exactly", func(t *testing.T) {
			// malayalamRepro is the exact stair-step repro string from the WP5 bug report — heavy in conjuncts/combining marks, the class of glyph where cell-width math (go-runewidth, under lipgloss) can disagree with a real terminal's rendering.
			const malayalamRepro = "batches?ണ്ടില്ല, ചതിവുകളെ ഇവിടെയുണ്ട്. ഏതെങ്കിലും കണ്ടുകൂടാ ശരി"
			m := newTestModel()
			width := 60

			rendered := m.renderMessage(Message{Sender: "ora", Content: malayalamRepro}, width)

			for i, line := range strings.Split(rendered, "\n") {
				if w := lipgloss.Width(line); w != width {
					t.Errorf("line %d %q rendered width %d, want exactly %d (background padding)", i, line, w, width)
				}
			}
		}},
		{"content wraps with a safety margin below full gutter width", func(t *testing.T) {
			m := newTestModel()
			width := 40
			content := strings.Repeat("a", width-GutterWidth)

			rendered := m.renderMessage(Message{Sender: "ora", Content: content}, width)
			lines := strings.Split(rendered, "\n")

			if len(lines) < 2 {
				t.Fatalf("expected the safety margin to force a wrap before the full gutter-width column, got %d line(s): %+v", len(lines), lines)
			}
		}},
		{"an ora message carries no background ANSI code", func(t *testing.T) {
			lipgloss.SetColorProfile(termenv.TrueColor)
			defer lipgloss.SetColorProfile(termenv.Ascii)

			m := newTestModel()
			out := m.renderMessage(Message{Sender: "ora", Content: "Hello there."}, 60)

			if noBackgroundCode.MatchString(out) {
				t.Errorf("expected no background ANSI code in a rendered ora message, got %q", out)
			}
		}},
		{"trailing newlines trimmed at render time", func(t *testing.T) {
			m := newTestModel()
			withTrailing := m.renderMessage(Message{Sender: "ora", Content: "Hello there.\n\n\n"}, 60)
			without := m.renderMessage(Message{Sender: "ora", Content: "Hello there."}, 60)

			if got, want := lipgloss.Height(withTrailing), lipgloss.Height(without); got != want {
				t.Errorf("expected trailing newlines trimmed at render time (height %d, matching no-trailing-newline content), got height %d", want, got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// TestUpdate_CtrlE_TogglesExpandThoughts verifies Ctrl+E flips the global fold state, and flips back on a second press. Textarea is empty (the zero value default) — this is the fold-toggle case; see TestUpdate_CtrlE_NonEmptyTextarea_MovesToLineEndInsteadOfToggling for the composing case (X1).
// TestUpdate_CtrlE is a table over Ctrl+E's two behaviours: with an empty textarea it toggles the
// thought fold (twice, back and forth), and with in-progress typed input it moves the cursor to
// line end (bubbles' native LineEnd) instead of toggling the fold — X1.
func TestUpdate_CtrlE(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"empty textarea toggles the thought fold", func(t *testing.T) {
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
		}},
		{"non-empty textarea moves to line end instead of toggling", func(t *testing.T) {
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// --- rendering smoke tests ---
//
// Real terminal rendering isn't unit-tested here (see waveform_test.go's TestWaveform_VisualDemo, which just prints for human inspection) — but View() panicking is a real regression, and driving bubbletea interactively needs a real TTY/daemon/API key this environment doesn't have. These tests just prove each new rendering path doesn't panic and the expected text shows up.

// TestView is a table of rendering smoke tests, one row per path: a live tool activity shows its
// label, a finished tool leaves its result in the collapsed transcript line, a thinking status
// shows its label, and with no activity at all the status line is omitted entirely.
func TestView(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"with tool activity shows the label", func(t *testing.T) {
			m := newTestModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
			m = next.(model)

			started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", ArgsSummary: `"Riddler"`, Phase: agent.ToolStarted})
			m = started.(model)

			out := m.View()
			if !strings.Contains(out, `query_memory("Riddler")`) {
				t.Errorf("expected the live status line's label in the rendered output, got:\n%s", out)
			}
		}},
		{"after tool finishes shows the transcript log line", func(t *testing.T) {
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
		}},
		{"with thinking status does not panic", func(t *testing.T) {
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
		}},
		{"no activity omits the status line entirely", func(t *testing.T) {
			m := newTestModel()
			next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
			m = next.(model)

			if got := m.renderStatusLine(); got != "" {
				t.Errorf("expected renderStatusLine to return \"\" when nothing is active, got %q", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
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
// TestRenderInput is a table over renderInput's per-state rendering rules: the mode chip shows in
// the normal state (the ram chip was deleted in WP6 — ReadMemStats sampling caused a periodic GC
// stop-the-world for cosmetic dev trivia); the daemon/connection warning chips WP8 moved off the
// deleted header row into the hint bar are quiet when healthy and appear only in their own
// degraded state (the header used to show "⊙ tracking"/"● live" pills unconditionally); and the
// WP10 addendum's ❯-alignment fix keeps the prompt pinned to the input deck's first row regardless
// of textarea height, rather than floating away from the cursor line on a multi-line textarea.
func TestRenderInput(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"normal state still shows the mode chip", func(t *testing.T) {
			m := newTestModel()
			m.width, m.height = 100, 40

			out := m.renderInput()

			if !strings.Contains(out, "mode") {
				t.Errorf("expected the normal-state input row to still show the mode chip, got:\n%s", out)
			}
		}},
		{"degraded-state chips only show when unhealthy", func(t *testing.T) {
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
		}},
		{"multi-row textarea keeps the prompt aligned to the top", func(t *testing.T) {
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
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// noBackgroundCode matches any RGB background-setting SGR sequence (lipgloss's Background() always encodes as 48;2;R;G;B in a forced true-color profile).
var noBackgroundCode = regexp.MustCompile(`48;2;`)

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

// TestCommandDelegate_Render_NeverWrapsPastDeclaredHeight is a WP7 finding: commandDelegate.Height() declares 1 row per item, but Render's description text had no width bound, so a long description (e.g. hitlItems' "Execute this command and return the result") word-wrapped to 2 physical lines despite the declared height — breaking SetHeight(len(items))'s assumption that N items need exactly N rows.
// noUnbackgroundedGap matches a bare ANSI reset (\x1b[0m) immediately followed by a printable byte with no fresh SGR in between — a nested lipgloss Render's own reset code lands mid-row and everything after it shows the terminal's own default background (black in most terminals) instead of the row's, until the next styled span re-arms it. A reset at the very end of the row (nothing after it) never matches, since there's no unbackgrounded content left to paint.
var noUnbackgroundedGap = regexp.MustCompile("\x1b\\[0m[^\x1b]")

// TestCommandDelegate_Render is a table over commandDelegate.Render's two regressions: it must
// never wrap a row past the delegate's declared Height() (a long description with no width bound
// used to word-wrap to 2 physical lines despite the declared height, breaking
// SetHeight(len(items))'s N-items-need-N-rows assumption), and the row background must stay
// continuous (WP9 item 1: the inner title/desc styles carried no explicit Background, so their
// own ANSI reset cut the outer row's background short wherever it landed).
func TestCommandDelegate_Render(t *testing.T) {
	t.Run("never wraps past the declared height", func(t *testing.T) {
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
	})

	// Forces a true-color profile since color codes are stripped entirely under go test's non-tty stdout, which would make this pass vacuously.
	t.Run("row background is continuous", func(t *testing.T) {
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
	})
}

// TestCommandList_Sizing is a table over the command/hitl list sizing rules from WP9: a freshly
// constructed cmdList sizes its height to the real unfiltered item count (it used to construct at
// a fixed DefaultListHeight=8 regardless of the 12-item set, forcing bubbles to paginate);
// FilterCommands resizes the list to the filtered set's length every time, not just the
// construction height, so a narrower filter shrinks the deck rather than padding it with blank
// rows; and with height sized to the real item count, both lists never paginate (no "•"/"○" dot
// row) or pad with filler rows to reach a taller fixed height.
func TestCommandList_Sizing(t *testing.T) {
	t.Run("newCommandList height matches the unfiltered item count", func(t *testing.T) {
		l := newCommandList(DefaultStyles())
		if got, want := l.Height(), len(commandItems); got != want {
			t.Errorf("newCommandList height = %d, want %d (len(commandItems))", got, want)
		}
	})

	t.Run("FilterCommands sets height to the filtered count", func(t *testing.T) {
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
	})

	t.Run("cmdList and hitlList views have no pagination dots or filler rows", func(t *testing.T) {
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
	})
}

// TestUpdate_WindowSizeMsg_ListsWidthedToRealDeckWidth is WP9 item 1's other half: cmdList/hitlList used to stay pinned at the fixed DefaultListWidth=60 forever, so a row's background band stopped mid-screen on any wider terminal even after the per-row Background fix above. They must track the actual input-deck content width (terminal width minus InputWrap's own horizontal padding) on every resize.
// TestUpdate_WindowSizeMsg is a table over two WP5/WP9 resize regressions: cmdList/hitlList must
// track the actual input-deck content width (terminal width minus InputWrap's own horizontal
// padding) on every resize, rather than staying pinned at a fixed DefaultListWidth=60 forever; and
// every row of the input deck (background included) must span the terminal's full width after a
// WindowSizeMsg — kept as a permanent regression guard for the same class of viewport-height
// overflow that TestView_RenderedHeightMatchesModelHeight's cases catch.
func TestUpdate_WindowSizeMsg(t *testing.T) {
	t.Run("lists widthed to the real deck width", func(t *testing.T) {
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
	})

	t.Run("input row renders at full width", func(t *testing.T) {
		m := newTestModel()
		next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
		nm := next.(model)

		lines := strings.Split(nm.renderInput(), "\n")
		for i, line := range lines {
			if w := lipgloss.Width(line); w != nm.width {
				t.Errorf("input deck line %d rendered width %d, want exactly m.width %d", i, w, nm.width)
			}
		}
	})
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
		// FINDING 8: recalcViewportHeight only relaxes the viewport floor to 0 when a menu is open, so with
		// no menu open on a short terminal the viewport stays clamped at MinViewportHeight no matter how
		// negative the available space goes — pushing the ❯ input prompt off the bottom. This got easier to
		// hit because renderInput adds a status-line row whenever a tool call or the thinking spinner is
		// active, plus "⚠ no tracker" / "⚠ reconnecting" warning chips, on top of the ordinary input row and
		// hint row. No menu is open here (mode stays ModeBoth) — only that extra chrome. 24 rows is the exact
		// height where the baseline chrome (no live status, no warning chips) already fits with nothing to
		// spare — turning on the extra status-line row is what tips it over.
		{"short terminal, no menu open, input prompt stays visible", func(m *model) {
			m.activity = &liveStatus{kind: statusTool, label: `shell_exec("ls -la")`}
			m.daemonOK = false
			m.isConnected = false
		}, false, 24},
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
// TestUpdate_SuggestChanges is a table over the "Suggest changes" edit flow opened by
// openSuggestChanges: opening it keeps activeToolReq set (unlike the other three menu entries it
// is not terminal — it hands off to ModeToolEdit, which still needs the request to deliver a
// result on) and prefills the textarea with the original command so the user edits it rather than
// retyping it; submitting an edited command runs that command and delivers its output rather than
// panicking on a nil activeToolReq; and cancelling the edit rejects the request rather than
// panicking on a nil activeToolReq.
func TestUpdate_SuggestChanges(t *testing.T) {
	t.Run("opening it keeps activeToolReq and prefills the textarea", func(t *testing.T) {
		m, _ := openSuggestChanges(t, "echo ora-test")

		if m.activeToolReq == nil {
			t.Error("activeToolReq was cleared by Suggest changes — the edit handler needs it to deliver a result")
		}
		if got := m.textarea.Value(); got != "echo ora-test" {
			t.Errorf("expected the textarea prefilled with the original command, got %q", got)
		}
	})

	t.Run("Enter runs the edited command", func(t *testing.T) {
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
	})

	t.Run("Esc rejects", func(t *testing.T) {
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
	})
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

// --- elapsed time on tool activity ---

// TestRenderStatusLine_ShowsElapsedSeconds: a running tool must look obviously alive rather than hung, so the status row carries how long it has been going, ticking up while it runs.
func TestRenderStatusLine_ShowsElapsedSeconds(t *testing.T) {
	m := newTestModel()
	m.activity = &liveStatus{kind: statusTool, id: "call-1", label: `recall("meeting notes")`, started: time.Now().Add(-3 * time.Second)}

	got := m.renderStatusLine()

	if !strings.Contains(got, "3.0s") {
		t.Errorf("expected the elapsed time in the status line, got %q", got)
	}
	if !strings.Contains(got, `recall("meeting notes")`) {
		t.Errorf("expected the tool label in the status line, got %q", got)
	}
}
