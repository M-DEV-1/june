package main

// Track 7's real logic is the turn loop, the read-only guarantee, the two prompts, and the report shape. The models are faked here; the one real run is the integration proof.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"ora/internal/agent"
	"ora/internal/db"
)

// hashFile is the read-only proof: the store's bytes before and after a run of write tools.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestRunTrajTurn_TerminatesAtTheToolRoundCap uses an arm that never stops calling tools. The loop has to end anyway, run exactly trajToolRounds tools, and say in the turn's error why it stopped.
func TestRunTrajTurn_TerminatesAtTheToolRoundCap(t *testing.T) {
	steps := 0
	arm := func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
		steps++
		if steps > 50 {
			t.Fatal("the turn loop never terminated")
		}
		return &trajCall{Name: "query_memory", Args: map[string]any{"query": fmt.Sprint(steps)}}, "still looking", nil
	}
	exec := func(ctx context.Context, name string, args map[string]any) string {
		return "row " + fmt.Sprint(args["query"])
	}

	turns := []trajTurn{{User: "what was I doing yesterday"}}
	got := runTrajTurn(context.Background(), "SYS", arm, exec, turns)

	if len(got.Calls) != trajToolRounds {
		t.Errorf("want %d tool calls, got %d", trajToolRounds, len(got.Calls))
	}
	if got.Calls[0].Result != "row 1" {
		t.Errorf("tool result not fed back: %q", got.Calls[0].Result)
	}
	if !strings.Contains(got.Err, "tool-round cap") {
		t.Errorf("turn should say why it stopped, got %q", got.Err)
	}
	if got.Reply != "still looking" {
		t.Errorf("the cap should keep whatever it had said, got %q", got.Reply)
	}
}

// TestRunTrajTurn_AnswersWithoutTools is the other end of the loop: an arm that replies straight away runs nothing and comes back clean.
func TestRunTrajTurn_AnswersWithoutTools(t *testing.T) {
	arm := func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
		return nil, "the eval harness, mostly", nil
	}
	exec := func(ctx context.Context, name string, args map[string]any) string {
		t.Fatal("no tool should have run")
		return ""
	}
	got := runTrajTurn(context.Background(), "SYS", arm, exec, []trajTurn{{User: "hey"}})
	if got.Reply != "the eval harness, mostly" || len(got.Calls) != 0 || got.Err != "" {
		t.Errorf("got %+v", got)
	}
}

// TestTrajExec_ReadsRealMemoryAndStubsEveryWrite is the read-only guarantee. The read tools go through the daemon's own dispatch and find a real row; every stubbed tool returns a plausible string and leaves the database byte-identical.
func TestTrajExec_ReadsRealMemoryAndStubsEveryWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	noteID, err := store.LogNote(ctx, "the user prefers oat milk lattes", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	ag := agent.NewAgent(nil, nil, store, nil, "FAKE_API_KEY")

	if got := trajExec(ag, ctx, "query_memory", map[string]any{"query": "oat milk"}); !strings.Contains(got, "oat milk lattes") {
		t.Errorf("query_memory should reach the real store, got %q", got)
	}
	if got := trajExec(ag, ctx, "get_recent", map[string]any{}); got == "" {
		t.Error("get_recent returned nothing at all")
	}

	before := hashFile(t, path)
	for name := range trajStubs {
		args := map[string]any{
			"content": "a fabricated note", "id": float64(noteID), "state": "wrong",
			"subject": "name", "command": "rm -rf /", "url": "http://example.com",
			"path": "/etc/passwd", "task": "catch me up",
		}
		got := trajExec(ag, ctx, name, args)
		if got != trajStubs[name] {
			t.Errorf("%s: want the stub %q, got %q", name, trajStubs[name], got)
		}
	}
	if after := hashFile(t, path); after != before {
		t.Errorf("a stubbed tool wrote to the store: %s -> %s", before, after)
	}

	// The note is still there and still says what it said, which is the check that matters more than the hash: a write that landed and got checkpointed later would still be visible here.
	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0].Content, "oat milk") {
		t.Errorf("the store changed under the stubs: %+v", notes)
	}
	if got := trajExec(ag, ctx, "no_such_tool", nil); !strings.HasPrefix(got, "error:") {
		t.Errorf("an unknown tool should come back as an error, got %q", got)
	}
}

// TestParseArmReply covers the Claude arm's text protocol: a tool line wins over surrounding narration, a spoken reply survives its own newlines, and a reply with no marker at all is kept whole rather than lost.
func TestParseArmReply(t *testing.T) {
	call, spoken, err := parseArmReply("TOOL: query_memory {\"query\":\"riddler\"}")
	if err != nil || call == nil || call.Name != "query_memory" || call.Args["query"] != "riddler" || spoken != "" {
		t.Errorf("tool line: %+v %q %v", call, spoken, err)
	}
	call, spoken, err = parseArmReply("SPOKEN: You were on the harness.\nAll morning.")
	if err != nil || call != nil || spoken != "You were on the harness.\nAll morning." {
		t.Errorf("spoken: %+v %q %v", call, spoken, err)
	}
	call, spoken, err = parseArmReply("Just an answer with no format.")
	if err != nil || call != nil || spoken != "Just an answer with no format." {
		t.Errorf("fallback: %+v %q %v", call, spoken, err)
	}
	if _, _, err := parseArmReply("TOOL: recall {not json}"); err == nil {
		t.Error("unreadable args should be an error, not a silent empty call")
	}
	call, _, _ = parseArmReply("TOOL: read_clipboard")
	if call == nil || call.Name != "read_clipboard" || len(call.Args) != 0 {
		t.Errorf("a no-argument tool call: %+v", call)
	}
}

// TestClaudeArmPrompt_CarriesTheHarness checks that the Claude arm's one prompt holds everything the Gemini arm gets over the wire: the real system prompt, the tool surface, the protocol, the history with its tool results, and the current message.
func TestClaudeArmPrompt_CarriesTheHarness(t *testing.T) {
	turns := []trajTurn{
		{User: "what was I doing yesterday", Calls: []trajCall{{Name: "recall", Args: map[string]any{"since": "2026-08-29"}, Result: "the eval harness"}}, Reply: "the harness, mostly"},
		{User: "and before that?"},
	}
	p := claudeArmPrompt("SYSPROMPT", describeTools(agent.ToolDeclarations()), turns)
	for _, want := range []string{
		"SYSPROMPT",
		"query_memory(",
		"TOOL: <name>",
		"SPOKEN: <what you say",
		"USER: what was I doing yesterday",
		`TOOL recall {"since":"2026-08-29"} -> the eval harness`,
		"YOU: the harness, mostly",
		"USER: and before that?",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("claude prompt missing %q", want)
		}
	}
}

// TestWriteTrajectoryFile_SideBySideShape checks the report: named for the run's start, headline counts, a per-arm coherence row, and both arms' tool calls and replies under each matched turn with the verdict.
func TestWriteTrajectoryFile_SideBySideShape(t *testing.T) {
	run := trajRun{
		Started: time.Date(2026, 8, 30, 16, 4, 5, 0, time.Local),
		Model:   "gemini-3.5-flash-lite",
		Notes:   []string{"a note about the run"},
		Gemini: []trajTurn{
			{User: "what was I doing yesterday", Calls: []trajCall{{Name: "recall", Args: map[string]any{"since": "2026-08-29"}, Result: "the eval harness"}}, Reply: "the harness"},
			{User: "and my meeting?", Reply: "nothing about that"},
		},
		Claude: []trajTurn{
			{User: "what was I doing yesterday", Calls: []trajCall{{Name: "query_memory", Args: map[string]any{"query": "yesterday"}, Result: "the eval harness"}}, Reply: "eval harness, all day"},
			{User: "who was in the standup?", Reply: "no minutes in there", Err: "judge: boom"},
		},
		Pairs: []trajPairVerdict{
			{Turn: 0, Verdict: "claude", Why: "more specific"},
			{Turn: 1, Verdict: "tie", Why: "both said nothing was there"},
		},
		Grades: map[string]trajGrade{
			"gemini": {Grade: "mixed", Why: "repeated a lookup"},
			"claude": {Grade: "strong", Why: "held the thread"},
		},
	}
	dir := t.TempDir()
	path, err := writeTrajectoryFile(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "2026-08-30T16-04-05.md" {
		t.Errorf("file name: %s", filepath.Base(path))
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"> Note: a note about the run",
		"| 2 | 0 | 1 | 1 |",
		"| gemini | 2 | 1 | mixed | repeated a lookup |",
		"| claude | 2 | 1 | strong | held the thread |",
		"## Turn 1",
		"**USER → GEMINI:** what was I doing yesterday",
		"`recall {\"since\":\"2026-08-29\"}`",
		"**GEMINI:** the harness",
		"**USER → CLAUDE:** what was I doing yesterday",
		"**CLAUDE:** eval harness, all day",
		"Verdict: **claude** — more specific",
		"## Turn 2",
		"Error: judge: boom",
		"Verdict: **tie** — both said nothing was there",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("trajectory file missing %q", want)
		}
	}
}

// TestTextDecls_DropsTheLiveOnlyBehavior guards the one difference between what the live session declares and what a text model can be handed: NON_BLOCKING is a Live API contract and generateContent rejects it. Names, descriptions and schemas must survive untouched, or the two arms are no longer on the same harness.
func TestTextDecls_DropsTheLiveOnlyBehavior(t *testing.T) {
	live := agent.ToolDeclarations()
	if len(live) == 0 {
		t.Fatal("no tool declarations came back")
	}
	got := textDecls(live)
	if len(got) != len(live) {
		t.Fatalf("want %d declarations, got %d", len(live), len(got))
	}
	for i, d := range got {
		if d.Behavior != "" {
			t.Errorf("%s kept its live-only behavior %q", d.Name, d.Behavior)
		}
		if d.Name != live[i].Name || d.Description != live[i].Description {
			t.Errorf("declaration %d changed: %s vs %s", i, d.Name, live[i].Name)
		}
		if live[i].Behavior == "" {
			t.Errorf("%s: the live declaration was mutated in place", d.Name)
		}
	}
	if !strings.Contains(describeTools(got), "query_memory(app, domain, kind, query, since, until)") {
		t.Errorf("the tool surface should list parameters in a stable order:\n%s", describeTools(got))
	}
}

// TestGeminiContents_RendersTheSameConversation checks the Gemini arm's side of the seam: one user part per message, a model function call and its response per tool, and the reply as a model turn — so both arms see the identical history in their own dialect.
func TestGeminiContents_RendersTheSameConversation(t *testing.T) {
	turns := []trajTurn{
		{User: "hey", Calls: []trajCall{{Name: "recall", Args: map[string]any{"since": "today"}, Result: "rows"}}, Reply: "the harness"},
		{User: "and before?"},
	}
	got := geminiContents(turns)
	if len(got) != 5 {
		t.Fatalf("want 5 contents, got %d", len(got))
	}
	if got[1].Parts[0].FunctionCall == nil || got[1].Parts[0].FunctionCall.Name != "recall" {
		t.Errorf("content 1 should be the function call: %+v", got[1].Parts[0])
	}
	fr := got[2].Parts[0].FunctionResponse
	if fr == nil || fr.Response["output"] != "rows" {
		t.Errorf("content 2 should be the function response: %+v", got[2].Parts[0])
	}
	if got[3].Parts[0].Text != "the harness" || got[4].Parts[0].Text != "and before?" {
		t.Errorf("reply and next user turn: %+v", got[3:])
	}
}

// TestGeminiContents_ReplaysTheSignedFunctionCall guards the one thing that cannot be rebuilt from Name and Args: Gemini 3 signs each function call with a thought_signature and rejects the next request if the history hands the call back without it. The captured model turn has to go back verbatim.
func TestGeminiContents_ReplaysTheSignedFunctionCall(t *testing.T) {
	raw := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionCall:     &genai.FunctionCall{Name: "recall", Args: map[string]any{"since": "today"}},
		ThoughtSignature: []byte("signed"),
	}}}
	got := geminiContents([]trajTurn{{User: "hey", Calls: []trajCall{{Name: "recall", Result: "rows", Raw: raw}}}})
	if len(got) != 3 || got[1] != raw {
		t.Fatalf("the captured model turn should be replayed as-is, got %+v", got)
	}
	if string(got[1].Parts[0].ThoughtSignature) != "signed" {
		t.Error("the thought signature was dropped")
	}
}
