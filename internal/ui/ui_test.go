package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ora/internal/agent"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// newTestModel gives Update() a fully-initialized model (textarea/viewport included) backed by a channel-only Agent — mirrors agent.NewAgent(nil, nil, nil, nil, "") already used throughout internal/agent's own tests.
func newTestModel() model {
	a := agent.NewAgent(nil, nil, nil, nil, "")
	return NewModel(a, "connected")
}

// --- daemon status polling ---

func TestPollDaemonHTTP_RespondsOK_ReturnsTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !pollDaemonHTTP(http.DefaultClient, srv.URL) {
		t.Error("expected true for a 200 OK response")
	}
}

func TestPollDaemonHTTP_NonOKStatus_ReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if pollDaemonHTTP(http.DefaultClient, srv.URL) {
		t.Error("expected false for a non-200 response")
	}
}

func TestPollDaemonHTTP_Unreachable_ReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	if pollDaemonHTTP(http.DefaultClient, url) {
		t.Error("expected false for an unreachable server")
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
	next, _ := m.Update(agent.ToolRequest{Command: "rm -rf /tmp/x", ResultChan: resChan})
	nm := next.(model)

	if nm.activity != nil {
		t.Errorf("expected the live status to be cleared once HITL approval takes over, got %+v", nm.activity)
	}
}

func TestUpdate_ResponseMsg_ClearsActivity(t *testing.T) {
	m := newTestModel()
	started, _ := m.Update(agent.ToolActivity{ID: "call-1", Name: "query_memory", Phase: agent.ToolStarted})
	m = started.(model)
	if m.activity == nil {
		t.Fatal("setup: expected activity to be set")
	}

	next, _ := m.Update(responseMsg("hi there"))
	nm := next.(model)
	if nm.activity != nil {
		t.Errorf("expected activity to be cleared on responseMsg, got %+v", nm.activity)
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
