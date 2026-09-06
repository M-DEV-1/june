package main

// The runner's only real logic is the log parser and the judge-reply decoder; everything else is a Gemini call or a string format. These are the checks that fail if either of those two breaks.

import (
	"bufio"
	"context"
	"strings"
	"testing"

	"ora/internal/agent"
	"ora/internal/db"

	"ora/internal/brain"
)

// TestParseTurnPairs_PairsEachReplyWithTheQuestionBeforeIt is the shape track 2 depends on: one pair per "ora said", carrying the last user turn before it, whether that turn was typed or spoken.
func TestParseTurnPairs_PairsEachReplyWithTheQuestionBeforeIt(t *testing.T) {
	log := `{"time":"2026-08-28T13:14:50+05:30","level":"DEBUG","msg":"sending text to model","text":"what have i been doing"}
{"time":"2026-08-28T13:15:00+05:30","level":"INFO","msg":"ora said","text":"You've been on the memory work."}
{"time":"2026-08-28T13:15:40+05:30","level":"INFO","msg":"user said (voice)","text":"what does that even mean"}
{"time":"2026-08-28T13:15:50+05:30","level":"INFO","msg":"ora said","text":"It was a search you ran."}
`
	pairs := parseTurnPairs(bufio.NewScanner(strings.NewReader(log)))
	if len(pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d", len(pairs))
	}
	if pairs[0].UserText != "what have i been doing" || pairs[0].UserMode != "typed" {
		t.Errorf("first pair: got %q (%s)", pairs[0].UserText, pairs[0].UserMode)
	}
	if pairs[1].UserText != "what does that even mean" || pairs[1].UserMode != "voice" {
		t.Errorf("second pair: got %q (%s)", pairs[1].UserText, pairs[1].UserMode)
	}
	if pairs[1].OraText != "It was a search you ran." {
		t.Errorf("second reply: got %q", pairs[1].OraText)
	}
}

// TestParseTurnPairs_SecondReplyDoesNotReuseTheQuestion guards the case that would silently corrupt the scorecard: a generation that emits two "ora said" lines must not score the second one against a question it was not answering, and a greeting on connect has no question at all.
func TestParseTurnPairs_SecondReplyDoesNotReuseTheQuestion(t *testing.T) {
	log := `{"time":"2026-08-28T13:14:50+05:30","msg":"sending text to model","text":"summarise my day"}
{"time":"2026-08-28T13:15:00+05:30","msg":"ora said","text":"first sentence"}
{"time":"2026-08-28T13:15:02+05:30","msg":"ora said","text":"second sentence"}
`
	pairs := parseTurnPairs(bufio.NewScanner(strings.NewReader(log)))
	if len(pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d", len(pairs))
	}
	if pairs[0].UserText != "summarise my day" {
		t.Errorf("first pair lost its question: %q", pairs[0].UserText)
	}
	if pairs[1].UserText != "" {
		t.Errorf("second pair reused the question: %q", pairs[1].UserText)
	}
}

// TestParseTurnPairs_StaleQuestionIsNotAttached checks the five-minute window: a reply long after the last thing the user said is a session-opening greeting, not an answer to it.
func TestParseTurnPairs_StaleQuestionIsNotAttached(t *testing.T) {
	log := `{"time":"2026-08-28T13:00:00+05:30","msg":"user said (voice)","text":"an hour ago"}
{"time":"2026-08-28T14:00:00+05:30","msg":"ora said","text":"Hey, welcome back."}
`
	pairs := parseTurnPairs(bufio.NewScanner(strings.NewReader(log)))
	if len(pairs) != 1 || pairs[0].UserText != "" {
		t.Fatalf("stale question attached: %+v", pairs)
	}
}

// TestParseTurnPairs_SkipsNonJSONAndOtherMessages checks the parser survives the real log, which carries thousands of lines that are neither turns nor, occasionally, valid JSON.
func TestParseTurnPairs_SkipsNonJSONAndOtherMessages(t *testing.T) {
	log := `not json at all
{"time":"2026-08-28T13:14:00+05:30","msg":"activity tracked","text":"whatever"}
{"time":"2026-08-28T13:14:50+05:30","msg":"sending text to model","text":"hi"}
{"time":"2026-08-28T13:15:00+05:30","msg":"ora said","text":"hello"}
`
	pairs := parseTurnPairs(bufio.NewScanner(strings.NewReader(log)))
	if len(pairs) != 1 || pairs[0].UserText != "hi" {
		t.Fatalf("got %+v", pairs)
	}
}

// TestStripFence covers the one way a judge reply is routinely unreadable: the model wraps its JSON in a markdown fence despite being asked for a JSON MIME type.
func TestStripFence(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```":     `{"a":1}`,
		`{"a":1}`:                 `{"a":1}`,
		"  {\"a\":1}  ":           `{"a":1}`,
	}
	for in, want := range cases {
		if got := brain.StripFence(in); got != want {
			t.Errorf("brain.StripFence(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRateExcludesNotApplicable is the rule that keeps the scorecard honest: a criterion the turn gave no occasion to test is left out of the denominator rather than counted as a free pass.
func TestRateExcludesNotApplicable(t *testing.T) {
	p, a := rate([]verdict{{Verdict: "pass"}, {Verdict: "fail"}, {Verdict: "na"}})
	if p != 1 || a != 2 {
		t.Fatalf("rate = %d/%d, want 1/2", p, a)
	}
	if got := pct(p, a); got != "1/2 (50%)" {
		t.Errorf("pct = %q", got)
	}
	if got := pct(0, 0); got != "n/a" {
		t.Errorf("pct with nothing applicable = %q, want n/a", got)
	}
}

// TestToolPathSearch_RunsQuestionsThroughTheRealQueryMemoryTool verifies the -tool-path replay: a question goes through the agent's ExecuteTool dispatch (the exact path the model's function calls take), a question's args ride along so a since/until window reaches the store, and the tool's honest none-in-window answer comes back as a row for the judge rather than being mistaken for content.
func TestToolPathSearch_RunsQuestionsThroughTheRealQueryMemoryTool(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.LogNote(ctx, "the user prefers oat milk lattes", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}
	search := toolPathSearch(agent.NewAgent(nil, nil, store, nil, "FAKE_API_KEY"))

	rows, err := search(ctx, question{Question: "oat milk"})
	if err != nil {
		t.Fatalf("plain question: %v", err)
	}
	if len(rows) == 0 || !strings.Contains(strings.Join(rows, "\n"), "oat milk lattes") {
		t.Errorf("expected the note to come back through the tool path, got: %v", rows)
	}

	rows, err = search(ctx, question{Question: "oat milk", Args: map[string]any{"since": "2020-01-01", "until": "2020-01-02"}})
	if err != nil {
		t.Fatalf("windowed question: %v", err)
	}
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "lattes") {
		t.Errorf("expected no out-of-window content, got: %v", rows)
	}
	if !strings.Contains(joined, "none") {
		t.Errorf("expected the tool's none-in-window answer to reach the judge as a row, got: %v", rows)
	}

	rows, err = search(ctx, question{Question: "zzqqxx absent topic"})
	if err != nil {
		t.Fatalf("absent topic: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf(`expected "no memory matches" to come back as zero rows, got: %v`, rows)
	}
}

// TestLiveGuard covers the refusal that keeps tracks 10 and 11 off the user's own machine by accident: both drive the running daemon over HTTP, so every click, every keystroke and every tool the model reaches for lands on the live screen and the live store. Running them there has to be asked for with -live; a daemon started on another port for the purpose is allowed without it.
func TestLiveGuard(t *testing.T) {
	if why := liveGuard(daemonAddr, false); why == "" {
		t.Errorf("liveGuard(%s, live=false) allowed the run; want a refusal naming the live daemon", daemonAddr)
	} else if !strings.Contains(why, "-live") {
		t.Errorf("liveGuard refusal does not say how to proceed: %q", why)
	}
	if why := liveGuard(daemonAddr, true); why != "" {
		t.Errorf("liveGuard(%s, live=true) = %q, want the run allowed", daemonAddr, why)
	}
	if why := liveGuard("http://127.0.0.1:7777", false); why != "" {
		t.Errorf("liveGuard on a non-default port = %q, want the run allowed", why)
	}
}
