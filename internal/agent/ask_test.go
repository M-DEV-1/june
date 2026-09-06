package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/util"

	"google.golang.org/genai"
)

func TestHandshakePrompt_IncludesImplicitContext(t *testing.T) {
	brain := &toolTestBrain{implicitContext: []string{"[now] Climate Risk Statement Builder ASRS"}}
	a := NewAgent(nil, nil, brain, nil, "")
	instruction, lines := a.HandshakePrompt(t.Context(), time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC))
	if !containsLine(lines, "Climate Risk Statement Builder ASRS") {
		t.Fatalf("expected handshake context lines to carry working state, got %q", lines)
	}
	if !strings.Contains(instruction, "Climate Risk Statement Builder ASRS") {
		t.Fatalf("expected frozen system instruction to embed handshake context, got %q", instruction)
	}
	if !strings.Contains(instruction, "Wednesday, 19 August 2026") {
		t.Errorf("expected date anchor in instruction, got %q", instruction)
	}
}

func TestEvalExecute_BlocksNonMemoryTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := a.evalExecute(t.Context(), "shell_exec", map[string]any{"command": "rm -rf /"})
	if !strings.Contains(got, "not available in an ask") {
		t.Fatalf("expected shell_exec to be blocked, got %q", got)
	}
	// read_file stays blocked too: askAllowedTools admits save_note and the other store-writing tools a real /ask turn needs, but not the ones gated behind ToolApprovalChan, which nothing in the daemon reads.
	got = a.evalExecute(t.Context(), "read_file", map[string]any{"path": "/etc/passwd"})
	if !strings.Contains(got, "not available in an ask") {
		t.Fatalf("expected read_file to be blocked, got %q", got)
	}
}

// A multi-turn eval hands the prior turns in as history and asks the next question against them. The question's own content has to land after the history, and the caller's slice must come back untouched so the same history can be reused for the turn after that.
func TestWithHistory_AppendsWithoutAliasing(t *testing.T) {
	hist := make([]*genai.Content, 1, 4)
	hist[0] = genai.NewContentFromText("earlier question", genai.RoleUser)

	got := withHistory(hist, []*genai.Content{genai.NewContentFromText("first", genai.RoleUser)})
	if len(got) != 2 || got[0].Parts[0].Text != "earlier question" || got[1].Parts[0].Text != "first" {
		t.Fatalf("history and turn did not concatenate in order: %+v", got)
	}
	again := withHistory(hist, []*genai.Content{genai.NewContentFromText("second", genai.RoleUser)})
	if len(hist) != 1 {
		t.Fatalf("caller's history grew to %d", len(hist))
	}
	if again[1].Parts[0].Text != "second" || got[1].Parts[0].Text != "first" {
		t.Fatalf("the two calls shared backing array: %q and %q", got[1].Parts[0].Text, again[1].Parts[0].Text)
	}
}

func TestAskWith_RejectsUnknownChannel(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	if _, err := a.AskWith(context.Background(), Channel("carrier pigeon"), "", nil, "hi"); err == nil {
		t.Fatal("expected an unknown channel to be an error")
	}
}

// A 50-question gold run on 2026-09-04 found answers with no evidence trail: the user could not see which stored row an answer came from, so a name a meeting misheard went unnoticed. evidenceFromToolHops reads the {"source":{...}} tags db.FormatHitWithSource/FormatNoteHitWithSource append to hit lines, in the order the hits arrived.
func TestEvidenceFromToolHops_ExtractsInOrder(t *testing.T) {
	hops := []ToolHop{
		{Name: "query_memory", Result: strings.Join([]string{
			`[note#12] the user prefers terse replies {"source":{"kind":"note","id":12,"title":"","when":"2026-08-20T09:00:00Z"}}`,
			`[episode] reviewing the PR (Code — main.go) {"source":{"kind":"episode","id":42,"title":"main.go","when":"2026-09-01T10:30:00Z"}}`,
		}, "\n")},
	}
	got := evidenceFromToolHops(hops)
	want := []Evidence{
		{Kind: "note", ID: 12, Title: "", When: "2026-08-20T09:00:00Z", Excerpt: "[note#12] the user prefers terse replies"},
		{Kind: "episode", ID: 42, Title: "main.go", When: "2026-09-01T10:30:00Z", Excerpt: "[episode] reviewing the PR (Code — main.go)"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d evidence entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A turn where the model answered from the handshake context alone, or from a tool that carries no source tag (shell_exec, save_note), must not fabricate evidence out of thin air.
func TestEvidenceFromToolHops_NoToolsYieldsNone(t *testing.T) {
	if got := evidenceFromToolHops(nil); got != nil {
		t.Errorf("expected no evidence with no tool hops, got %+v", got)
	}
	hops := []ToolHop{{Name: "shell_exec", Result: "total 0\ndrwxr-xr-x  2 user user 4096 Sep  4 12:00 ."}}
	if got := evidenceFromToolHops(hops); got != nil {
		t.Errorf("expected no evidence from an untagged tool result, got %+v", got)
	}
}

// A tool result carrying more hits than evidenceLimit must be capped, keeping the most relevant (earliest) ones — the point of Evidence is showing the few rows an answer leaned on, not reproducing the whole search.
func TestEvidenceFromToolHops_CapsAtEvidenceLimit(t *testing.T) {
	var lines []string
	for i := 0; i < evidenceLimit+3; i++ {
		lines = append(lines, fmt.Sprintf(`[note#%d] fact %d {"source":{"kind":"note","id":%d,"title":"","when":""}}`, i, i, i))
	}
	hops := []ToolHop{{Result: strings.Join(lines, "\n")}}
	got := evidenceFromToolHops(hops)
	if len(got) != evidenceLimit {
		t.Fatalf("got %d entries, want the cap of %d", len(got), evidenceLimit)
	}
	if got[0].ID != 0 || got[evidenceLimit-1].ID != int64(evidenceLimit-1) {
		t.Errorf("expected the earliest %d hits kept in order, got %+v", evidenceLimit, got)
	}
}

func containsLine(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// The evals gate write tools off so a replay cannot touch the store. A stress run against a snapshot wants them on, so the gate is a switch on the agent rather than a constant. shell_exec (not action_items — askAllowedTools now admits that one, since production /ask needs it too) stands in for a tool the gate must still block by default.
func TestEvalExecute_WriteToolsGate(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	if got := a.evalExecute(context.Background(), "shell_exec", nil); !strings.Contains(got, "not available in an ask") {
		t.Fatalf("writes must be off by default, got %q", got)
	}
	a.AllowEvalWrites()
	if got := a.evalExecute(context.Background(), "no_such_tool", nil); strings.Contains(got, "not available in an ask") {
		t.Fatalf("with writes allowed the gate must not fire, got %q", got)
	}
}

// TestExecuteTool_NilBrainNeverPanics builds an agent with a nil brain — the shape NewAgent(nil, nil, nil, nil, "") produces, and the shape a not-yet-connected daemon can hand a tool call — and runs every tool the ask gate or the subtask gate can reach through it. Widening askAllowedTools to admit more memory tools exposed a path where such a call reached a.brain.SomeMethod(...) and panicked on the nil interface's own method dispatch instead of failing like every other executeTool error path does. None of these calls may panic, and each must come back as a plain "error: ..." string, the same shape every other failure path in executeTool already uses.
func TestExecuteTool_NilBrainNeverPanics(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	names := make(map[string]bool, len(subtaskAllowedTools)+len(askAllowedTools))
	for name := range subtaskAllowedTools {
		names[name] = true
	}
	for name := range askAllowedTools {
		names[name] = true
	}
	for name := range names {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("executeTool(%q) panicked with a nil brain: %v", name, r)
				}
			}()
			got := a.executeTool(context.Background(), name, nil)
			if got == "" {
				t.Fatalf("executeTool(%q) with a nil brain returned an empty result", name)
			}
			// Only the tools that actually read or write through a.brain must fail: the rest (observe_screen and the other screen tools, open_url) don't touch it at all, and can come back with a real result when this test runs on a desktop that has a screen to look at.
			if brainRequiredTools[name] && !strings.HasPrefix(got, "error:") {
				t.Fatalf("executeTool(%q) touches a nil brain and must fail as \"error: ...\", got %q", name, got)
			}
		})
	}
}

// The Live API's declarations carry Behavior: NON_BLOCKING. generateContent rejects a request holding one — "FunctionDeclaration.behavior is only supported by the BidiGenerateContent method" — which failed every text-channel question in the gold run until this was stripped.
func TestTextAskTools_StripsLiveOnlyBehavior(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	var live, text int
	for _, tool := range a.askTools() {
		for _, d := range tool.FunctionDeclarations {
			if d.Behavior != "" {
				live++
			}
		}
	}
	if live == 0 {
		t.Skip("the live tool list carries no behavior flags, nothing to strip")
	}
	for _, tool := range a.textAskTools() {
		for _, d := range tool.FunctionDeclarations {
			if d.Behavior != "" {
				text++
			}
		}
	}
	if text != 0 {
		t.Errorf("%d declarations still carry a behavior flag on the text channel", text)
	}
}

// The text the model writes beside a tool call is narration about what it is doing, not the answer; only the text of the round that ends without a tool call may be spoken. splitParts is the seam that keeps thoughts apart from user-facing text.
func TestSplitParts_KeepsThoughtsApartFromText(t *testing.T) {
	parts := []*genai.Part{
		{Text: "thinking about it", Thought: true},
		{Text: "Saving a note. "},
		nil,
		{Text: "One down."},
	}
	text, thoughts := splitParts(parts)
	if text != "Saving a note. One down." {
		t.Errorf("text = %q", text)
	}
	if len(thoughts) != 1 || thoughts[0] != "thinking about it" {
		t.Errorf("thoughts = %v", thoughts)
	}
}

// A turn's token counts are the sum of every round of its tool loop, not the last round's counts: a question answered after two tool calls cost what all three model calls cost together. On the text channel the counts arrive on the response's UsageMetadata, where the prompt count is the input and the candidates count plus the thoughts count is the output, because thinking tokens are billed as output and are reported apart from the candidates.
func TestTokenUsage_AddGeminiSumsEveryRound(t *testing.T) {
	var use TokenUsage
	use.addGemini(&genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100, CandidatesTokenCount: 20, ThoughtsTokenCount: 5, TotalTokenCount: 125})
	use.addGemini(&genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 200, CandidatesTokenCount: 10, TotalTokenCount: 210})
	if use.InputTokens != 300 || use.OutputTokens != 35 || use.TotalTokens != 335 {
		t.Errorf("two rounds summed to %+v, want 300 in, 35 out, 335 total", use)
	}
	// A round the API reported nothing for adds nothing: a made-up count is worse than a zero.
	use.addGemini(nil)
	if use.InputTokens != 300 || use.OutputTokens != 35 || use.TotalTokens != 335 {
		t.Errorf("a round with no usage metadata changed the counts to %+v", use)
	}
}

// Gemini reports the part of a prompt it served from its own cache as cachedContentTokenCount, and it is part of the prompt count rather than extra to it. Until this was read, every Gemini turn filed zero cached tokens whatever the API said, so the usage screen could not tell a cache that was working from one that was not.
func TestTokenUsage_AddGeminiRecordsCachedPromptTokens(t *testing.T) {
	var use TokenUsage
	use.addGemini(&genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10000, CachedContentTokenCount: 8000, CandidatesTokenCount: 50, TotalTokenCount: 10050})
	use.addGemini(&genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 2000, CandidatesTokenCount: 10, TotalTokenCount: 2010})
	if use.CachedInputTokens != 8000 {
		t.Errorf("cached tokens summed to %d, want 8000", use.CachedInputTokens)
	}
	// The cached count is part of the input, so reading it must not change what the input count says.
	if use.InputTokens != 12000 {
		t.Errorf("input tokens are %d, want 12000 — the cached count is part of the input, not extra to it", use.InputTokens)
	}
}

// The Live API names the same field on its own usage message, so a spoken turn reports its cache the way a typed one does.
func TestTokenUsage_AddLiveRecordsCachedPromptTokens(t *testing.T) {
	var use TokenUsage
	use.addLive(&genai.UsageMetadata{PromptTokenCount: 5000, CachedContentTokenCount: 4096, ResponseTokenCount: 12, TotalTokenCount: 5012})
	if use.CachedInputTokens != 4096 || use.InputTokens != 5000 {
		t.Errorf("got %d cached of %d input, want 4096 of 5000", use.CachedInputTokens, use.InputTokens)
	}
}

// The Live API reports usage on the server message rather than on a response, naming the prompt, response and thoughts counts. Most messages of a turn carry none, and those must add nothing.
func TestTokenUsage_AddLiveSumsEveryMessage(t *testing.T) {
	var use TokenUsage
	use.addLive(&genai.UsageMetadata{PromptTokenCount: 40, ResponseTokenCount: 8, ThoughtsTokenCount: 2, TotalTokenCount: 50})
	use.addLive(nil)
	use.addLive(&genai.UsageMetadata{PromptTokenCount: 60, ResponseTokenCount: 4, TotalTokenCount: 64})
	if use.InputTokens != 100 || use.OutputTokens != 14 || use.TotalTokens != 114 {
		t.Errorf("two reporting messages summed to %+v, want 100 in, 14 out, 114 total", use)
	}
}

// A turn is filed under the provider it went to even when the call failed before a single token was counted, so the ledger shows the call happened. askText stamps the provider on the trace before it builds a client or sends anything; a cancelled context makes the send fail without reaching the network.
func TestAskText_StampsTheProviderOnAFailedCall(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr, err := a.askText(ctx, "gemini-3-flash", nil, "hi")
	if err == nil {
		t.Fatal("a cancelled ask must fail")
	}
	if tr.Usage.Provider != ProviderGemini {
		t.Errorf("provider = %q, want %q", tr.Usage.Provider, ProviderGemini)
	}
	if tr.Model != "gemini-3-flash" || tr.Question != "hi" {
		t.Errorf("the failed trace lost the call it was: %+v", tr)
	}
	if tr.Usage.InputTokens != 0 || tr.Usage.OutputTokens != 0 || tr.Usage.TotalTokens != 0 {
		t.Errorf("a call that never answered counted %+v tokens, want zeroes", tr.Usage)
	}
}

// The voice channel is Gemini too, and stamps the same provider on its trace before it connects.
func TestAskVoice_StampsTheProviderOnAFailedCall(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr, err := a.askVoice(ctx, "gemini-3-flash-live", nil, "hi")
	if err == nil {
		t.Fatal("a cancelled ask must fail")
	}
	if tr.Usage.Provider != ProviderGemini {
		t.Errorf("provider = %q, want %q", tr.Usage.Provider, ProviderGemini)
	}
}

// textOf joins the text of a history content's parts, so a test can read back what one prior turn carried.
func textOf(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// The window shows a thread and the store keeps it, but the model was given none of it: the user asked the hover to ring a button, it answered "I ringed the Pause button", and "do it again" came back "What do you want repeated?". HistoryFromTurns is the seam that carries the thread — the question as a user turn, the answer as a model turn, oldest first — and it carries nothing else: not the failed answers, not the tool names or the evidence behind an answer, since a screen listing from a minute ago would tell the model the wrong thing about what is on screen now.
func TestHistoryFromTurns_CarriesBothSidesOldestFirst(t *testing.T) {
	turns := []db.Turn{
		{Role: "you", Text: "draw a ring around any one button you can see", Kind: "ask"},
		{Role: "ora", Text: "I ringed the Pause button.", Kind: "ask", Tools: []string{"observe_screen", "point_at"}, Evidence: json.RawMessage(`[{"title":"screen"}]`)},
		{Role: "ora", Text: "ask text: generate (iteration 0): 503 UNAVAILABLE", Kind: "error"},
		{Role: "you", Text: "   ", Kind: "ask"},
		{Role: "you", Text: "do it again", Kind: "ask"},
	}
	got := HistoryFromTurns(turns)
	if len(got) != 2 {
		t.Fatalf("got %d history turns, want the question and its answer: %+v", len(got), got)
	}
	if got[0].Role != genai.RoleUser || textOf(got[0]) != "draw a ring around any one button you can see" {
		t.Errorf("first history turn = %s %q", got[0].Role, textOf(got[0]))
	}
	if got[1].Role != genai.RoleModel || textOf(got[1]) != "I ringed the Pause button." {
		t.Errorf("second history turn = %s %q", got[1].Role, textOf(got[1]))
	}
	if got := HistoryFromTurns(nil); len(got) != 0 {
		t.Errorf("an empty conversation must give no history, got %+v", got)
	}
}

// The whole history is re-sent on every question, so a long thread has to be cut down: the newest maxHistoryTurns turns are kept and the older ones dropped.
func TestHistoryFromTurns_KeepsTheNewestTurnsWithinTheCountCap(t *testing.T) {
	var turns []db.Turn
	for i := 0; i < maxHistoryTurns*3; i++ {
		role := "you"
		if i%2 == 1 {
			role = "ora"
		}
		turns = append(turns, db.Turn{Role: role, Text: fmt.Sprintf("turn %d", i), Kind: "ask"})
	}
	got := HistoryFromTurns(turns)
	if len(got) != maxHistoryTurns {
		t.Fatalf("got %d history turns, want the cap of %d", len(got), maxHistoryTurns)
	}
	if want := fmt.Sprintf("turn %d", len(turns)-maxHistoryTurns); textOf(got[0]) != want {
		t.Errorf("oldest kept turn = %q, want %q", textOf(got[0]), want)
	}
	if want := fmt.Sprintf("turn %d", len(turns)-1); textOf(got[len(got)-1]) != want {
		t.Errorf("newest kept turn = %q, want %q", textOf(got[len(got)-1]), want)
	}
}

// A thread of a few very long turns is under the count cap and still too big to re-send, so the size cap drops the oldest until what is left fits, and one turn longer than the whole per-turn cap is truncated rather than dropped.
func TestHistoryFromTurns_DropsOldestWhenTheThreadIsTooBig(t *testing.T) {
	// Six turns of a full per-turn cap each are 24 KB, half again over the size cap, and well under the count cap, so the size cap is what does the cutting here.
	big := strings.Repeat("x", maxHistoryTurnBytes)
	var turns []db.Turn
	for i, label := range []string{"oldest", "second", "third", "fourth", "fifth", "newest"} {
		role := "you"
		if i%2 == 1 {
			role = "ora"
		}
		turns = append(turns, db.Turn{Role: role, Text: label + " " + big, Kind: "ask"})
	}
	got := HistoryFromTurns(turns)
	if len(got) == 0 || len(got) >= len(turns) {
		t.Fatalf("got %d history turns, want the oldest of %d dropped", len(got), len(turns))
	}
	total := 0
	for _, c := range got {
		total += len(textOf(c))
	}
	if total > maxHistoryBytes {
		t.Errorf("history is %d bytes, over the cap of %d", total, maxHistoryBytes)
	}
	if !strings.HasPrefix(textOf(got[len(got)-1]), "newest") {
		t.Errorf("the newest turn must survive, last kept = %q", util.Runes(textOf(got[len(got)-1]), 20))
	}
	if strings.HasPrefix(textOf(got[0]), "oldest") {
		t.Errorf("the oldest turn should have been dropped first, first kept = %q", util.Runes(textOf(got[0]), 20))
	}

	long := HistoryFromTurns([]db.Turn{{Role: "ora", Text: strings.Repeat("y", maxHistoryTurnBytes*2), Kind: "ask"}})
	if len(long) != 1 {
		t.Fatalf("one overlong turn must be kept, got %d", len(long))
	}
	if n := len(textOf(long[0])); n != maxHistoryTurnBytes {
		t.Errorf("overlong turn kept %d bytes, want it truncated to %d", n, maxHistoryTurnBytes)
	}
}

// The Gemini text path must put the thread in the request itself: the prior question and its answer come before this turn's context and question, in that order. A question asked with no history carries none of it, so the existing single-question entry point sends what it always sent.
func TestAskTextWith_SendsThePriorTurnsBeforeTheQuestion(t *testing.T) {
	var bodies []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Ringed the Pause button again."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "draw a ring around any one button you can see", Kind: "ask"},
		{Role: "ora", Text: "I ringed the Pause button.", Kind: "ask"},
	})
	tr, err := a.AskTextWith(t.Context(), history, "do it again")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "Ringed the Pause button again." {
		t.Errorf("answer = %q", tr.Answer)
	}
	if len(bodies) != 1 {
		t.Fatalf("%d requests, want 1", len(bodies))
	}
	asked := strings.Index(bodies[0], "draw a ring around any one button you can see")
	answered := strings.Index(bodies[0], "I ringed the Pause button.")
	now := strings.Index(bodies[0], "do it again")
	if asked < 0 || answered < 0 || now < 0 {
		t.Fatalf("request carried question/answer/new question at %d/%d/%d", asked, answered, now)
	}
	if !(asked < answered && answered < now) {
		t.Errorf("the thread must be in order, got question at %d, answer at %d, new question at %d", asked, answered, now)
	}

	if _, err := a.AskText(t.Context(), "do it again"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	if strings.Contains(bodies[1], "I ringed the Pause button.") {
		t.Error("a question asked with no history must not carry a prior answer")
	}
}

// TestToolLogDetail checks that the "ask: tool" log line carries no result text for the tools that read the screen, because that text is the front window's title and the head of its accessibility list, which can be a password manager or an inbox, while a memory search's result is still logged for debugging.
func TestToolLogDetail(t *testing.T) {
	if got := toolLogDetail("observe_screen", "Brave · Inbox\n[1] text \"Reset your password\""); got != "" {
		t.Errorf("observe_screen detail logged as %q, want nothing", got)
	}
	for _, name := range []string{"point_at", "show_marks", "click", "scroll_to", "type_text"} {
		if got := toolLogDetail(name, "clicked [3]"); got != "" {
			t.Errorf("%s detail logged as %q, want nothing", name, got)
		}
	}
	if got := toolLogDetail("query_memory", "three notes about the venue"); got != "three notes about the venue" {
		t.Errorf("query_memory detail = %q, want the result kept", got)
	}
}

// TestEvalExecute_RefusalNamesTheAskNotAnEval pins the wording of the gate's refusal. It used to read "disabled in evals (read-only memory eval)", which describes a situation a user typing /ask is not in: the model read it, the user read it in the trace, and neither was running an eval. The refusal has to say the plain true thing — the tool is not available in an ask.
func TestEvalExecute_RefusalNamesTheAskNotAnEval(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := a.evalExecute(t.Context(), "shell_exec", map[string]any{"command": "ls"})
	if !strings.Contains(got, "not available in an ask") {
		t.Errorf("refusal = %q, want it to say the tool is not available in an ask", got)
	}
	if strings.Contains(strings.ToLower(got), "eval") {
		t.Errorf("refusal = %q, want no mention of an eval the user is not running", got)
	}
}

// TestAskTools_OfferNothingTheGateWouldRefuse checks the three ask paths hand the model only tools evalExecute will actually run. The failing run called shell_exec and branch, was refused by the gate both times, and spent two of its twelve rounds learning that tools it had been offered do not work — a tool on the list that can only ever come back refused is a trap the prompt cannot talk the model out of.
func TestAskTools_OfferNothingTheGateWouldRefuse(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	offered := map[string]bool{}
	for _, decl := range a.askToolDeclarations() {
		offered[decl.Name] = true
		if !askAllowedTools[decl.Name] && !subtaskAllowedTools[decl.Name] {
			t.Errorf("tool %q is offered but the ask gate refuses it", decl.Name)
		}
	}
	for _, want := range []string{"observe_screen", "click", "scroll_to", "query_memory"} {
		if !offered[want] {
			t.Errorf("tool %q must stay on the ask's list", want)
		}
	}
	for _, tool := range a.textAskTools() {
		for _, decl := range tool.FunctionDeclarations {
			if !offered[decl.Name] {
				t.Errorf("the text channel offers %q, which the declaration list does not", decl.Name)
			}
		}
	}
	// A stress run against a snapshot lifts the gate, so it must also get the full tool surface back rather than the narrowed one.
	a.AllowEvalWrites()
	if len(a.askToolDeclarations()) != len(ToolDeclarations()) {
		t.Errorf("with the gate lifted the ask offers %d tools, want all %d", len(a.askToolDeclarations()), len(ToolDeclarations()))
	}
}

// TestLastObservedWindow_ReadsTheTitleOffTheNewestLook checks the title an out-of-steps message quotes comes from the last observe_screen that worked, with the app name ahead of the separator dropped, and that a failed or absent look leaves it empty.
func TestLastObservedWindow_ReadsTheTitleOffTheNewestLook(t *testing.T) {
	hops := []ToolHop{
		{Name: "observe_screen", Result: "brave · Home\n[1] link \"Sign in\""},
		{Name: "click", Result: "clicked [1]"},
		{Name: "observe_screen", Result: "brave · PR #13 · GitHub\n[1] push button \"Merge\""},
		{Name: "observe_screen", Result: "error: could not look at the screen: no session"},
	}
	if got := lastObservedWindow(hops); got != "PR #13 · GitHub" {
		t.Errorf("lastObservedWindow = %q, want the newest working look's title", got)
	}
	if got := lastObservedWindow([]ToolHop{{Name: "query_memory", Result: "no memory matches"}}); got != "" {
		t.Errorf("lastObservedWindow = %q, want nothing when the turn never looked", got)
	}
	if got := lastObservedWindow([]ToolHop{{Name: "observe_screen", Result: "gnome-shell · \n[1] text \"9%\""}}); got != "gnome-shell" {
		t.Errorf("lastObservedWindow = %q, want the whole line when the window has no title", got)
	}
}

// TestCapError_SaysWhereItGotTo pins the message a turn ends with when it runs out of steps. "I looked 12 times and could not settle on an answer" told the user nothing he could act on: not which page it reached, not what it last saw, not whether it had got anywhere at all. The message has to name the window it last observed and how many steps it took.
func TestCapError_SaysWhereItGotTo(t *testing.T) {
	hops := []ToolHop{
		{Name: "observe_screen", Result: "brave · PR #13 · GitHub\n[1] push button \"Merge\""},
		{Name: "click", Result: "clicked [1]"},
	}
	got := capError(hops).Error()
	if !strings.Contains(got, "PR #13 · GitHub") {
		t.Errorf("out-of-steps message = %q, want the window it last saw", got)
	}
	if !strings.Contains(got, "2") {
		t.Errorf("out-of-steps message = %q, want how far it got", got)
	}
	if strings.Contains(got, "could not settle on an answer") {
		t.Errorf("out-of-steps message = %q, want it replaced, not kept", got)
	}
	blind := capError([]ToolHop{{Name: "query_memory", Result: "no memory matches"}}).Error()
	if strings.Contains(blind, "saw") {
		t.Errorf("a turn that never looked at the screen must not claim to have seen one, got %q", blind)
	}
}

// TestCapError_SaysWhatItDid covers the rest of what "could not settle on an answer" left out: not only where the turn got to, but the last thing it actually did there. A run out of steps mid-scroll is not the same story as one that never acted at all, and the message must tell them apart.
func TestCapError_SaysWhatItDid(t *testing.T) {
	hops := []ToolHop{
		{Name: "observe_screen", Result: "brave · Family Guy · JioHotstar\n[1] link \"S16 E7\"\n[2] link \"S16 E8\""},
		{Name: "scroll_to", Result: "scrolled to [2] link \"S16 E8\"; call observe_screen to see the page now"},
	}
	got := capError(hops).Error()
	if !strings.Contains(got, "S16 E8") {
		t.Errorf("out-of-steps message = %q, want it to say what it last did", got)
	}
	if !strings.Contains(got, "Family Guy") {
		t.Errorf("out-of-steps message = %q, want it still to name the window", got)
	}

	// A failed action is not a thing done — the window it last saw still counts, but not an action that never happened.
	failedAction := []ToolHop{
		{Name: "observe_screen", Result: "brave · Family Guy · JioHotstar\n[1] link \"S16 E7\""},
		{Name: "click", Result: "error: could not click [1]: element gone"},
	}
	got = capError(failedAction).Error()
	if strings.Contains(got, "could not click") {
		t.Errorf("out-of-steps message = %q, want a failed action left out, not quoted back", got)
	}
	if !strings.Contains(got, "Family Guy") {
		t.Errorf("out-of-steps message = %q, want the window it last saw kept even though the action failed", got)
	}

	// A turn that only ever looked has nothing done to report.
	onlyLooked := []ToolHop{{Name: "observe_screen", Result: "brave · Home\n[1] link \"Sign in\""}}
	got = capError(onlyLooked).Error()
	if strings.Contains(got, "having just") {
		t.Errorf("out-of-steps message = %q, want no action clause when the turn never acted", got)
	}
}

// TestIsScreenTask covers the heuristic that lets the very first round of an ask be treated as a screen task, before any tool has run: the words the request itself uses, or (once at least one round has gone by) a screen tool it has already called.
func TestIsScreenTask(t *testing.T) {
	cases := map[string]bool{
		"click the merge button":                  true,
		"open the settings page":                  true,
		"type my address into the search field":   true,
		"play the next episode":                   true,
		"navigate to the dashboard":               true,
		"look at my screen and tell me the time":  true,
		"what window is in front":                 true,
		"recommend me a good app for note taking": true,
		"what is the capital of France":           false,
		"remind me what we discussed yesterday":   false,
	}
	for question, want := range cases {
		if got := mentionsScreenTask(question); got != want {
			t.Errorf("mentionsScreenTask(%q) = %v, want %v", question, got, want)
		}
		if got := isScreenTask(question, nil); got != want {
			t.Errorf("isScreenTask(%q, nil) = %v, want %v", question, got, want)
		}
	}
	// A question with none of the words still reads as a screen task once a screen tool has actually run.
	if isScreenTask("what is the capital of France", []ToolHop{{Name: "observe_screen"}}) != true {
		t.Error("isScreenTask must still say true once a screen tool has run, whatever the question said")
	}
}

// TestAskText_NarrowsToolsFromTheFirstRoundWhenTheRequestNamesAScreen covers the fix for the 2026-09-05 runs that reached for shell_exec, branch and query_memory on a screen task and were refused: a request that plainly means the screen must be offered only the screen tools, save_note and open_url from its very first round, not only once a screen tool has already run and proven it after the fact.
func TestAskText_NarrowsToolsFromTheFirstRoundWhenTheRequestNamesAScreen(t *testing.T) {
	var body string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _ := observingAgent(t)
	if _, err := a.askText(t.Context(), "gemini-test", nil, "click the merge button"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"observe_screen", "click", "save_note", "open_url"} {
		if !strings.Contains(body, `"`+want+`"`) {
			t.Errorf("the first round's tools lack %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{"query_memory", "personal_context", "action_items", "query_store", "revise"} {
		if strings.Contains(body, `"`+unwanted+`"`) {
			t.Errorf("the first round of a screen task still offered %q", unwanted)
		}
	}
}

// TestSameScreenAgain_OnlyCountsALookThatShowedSomethingNew covers the accounting the cap runs on. A round that only called observe_screen and got back exactly what it already had has not moved the task on and must not spend the budget; a first look, a changed screen, or any round that also did something all count.
func TestSameScreenAgain_OnlyCountsALookThatShowedSomethingNew(t *testing.T) {
	screen := ""
	first := []ToolHop{{Name: "observe_screen", Result: "brave · Home\n[1] link \"Sign in\""}}
	if sameScreenAgain(&screen, first) {
		t.Error("the first look at a screen must count as a step")
	}
	if !sameScreenAgain(&screen, first) {
		t.Error("a second look returning the same screen must not count as a step")
	}
	if !sameScreenAgain(&screen, first) {
		t.Error("a third identical look must not count either")
	}
	moved := []ToolHop{{Name: "observe_screen", Result: "brave · PR #13 · GitHub\n[1] push button \"Merge\""}}
	if sameScreenAgain(&screen, moved) {
		t.Error("a look that showed a different screen must count as a step")
	}
	if sameScreenAgain(&screen, []ToolHop{{Name: "click", Result: "clicked [1]"}}) {
		t.Error("a round that acted must always count as a step")
	}
	if sameScreenAgain(&screen, nil) {
		t.Error("a round with no tool calls at all must count as a step")
	}
}

// TestAskText_RepeatedLooksDoNotSpendTheCap runs the whole text loop against a model that does nothing but call observe_screen. Every look comes back with the same window, so none of them moves the task on: the loop must keep going past maxAskIterations rather than stopping twelve identical looks in, must still stop at the hard round bound rather than spinning forever, and must end by naming the window it was looking at all along.
func TestAskText_RepeatedLooksDoNotSpendTheCap(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _ := observingAgent(t)
	tr, err := a.askText(t.Context(), "gemini-test", nil, "play the eighth one")
	if err == nil {
		t.Fatal("a loop that never answers must come back as an error")
	}
	if len(tr.ToolHops) <= maxAskIterations {
		t.Errorf("%d looks, want more than %d: identical looks must not spend the cap", len(tr.ToolHops), maxAskIterations)
	}
	if len(tr.ToolHops) != maxAskRounds {
		t.Errorf("%d looks, want the hard bound of %d to stop it", len(tr.ToolHops), maxAskRounds)
	}
	if !strings.Contains(err.Error(), "PR #13 · GitHub") {
		t.Errorf("err = %q, want it to name the window it last saw", err)
	}
}

// The Gemini text path accumulates its rounds the same way the Codex path does, so it pays for an old screen listing on every round the same way and must drop the superseded ones the same way.
func TestAskText_KeepsOnlyTheNewestScreenListing(t *testing.T) {
	var bodies []string
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		n++
		if n <= 2 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := movingScreenAgent(t)
	if _, err := a.askText(t.Context(), "gemini-test", nil, "click the merge button"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d rounds, want 3", len(bodies))
	}
	if !strings.Contains(bodies[1], `[1] push button \"Merge\"`) {
		t.Error("the round after one look must carry that list whole")
	}
	if strings.Contains(bodies[2], `[1] push button \"Merge\"`) {
		t.Error("the last round carried the superseded listing again")
	}
	if !strings.Contains(bodies[2], `[1] push button \"Approve\"`) {
		t.Error("the last round must carry the newest listing whole")
	}
	if !strings.Contains(bodies[2], supersededListingNote) {
		t.Error("the superseded listing must be replaced by the note")
	}
}

// The Gemini text path trims the same two things the Codex path trims once a turn is a screen task: the handshake down to the screen guidance and the stop line, and the thread down to the last two turns. The question here names no screen task in its own words, so the trimming is what the first observe_screen call proves after the fact; a question that does name one opens on the trimmed prompt at round 0 instead (see TestAskText_ScreenAskSendsOnePrefixOnEveryRound).
func TestAskText_TrimsThePromptAndTheThreadOnAScreenTask(t *testing.T) {
	var bodies []string
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _ := observingAgent(t)
	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "first question", Kind: "ask"},
		{Role: "ora", Text: "first answer", Kind: "ask"},
		{Role: "you", Text: "second question", Kind: "ask"},
		{Role: "ora", Text: "second answer", Kind: "ask"},
	})
	if _, err := a.askText(t.Context(), "gemini-test", history, "what did we settle on for the venue"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d rounds, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], "Talk like a sharp friend") {
		t.Fatal("the first round must still carry the whole handshake")
	}
	if strings.Contains(bodies[1], "Talk like a sharp friend") {
		t.Error("a screen round still carried the conversational handshake")
	}
	if !strings.Contains(bodies[1], "one task to see through") || !strings.Contains(bodies[1], "just said") {
		t.Error("a screen round must keep the screen guidance and the stop line")
	}
	if strings.Contains(bodies[1], "first question") {
		t.Error("a screen round still carried the older turns of the thread")
	}
	if !strings.Contains(bodies[1], "second question") {
		t.Error("a screen round must keep the last two turns")
	}
}

// A click the stop line refuses ends the turn on the spot: the tool's own "Stopped before ..." result becomes the answer, with no further round given a chance to click the same button a different way or talk around the refusal.
func TestAskText_StopsBeforeIrreversibleClickAndEndsTheTurn(t *testing.T) {
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"click","args":{"n":1}}}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, f, _ := guardedClickAgent(t)
	tr, err := a.askText(t.Context(), "gemini-test", nil, "click send")
	if err != nil {
		t.Fatalf("askText: %v", err)
	}
	if len(f.clicked) != 0 {
		t.Errorf("clicked = %v, want the click refused", f.clicked)
	}
	if !strings.HasPrefix(tr.Answer, "Stopped before ") {
		t.Errorf("answer = %q, want it to begin \"Stopped before \"", tr.Answer)
	}
	if !strings.Contains(tr.Answer, `"yes, send it"`) {
		t.Errorf("answer = %q, want the one-line question naming the phrase to say", tr.Answer)
	}
	if n != 2 {
		t.Errorf("rounds = %d, want exactly 2 — the turn must end the moment the stop line fires, not spend a third round on it", n)
	}
}

// The stop line is the one safety rule that has to survive every prompt this package trims, so the screen-task prompt and the full handshake must carry the same sentence. Two copies that drift apart is a screen task told nothing about what it must not click.
func TestStopLineText_IsTheSentenceTheHandshakeCarries(t *testing.T) {
	full := systemInstructionText(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), "linux", "amd64", "bash", "", "  nothing", 20)
	if !strings.Contains(full, stopLineText) {
		t.Errorf("the handshake no longer carries the stop line %q; the two copies have drifted", stopLineText)
	}
	if !strings.Contains(screenTaskInstruction(), stopLineText) {
		t.Error("the screen-task prompt dropped the stop line")
	}
}

// The reference block for a close past run is built fresh from the question and the store, so it belongs with the clock and the screen listing at the dynamic end of the prompt, never in the part the prompt cache is meant to match.
func TestAskText_CarriesTheActReferenceWithTheTurnNotTheInstruction(t *testing.T) {
	var bodies []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	brain := &actLookupBrain{toolTestBrain: &toolTestBrain{}, matches: []db.ActMatch{subtitlesMatch(6 * time.Hour)}}
	a := NewAgent(nil, nil, brain, nil, "test-key")
	if _, err := a.askText(t.Context(), "gemini-test", nil, "show me how to change subtitles"); err != nil {
		t.Fatal(err)
	}
	if brain.asked != "show me how to change subtitles" {
		t.Fatalf("the lookup was asked %q, want the question the ask was given", brain.asked)
	}
	if !strings.Contains(bodies[0], "Something close to this was asked before") {
		t.Fatalf("the reference block never reached the request: %q", bodies[0])
	}
	// The instruction is what the prompt cache is meant to match from one ask to the next, so a block built fresh from this question and this store must not be in it.
	instruction, _ := a.HandshakePrompt(t.Context(), time.Now())
	if strings.Contains(instruction, "Something close to this was asked before") {
		t.Error("the reference block landed in the system instruction, where it would break every prefix match")
	}
}

// TestHandshakePrompt_HasAStableHeadForThePromptCache pins how much of the opening prompt is the same bytes from one ask to the next, which is all a prompt cache can match. Everything up to the clock sentence is stable; the clock is what ends the stable run, and it sits about six thousand bytes in, so the opening round of an ask caches roughly that much and nothing after it — not the personal context block, not the tool definitions. The rounds after the first carry no clock at all (see screenTaskInstruction), which is where the caching actually pays.
func TestHandshakePrompt_HasAStableHeadForThePromptCache(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	morning, _ := a.HandshakePrompt(t.Context(), time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC))
	evening, _ := a.HandshakePrompt(t.Context(), time.Date(2026, 9, 6, 21, 30, 0, 0, time.UTC))
	if morning == evening {
		t.Fatal("two prompts an hour and a day apart came out identical; the clock is meant to differ")
	}
	shared := 0
	for shared < len(morning) && shared < len(evening) && morning[shared] == evening[shared] {
		shared++
	}
	if shared < 5000 {
		t.Errorf("only the first %d bytes of the opening prompt are stable; the parts that move must stay at the end of it", shared)
	}
	if clock := strings.Index(morning, "Right now it is"); shared < clock {
		t.Errorf("the stable head ends at %d, before the clock sentence at %d; something earlier in the prompt is moving too", shared, clock)
	}
}

// A look is only worth taking if the model is actually shown the picture. The Gemini path sends it as inline data in a user turn straight after the tool result that produced it, so the answer to "what is in this video" is read off the pixels rather than guessed.
func TestAskText_SendsTheLookPictureToTheModel(t *testing.T) {
	var bodies []string
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"look","args":{}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Stewie is on the left."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _, _ := lookingAgent(t)
	tr, err := a.askText(t.Context(), "gemini-test", nil, "who is who on screen")
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d rounds, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], base64.StdEncoding.EncodeToString([]byte("fake-jpeg-bytes"))) {
		t.Error("the round after a look must carry the picture's own bytes")
	}
	if !strings.Contains(bodies[1], `"image/jpeg"`) {
		t.Error("the picture must be sent with its media type")
	}
	if tr.ImageTokens != lookTokenCost(1280, 704) {
		t.Errorf("ImageTokens = %d, want %d", tr.ImageTokens, lookTokenCost(1280, 704))
	}
}

// refusingGate is a request gate that turns every Gemini request away with the error it was given.
type refusingGate struct{ err error }

func (g refusingGate) Allow(string) error { return g.err }

// An interactive ask counts against the same daily Gemini allowance as the nightly jobs: when the gate refuses, no request reaches Gemini, and the refusal is the 429 shape the Codex hand-over already recognises, so the ask moves on instead of failing.
func TestAskText_GateRefusalReachesNoBackendAndReadsAsQuota(t *testing.T) {
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"never"}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	a.SetRequestGate(refusingGate{err: genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "daily request quota of 20 reached"}})
	_, err := a.askText(t.Context(), "gemini-3.5-flash", nil, "what is in front of me")
	if err == nil {
		t.Fatal("askText succeeded with a refusing gate")
	}
	if !GeminiCannotAnswer(err) {
		t.Errorf("error %v is not the quota shape the hand-over looks for", err)
	}
	if requests != 0 {
		t.Errorf("%d requests reached the backend, want 0", requests)
	}
}

// A permitting gate changes nothing: the request goes out and the answer comes back as before.
func TestAskText_PermittingGateLeavesTheAskAlone(t *testing.T) {
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"a browser"}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	a.SetRequestGate(refusingGate{err: nil})
	tr, err := a.askText(t.Context(), "gemini-3.5-flash", nil, "what is in front of me")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "a browser" || requests != 1 {
		t.Errorf("answer %q after %d requests, want the backend's answer after 1", tr.Answer, requests)
	}
}

// A turn that only read the screen may still be handed to Codex or Claude when Gemini's allowance runs out: repeating observe_screen on the other provider changes nothing, where repeating a click would click twice. Counting every hop instead — which is what AskTextWith used to pass — refused the hand-over to any turn that had merely looked, and that is how a routine returned a raw 429 to the user on 2026-09-05.
func TestAskTextWith_HandsOnAfterAReadOnlyToolButNotAfterAnAction(t *testing.T) {
	round := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _ := observingAgent(t)
	// askText is the call AskTextWith makes; the two hand-over decisions below are the expressions it evaluates on what came back, checked here rather than by letting the test run the Codex backend or the Claude command line for real.
	tr, err := a.askText(t.Context(), "gemini-test", nil, "what is on my screen")
	if err == nil {
		t.Fatal("the ask must fail once the model answers 429")
	}
	if len(tr.ToolHops) != 1 || tr.ToolHops[0].Name != "observe_screen" {
		t.Fatalf("tool hops = %+v, want the one read-only look the model made", tr.ToolHops)
	}
	if !codexFallbackWanted(err, actionHops(tr.ToolHops), true) {
		t.Error("a turn that only looked at the screen must still be handed to Codex")
	}
	// The Claude check runs on what Codex answered, so its own spent-allowance error stands in for that step; the hops it counts are the same ones.
	if !claudeFallbackWanted(codexHTTPError{Code: http.StatusTooManyRequests}, actionHops(tr.ToolHops), true) {
		t.Error("a turn that only looked at the screen must still be handed to Claude")
	}
	acted := append(tr.ToolHops, ToolHop{Name: "click", Result: "clicked [1] push button \"Merge\""})
	if codexFallbackWanted(err, actionHops(acted), true) {
		t.Error("a turn that clicked something must not be replayed on another provider")
	}
}

// TestHandshakeInstruction_EverythingStableComesBeforeAnythingThatChanges checks the cache property the layout exists for: two asks a minute apart, with different memory lines and personal context, share a prefix that already holds the whole stable text, the stop line, the tool count and the screen guidance included, so a provider caching by prefix reuses all of it.
func TestHandshakeInstruction_EverythingStableComesBeforeAnythingThatChanges(t *testing.T) {
	a := handshakeInstruction(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), "Personal context:\n- likes tea", "  reading the Hotstar page", 20)
	b := handshakeInstruction(time.Date(2026, 9, 5, 12, 1, 0, 0, time.UTC), "Personal context:\n- likes coffee", "  writing an email", 20)
	shared := 0
	for shared < len(a) && shared < len(b) && a[shared] == b[shared] {
		shared++
	}
	prefix := a[:shared]
	for _, want := range []string{stopLineText, "You have 20 tools", screenTaskGuidance} {
		if !strings.Contains(prefix, want) {
			t.Errorf("the shared prefix (%d bytes) lacks %.60q", shared, want)
		}
	}
	for _, volatile := range []string{"likes tea", "reading the Hotstar page", "Right now it is"} {
		if strings.Contains(prefix, volatile) {
			t.Errorf("the shared prefix carries changing text %q", volatile)
		}
	}
}

// On 2026-09-04 gemini-3.5-flash-lite answered 503 UNAVAILABLE ("high demand") for a whole evening and every ask through the window died with it. A 503 is the one failure a second model can answer; everything else (a bad key, a bad request, a cancelled context) would fail the same way on any model and must not be retried.
func TestShouldFallBack_OnlyOnUnavailable(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"503":         {genai.APIError{Code: 503, Status: "UNAVAILABLE"}, true},
		"503 pointer": {&genai.APIError{Code: 503}, true},
		"wrapped 503": {fmt.Errorf("ask text: generate (iteration 0): %w", genai.APIError{Code: 503}), true},
		"429":         {genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"}, false},
		"400":         {genai.APIError{Code: 400}, false},
		"plain":       {errors.New("dial tcp: connection refused"), false},
		"nil":         {nil, false},
	}
	for name, c := range cases {
		if got := shouldFallBack(c.err); got != c.want {
			t.Errorf("%s: shouldFallBack = %v, want %v", name, got, c.want)
		}
	}
	if config.TextFallbackModel == "" || config.TextFallbackModel == config.TextModel {
		t.Errorf("TextFallbackModel = %q, want a model other than TextModel %q", config.TextFallbackModel, config.TextModel)
	}
}

