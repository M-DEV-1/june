package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/genai"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/db"
)

// fakeAgySession is a scripted agySessionRunner: each Send call consumes the next entry of responses (making it that turn's "result" event) unless sendErr is armed, in which case that one Send fails instead, as a broken pipe would. toolCalls, when set, names the ora tools to call over MCP before a given turn's response — index-aligned with responses — so a test can check that hops still get recorded through a session's long-lived tool server.
type fakeAgySession struct {
	mu        sync.Mutex
	startErr  error
	sendErr   bool
	responses []string
	toolCalls map[int][]string
	sends     []string
	lastEnv   []string
	starts    int
	killed    bool
	turn      int
	events    chan []byte
}

func (f *fakeAgySession) Start(ctx context.Context, env, args []string) error {
	f.mu.Lock()
	f.starts++
	f.lastEnv = env
	f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.events = make(chan []byte, 4)
	return nil
}

func (f *fakeAgySession) Send(line string) error {
	f.mu.Lock()
	f.sends = append(f.sends, line)
	turn := f.turn
	f.turn++
	failing := f.sendErr
	if failing {
		f.sendErr = false
	}
	env := f.lastEnv
	f.mu.Unlock()
	if failing {
		return errors.New("fakeAgySession: broken pipe")
	}
	for _, name := range f.toolCalls[turn] {
		if err := callFakeAgyTool(env, name); err != nil {
			return err
		}
	}
	if turn >= len(f.responses) {
		return fmt.Errorf("fakeAgySession: no scripted response for turn %d", turn)
	}
	body, _ := json.Marshal(json.RawMessage(fmt.Sprintf(`{"event":"result","result":%s}`, f.responses[turn])))
	f.events <- body
	return nil
}

func (f *fakeAgySession) Events() <-chan []byte { return f.events }

func (f *fakeAgySession) Kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
}

func (f *fakeAgySession) wasKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

// callFakeAgyTool calls one ora tool over MCP against the server named in env's HOME's mcp config, the way the real agy CLI would from inside a turn.
func callFakeAgyTool(env []string, name string) error {
	home := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "HOME="); ok {
			home = v
		}
	}
	if home == "" {
		return errors.New("fakeAgySession: no HOME in the session's environment")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	if err != nil {
		return err
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL string `json:"serverUrl"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	url := cfg.MCPServers[agyMCPServerName].URL
	if url == "" {
		return errors.New("fakeAgySession: no ora server in the mcp config")
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{"purpose": "looking"}}})
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// agyLineText decodes one NDJSON line Send received back into the plain text it carried, the way agy itself reads its stdin.
func agyLineText(t *testing.T, line string) string {
	t.Helper()
	var parsed struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("the line sent to agy is not the expected shape: %v (%s)", err, line)
	}
	return parsed.Message.Content
}

// A second ask on the same conversation reuses the process: only one session gets started, and the second send carries just the new question, none of the instruction or the first turn's thread.
// A different Ora conversation must not inherit a live process that remembers another one: the process is reused only when the history handed in is the one it has already been told, and the second ask of a conversation carries the first turn as history.
func TestAskAgy_ADifferentConversationStartsANewProcess(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"hi there"}`, `{"status":"SUCCESS","response":"again"}`, `{"status":"SUCCESS","response":"fresh"}`}}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)

	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", nil, "hello"); err != nil {
		t.Fatal(err)
	}
	same := History{genai.NewContentFromText("hello", genai.RoleUser), genai.NewContentFromText("hi there", genai.RoleModel)}
	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", same, "again"); err != nil {
		t.Fatal(err)
	}
	if fake.starts != 1 {
		t.Fatalf("the same conversation continuing started %d processes, want 1", fake.starts)
	}
	other := History{genai.NewContentFromText("what is my manager's name", genai.RoleUser), genai.NewContentFromText("Priya", genai.RoleModel)}
	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", other, "and her role?"); err != nil {
		t.Fatal(err)
	}
	if fake.starts != 2 {
		t.Errorf("a different conversation reused the process: starts = %d, want 2", fake.starts)
	}
	if !strings.Contains(fake.sends[2], "what is my manager's name") {
		t.Errorf("the new process was not given the other conversation's history: %q", fake.sends[2])
	}
}

func TestAskAgy_SecondAskReusesTheProcess(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"hi there"}`, `{"status":"SUCCESS","response":"again"}`}}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)

	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", nil, "hello"); err != nil {
		t.Fatal(err)
	}
	first := History{genai.NewContentFromText("hello", genai.RoleUser), genai.NewContentFromText("hi there", genai.RoleModel)}
	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", first, "again"); err != nil {
		t.Fatal(err)
	}

	if fake.starts != 1 {
		t.Errorf("the process was started %d times, want 1", fake.starts)
	}
	if len(fake.sends) != 2 {
		t.Fatalf("sends = %d, want 2", len(fake.sends))
	}
	if !strings.Contains(fake.sends[0], "You are Ora.") {
		t.Errorf("the first send is missing the instruction: %q", fake.sends[0])
	}
	if strings.Contains(fake.sends[1], "You are Ora.") || strings.Contains(fake.sends[1], "hello") {
		t.Errorf("the second send should carry only the new question, not the instruction or the first turn: %q", fake.sends[1])
	}
	if !strings.Contains(fake.sends[1], "again") {
		t.Errorf("the second send is missing the question: %q", fake.sends[1])
	}
}

// A model change starts a fresh process instead of reusing the one that answered under the old model, and kills the old one.
func TestAskAgy_ModelChangeStartsANewProcess(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake1 := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"a"}`}}
	fake2 := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"b"}`}}
	sessions := []*fakeAgySession{fake1, fake2}
	i := 0
	newProc := func() agySessionRunner {
		s := sessions[i]
		i++
		return s
	}
	t.Cleanup(a.CloseAgySession)

	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", nil, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.askAgy(t.Context(), newProc, "gemini-3-flash", nil, "hi"); err != nil {
		t.Fatal(err)
	}

	if i != 2 {
		t.Errorf("newProc was called %d times, want 2 — a model change must start a fresh process", i)
	}
	if !fake1.wasKilled() {
		t.Errorf("the old model's process was not killed")
	}
	if !strings.Contains(fake2.sends[0], "You are Ora.") {
		t.Errorf("the new process's first send must carry the instruction again: %q", fake2.sends[0])
	}
}

// A process that has died since the last turn (here: its stdin write fails) is restarted transparently, with the instruction and history sent again as if to a fresh process.
func TestAskAgy_RestartsADeadProcessWithInstructionAndHistory(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	dying := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"a"}`}}
	fresh := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"back"}`}}
	sessions := []*fakeAgySession{dying, fresh}
	i := 0
	newProc := func() agySessionRunner {
		s := sessions[i]
		i++
		return s
	}
	t.Cleanup(a.CloseAgySession)

	history := HistoryFromTurns([]db.Turn{{Role: "you", Text: "who did I meet"}, {Role: "ora", Text: "Priya"}})
	if _, err := a.askAgy(t.Context(), newProc, "", history, "hello"); err != nil {
		t.Fatal(err)
	}
	dying.sendErr = true

	tr, err := a.askAgy(t.Context(), newProc, "", history, "when")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "back" {
		t.Errorf("answer = %q, want the restarted process's answer", tr.Answer)
	}
	if i != 2 {
		t.Errorf("newProc was called %d times, want 2 — the dead process must be replaced", i)
	}
	if !strings.Contains(fresh.sends[0], "You are Ora.") || !strings.Contains(fresh.sends[0], "who did I meet") {
		t.Errorf("the restarted process's first send must carry the instruction and history again: %q", fresh.sends[0])
	}
}

// A session left unused for longer than agyIdleTimeout is killed, freeing the process without anyone having to ask again.
func TestAskAgy_IdleTimeoutKillsTheProcess(t *testing.T) {
	old := agyIdleTimeout
	agyIdleTimeout = time.Millisecond
	defer func() { agyIdleTimeout = old }()

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"hi"}`}}
	newProc := func() agySessionRunner { return fake }

	if _, err := a.askAgy(t.Context(), newProc, "", nil, "hello"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !fake.wasKilled() {
		if time.Now().After(deadline) {
			t.Fatal("the idle process was never killed")
		}
		time.Sleep(time.Millisecond)
	}
}

// Tool calls made mid-turn still get recorded, through the session's own long-lived tool server, exactly as they were on the one-shot path.
func TestAskAgy_RunsToolsThroughTheLiveSession(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{
		responses: []string{`{"status":"SUCCESS","response":"Brave is in front."}`},
		toolCalls: map[int][]string{0: {"observe_screen"}},
	}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)

	tr, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", nil, "what window is in front")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "Brave is in front." || tr.Model != "agy/gemini-3-pro" {
		t.Errorf("trace = %+v", tr)
	}
	if len(tr.ToolHops) != 1 || tr.ToolHops[0].Name != "observe_screen" {
		t.Errorf("hops = %+v", tr.ToolHops)
	}
}
