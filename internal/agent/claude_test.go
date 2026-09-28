package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ora/internal/db"
)

// rpcPost sends one JSON-RPC request to the tool server and returns the decoded reply. Input: the server, the method, the id and the params. Output: the whole reply object.
func rpcPost(t *testing.T, s *claudeToolServer, method string, id int, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, s.URL(), bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decoding the %s reply: %v", method, err)
	}
	return reply
}

// The tool server answers initialize with the tools capability and lists every tool an ask offers, each with the JSON Schema its arguments take.
func TestClaudeToolServer_InitializesAndListsTheAskTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	init := rpcPost(t, s, "initialize", 0, map[string]any{"protocolVersion": "2025-11-25"})
	result, ok := init["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize reply = %v", init)
	}
	if result["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocol version = %v", result["protocolVersion"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities = %v", result["capabilities"])
	}

	list := rpcPost(t, s, "tools/list", 1, nil)
	tools, ok := list["result"].(map[string]any)["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list reply = %v", list)
	}
	if len(tools) != len(a.askToolDeclarations()) {
		t.Fatalf("%d tools listed, %d declared", len(tools), len(a.askToolDeclarations()))
	}
	found := false
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] != "observe_screen" {
			continue
		}
		found = true
		if tool["description"] == "" {
			t.Errorf("observe_screen has no description")
		}
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("observe_screen input schema = %v", tool["inputSchema"])
		}
	}
	if !found {
		t.Errorf("observe_screen was not listed")
	}
}

// A tools/call runs the tool through the ask's own gate, answers with its result as MCP text content, and records the call as a tool hop the trace can carry. Once a run has spent all of its maxAskIterations steps, every call after that is refused without running, so askClaude can end the turn on capError (see Capped()) rather than trusting whatever the CLI does with a refusal. Nothing here stops the CLI mid-run on its own; the wall clock it is started under (claudeAskTimeout) is the other hard stop.
func TestClaudeToolServer_RefusesToolCallsPastTheStepCap(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var first, last string
	for i := 0; i < maxAskIterations+1; i++ {
		call := rpcPost(t, s, "tools/call", 3, map[string]any{"name": "shell_exec", "arguments": map[string]any{"command": "ls"}})
		text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if i == 0 {
			first = text
		}
		last = text
	}
	if !strings.Contains(first, "not available in an ask") {
		t.Errorf("the first call's result text = %q", first)
	}
	if !strings.Contains(last, "run out of steps") {
		t.Errorf("the call past the step cap did not carry the refusal: %q", last)
	}
	hops := s.Hops()
	if len(hops) != maxAskIterations {
		t.Errorf("%d hops recorded, want exactly the cap of %d (the call past it must not run)", len(hops), maxAskIterations)
	}
	if !s.Capped() {
		t.Error("Capped() = false, want true once the step cap is spent")
	}
	if hops[0].Name != "shell_exec" || hops[0].Args["command"] != "ls" || hops[0].Result != first {
		t.Errorf("first hop = %+v", hops[0])
	}
}

// Two tool calls that arrive at once when the run is one step below its cap must not both be admitted: the read and the increment in reserveStep have to happen in the same locked section, or two concurrent calls could each see the cap as not yet reached and the run would spend one more step than maxAskIterations allows.
func TestClaudeToolServer_TwoConcurrentCallsAtTheCapOnlyOneRuns(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < maxAskIterations-1; i++ {
		s.call("shell_exec", map[string]any{"command": "ls"})
	}

	var wg sync.WaitGroup
	results := make([]string, 2)
	wg.Add(2)
	for i := range results {
		i := i
		go func() {
			defer wg.Done()
			results[i] = s.call("shell_exec", map[string]any{"command": "ls"})
		}()
	}
	wg.Wait()

	refused := 0
	for _, r := range results {
		if strings.Contains(r, "run out of steps") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("%d of the two concurrent calls at the cap were refused, want exactly 1", refused)
	}
	if len(s.Hops()) != maxAskIterations {
		t.Errorf("%d hops recorded, want exactly the cap of %d", len(s.Hops()), maxAskIterations)
	}
}

// A request that is not a POST to the tool path is refused, so nothing but the CLI Ora started can drive Ora's tools.
func TestClaudeToolServer_RefusesAnythingButAPostToItsOwnPath(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	get, err := http.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d", get.StatusCode)
	}
	guess, err := http.Post(strings.TrimSuffix(s.URL(), s.path)+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	guess.Body.Close()
	if guess.StatusCode != http.StatusNotFound {
		t.Errorf("a POST to a guessed path = %d", guess.StatusCode)
	}
}

// claudeStub answers as the CLI would: it does the MCP handshake against the tool server named in the arguments, calls the tools it was asked to call, and prints the result object `claude -p --output-format json` prints.
func claudeStub(callNames []string, result string, usage string) claudeRunner {
	return func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		url := ""
		for i, arg := range args {
			if arg == "--mcp-config" {
				raw, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				var cfg struct {
					MCPServers map[string]struct {
						URL string `json:"url"`
					} `json:"mcpServers"`
				}
				if err := json.Unmarshal(raw, &cfg); err != nil {
					return nil, err
				}
				url = cfg.MCPServers["ora"].URL
			}
		}
		if url == "" {
			return nil, errors.New("the stub was given no mcp config")
		}
		post := func(method string, id int, params any) error {
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
			resp, err := http.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				return err
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		if err := post("initialize", 0, map[string]any{"protocolVersion": "2025-11-25"}); err != nil {
			return nil, err
		}
		if err := post("tools/list", 1, nil); err != nil {
			return nil, err
		}
		for i, name := range callNames {
			if err := post("tools/call", 2+i, map[string]any{"name": name, "arguments": map[string]any{"purpose": "looking"}}); err != nil {
				return nil, err
			}
		}
		out, _ := json.Marshal(json.RawMessage(fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":%s,"usage":%s}`, mustJSON(result), usage)))
		return out, nil
	}
}

// mustJSON is s as a JSON string literal, for building a canned CLI reply in a test.
func mustJSON(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

// A whole ask through the CLI fills the trace: the answer, the tools the model ran with their results, the recalled lines, the model name and what the run cost in tokens.
func TestAskClaude_RunsToolsAndFillsTheTrace(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{retrieveRelevantResult: []string{"recalled line"}}, nil, "")
	usage := `{"input_tokens":4,"output_tokens":106,"cache_creation_input_tokens":7283,"cache_read_input_tokens":7143}`
	run := claudeStub([]string{"observe_screen"}, "  Brave is in front.  ", usage)

	tr, err := a.askClaude(t.Context(), run, "sonnet", nil, "what window is in front")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "Brave is in front." || tr.Model != "claude/sonnet" || tr.Channel != ChannelText || tr.Question != "what window is in front" || tr.Duration <= 0 {
		t.Errorf("trace = %+v", tr)
	}
	if len(tr.ToolHops) != 1 || tr.ToolHops[0].Name != "observe_screen" {
		t.Errorf("hops = %+v", tr.ToolHops)
	}
	if len(tr.Injected) != 1 || tr.Injected[0] != "recalled line" {
		t.Errorf("injected = %v", tr.Injected)
	}
	// The whole input is what it read afresh plus what it wrote into and read out of its prompt cache, and the cached part is counted inside that rather than beside it.
	if tr.Usage != (TokenUsage{Provider: ProviderClaude, InputTokens: 14430, OutputTokens: 106, TotalTokens: 14536, Rounds: 2, CachedInputTokens: 7143}) {
		t.Errorf("usage = %+v", tr.Usage)
	}
}

// The arguments the CLI is run with keep the subscription login, offer only Ora's own tools, and shut out the user's own settings, hooks, skills and MCP servers, because the prompt carries text nobody vetted.
// One askClaude call has to get the CLI invocation right in three unrelated ways at once: run under the subscription with only Ora's tools allowed, and do it through a temp dir that exists while the CLI is meant to be reading it and is gone once the ask ends (see TestAskClaude_PutsTheMCPConfigAndSystemPromptInFilesNotArgv for what that dir must hold).
func TestAskClaude_RunsTheCLIUnderTheSubscriptionWithOnlyOraTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var seen []string
	var prompt, dir string
	var existedDuringTheRun bool
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		seen, prompt = args, stdin
		for i, arg := range args {
			if arg == "--mcp-config" {
				dir = filepath.Dir(args[i+1])
			}
		}
		if dir != "" {
			_, err := os.Stat(dir)
			existedDuringTheRun = err == nil
		}
		return []byte(`{"result":"done","is_error":false}`), nil
	}
	if _, err := a.askClaude(t.Context(), run, "sonnet", nil, "hello"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, " ")
	for _, want := range []string{"-p", "--output-format json", "--model sonnet", "--strict-mcp-config", "--restricted", "--disable-slash-commands", "--no-session-persistence", "--permission-prompts none"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the arguments are missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--bare") {
		t.Errorf("--bare would bill the API key instead of the subscription: %s", joined)
	}
	if !strings.Contains(joined, "mcp__ora__observe_screen") {
		t.Errorf("Ora's tools were not allowed: %s", joined)
	}
	if !strings.HasSuffix(prompt, "hello") {
		t.Errorf("the question is not the last thing the model reads: %q", prompt)
	}

	if dir == "" {
		t.Fatal("never saw the mcp config path")
	}
	if !existedDuringTheRun {
		t.Fatal("the temp dir did not exist while the CLI was meant to be reading it")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the ask's temp dir still exists after the ask ended: %v", err)
	}
}

// The tool server's URL and the system prompt (personal context included) never sit in argv: any local process can read another process's argv for the life of a run via /proc/<pid>/cmdline on Linux, so both go into 0600 files inside a temp directory instead, and only the file paths are on the command line.
func TestAskClaude_PutsTheMCPConfigAndSystemPromptInFilesNotArgv(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{personal: map[string]string{"home": "the user's home address is 42 Example Street"}}, nil, "")
	var seenArgs []string
	var mcpConfigPath, systemPromptPath string
	// The temp dir is removed the moment askClaude returns, so the files can only be inspected from inside run, while the CLI is meant to be reading them.
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		seenArgs = args
		for i, arg := range args {
			switch arg {
			case "--mcp-config":
				mcpConfigPath = args[i+1]
			case "--system-prompt-file":
				systemPromptPath = args[i+1]
			}
		}
		if mcpConfigPath == "" || systemPromptPath == "" {
			t.Fatal("the CLI was not pointed at either file")
		}
		for _, path := range []string{mcpConfigPath, systemPromptPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600 so no other user on the machine can read it", path, info.Mode().Perm())
			}
		}
		config, err := os.ReadFile(mcpConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(config), "http://127.0.0.1") {
			t.Errorf("mcp config file = %q, want it to carry the tool server's URL", config)
		}
		instruction, err := os.ReadFile(systemPromptPath)
		if err != nil {
			t.Fatal(err)
		}
		// Claude is a text ask, so its instruction is the lean prompt (see LeanPrompt in ask.go): the persona and the tool guidance, but not the personal-context store.
		if !strings.Contains(string(instruction), "You are Ora.") {
			t.Errorf("system prompt file = %q, want the lean instruction in it", instruction)
		}
		if strings.Contains(string(instruction), "42 Example Street") {
			t.Errorf("system prompt file = %q, a text ask's instruction must not carry the personal-context store", instruction)
		}
		return []byte(`{"result":"done","is_error":false}`), nil
	}
	if _, err := a.askClaude(t.Context(), run, "sonnet", nil, "hello"); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(seenArgs, " ")
	if strings.Contains(joined, "http://127.0.0.1") {
		t.Errorf("argv carries the tool server's own URL: %s", joined)
	}
	if strings.Contains(joined, "42 Example Street") {
		t.Errorf("argv carries the user's personal context: %s", joined)
	}
	if strings.Contains(joined, "--system-prompt ") {
		t.Errorf("the system prompt is still on argv rather than in a file: %s", joined)
	}
}

// The prior turns go in ahead of the question, so a follow-up reads as a follow-up.
func TestAskClaudeWith_SendsThePriorTurns(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var prompt string
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		prompt = stdin
		return []byte(`{"result":"done","is_error":false}`), nil
	}
	history := HistoryFromTurns([]db.Turn{{Role: "you", Text: "who did I meet"}, {Role: "ora", Text: "Vexil"}})
	if _, err := a.askClaude(t.Context(), run, "sonnet", history, "when"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "who did I meet") || !strings.Contains(prompt, "Vexil") {
		t.Errorf("the thread is missing from the prompt: %q", prompt)
	}
	if strings.Index(prompt, "Vexil") > strings.Index(prompt, "when") {
		t.Errorf("the thread came after the question: %q", prompt)
	}
}

// claudeCapturingRun returns a runner that reads the system prompt file and lists the tools the tool server offers, for a test that needs to see what an askClaude call sent without driving a whole tool call through the stub.
func claudeCapturingRun(t *testing.T, systemPrompt *string, toolNames *[]string) claudeRunner {
	t.Helper()
	return func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		var url string
		for i, arg := range args {
			switch arg {
			case "--system-prompt-file":
				raw, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				*systemPrompt = string(raw)
			case "--mcp-config":
				raw, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				var cfg struct {
					MCPServers map[string]struct {
						URL string `json:"url"`
					} `json:"mcpServers"`
				}
				if err := json.Unmarshal(raw, &cfg); err != nil {
					return nil, err
				}
				url = cfg.MCPServers["ora"].URL
			}
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var reply struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			return nil, err
		}
		for _, tool := range reply.Result.Tools {
			*toolNames = append(*toolNames, tool.Name)
		}
		return []byte(`{"result":"done","is_error":false}`), nil
	}
}

// A screen question ("open spotify and play back in black") is recognised as a screen task before any tool has run, so it gets the short screen prompt rather than the full handshake, and the tool server offers it only the screen tools plus the memory-reading tools, not the full ask tool set.
func TestAskClaude_ScreenQuestionGetsTheShortPromptAndScreenTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var systemPrompt string
	var toolNames []string
	run := claudeCapturingRun(t, &systemPrompt, &toolNames)
	if _, err := a.askClaude(t.Context(), run, "sonnet", nil, "open spotify and play back in black"); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(systemPrompt, "<persona>") || strings.Contains(systemPrompt, "memory_guidelines") {
		t.Errorf("system prompt still carries the persona/memory teaching text: %q", systemPrompt)
	}
	if !strings.Contains(systemPrompt, "working the user's screen for them") || !strings.Contains(systemPrompt, "Never click anything that sends, pays, deletes or submits") {
		t.Errorf("system prompt = %q, want the screen task guidance and the stop line", systemPrompt)
	}

	got := make(map[string]bool, len(toolNames))
	for _, name := range toolNames {
		got[name] = true
	}
	if !got["click"] || !got["query_memory"] {
		t.Errorf("tools/list = %v, want click and query_memory", toolNames)
	}
	if got["delegate"] || got["branch"] {
		t.Errorf("tools/list = %v, want no delegate or web-only tools on a screen round", toolNames)
	}
}

// A memory question ("what is my manager's name") is not a screen task, so it keeps the full handshake prompt and every tool the ask gate allows.
func TestAskClaude_MemoryQuestionGetsTheFullPromptAndAllTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var systemPrompt string
	var toolNames []string
	run := claudeCapturingRun(t, &systemPrompt, &toolNames)
	if _, err := a.askClaude(t.Context(), run, "sonnet", nil, "what is my manager's name"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(systemPrompt, "<persona>") || !strings.Contains(systemPrompt, "memory_guidelines") {
		t.Errorf("system prompt = %q, want the full handshake teaching", systemPrompt)
	}

	if len(toolNames) != len(a.askToolDeclarations()) {
		t.Errorf("tools/list = %d tools, want the full %d the ask gate allows", len(toolNames), len(a.askToolDeclarations()))
	}
}

// A CLI run that failed is an error carrying what it said, not an answer.
func TestAskClaude_ReportsAFailedRun(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		return []byte(`{"is_error":true,"subtype":"error_during_execution","result":"Claude AI usage limit reached"}`), nil
	}
	tr, err := a.askClaude(t.Context(), run, "sonnet", nil, "hello")
	if err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Errorf("err = %v", err)
	}
	if tr.Answer != "" {
		t.Errorf("answer = %q", tr.Answer)
	}
}

// MCP hands a picture back as an image content item beside the text, which is how the Claude command line gets to see the screen a look took.
func TestClaudeToolServer_ReturnsTheLookPictureAsAnImage(t *testing.T) {
	a, _, _ := lookingAgent(t)
	// askClaude attaches this before it ever starts the tool server; done here too so the look the test drives through the server has somewhere to leave its picture.
	s, err := a.startClaudeToolServer(withAskLookState(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	call := rpcPost(t, s, "tools/call", 2, map[string]any{"name": "look", "arguments": map[string]any{}})
	content := call["result"].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("%d content items, want the text and the picture", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("first item = %v, want the text result", content[0])
	}
	img := content[1].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/jpeg" {
		t.Errorf("second item = %v, want an image/jpeg", img)
	}
	if img["data"] != base64.StdEncoding.EncodeToString([]byte("fake-jpeg-bytes")) {
		t.Errorf("data = %v, want the picture's bytes in base64", img["data"])
	}
}

// recordedUsage is a UsageRecorder that keeps what it was handed, so a test can check what a refresh recorded.
type recordedUsage struct {
	mu        sync.Mutex
	byName    map[string][]UsageLimit
	signedOut string
}

func (r *recordedUsage) RecordSignedOut(provider, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signedOut = provider
}

func (r *recordedUsage) Record(provider string, limits []UsageLimit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		r.byName = map[string][]UsageLimit{}
	}
	r.byName[provider] = limits
}

// writeClaudeCredentials writes a stand-in for ~/.claude/.credentials.json holding token as the subscription's OAuth access token, and returns its path.
func writeClaudeCredentials(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","expiresAt":9999999999999,"subscriptionType":"max"}}`, token)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

// TestClaudeUsage_ReadsTheOAuthUsageEndpoint checks the subscription's own usage windows are read the way Claude Code's /usage reads them, and become the bars the picker draws. The endpoint, its headers and its response shape are the undocumented OAuth usage API recorded at https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor/issues/202: GET https://api.anthropic.com/api/oauth/usage with Authorization: Bearer <oauth access token>, anthropic-beta: oauth-2025-04-20 and User-Agent: claude-code/<version>, answering {"five_hour":{"utilization":65,"resets_at":"..."},"seven_day":{...},"seven_day_opus":null,"seven_day_sonnet":{...}} where utilization is a percentage from 0 to 100.
func TestClaudeUsage_ReadsTheOAuthUsageEndpoint(t *testing.T) {
	const token = "sk-ant-oat-do-not-leak"
	var gotAuth, gotBeta, gotAgent, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta, gotAgent, gotPath = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("User-Agent"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"five_hour": {"utilization": 65, "resets_at": "2026-09-05T17:56:00Z"},
			"seven_day": {"utilization": 37, "resets_at": "2026-09-08T09:00:00Z"},
			"seven_day_opus": null,
			"seven_day_sonnet": {"utilization": 12.5, "resets_at": "2026-09-08T09:00:00Z"},
			"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null}
		}`)
	}))
	defer srv.Close()

	limits, err := claudeUsage(context.Background(), srv.Client(), srv.URL+"/api/oauth/usage", writeClaudeCredentials(t, token))
	if err != nil {
		t.Fatalf("claudeUsage: %v", err)
	}
	if gotPath != "/api/oauth/usage" {
		t.Errorf("path = %q, want the oauth usage route", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("the request did not carry the subscription's bearer token")
	}
	if gotBeta != claudeOAuthBeta {
		t.Errorf("anthropic-beta = %q, want %q", gotBeta, claudeOAuthBeta)
	}
	if !strings.HasPrefix(gotAgent, "claude-code/") {
		t.Errorf("User-Agent = %q, want claude-code/<version>: the endpoint rate-limits anything else hard", gotAgent)
	}

	if len(limits) != 3 {
		t.Fatalf("limits = %+v, want the five-hour, weekly and weekly-sonnet windows and not the null opus one", limits)
	}
	if limits[0].Window != "5h" || limits[0].UsedFraction != 0.65 {
		t.Errorf("five-hour window = %+v, want 5h at 0.65", limits[0])
	}
	if want := time.Date(2026, 9, 5, 17, 56, 0, 0, time.UTC); !limits[0].ResetsAt.Equal(want) {
		t.Errorf("five-hour window resets at %v, want %v", limits[0].ResetsAt, want)
	}
	if limits[1].Window != "weekly" || limits[1].UsedFraction != 0.37 {
		t.Errorf("weekly window = %+v, want weekly at 0.37", limits[1])
	}
	if limits[2].Window != "weekly_sonnet" || limits[2].UsedFraction != 0.125 {
		t.Errorf("per-model weekly window = %+v, want weekly_sonnet at 0.125", limits[2])
	}
}

// TestRefreshClaudeUsage_NeverLogsTheToken checks the failure path says what went wrong without the access token in it: this is the one place in Ora that reads ~/.claude/.credentials.json, and a token in ora.log would outlive the run.
func TestRefreshClaudeUsage_NeverLogsTheToken(t *testing.T) {
	const token = "sk-ant-oat-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A refusal that echoes the request back is the worst case: the body an error message might quote holds the token.
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"message":"invalid bearer %s"}}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(restore)

	rec := &recordedUsage{}
	refreshClaudeUsage(context.Background(), srv.Client(), srv.URL+"/api/oauth/usage", writeClaudeCredentials(t, token), rec)

	if strings.Contains(logged.String(), token) {
		t.Fatalf("the access token reached a log line: %s", logged.String())
	}
	if len(rec.byName) != 0 {
		t.Errorf("a failed read recorded %+v, want nothing so the last good reading stands", rec.byName)
	}
}

// TestRefreshClaudeUsage_PollsAtMostEveryTenMinutes checks a window polling /brains every few seconds does not poll Anthropic with it.
func TestRefreshClaudeUsage_PollsAtMostEveryTenMinutes(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"five_hour":{"utilization":1,"resets_at":"2026-09-05T17:56:00Z"}}`)
	}))
	defer srv.Close()

	claudeUsagePolled.Lock()
	claudeUsagePolled.at = time.Time{}
	claudeUsagePolled.Unlock()

	rec := &recordedUsage{}
	creds := writeClaudeCredentials(t, "sk-ant-oat-do-not-leak")
	for range 3 {
		refreshClaudeUsage(context.Background(), srv.Client(), srv.URL+"/api/oauth/usage", creds, rec)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("three refreshes made %d requests, want 1", got)
	}
	if len(rec.byName[ProviderClaude]) != 1 {
		t.Errorf("recorded %+v, want the one window the endpoint named", rec.byName)
	}
}
