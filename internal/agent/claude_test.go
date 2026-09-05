package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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

// A tools/call runs the tool through the ask's own gate, answers with its result as MCP text content, and records the call as a tool hop the trace can carry.
func TestClaudeToolServer_RunsAToolThroughTheGateAndRecordsTheHop(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	call := rpcPost(t, s, "tools/call", 2, map[string]any{"name": "shell_exec", "arguments": map[string]any{"command": "ls"}})
	content := call["result"].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "not available in an ask") {
		t.Errorf("result text = %q", text)
	}
	hops := s.Hops()
	if len(hops) != 1 || hops[0].Name != "shell_exec" || hops[0].Args["command"] != "ls" || hops[0].Result != text {
		t.Errorf("hops = %+v", hops)
	}
}

// Past the step cap the server stops running tools and says so, so a model that keeps calling cannot spend the user's machine without end.
func TestClaudeToolServer_StopsAtTheStepCap(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var last string
	for i := 0; i < maxAskIterations+1; i++ {
		call := rpcPost(t, s, "tools/call", 3, map[string]any{"name": "shell_exec", "arguments": map[string]any{"command": "ls"}})
		last = call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if !strings.Contains(last, "no steps left") {
		t.Errorf("the call past the cap answered %q", last)
	}
	if len(s.Hops()) != maxAskIterations {
		t.Errorf("%d hops recorded, cap is %d", len(s.Hops()), maxAskIterations)
	}
}

// Two tool calls that arrive at once when the run is one step below its cap must not both spend the last step: the check and the reservation of the step have to happen in the same locked section, or two calls can each see one step left and both take it, running one more tool than the cap allows.
func TestClaudeToolServer_TwoConcurrentCallsAtCapMinusOneOnlyOneWins(t *testing.T) {
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
		if strings.Contains(r, "no steps left") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("%d of the two concurrent calls at the last step were refused, want exactly 1", refused)
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
func TestAskClaude_RunsTheCLIUnderTheSubscriptionWithOnlyOraTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var seen []string
	var prompt string
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		seen, prompt = args, stdin
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
		if !strings.Contains(string(instruction), "42 Example Street") {
			t.Errorf("system prompt file = %q, want the personal context in it", instruction)
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

// The temp directory holding the two files is removed once the ask ends, so no stray file carrying the user's personal context survives the process.
func TestAskClaude_RemovesItsTempDirWhenTheAskEnds(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var dir string
	var existedDuringTheRun bool
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
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

// The prior turns go in ahead of the question, so a follow-up reads as a follow-up.
func TestAskClaudeWith_SendsThePriorTurns(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var prompt string
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		prompt = stdin
		return []byte(`{"result":"done","is_error":false}`), nil
	}
	history := HistoryFromTurns([]db.Turn{{Role: "you", Text: "who did I meet"}, {Role: "ora", Text: "Priya"}})
	if _, err := a.askClaude(t.Context(), run, "sonnet", history, "when"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "who did I meet") || !strings.Contains(prompt, "Priya") {
		t.Errorf("the thread is missing from the prompt: %q", prompt)
	}
	if strings.Index(prompt, "Priya") > strings.Index(prompt, "when") {
		t.Errorf("the thread came after the question: %q", prompt)
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

// Claude is the last resort after Codex: only when Codex refused because the allowance is spent, only before a tool has run so no action is taken twice, and only when the machine has a Claude login.
func TestClaudeFallbackWanted(t *testing.T) {
	spent := codexHTTPError{Code: http.StatusTooManyRequests}
	other := codexHTTPError{Code: http.StatusBadGateway}
	cases := []struct {
		name     string
		err      error
		hops     int
		loggedIn bool
		want     bool
	}{
		{"a spent allowance before any tool ran", spent, 0, true, true},
		{"a spent allowance after a tool ran", spent, 1, true, false},
		{"a spent allowance with no claude login", spent, 0, false, false},
		{"any other failure", other, 0, true, false},
		{"no failure at all", nil, 0, true, false},
	}
	for _, c := range cases {
		if got := claudeFallbackWanted(c.err, c.hops, c.loggedIn); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// The brain wrapper the daemon registers forwards to the agent, with and without a thread.
func TestClaudeBrain_ForwardsToTheAgent(t *testing.T) {
	var b any = ClaudeBrain{}
	if _, ok := b.(interface {
		AskText(context.Context, string) (TurnTrace, error)
	}); !ok {
		t.Errorf("ClaudeBrain does not answer an ask")
	}
	if _, ok := b.(interface {
		AskTextWith(context.Context, History, string) (TurnTrace, error)
	}); !ok {
		t.Errorf("ClaudeBrain does not answer an ask with a thread")
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

// A tool that took no picture sends no picture: only the text goes back, as it always did.
func TestClaudeToolServer_SendsNoImageForAToolThatTookNone(t *testing.T) {
	a, _, _ := lookingAgent(t)
	s, err := a.startClaudeToolServer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	call := rpcPost(t, s, "tools/call", 2, map[string]any{"name": "observe_screen", "arguments": map[string]any{}})
	if content := call["result"].(map[string]any)["content"].([]any); len(content) != 1 {
		t.Errorf("%d content items, want the text alone", len(content))
	}
}

// A CLI error long enough to be cut is cut on a rune boundary, not a byte one: 299 ASCII characters followed by a two-byte rune used to be sliced through the middle of that rune and hand the log invalid UTF-8.
func TestClaudeHead_CutsOnARuneBoundary(t *testing.T) {
	got := claudeHead(strings.Repeat("a", 299) + "é" + "tail")
	if !utf8.ValidString(got) {
		t.Errorf("claudeHead returned invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", 299) + "é" + "…"; got != want {
		t.Errorf("claudeHead = %q, want the first 300 runes plus an ellipsis", got)
	}
}

// Whitespace is still squeezed out, and a short line comes back whole with no ellipsis.
func TestClaudeHead_FlattensAndLeavesShortTextAlone(t *testing.T) {
	if got := claudeHead("  error:\n  could not\tstart\n"); got != "error: could not start" {
		t.Errorf("claudeHead = %q, want the words on one line", got)
	}
}
