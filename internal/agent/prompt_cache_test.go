package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"june/internal/db"
)

// geminiRequest is the part of one recorded GenerateContent body this file compares between rounds.
type geminiRequest struct {
	Contents          []json.RawMessage `json:"contents"`
	SystemInstruction json.RawMessage   `json:"systemInstruction"`
	Tools             json.RawMessage   `json:"tools"`
}

// geminiScript stands a fake Gemini backend up that answers each round with the next scripted response body and records every request it received, and points geminiBaseURL at it for the length of the test. Input: the test, where to collect the requests, and one JSON response per round, the last of which is repeated once the script runs out. Output: none.
func geminiScript(t *testing.T, got *[]geminiRequest, rounds ...string) {
	t.Helper()
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req geminiRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
		}
		*got = append(*got, req)
		i := n
		if i >= len(rounds) {
			i = len(rounds) - 1
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, rounds[i])
	}))
	t.Cleanup(backend.Close)
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })
}

// geminiObserveRound is a response that calls observe_screen once; geminiAnswerRound is one that answers and ends the loop.
const (
	geminiObserveRound = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`
	geminiAnswerRound  = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]}}]}`
)

// stableHead is the part of an ask's system instruction that must read the same on every ask: everything up to the end of the screen-task guidance, which is where systemInstructionTail — the personal block, the memory lines and the clock — begins. Input: a rendered instruction. Output: how many bytes of it are the stable head, or -1 when the guidance is not in it at all.
func stableHead(instruction string) int {
	i := strings.Index(instruction, screenTaskGuidance)
	if i < 0 {
		return -1
	}
	return i + len(screenTaskGuidance)
}

// Gemini serves a repeated prompt out of its own implicit cache only when the request's prefix is the same bytes, and only from 2,048 tokens of prefix upwards on the flash models. A screen ask therefore has to send one prefix for the whole of itself: the same system instruction, the same tool declarations, and the same leading contents on every round, with each round only appending to what the round before it sent. Before this, round 0 sent the 26 KB handshake, the whole tool list and the whole thread, and round 1 swapped all three, so nothing of round 0 could be reused.
func TestAskText_ScreenAskSendsOnePrefixOnEveryRound(t *testing.T) {
	var got []geminiRequest
	geminiScript(t, &got, geminiObserveRound, geminiObserveRound, geminiAnswerRound)
	a, _ := observingAgent(t)

	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "first question", Kind: "ask"},
		{Role: "june", Text: "first answer", Kind: "ask"},
		{Role: "you", Text: "second question", Kind: "ask"},
		{Role: "june", Text: "second answer", Kind: "ask"},
		{Role: "you", Text: "third question", Kind: "ask"},
		{Role: "june", Text: "third answer", Kind: "ask"},
	})
	if _, err := a.askText(t.Context(), "gemini-test", history, "click the merge button"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d rounds, want 3", len(got))
	}
	// Round 0 sends the thread and this turn and nothing else, so its whole contents array is exactly the prefix every later round has to open with.
	prefix := len(got[0].Contents)
	for i := 1; i < len(got); i++ {
		if string(got[i].SystemInstruction) != string(got[0].SystemInstruction) {
			t.Errorf("round %d sent a different system instruction than round 0:\n%s\n%s", i, got[i].SystemInstruction, got[0].SystemInstruction)
		}
		if string(got[i].Tools) != string(got[0].Tools) {
			t.Errorf("round %d sent different tool declarations than round 0:\n%s\n%s", i, got[i].Tools, got[0].Tools)
		}
		if len(got[i].Contents) < prefix {
			t.Fatalf("round %d sent %d contents, fewer than round 0's %d: the front of the thread was trimmed", i, len(got[i].Contents), prefix)
		}
		for j := 0; j < prefix; j++ {
			if string(got[i].Contents[j]) != string(got[0].Contents[j]) {
				t.Errorf("round %d content %d differs from round 0's:\n%s\n%s", i, j, got[i].Contents[j], got[0].Contents[j])
			}
		}
	}
	if !strings.Contains(string(got[0].SystemInstruction), "one task to see through") {
		t.Errorf("round 0 of a question naming a screen task must open on the screen instruction, got %s", got[0].SystemInstruction)
	}
	if body := string(got[0].Contents[0]); strings.Contains(body, "first question") || strings.Contains(body, "second question") {
		t.Errorf("round 0 must already carry the cut thread, got %s", body)
	}
}

// The handshake the ask paths open with is what a provider's prompt cache has to match, and a cache can only match a prefix. Everything that reads the same on every ask therefore comes first — who June is, how it talks, this machine, the screen-task guidance — and the three things that change (the personal block, the memory lines, the clock) are the tail. Two asks a minute apart must agree on every byte up to that tail.
func TestHandshakeInstruction_OnlyTheTailChangesBetweenAsks(t *testing.T) {
	const personal = "Personal context — things known for certain about the user and their world:\n  Their name is Vexil."
	const contextStr = "  [working] Brave: some tab"
	now := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	first := handshakeInstruction(now, personal, contextStr, 20)
	second := handshakeInstruction(now.Add(time.Minute), personal, contextStr, 20)

	if first == second {
		t.Fatal("a minute apart must still render a different clock sentence, or this test proves nothing")
	}
	head := stableHead(first)
	if head < 0 {
		t.Fatal("the handshake no longer carries the screen-task guidance the stable head ends at")
	}
	if first[:head] != second[:head] {
		t.Error("two asks a minute apart differ before the tail; everything stable must come first")
	}
	if strings.Contains(first[:head], "Their name is Vexil.") {
		t.Error("the personal block is in the stable head; it changes between asks and belongs in the tail")
	}
	// 2,048 tokens is the smallest prefix the flash models will serve out of their implicit cache, and this codebase estimates four characters to the token (see internal/tally/weekly.go), so the stable head has to be at least this many characters to be cacheable at all.
	const minCacheableChars = 2048 * 4
	if head < minCacheableChars {
		t.Errorf("the stable head is %d characters, under the %d an implicit cache will match", head, minCacheableChars)
	}
}

// The Claude CLI caches the system prompt and the tool list it was started with, so two asks minutes apart read out of that cache only for as far as both are the same bytes. The system prompt goes into a fresh file every ask; this checks what is written into it — the same stable head, and only then the part that genuinely changed.
func TestAskClaude_SystemPromptFileIsTheSameUpToItsTail(t *testing.T) {
	var prompts []string
	stub := claudeStub(nil, "Done.", `{"input_tokens":1,"output_tokens":1}`)
	run := func(ctx context.Context, args []string, stdin string) ([]byte, error) {
		for i, arg := range args {
			if arg == "--system-prompt-file" {
				raw, err := os.ReadFile(args[i+1])
				if err != nil {
					return nil, err
				}
				prompts = append(prompts, string(raw))
			}
		}
		return stub(ctx, args, stdin)
	}
	a := NewAgent(nil, nil, &toolTestBrain{personal: map[string]string{"identity": "Their name is Vexil."}}, nil, "")
	for _, question := range []string{"what did we settle on for the venue", "and what about the date"} {
		if _, err := a.askClaude(t.Context(), run, "claude-test", nil, question); err != nil {
			t.Fatal(err)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("%d system prompts written, want 2", len(prompts))
	}
	head := stableHead(prompts[0])
	if head < 0 {
		t.Fatal("the system prompt no longer carries the screen-task guidance the stable head ends at")
	}
	if prompts[0][:head] != prompts[1][:head] {
		t.Error("two asks wrote different system prompts before the tail; everything stable must come first")
	}
	if strings.Contains(prompts[0][:head], "Their name is Vexil.") {
		t.Error("the personal block is in the stable head; it changes between asks and belongs in the tail")
	}
}

// The CLI is handed its tool list on the command line, and a list built by ranging over a Go map would come out in a different order every ask — which changes the bytes the CLI caches even though the tools themselves are identical. This pins the order as the order the declarations are declared in.
func TestClaudeArgs_ToolOrderIsTheSameOnEveryAsk(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	allowed := func() string {
		names := make([]string, 0)
		for _, d := range a.askToolDeclarations() {
			names = append(names, d.Name)
		}
		args := claudeArgs("claude-test", "/tmp/mcp.json", "/tmp/system.txt", names)
		for i, arg := range args {
			if arg == "--allowed-tools" {
				return args[i+1]
			}
		}
		t.Fatal("no --allowed-tools in the arguments")
		return ""
	}
	first := allowed()
	if first == "" {
		t.Fatal("the allowed-tools list came out empty")
	}
	for i := 0; i < 20; i++ {
		if got := allowed(); got != first {
			t.Fatalf("the allowed-tools list changed between asks:\n%s\n%s", got, first)
		}
	}
}
