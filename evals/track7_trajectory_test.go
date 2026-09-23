package main

// Track 7's real logic is the turn loop, the read-only guarantee, the two prompts, and the report shape. The models are faked here; the one real run is the integration proof.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	if got := trajExec(ag, ctx, "recall", map[string]any{}); got == "" {
		t.Error("recall returned nothing at all")
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

// TestTrajExec_AnswersEveryDeclaredTool walks the real declarations both arms are handed and asserts every one of them is handled here, either by the daemon's own read path or by a stub. The loop above can only ever check the stubs against themselves; this is the check that notices a tool the harness declares and then does not answer, which tells the model "there is no tool called X" and costs the arm that reached for it a round it lost to the harness rather than to the other arm. The store still has to be byte-identical when the sweep is done.
func TestTrajExec_AnswersEveryDeclaredTool(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.LogNote(ctx, "the user prefers oat milk lattes", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	ag := agent.NewAgent(nil, nil, store, nil, "FAKE_API_KEY")

	args := map[string]map[string]any{
		"query_memory": {"query": "oat milk"},
		"query_store":  {"query": "SELECT count(*) AS n FROM notes"},
		"recall":       {},
		"action_items": {},
		"save_note":    {"content": "a fabricated note"},
		"revise":       {"ref": "note#1", "remove": true},
		"shell_exec":   {"command": "rm -rf /"},
		"type_text":    {"text": "rm -rf /", "enter": true},
		"click":        {"n": float64(1)},
	}

	before := hashFile(t, path)
	for _, d := range agent.ToolDeclarations() {
		got := trajExec(ag, ctx, d.Name, args[d.Name])
		if strings.Contains(got, "there is no tool called") {
			t.Errorf("%s is declared to both arms but the eval has no executor for it: %q", d.Name, got)
		}
	}
	if after := hashFile(t, path); after != before {
		t.Errorf("the declared-tool sweep wrote to the store: %s -> %s", before, after)
	}

	// The read tools have to be the daemon's own, not a stub: query_store answers with the column header it built from the query, and action_items with the store's own empty-list sentinel.
	if got := trajExec(ag, ctx, "query_store", args["query_store"]); !strings.HasPrefix(got, "n\n") {
		t.Errorf("query_store should run against the snapshot, got %q", got)
	}
	if got := trajExec(ag, ctx, "action_items", nil); !strings.Contains(got, "nothing outstanding") {
		t.Errorf("action_items should reach the real store, got %q", got)
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

// TestParseArmReply_SpokenKeepsAMarkerInsideItsOwnText pins the edge of the anywhere-in-the-text scan. Looking for TOOL: anywhere is what lets a model that runs its narration and its protocol line together still be read as a call, but it must not reach inside a reply that has already declared itself spoken: a refusal that mentions the marker is a reply, not a call for a tool named "for".
func TestParseArmReply_SpokenKeepsAMarkerInsideItsOwnText(t *testing.T) {
	call, spoken, err := parseArmReply("SPOKEN: I can't run that here — there is no TOOL: for it in this eval.")
	if err != nil || call != nil {
		t.Fatalf("a spoken reply that mentions the marker became %+v (err %v)", call, err)
	}
	if spoken != "I can't run that here — there is no TOOL: for it in this eval." {
		t.Errorf("spoken: %q", spoken)
	}
	// The shape the anywhere-scan was added for — narration and the protocol line with no newline between them — still has to parse as a call.
	call, _, err = parseArmReply(`I'll pull this week up.TOOL: recall {"since":"today"}`)
	if err != nil || call == nil || call.Name != "recall" || call.Args["since"] != "today" {
		t.Errorf("narration run together with the protocol line: %+v %v", call, err)
	}
}

// TestParseArmReply_IgnoresMarkupAroundTheMarker is the other edge of the anywhere-scan. A model that bolds the protocol line used to fail the "TOOL:" prefix check and fall through to being read as a spoken reply; found anywhere in the text, the marker now matches and the next word — the markup, not the tool — became the tool name, so the trajectory carried a call to a tool nobody named and the judge saw the arm reaching for a tool that does not exist.
func TestParseArmReply_IgnoresMarkupAroundTheMarker(t *testing.T) {
	call, _, err := parseArmReply(`**TOOL:** recall {"since":"today"}`)
	if err != nil || call == nil || call.Name != "recall" || call.Args["since"] != "today" {
		t.Errorf("a bolded protocol line: %+v %v", call, err)
	}
	call, _, err = parseArmReply("TOOL: `query_memory` {\"query\":\"riddler\"}")
	if err != nil || call == nil || call.Name != "query_memory" {
		t.Errorf("a quoted tool name: %+v %v", call, err)
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

// TestWriteTrajectoryFile_SideBySideShape checks the report: named for the run's start, headline counts with the judge errors alongside them, a per-arm coherence row, and both arms' tool calls and replies under each matched turn with the verdict. The gemini arm is missing message 3, so the section for it holds only the arm that answered and the sections are numbered by user message rather than by position — numbering by position relabelled every turn after a skip.
func TestWriteTrajectoryFile_SideBySideShape(t *testing.T) {
	run := trajRun{
		Started: time.Date(2026, 8, 30, 16, 4, 5, 0, time.Local),
		Model:   "gemini-3.5-flash-lite",
		Notes:   []string{"a note about the run"},
		Gemini: []trajTurn{
			{Msg: 1, User: "what was I doing yesterday", Calls: []trajCall{{Name: "recall", Args: map[string]any{"since": "2026-08-29"}, Result: "the eval harness"}}, Reply: "the harness"},
			{Msg: 2, User: "and my meeting?", Reply: "nothing about that"},
			{Msg: 4, User: "remember I moved desks", Reply: "got it"},
		},
		Claude: []trajTurn{
			{Msg: 1, User: "what was I doing yesterday", Calls: []trajCall{{Name: "query_memory", Args: map[string]any{"query": "yesterday"}, Result: "the eval harness"}}, Reply: "eval harness, all day"},
			{Msg: 2, User: "who was in the standup?", Reply: "no minutes in there", Err: "judge: boom"},
			{Msg: 3, User: "what did I say to mira", Reply: "nothing in there about mira"},
			{Msg: 4, User: "remember I moved desks", Reply: "saved"},
		},
		Pairs: []trajPairVerdict{
			{Turn: 1, Verdict: "claude", Why: "more specific"},
			{Turn: 2, Verdict: "tie", Why: "both said nothing was there"},
			{Turn: 4, Err: `the judge answered "whichever", which is neither A, B nor tie`},
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
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		"> Note: a note about the run",
		"| Matched turns | Gemini preferred | Tie | Claude preferred | Judge errored |",
		"| 3 | 0 | 1 | 1 | 1 |",
		"| gemini | 3 | 1 | mixed | repeated a lookup |",
		"| claude | 4 | 1 | strong | held the thread |",
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
		"## Turn 3",
		"## Turn 4",
		`Judge error: the judge answered "whichever", which is neither A, B nor tie`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("trajectory file missing %q", want)
		}
	}

	// Message 3 is the one the gemini arm never answered. Its section has to show the claude side alone rather than borrowing the gemini turn that came after the skip.
	three := body[strings.Index(body, "## Turn 3"):strings.Index(body, "## Turn 4")]
	if strings.Contains(three, "GEMINI") {
		t.Errorf("the skipped message pulled in a gemini turn that answered something else:\n%s", three)
	}
	if !strings.Contains(three, "**CLAUDE:** nothing in there about mira") {
		t.Errorf("the skipped message lost the arm that did answer it:\n%s", three)
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

// trajReplyArm is an arm that answers straight away with a fixed line and never runs a tool, so a conversation test moves one turn per message.
func trajReplyArm(reply string) armStep {
	return func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
		return nil, reply + " to " + turns[len(turns)-1].User, nil
	}
}

// trajNoExec fails the test if an arm runs a tool it was not supposed to run.
func trajNoExec(t *testing.T) func(context.Context, string, map[string]any) string {
	return func(ctx context.Context, name string, args map[string]any) string {
		t.Fatalf("no tool should have run, got %s", name)
		return ""
	}
}

// TestRunTrajConversations_ARoleplayFailureSkipsOneMessageAndTheRunCarriesOn is the rate-limit path. The roleplay model fails on one arm's third message, so that arm never gets a turn for it and its conversation ends up shorter than the message count. The run has to finish anyway — indexing the loop counter into the short slice panicked and killed the rest of a paid run — and every turn has to carry the message number it answered, because after a skip that is the only thing that still lines the two arms up.
func TestRunTrajConversations_ARoleplayFailureSkipsOneMessageAndTheRunCarriesOn(t *testing.T) {
	arms := []trajArm{
		{Name: "gemini", Step: trajReplyArm("gemini")},
		{Name: "claude", Step: trajReplyArm("claude")},
	}
	next := func(ctx context.Context, arm string, convo []trajTurn, msgNo int) (string, error) {
		if arm == "gemini" && msgNo == 3 {
			return "", fmt.Errorf("429 out of quota")
		}
		return fmt.Sprintf("%s message %d", arm, msgNo), nil
	}

	convo, notes := runTrajConversations(context.Background(), "SYS", arms, trajNoExec(t), next, 4)

	if got := trajTestMsgNos(convo["gemini"]); !slices.Equal(got, []int{1, 2, 4}) {
		t.Errorf("the gemini arm should be missing only the message that failed, got %v", got)
	}
	if got := trajTestMsgNos(convo["claude"]); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Errorf("the claude arm should have every message, got %v", got)
	}
	last := convo["gemini"][len(convo["gemini"])-1]
	if last.User != "gemini message 4" || last.Reply != "gemini to gemini message 4" {
		t.Errorf("the turn after the skip is the wrong one: %+v", last)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "gemini") || !strings.Contains(notes[0], "out of quota") {
		t.Errorf("a skipped message should leave a note saying so, got %v", notes)
	}
}

// trajTestMsgNos reads the message number off each turn, so a test can say which messages an arm actually answered.
func trajTestMsgNos(turns []trajTurn) []int {
	out := make([]int, 0, len(turns))
	for _, t := range turns {
		out = append(out, t.Msg)
	}
	return out
}

// TestPairTrajTurns_PairsByMessageNumberNotIndex is the skip that used to poison every later verdict. The gemini arm is missing message 3, so from that point on the slice indexes disagree: index pairing put gemini's message 4 against claude's message 3 and labelled the row with neither. Pairing by message number keeps every matched turn matched and drops the one only one arm answered.
func TestPairTrajTurns_PairsByMessageNumberNotIndex(t *testing.T) {
	gemini := []trajTurn{{Msg: 1, User: "g1"}, {Msg: 2, User: "g2"}, {Msg: 4, User: "g4"}}
	claude := []trajTurn{{Msg: 1, User: "c1"}, {Msg: 2, User: "c2"}, {Msg: 3, User: "c3"}, {Msg: 4, User: "c4"}}

	pairs := pairTrajTurns(gemini, claude)
	if len(pairs) != 3 {
		t.Fatalf("want the three messages both arms answered, got %d: %+v", len(pairs), pairs)
	}
	for i, want := range []int{1, 2, 4} {
		if pairs[i].Msg != want {
			t.Errorf("pair %d is message %d, want %d", i, pairs[i].Msg, want)
		}
		if pairs[i].Gemini.User != fmt.Sprintf("g%d", want) || pairs[i].Claude.User != fmt.Sprintf("c%d", want) {
			t.Errorf("pair %d compares two different points: %q vs %q", i, pairs[i].Gemini.User, pairs[i].Claude.User)
		}
	}
}

// TestTrajPairFromJudge_AnUnreadableVerdictIsAnErrorNotATie checks the whole mapping from one judge answer to one pair verdict, including the two failure paths: the call itself failing, and the call coming back with a verdict that is none of A, B or tie.
func TestTrajPairFromJudge_AnUnreadableVerdictIsAnErrorNotATie(t *testing.T) {
	v := trajPairFromJudge(3, "B", "more specific", nil)
	if v.Turn != 3 || v.Verdict != "claude" || v.Why != "more specific" || v.Err != "" {
		t.Errorf("a clean verdict: %+v", v)
	}
	v = trajPairFromJudge(3, "whichever", "hard to call", nil)
	if v.Verdict != "" {
		t.Errorf("an unrecognised verdict scored %q instead of erroring", v.Verdict)
	}
	if !strings.Contains(v.Err, "whichever") {
		t.Errorf("the error should quote what the judge actually said, got %q", v.Err)
	}
	v = trajPairFromJudge(3, "A", "", fmt.Errorf("429 out of quota"))
	if v.Verdict != "" || !strings.Contains(v.Err, "out of quota") {
		t.Errorf("a failed judge call: %+v", v)
	}
}

// TestTrajCounts_ErrorsAreTheirOwnBucket is the headline that used to lie. An errored pair counts in the matched-turn total and in none of the three buckets, so a run whose judge failed four times out of ten reported six verdicts under a total of ten and said nothing about the gap.
func TestTrajCounts_ErrorsAreTheirOwnBucket(t *testing.T) {
	pairs := []trajPairVerdict{
		{Turn: 1, Verdict: "gemini"},
		{Turn: 2, Verdict: "tie"},
		{Turn: 3, Err: "429 out of quota"},
		{Turn: 4, Err: "the judge answered \"whichever\""},
		{Turn: 5, Verdict: "claude"},
	}
	gemini, tie, claude, errs := trajCounts(pairs)
	if gemini != 1 || tie != 1 || claude != 1 || errs != 2 {
		t.Errorf("got gemini %d, tie %d, claude %d, errors %d", gemini, tie, claude, errs)
	}
	if gemini+tie+claude+errs != len(pairs) {
		t.Error("the buckets should account for every pair")
	}
}
