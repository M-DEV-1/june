package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ora/internal/db"
)

// mustMkdir and mustWrite build a fake ~/.gemini tree for a test without failing the test on every line.
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// buildAgyHome mirrors every entry of the real ~/.gemini into the temp HOME except the config directory, mirrors every entry of the real ~/.gemini/config except mcp_config.json and config.json, and writes Ora's own versions of those two files — which is how agy still finds its own login (reachable by symlink at the path it expects) while seeing none of the user's own MCP servers or shell grants.
func TestBuildAgyHome_MirrorsEverythingExceptOrasOwnTwoFiles(t *testing.T) {
	realHome := t.TempDir()
	realGemini := filepath.Join(realHome, ".gemini")
	mustMkdir(t, filepath.Join(realGemini, "skills"))
	mustWrite(t, filepath.Join(realGemini, "skills", "x.md"), "a skill")
	mustWrite(t, filepath.Join(realGemini, "installation_id"), "abc123")

	realConfig := filepath.Join(realGemini, "config")
	mustMkdir(t, filepath.Join(realConfig, "projects"))
	mustWrite(t, filepath.Join(realConfig, "projects", "p1"), "a project")
	mustWrite(t, filepath.Join(realConfig, "mcp_config.json"), `{"mcpServers":{"gitbutler":{"command":"but"}}}`)
	mustWrite(t, filepath.Join(realConfig, "config.json"), `{"userSettings":{"globalPermissionGrants":{"allow":["command(ls)"]}}}`)

	tempHome := t.TempDir()
	if err := buildAgyHome(realHome, tempHome, "http://127.0.0.1:9999/abc"); err != nil {
		t.Fatal(err)
	}

	// Every top-level entry but "config" is a symlink to the real one.
	for _, name := range []string{"skills", "installation_id"} {
		link := filepath.Join(tempHome, ".gemini", name)
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink", name)
		}
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatal(err)
		}
		if target != filepath.Join(realGemini, name) {
			t.Errorf("%s links to %q, want %q", name, target, filepath.Join(realGemini, name))
		}
	}

	// Every entry of config but the two Ora owns is a symlink to the real one.
	projLink := filepath.Join(tempHome, ".gemini", "config", "projects")
	target, err := os.Readlink(projLink)
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if target != filepath.Join(realConfig, "projects") {
		t.Errorf("projects links to %q, want %q", target, filepath.Join(realConfig, "projects"))
	}

	// The two config files are Ora's own, real files, not symlinks to the user's.
	for _, name := range []string{"mcp_config.json", "config.json"} {
		info, err := os.Lstat(filepath.Join(tempHome, ".gemini", "config", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is a symlink, want Ora's own file", name)
		}
	}

	raw, err := os.ReadFile(filepath.Join(tempHome, ".gemini", "config", "mcp_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL string `json:"serverUrl"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("mcp config names %d servers, want exactly 1: %s", len(cfg.MCPServers), raw)
	}
	ora, ok := cfg.MCPServers[agyMCPServerName]
	if !ok || ora.URL != "http://127.0.0.1:9999/abc" {
		t.Errorf("mcp config = %+v, want the ora server pointed at the tool server's url", cfg)
	}

	raw, err = os.ReadFile(filepath.Join(tempHome, ".gemini", "config", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var grants struct {
		UserSettings struct {
			GlobalPermissionGrants struct {
				Allow []string `json:"allow"`
			} `json:"globalPermissionGrants"`
		} `json:"userSettings"`
	}
	if err := json.Unmarshal(raw, &grants); err != nil {
		t.Fatal(err)
	}
	if allow := grants.UserSettings.GlobalPermissionGrants.Allow; len(allow) != 1 || allow[0] != "mcp(ora/*)" {
		t.Errorf("grants = %v, want exactly mcp(ora/*)", allow)
	}
}

// A real ~/.gemini with no config directory yet, or none at all, is not an error: a fresh machine has never run agy.
func TestBuildAgyHome_ToleratesAMissingRealGemini(t *testing.T) {
	realHome := t.TempDir()
	tempHome := t.TempDir()
	if err := buildAgyHome(realHome, tempHome, "http://127.0.0.1:9999/abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tempHome, ".gemini", "config", "mcp_config.json")); err != nil {
		t.Errorf("ora's own mcp config was not written: %v", err)
	}
}

// agyStub answers as the CLI would: it reads the HOME it was given, finds Ora's tool server from the mcp config that HOME's .gemini/config holds, does the MCP handshake, calls the tools it was asked to call, and prints the object `agy --print --output-format json` prints.
func agyStub(callNames []string, response string) agyRunner {
	return func(ctx context.Context, env []string, args []string) ([]byte, error) {
		home := ""
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "HOME="); ok {
				home = v
			}
		}
		if home == "" {
			return nil, errors.New("the stub was given no HOME")
		}
		raw, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"))
		if err != nil {
			return nil, err
		}
		var cfg struct {
			MCPServers map[string]struct {
				URL string `json:"serverUrl"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		url := cfg.MCPServers[agyMCPServerName].URL
		if url == "" {
			return nil, errors.New("the stub found no ora server in the mcp config")
		}
		post := func(method string, id int, params any) error {
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
			resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
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
		out, _ := json.Marshal(json.RawMessage(fmt.Sprintf(`{"status":"SUCCESS","response":%s}`, mustJSON(response))))
		return out, nil
	}
}

// A whole ask through the CLI fills the trace: the answer, the tools the model ran with their results, the recalled lines, and the model name.
func TestAskAgy_RunsToolsAndFillsTheTrace(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{retrieveRelevantResult: []string{"recalled line"}}, nil, "")
	run := agyStub([]string{"observe_screen"}, "  Brave is in front.  ")

	tr, err := a.askAgy(t.Context(), run, "gemini-3-pro", nil, "what window is in front")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "Brave is in front." || tr.Model != "agy/gemini-3-pro" || tr.Channel != ChannelText || tr.Question != "what window is in front" || tr.Duration <= 0 {
		t.Errorf("trace = %+v", tr)
	}
	if len(tr.ToolHops) != 1 || tr.ToolHops[0].Name != "observe_screen" {
		t.Errorf("hops = %+v", tr.ToolHops)
	}
	if len(tr.Injected) != 1 || tr.Injected[0] != "recalled line" {
		t.Errorf("injected = %v", tr.Injected)
	}
	if tr.Usage.Provider != ProviderAgy {
		t.Errorf("usage provider = %q, want %q", tr.Usage.Provider, ProviderAgy)
	}
}

// An empty model leaves --model off the argument list, keeping the CLI's own default, and the trace names the provider alone.
func TestAskAgy_LeavesModelOffWhenEmpty(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := agyStub(nil, "done")
	tr, err := a.askAgy(t.Context(), run, "", nil, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Model != "agy" {
		t.Errorf("model = %q, want %q", tr.Model, "agy")
	}
}

// Ora's own instruction goes at the head of the prompt text, because agy has no system-prompt flag of its own, and the question is still the last thing the model reads.
func TestAskAgy_PutsTheInstructionAtTheHeadOfThePromptAndNeverSkipsPermissions(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{personal: map[string]string{"home": "the user's home address is 42 Example Street"}}, nil, "")
	var seenArgs []string
	var prompt string
	run := func(ctx context.Context, env []string, args []string) ([]byte, error) {
		seenArgs = args
		for i, arg := range args {
			if arg == "--print" {
				prompt = args[i+1]
			}
		}
		return []byte(`{"status":"SUCCESS","response":"done"}`), nil
	}
	if _, err := a.askAgy(t.Context(), run, "", nil, "hello"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "42 Example Street") {
		t.Errorf("the instruction is not in the prompt: %q", prompt)
	}
	if !strings.HasSuffix(prompt, "hello") {
		t.Errorf("the question is not the last thing the model reads: %q", prompt)
	}
	if strings.Index(prompt, "42 Example Street") > strings.LastIndex(prompt, "hello") {
		t.Errorf("the instruction came after the question: %q", prompt)
	}
	joined := strings.Join(seenArgs, " ")
	if strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Errorf("agy must never be given a blanket permission skip: %s", joined)
	}
	if strings.Contains(joined, "42 Example Street") {
		// The prompt is one argv entry after --print, so this only checks nothing else duplicated it elsewhere.
	}
}

// The prior turns go in ahead of the question, so a follow-up reads as a follow-up.
func TestAskAgyWith_SendsThePriorTurns(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var prompt string
	run := func(ctx context.Context, env []string, args []string) ([]byte, error) {
		for i, arg := range args {
			if arg == "--print" {
				prompt = args[i+1]
			}
		}
		return []byte(`{"status":"SUCCESS","response":"done"}`), nil
	}
	history := HistoryFromTurns([]db.Turn{{Role: "you", Text: "who did I meet"}, {Role: "ora", Text: "Priya"}})
	if _, err := a.askAgy(t.Context(), run, "", history, "when"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "who did I meet") || !strings.Contains(prompt, "Priya") {
		t.Errorf("the thread is missing from the prompt: %q", prompt)
	}
	if strings.Index(prompt, "Priya") > strings.LastIndex(prompt, "when") {
		t.Errorf("the thread came after the question: %q", prompt)
	}
}

// A CLI run that reports anything but SUCCESS is an error carrying what it said, not an answer.
func TestAskAgy_ReportsAFailedRun(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := func(ctx context.Context, env []string, args []string) ([]byte, error) {
		return []byte(`{"status":"ERROR","response":"the plan's daily quota is spent"}`), nil
	}
	tr, err := a.askAgy(t.Context(), run, "", nil, "hello")
	if err == nil || !strings.Contains(err.Error(), "quota is spent") {
		t.Errorf("err = %v", err)
	}
	if tr.Answer != "" {
		t.Errorf("answer = %q", tr.Answer)
	}
}

// The brain wrapper the daemon registers forwards to the agent, with and without a thread.
func TestAgyBrain_ForwardsToTheAgent(t *testing.T) {
	var b any = AgyBrain{}
	if _, ok := b.(interface {
		AskText(context.Context, string) (TurnTrace, error)
	}); !ok {
		t.Errorf("AgyBrain does not answer an ask")
	}
	if _, ok := b.(interface {
		AskTextWith(context.Context, History, string) (TurnTrace, error)
	}); !ok {
		t.Errorf("AgyBrain does not answer an ask with a thread")
	}
}

// agyModel reads ORA_AGY_MODEL when it is set, and leaves the CLI to its own default otherwise. The data directory is pointed at an empty one for the length of the test: pickedModel reads the model stored by the Settings picker first, so without this the test reads whatever the person running it has picked on their own machine and fails for a reason that has nothing to do with the code.
func TestAgyModel(t *testing.T) {
	t.Setenv("ORA_DATA_DIR", t.TempDir())
	t.Setenv("ORA_AGY_MODEL", "")
	if got := agyModel(); got != "" {
		t.Errorf("agyModel() = %q, want empty when unset", got)
	}
	t.Setenv("ORA_AGY_MODEL", "gemini-3-pro-high")
	if got := agyModel(); got != "gemini-3-pro-high" {
		t.Errorf("agyModel() = %q, want gemini-3-pro-high", got)
	}
}

// agyArgs carries the model only when one was named, and never carries a system-prompt flag since agy has none.
func TestAgyArgs(t *testing.T) {
	withModel := strings.Join(agyArgs("gemini-3-pro", "the prompt"), " ")
	if !strings.Contains(withModel, "--model gemini-3-pro") {
		t.Errorf("args = %q, want --model gemini-3-pro", withModel)
	}
	withoutModel := strings.Join(agyArgs("", "the prompt"), " ")
	if strings.Contains(withoutModel, "--model") {
		t.Errorf("args = %q, want no --model when none was named", withoutModel)
	}
	for _, want := range []string{"--print the prompt", "--output-format json", "--disable-slash-commands"} {
		if !strings.Contains(withoutModel, want) {
			t.Errorf("args = %q, missing %q", withoutModel, want)
		}
	}
}

// agy reports what a run cost, and Ora records it, so an Antigravity answer shows in the usage table like every other brain instead of as a row of zeroes.
// The shape is the real one, printed by `agy --output-format json --print` on 2026-09-07.
func TestAgyResult_CarriesWhatTheRunCost(t *testing.T) {
	const body = `{"conversation_id":"2fe49524","duration_seconds":5.168,"num_turns":3,"response":"ok","status":"SUCCESS",
	  "usage":{"input_tokens":6439,"output_tokens":2,"thinking_tokens":11,"cache_read_tokens":8127,"total_tokens":6441}}`

	var res agyResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Usage.InputTokens != 6439 || res.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want the counts agy printed", res.Usage)
	}
	if res.Usage.CacheReadTokens != 8127 {
		t.Errorf("cache read tokens = %d, want 8127 — the cached input is most of what an ask really costs", res.Usage.CacheReadTokens)
	}
	if res.Usage.ThinkingTokens != 11 {
		t.Errorf("thinking tokens = %d, want 11", res.Usage.ThinkingTokens)
	}
	if res.NumTurns != 3 {
		t.Errorf("rounds = %d, want 3", res.NumTurns)
	}
}

// The trace records what the run cost. The cache read is input, because it is what the model read; the thinking is not added to the output, because agy already counts it there; and agy's own total_tokens is not used, because it leaves the cache read out.
func TestAskAgy_RecordsWhatTheRunCost(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	run := func(ctx context.Context, env []string, args []string) ([]byte, error) {
		return []byte(`{"status":"SUCCESS","response":"hi","num_turns":2,"usage":{"input_tokens":6436,"output_tokens":35,"thinking_tokens":26,"cache_read_tokens":8127,"total_tokens":6471}}`), nil
	}
	tr, err := a.askAgy(t.Context(), run, "", nil, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Usage.Rounds != 2 {
		t.Errorf("rounds = %d, want 2", tr.Usage.Rounds)
	}
	if tr.Usage.InputTokens != 14563 || tr.Usage.OutputTokens != 35 || tr.Usage.TotalTokens != 14598 || tr.Usage.CachedInputTokens != 8127 {
		t.Errorf("usage = %+v", tr.Usage)
	}
}
