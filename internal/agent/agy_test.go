package agent

import (
	"strings"
	"testing"
)

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

// Antigravity reports its own plan allowance to whatever statusline command it is given, on every run including a --print one, and nowhere else: the stream-json events carry token counts only, and the endpoint behind the numbers (v1internal:retrieveUserQuotaSummary) needs the OAuth token out of the user's keyring. So June supplies the statusline command itself, in the throwaway HOME it already builds for every agy run, and reads the payload agy pipes to it. That works on any machine with agy installed, whether or not the user has a statusline of their own, and costs nothing beyond the ask that was happening anyway.
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
	// June draws how much is spent; agy reports how much is left.
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
