package agent

import (
	"encoding/json"
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

// A whole ask through the CLI fills the trace: the answer, the tools the model ran with their results, the recalled lines, and the model name.
func TestAskAgy_RunsToolsAndFillsTheTrace(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{retrieveRelevantResult: []string{"recalled line"}}, nil, "")
	fake := &fakeAgySession{
		responses: []string{`{"status":"SUCCESS","response":"  Brave is in front.  "}`},
		toolCalls: map[int][]string{0: {"observe_screen"}},
	}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)

	tr, err := a.askAgy(t.Context(), newProc, "gemini-3-pro", nil, "what window is in front")
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

// Ora's own instruction goes at the head of the prompt text, because agy has no system-prompt flag of its own, and the question is still the last thing the model reads.
// agy is a text ask, so its instruction is the lean prompt (see LeanPrompt in ask.go): personal context does not belong in it, only the persona, the tool guidance and the stop line — a fact from the personal-context store must not be in the prompt at all.
func TestAskAgy_PutsTheInstructionAtTheHeadOfThePromptAndNeverSkipsPermissions(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{personal: map[string]string{"home": "the user's home address is 42 Example Street"}}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"done"}`}}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)
	if _, err := a.askAgy(t.Context(), newProc, "", nil, "hello"); err != nil {
		t.Fatal(err)
	}
	prompt := agyLineText(t, fake.sends[0])
	if !strings.Contains(prompt, "You are Ora.") {
		t.Errorf("the instruction is not in the prompt: %q", prompt)
	}
	if strings.Contains(prompt, "42 Example Street") {
		t.Errorf("a text ask's prompt must not carry the personal-context store: %q", prompt)
	}
	if !strings.HasSuffix(prompt, "hello") {
		t.Errorf("the question is not the last thing the model reads: %q", prompt)
	}
	if strings.Index(prompt, "You are Ora.") > strings.LastIndex(prompt, "hello") {
		t.Errorf("the instruction came after the question: %q", prompt)
	}
	if strings.Contains(prompt, "--dangerously-skip-permissions") {
		t.Errorf("agy must never be given a blanket permission skip: %q", prompt)
	}
}

// The prior turns go in ahead of the question, so a follow-up reads as a follow-up.
func TestAskAgyWith_SendsThePriorTurns(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"done"}`}}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)
	history := HistoryFromTurns([]db.Turn{{Role: "you", Text: "who did I meet"}, {Role: "ora", Text: "Vexil"}})
	if _, err := a.askAgy(t.Context(), newProc, "", history, "when"); err != nil {
		t.Fatal(err)
	}
	prompt := fake.sends[0]
	if !strings.Contains(prompt, "who did I meet") || !strings.Contains(prompt, "Vexil") {
		t.Errorf("the thread is missing from the prompt: %q", prompt)
	}
	if strings.Index(prompt, "Vexil") > strings.LastIndex(prompt, "when") {
		t.Errorf("the thread came after the question: %q", prompt)
	}
}

// A run that reports anything but SUCCESS is an error carrying what it said, not an answer.
func TestAskAgy_ReportsAFailedRun(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"ERROR","response":"the plan's daily quota is spent"}`}}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)
	tr, err := a.askAgy(t.Context(), newProc, "", nil, "hello")
	if err == nil || !strings.Contains(err.Error(), "quota is spent") {
		t.Errorf("err = %v", err)
	}
	if tr.Answer != "" {
		t.Errorf("answer = %q", tr.Answer)
	}
}

// The trace records what the run cost. The cache read is input, because it is what the model read; the thinking is not added to the output, because agy already counts it there; and agy's own total_tokens is not used, because it leaves the cache read out.
func TestAskAgy_RecordsWhatTheRunCost(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	fake := &fakeAgySession{responses: []string{`{"status":"SUCCESS","response":"hi","num_turns":2,"usage":{"input_tokens":6436,"output_tokens":35,"thinking_tokens":26,"cache_read_tokens":8127,"total_tokens":6471}}`}}
	newProc := func() agySessionRunner { return fake }
	t.Cleanup(a.CloseAgySession)
	tr, err := a.askAgy(t.Context(), newProc, "", nil, "hello")
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

// Antigravity reports its own plan allowance to whatever statusline command it is given, on every run including a --print one, and nowhere else: the stream-json events carry token counts only, and the endpoint behind the numbers (v1internal:retrieveUserQuotaSummary) needs the OAuth token out of the user's keyring. So Ora supplies the statusline command itself, in the throwaway HOME it already builds for every agy run, and reads the payload agy pipes to it. That works on any machine with agy installed, whether or not the user has a statusline of their own, and costs nothing beyond the ask that was happening anyway.
// This is the payload agy 1.2.3 wrote on 2026-09-15, trimmed to the fields read. Four windows: five-hour and weekly, each split between the Gemini models and the third-party ones the Antigravity plan also carries.
func TestAgyQuotaLimits_ReadsBothWindowsForBothModelFamilies(t *testing.T) {
	payload := `{"product":"antigravity","plan_tier":"Google AI Pro","quota":{
		"3p-5h":{"remaining_fraction":1,"reset_time":"2026-09-15T15:46:20Z"},
		"3p-weekly":{"remaining_fraction":0.98339945,"reset_time":"2026-09-17T11:36:03Z"},
		"gemini-5h":{"remaining_fraction":0.961293,"reset_time":"2026-09-15T10:59:10Z"},
		"gemini-weekly":{"remaining_fraction":0.9560392,"reset_time":"2026-09-19T09:13:44Z"}}}`

	limits := agyQuotaLimits([]byte(payload))
	byWindow := map[string]UsageLimit{}
	for _, l := range limits {
		byWindow[l.Window] = l
	}
	if len(limits) != 4 {
		t.Fatalf("read %d windows from the payload, want 4: %+v", len(limits), limits)
	}
	// Ora draws how much is spent; agy reports how much is left.
	if got := byWindow["gemini-5h"].UsedFraction; got < 0.038 || got > 0.039 {
		t.Errorf("gemini-5h used = %v, want 1 - 0.961293", got)
	}
	if got := byWindow["3p-5h"].UsedFraction; got != 0 {
		t.Errorf("3p-5h used = %v, want 0 for an untouched window", got)
	}
	if byWindow["gemini-weekly"].ResetsAt.IsZero() {
		t.Error("gemini-weekly carries no reset time, so the window cannot say when the bar refills")
	}
	if byWindow["3p-weekly"].Source == "" {
		t.Error("3p-weekly names no source, so a number on screen cannot be traced back")
	}
}

// The throwaway HOME carries Ora's own statusline command, which is how the run's plan allowance is read, while everything else the user has under antigravity-cli is still reachable. settings.json is Ora's own file rather than a symlink, and it keeps the settings the user already had: it used to be symlinked through whole, so replacing it with a bare statusline would silently drop their theme, their model default and everything else in there.
func TestBuildAgyHome_CarriesOrasStatusLineAndKeepsTheUsersSettings(t *testing.T) {
	realHome := t.TempDir()
	realCLI := filepath.Join(realHome, ".gemini", "antigravity-cli")
	mustMkdir(t, filepath.Join(realCLI, "brain"))
	mustWrite(t, filepath.Join(realCLI, "brain", "b1"), "a conversation")
	mustWrite(t, filepath.Join(realCLI, "settings.json"), `{"theme":"midnight","statusLine":{"command":"the user's own"}}`)

	tempHome := t.TempDir()
	if err := buildAgyHome(realHome, tempHome, "http://127.0.0.1:9999/abc"); err != nil {
		t.Fatal(err)
	}

	// Everything else under antigravity-cli is still the user's own, by symlink.
	if _, err := os.Readlink(filepath.Join(tempHome, ".gemini", "antigravity-cli", "brain")); err != nil {
		t.Errorf("the run cannot reach the user's own antigravity-cli entries: %v", err)
	}

	settings := filepath.Join(tempHome, ".gemini", "antigravity-cli", "settings.json")
	info, err := os.Lstat(settings)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("settings.json is a symlink, so writing Ora's statusline would edit the user's own file")
	}
	var got map[string]any
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("settings.json is not JSON: %v (%s)", err, data)
	}
	if got["theme"] != "midnight" {
		t.Errorf("the user's own settings were dropped: %v", got)
	}
	line, _ := got["statusLine"].(map[string]any)
	if line["command"] != agyStatusLine(tempHome) {
		t.Errorf("statusLine command = %v, want Ora's own %q", line["command"], agyStatusLine(tempHome))
	}
}
