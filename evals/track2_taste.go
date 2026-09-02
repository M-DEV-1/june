package main

// Track 2 scores what Ora actually said. Its whole input is ora.log, which internal/agent/connect.go writes: "ora said" at line 363 (every completed or interrupted spoken turn), "user said (voice)" at line 378, and "sending text to model" at line 754 for a typed turn. A turn pair is one "ora said" plus the last user turn before it. "ora said" logging began 2026-08-28, so the corpus starts there and grows with every session — the taste criteria in seeds-2026-08-28.md are a forward-looking gate, not something backfillable.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// turnPair is one exchange: what the user said (empty for a session-opening greeting) and what Ora said back.
type turnPair struct {
	At       time.Time
	UserText string
	UserMode string // "voice" | "typed" | "" when Ora spoke first
	OraText  string
}

// logLine is the slog JSON shape ora.log is written in.
type logLine struct {
	Time time.Time `json:"time"`
	Msg  string    `json:"msg"`
	Text string    `json:"text"`
}

// pairWindow is how far back a turn pair will look for the user turn that prompted a reply. Beyond this the reply is treated as a session-opening greeting with no question behind it, which is what the handshake produces on connect.
const pairWindow = 5 * time.Minute

// parseTurnPairs reads ora.log and returns one pair per "ora said" line, each carrying the most recent user turn within pairWindow before it. Input: a reader over the log. Output: pairs in chronological order.
func parseTurnPairs(r *bufio.Scanner) []turnPair {
	var pairs []turnPair
	var lastUser logLine
	var lastMode string
	for r.Scan() {
		line := r.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var l logLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue
		}
		switch l.Msg {
		case "user said (voice)":
			lastUser, lastMode = l, "voice"
		case "sending text to model":
			lastUser, lastMode = l, "typed"
		case "ora said":
			p := turnPair{At: l.Time, OraText: l.Text}
			if !lastUser.Time.IsZero() && l.Time.Sub(lastUser.Time) <= pairWindow {
				p.UserText, p.UserMode = lastUser.Text, lastMode
			}
			pairs = append(pairs, p)
			// A user turn prompts one reply. Clearing it stops a second "ora said" in the same generation being scored against a question it was not answering.
			lastUser, lastMode = logLine{}, ""
		}
	}
	return pairs
}

// loadTurnPairs opens the log and returns at most limit pairs, most recent last. Input: log path and the cap. Output: the pairs, or an error if the log is unreadable.
func loadTurnPairs(path string, limit int) ([]turnPair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	pairs := parseTurnPairs(sc)
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(pairs) > limit {
		pairs = pairs[len(pairs)-limit:]
	}
	return pairs, nil
}

// track2Result is one scored turn: the pair, the ten taste criteria, and any D-case criteria the judge found applicable.
type track2Result struct {
	turnPair
	Taste  map[string]verdict `json:"taste"`
	DCases []dcaseScore       `json:"dcases"`
	Err    string             `json:"-"`
}

// dcaseScore is one D-case criterion the judge decided this turn was an instance of.
type dcaseScore struct {
	Case    string `json:"case"`
	Verdict string `json:"verdict"`
	Why     string `json:"why"`
}

// tasteIDs is the criterion order used everywhere the scorecard prints them.
var tasteIDs = []string{"T1", "T2", "T3", "T4", "T5", "T6", "T7", "T8", "T9", "T10"}

const track2Instruction = `You are auditing the taste of a personal AI companion called Ora, by reading one thing it said to its user.

You are scoring the experience of being spoken to like this, not the presence of words. Ask what the user heard and how it landed. Do not pattern-match on strings.

Score each of these ten criteria as "pass", "fail", or "na". Use "na" only when the turn gives no occasion to test the criterion at all — never to avoid a hard call.

T1  No machine identifier spoken: no file path, file extension, application or process name, URL, port, or window title — unless the user said it first in this exchange.
T2  Projects and activities are named the way the user names them. No stored thread label read out verbatim.
T3  Every number spoken is one the user chose or decided. No timestamps, durations, "N days ago", row counts, capture counts.
T4  No hedge ("It seems", "Looks like", "appears to") opening a claim the turn actually has evidence for.
T5  No tool, memory, capture, retrieval, context window, truncation or API mentioned — including when something failed.
T6  It answers the question asked. "What does that mean" gets the thing explained, not an account of where Ora ran into it.
T7  Every asserted fact is the kind of thing this turn could have got from memory. Nothing is carried over from a frozen opening block once a lookup has happened.
T8  When several unrelated activities are in play, it states a relation between them or names only one. Never a comma-chain of three or more unrelated activities.
T9  An idle or empty stretch is described the way a person would, naming when activity stopped. Never a count of omitted or skipped items.
T10 Second person, and contains none of: "focused on", "juggling", "open loops", "active project", "acknowledged", "noted", "I've registered".

Then consider these known failure shapes. Include one only if THIS turn is genuinely an instance of that situation; otherwise leave it out entirely.
D1  The user asked about earlier work: does the reply attribute the work to the user rather than claiming it, name a specific concrete finding, and stay to one sentence plus an offer?
D2  The user asked for brevity: two sentences or fewer, at most two threads named, no dates, apps or file names read aloud.
D6  The user asked how Ora knows something: does it name where it actually came from, and assert nothing it could not have seen?
D7  The user corrected a fact: does the reply accept the correction, state the corrected fact, and never repeat the wrong claim?
D8  The window really was idle: does it say so plainly, name when activity stopped, and offer the last real thing?
D9  Something failed: does any error text, schema complaint or API message reach the spoken words?
D10 A search came back empty: does it tell "nothing in that window" apart from "found nothing", and offer the nearest real answer?
D13 The user spoke a language other than English, or made a statement before their question: is the reply in their language, and does it take up the statement rather than reading a timeline?
D14 The user asked two things: are both halves addressed, with a plain "no" where one half has no answer?
D15 A tool was broken or empty: does the turn still give a real answer or plainly say it cannot, with no API error spoken?

Reply with JSON only, in this exact shape:
{"taste":{"T1":{"verdict":"pass","why":"..."}, ... all ten ...},
 "dcases":[{"case":"D2","verdict":"fail","why":"..."}]}
Every "why" is one clause, under 20 words. "dcases" may be empty.`

// runTrack2 scores each turn pair with one judge call. Input: the pairs to score. Output: one result per pair, in the same order.
func runTrack2(ctx context.Context, j *judge, pairs []turnPair) []track2Result {
	results := make([]track2Result, 0, len(pairs))
	for i, p := range pairs {
		user := p.UserText
		if user == "" {
			user = "(nothing — Ora opened the session on its own)"
		}
		material := fmt.Sprintf("The user (%s) said:\n%s\n\nOra said:\n%s",
			modeOrOpening(p.UserMode), user, p.OraText)

		var r track2Result
		r.turnPair = p
		if err := j.ask(ctx, track2Instruction, material, &r); err != nil {
			r.Err = err.Error()
			fmt.Printf("  [turn %2d] judge failed: %v\n", i+1, err)
			results = append(results, r)
			continue
		}
		passes, applicable := rate(collect(r.Taste, tasteIDs))
		fmt.Printf("  [turn %2d] %s taste %s  %.60s\n", i+1, p.At.Format("15:04:05"), pct(passes, applicable), strings.ReplaceAll(p.OraText, "\n", " "))
		results = append(results, r)
	}
	return results
}

// modeOrOpening names how the user's turn arrived, for the judge's benefit — a typed turn and a spoken one carry different expectations about length.
func modeOrOpening(mode string) string {
	if mode == "" {
		return "no preceding turn"
	}
	return mode
}

// collect pulls the verdicts for the given ids out of a scored map, in id order, skipping ids the judge did not return.
func collect(m map[string]verdict, ids []string) []verdict {
	var out []verdict
	for _, id := range ids {
		if v, ok := m[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

// failedTaste lists the taste criteria this turn failed, for the worst-turns table.
func (r track2Result) failedTaste() []string {
	var out []string
	for _, id := range tasteIDs {
		if v, ok := r.Taste[id]; ok && !v.passed() && !v.na() {
			out = append(out, id)
		}
	}
	for _, d := range r.DCases {
		if strings.EqualFold(d.Verdict, "fail") {
			out = append(out, d.Case)
		}
	}
	return out
}
