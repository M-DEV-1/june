package main

// The runner's only real logic is the log parser and the judge-reply decoder; everything else is a Gemini call or a string format. These are the checks that fail if either of those two breaks.

import (
	"bufio"
	"strings"
	"testing"
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
		if got := stripFence(in); got != want {
			t.Errorf("stripFence(%q) = %q, want %q", in, got, want)
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
