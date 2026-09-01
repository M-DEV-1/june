package main

// Track 5's real logic is the session reconstruction, the prompt composition, and the output file shape; the teacher and the judge are faked here, and the one real run is the integration proof.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureLog = `not json
{"time":"2026-08-30T14:00:00+05:30","msg":"connected to Live API"}
{"time":"2026-08-30T14:00:01+05:30","msg":"ora said","text":"Hey, welcome back."}
{"time":"2026-08-30T14:17:22+05:30","msg":"connected to Live API"}
{"time":"2026-08-30T14:17:30+05:30","msg":"ora said","text":"Morning."}
{"time":"2026-08-30T14:17:40+05:30","msg":"user said (voice)","text":"what was I doing yesterday"}
{"time":"2026-08-30T14:17:41+05:30","msg":"tool call received","tool":"query_memory","args":{"query":"yesterday"}}
{"time":"2026-08-30T14:17:42+05:30","msg":"tool result","tool":"query_memory","result":"[thread#1] eval harness work"}
{"time":"2026-08-30T14:17:45+05:30","msg":"ora said","text":"You were on the eval harness."}
{"time":"2026-08-30T14:18:00+05:30","msg":"sending text to model","text":"and before that?"}
{"time":"2026-08-30T14:18:05+05:30","msg":"ora said","text":"Before that, the recorder."}
`

// TestParseReplaySessions_ReconstructsTurnsAndToolResults is the shape everything downstream depends on: sessions split on the connect marker, a session with no user turn dropped, and a tool result attached to its call.
func TestParseReplaySessions_ReconstructsTurnsAndToolResults(t *testing.T) {
	sessions := parseReplaySessions(bufio.NewScanner(strings.NewReader(fixtureLog)))
	if len(sessions) != 1 {
		t.Fatalf("want 1 session (the greeting-only one dropped), got %d", len(sessions))
	}
	s := sessions[0]
	if s.Start.Format("15:04:05") != "14:17:22" {
		t.Errorf("session start: got %s", s.Start.Format(time.RFC3339))
	}
	if len(s.Events) != 6 {
		t.Fatalf("want 6 events, got %d: %+v", len(s.Events), s.Events)
	}
	if idx := userTurnIndexes(s); len(idx) != 2 || s.Events[idx[0]].Mode != "voice" || s.Events[idx[1]].Mode != "typed" {
		t.Errorf("user turns: %v", idx)
	}
	var tool *replayEvent
	for i := range s.Events {
		if s.Events[i].Kind == "tool" {
			tool = &s.Events[i]
		}
	}
	if tool == nil || tool.Result != "[thread#1] eval harness work" || !strings.Contains(tool.Args, "yesterday") {
		t.Errorf("tool event: %+v", tool)
	}
}

// TestComposeReplayPrompt_HistoryAndLabeledEvidence checks the counterfactual's contract: the prompt for turn N carries the system prompt, the real history (live replies included), the current user turn, and this turn's tool results labeled as evidence — and the next turn's history carries the LIVE reply, not Claude's.
func TestComposeReplayPrompt_HistoryAndLabeledEvidence(t *testing.T) {
	s := parseReplaySessions(bufio.NewScanner(strings.NewReader(fixtureLog)))[0]
	turns := userTurnIndexes(s)

	p := composeReplayPrompt("SYSPROMPT", s, turns[0])
	for _, want := range []string{
		"SYSPROMPT",
		"ORA: Morning.",
		"The user now says (voice): what was I doing yesterday",
		"TOOL CALL: query_memory",
		"TOOL RESULT: [thread#1] eval harness work",
		"treat each result below as the result of a tool call you made yourself",
		"TOOLS:",
		"SPOKEN:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("turn 1 prompt missing %q", want)
		}
	}
	if strings.Contains(p, "and before that?") {
		t.Error("turn 1 prompt leaked a future turn")
	}

	p2 := composeReplayPrompt("SYSPROMPT", s, turns[1])
	if !strings.Contains(p2, "ORA: You were on the eval harness.") {
		t.Error("turn 2 history is missing the LIVE reply for turn 1")
	}
	if !strings.Contains(p2, "The live session called no tools for this turn.") {
		t.Error("turn 2 should say no tools were called")
	}
}

// TestParseReplayReply covers the teacher's two-line format and the fallback that keeps a free-form reply as the spoken row.
func TestParseReplayReply(t *testing.T) {
	tools, spoken := parseReplayReply("TOOLS: query_memory({\"query\":\"x\"})\nSPOKEN: You were on the harness.")
	if tools != `query_memory({"query":"x"})` || spoken != "You were on the harness." {
		t.Errorf("got %q / %q", tools, spoken)
	}
	tools, spoken = parseReplayReply("Just an answer with no format.")
	if tools != "" || spoken != "Just an answer with no format." {
		t.Errorf("fallback: got %q / %q", tools, spoken)
	}
}

// TestWriteReplayFile_SideBySideShape checks the output file: named for the session start, headline counts, and per-turn USER/LIVE/CLAUDE sections with the live tool calls and the verdict.
func TestWriteReplayFile_SideBySideShape(t *testing.T) {
	s := parseReplaySessions(bufio.NewScanner(strings.NewReader(fixtureLog)))[0]
	turns := userTurnIndexes(s)
	rep := sessionReplay{Session: s, Turns: []replayTurnResult{
		{UserIdx: turns[0], ClaudeTools: "none", ClaudeSpoken: "The eval harness, mostly.", V: replayVerdict{Verdict: "claude", Why: "more direct"}},
		{UserIdx: turns[1], ClaudeTools: "none", ClaudeSpoken: "The recorder.", V: replayVerdict{Verdict: "tie", Why: "same content"}},
	}}
	dir := t.TempDir()
	path, err := writeReplayFile(dir, rep)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "2026-08-30T14-17-22.md" {
		t.Errorf("file name: %s", filepath.Base(path))
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| 2 | 1 | 1 | 0 |",
		"**USER (voice):** what was I doing yesterday",
		"`query_memory",
		"**LIVE (gemini):** You were on the eval harness.",
		"**CLAUDE** (tools: none)**:** The eval harness, mostly.",
		"Verdict: **claude** — more direct",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("replay file missing %q", want)
		}
	}
}
