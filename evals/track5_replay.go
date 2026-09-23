package main

// Track 5 is the counterfactual replay: knowledge distillation with Claude as the teacher and the live voice model as the student. Each real session in ora.log is replayed turn by turn — Claude gets the same system prompt shape the live model got at handshake, the conversation exactly as it happened (the LIVE replies stay in the history, so the counterfactual is per-turn), the current user turn, and the tool results the live session received for that turn presented as evidence. Claude says which tools it would have called and what it would have spoken; a judge then picks the better reply. The output is one side-by-side markdown file per session under evals/replays/, which is both a quality baseline (is the live model's weakness capacity?) and a distillation corpus.
//
// Fidelity gaps vs the live session, all because ora.log does not record them: the handshake context block (implicit context, [working] lines, focus hits) and the personal context block are not logged, so the system prompt carries a placeholder saying so; voice turns reach Claude as their transcription text; tool results were truncated at 10KB when logged; and Claude answers in one text turn where the live model streamed audio with barge-ins.

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ora/internal/agent"
	"ora/internal/brain"
)

// replayEvent is one line of a session's transcript: a user turn, a spoken reply, or a tool call (whose Result is filled in when the matching "tool result" line arrives).
type replayEvent struct {
	At     time.Time
	Kind   string // "user" | "ora" | "tool"
	Text   string // user or ora text
	Mode   string // "voice" | "typed", user events only
	Tool   string // tool events only
	Args   string // tool call arguments as JSON text
	Result string // tool result, "" until it arrived
}

// replaySession is one live connection's worth of transcript, from a "connected to Live API" line to the next.
type replaySession struct {
	Start  time.Time
	Events []replayEvent
}

// replayLogLine is the slog JSON shape of the lines track 5 reads.
type replayLogLine struct {
	Time   time.Time       `json:"time"`
	Msg    string          `json:"msg"`
	Text   string          `json:"text"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Result string          `json:"result"`
}

// parseReplaySessions reads ora.log and reconstructs sessions: one per "connected to Live API" marker, each an ordered transcript of user turns, ora replies, and tool calls with their results. Sessions with no user turn (a reconnect that never spoke) are dropped, as are events before the first marker. Input: a scanner over the log. Output: sessions in chronological order.
func parseReplaySessions(sc *bufio.Scanner) []replaySession {
	var sessions []replaySession
	var cur *replaySession
	flush := func() {
		if cur == nil {
			return
		}
		for _, e := range cur.Events {
			if e.Kind == "user" {
				sessions = append(sessions, *cur)
				return
			}
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var l replayLogLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue
		}
		switch l.Msg {
		case "connected to Live API":
			flush()
			cur = &replaySession{Start: l.Time}
		case "user said (voice)", "sending text to model":
			if cur == nil {
				continue
			}
			mode := "voice"
			if l.Msg == "sending text to model" {
				mode = "typed"
			}
			cur.Events = append(cur.Events, replayEvent{At: l.Time, Kind: "user", Text: l.Text, Mode: mode})
		case "ora said":
			if cur == nil {
				continue
			}
			cur.Events = append(cur.Events, replayEvent{At: l.Time, Kind: "ora", Text: l.Text})
		case "tool call received":
			if cur == nil {
				continue
			}
			cur.Events = append(cur.Events, replayEvent{At: l.Time, Kind: "tool", Tool: l.Tool, Args: string(l.Args)})
		case "tool result":
			if cur == nil {
				continue
			}
			// The result line carries only the tool name, so it is attached to the most recent still-unanswered call of that tool.
			for i := len(cur.Events) - 1; i >= 0; i-- {
				e := &cur.Events[i]
				if e.Kind == "tool" && e.Tool == l.Tool && e.Result == "" {
					e.Result = l.Result
					break
				}
			}
		}
	}
	flush()
	return sessions
}

// userTurnIndexes returns the positions of the user events in a session, in order. Each one is a replayable turn.
func userTurnIndexes(s replaySession) []int {
	var idx []int
	for i, e := range s.Events {
		if e.Kind == "user" {
			idx = append(idx, i)
		}
	}
	return idx
}

// historyToolBudget caps a tool result quoted in the conversation-so-far block. The current turn's results go in full; history only needs enough to show what the live model was working from.
const historyToolBudget = 1000

// composeReplayPrompt builds the one prompt for replaying the turn at event index userIdx: the real system prompt shape, the conversation exactly as it happened up to this turn (live replies included — the counterfactual is per-turn), the current user turn, and the tool results the live session received for this turn labeled as evidence. Input: the rendered system prompt, the session, and the index of a user event in it. Output: the prompt text for the teacher model.
func composeReplayPrompt(sysPrompt string, s replaySession, userIdx int) string {
	var b strings.Builder
	b.WriteString(sysPrompt)
	b.WriteString("\n\n---\nREPLAY. This is a replay of a real conversation you (Ora) had with the user. The conversation so far, exactly as it happened:\n\n")
	any := false
	for _, e := range s.Events[:userIdx] {
		any = true
		switch e.Kind {
		case "user":
			fmt.Fprintf(&b, "USER (%s): %s\n", e.Mode, e.Text)
		case "ora":
			fmt.Fprintf(&b, "ORA: %s\n", e.Text)
		case "tool":
			fmt.Fprintf(&b, "TOOL %s %s -> %s\n", e.Tool, e.Args, truncateRunes(e.Result, historyToolBudget))
		}
	}
	if !any {
		b.WriteString("(the conversation is just starting)\n")
	}
	cur := s.Events[userIdx]
	fmt.Fprintf(&b, "\nThe user now says (%s): %s\n", cur.Mode, cur.Text)

	tools := turnTools(s, userIdx)
	if len(tools) == 0 {
		b.WriteString("\nThe live session called no tools for this turn.\n")
	} else {
		b.WriteString("\nFor this turn the live session called these tools and received these results. You are not executing tools in this replay: treat each result below as the result of a tool call you made yourself, and use it as your evidence.\n\n")
		for _, e := range tools {
			fmt.Fprintf(&b, "TOOL CALL: %s %s\nTOOL RESULT: %s\n\n", e.Tool, e.Args, e.Result)
		}
	}
	b.WriteString("\nReply in exactly this format and nothing else:\nTOOLS: <the tool calls you would make for this turn, as name(args), comma separated — or \"none\">\nSPOKEN: <the words you would speak to the user, following the voice rules above>\n")
	return b.String()
}

// turnTools returns the tool events belonging to the user turn at userIdx: everything between it and the next user event.
func turnTools(s replaySession, userIdx int) []replayEvent {
	var out []replayEvent
	for _, e := range s.Events[userIdx+1:] {
		if e.Kind == "user" {
			break
		}
		if e.Kind == "tool" {
			out = append(out, e)
		}
	}
	return out
}

// liveReply returns what the live model actually spoke for the user turn at userIdx: every "ora said" up to the next user event, joined.
func liveReply(s replaySession, userIdx int) string {
	var parts []string
	for _, e := range s.Events[userIdx+1:] {
		if e.Kind == "user" {
			break
		}
		if e.Kind == "ora" {
			parts = append(parts, e.Text)
		}
	}
	return strings.Join(parts, " ")
}

// parseReplayReply splits the teacher model's reply into its tool-choice line and its spoken words. A reply that ignores the format comes back whole as the spoken part, so a formatting slip never loses the corpus row.
func parseReplayReply(text string) (tools, spoken string) {
	if i := strings.Index(text, "SPOKEN:"); i >= 0 {
		spoken = strings.TrimSpace(text[i+len("SPOKEN:"):])
		head := text[:i]
		if k := strings.Index(head, "TOOLS:"); k >= 0 {
			tools = strings.TrimSpace(head[k+len("TOOLS:"):])
		}
		return tools, spoken
	}
	return "", strings.TrimSpace(text)
}

const track5Instruction = `You are judging one turn of a spoken conversation between a user and their personal AI companion. You are given what the user said, the evidence the assistant had (tool results, possibly none), and two candidate spoken replies: A and B.

Pick the reply a person would rather have heard. Weigh, in order: does it actually answer what the user asked; is it grounded in the evidence rather than invented or dodged; is it clean speech — no fragments, filler, self-narration, or machine identifiers read aloud.

Reply with JSON only: {"verdict":"A","why":"..."} where verdict is "A", "B", or "tie", and why is one clause under 25 words.`

// replayVerdict is the judge's pick for one turn, already mapped from A/B to the model names.
type replayVerdict struct {
	Verdict string `json:"verdict"` // "live" | "claude" | "tie"
	Why     string `json:"why"`
}

// replayTurnResult is one replayed turn: the user event's index, what the live model did, what the teacher would have done, and the judge's pick.
type replayTurnResult struct {
	UserIdx      int
	ClaudeTools  string
	ClaudeSpoken string
	V            replayVerdict
	Err          string
}

// sessionReplay is one session's full replay, held until its markdown is written.
type sessionReplay struct {
	Session replaySession
	Turns   []replayTurnResult
}

// judgeToolBudget caps the evidence quoted to the judge, which needs enough to check grounding, not the whole 10KB.
const judgeToolBudget = 3000

// judgeReplayTurn asks the judge which reply was better. A is always the live reply and B always Claude's; the mapping is fixed and stated here rather than randomized. ponytail: fixed A/B ordering has a position bias; shuffle and re-map if the verdicts ever look suspiciously one-sided toward a slot.
func judgeReplayTurn(ctx context.Context, j *judge, s replaySession, r *replayTurnResult) {
	var ev strings.Builder
	for _, e := range turnTools(s, r.UserIdx) {
		fmt.Fprintf(&ev, "TOOL %s %s -> %s\n", e.Tool, e.Args, truncateRunes(e.Result, judgeToolBudget))
	}
	evidence := ev.String()
	if evidence == "" {
		evidence = "(none)"
	}
	material := fmt.Sprintf("The user said:\n%s\n\nEvidence the assistant had:\n%s\n\nReply A:\n%s\n\nReply B:\n%s",
		s.Events[r.UserIdx].Text, evidence, liveReply(s, r.UserIdx), r.ClaudeSpoken)
	var raw struct {
		Verdict string `json:"verdict"`
		Why     string `json:"why"`
	}
	if err := j.ask(ctx, track5Instruction, material, &raw); err != nil {
		r.Err = appendErr(r.Err, "judge: "+err.Error())
		return
	}
	switch strings.ToUpper(strings.TrimSpace(raw.Verdict)) {
	case "A":
		r.V = replayVerdict{Verdict: "live", Why: raw.Why}
	case "B":
		r.V = replayVerdict{Verdict: "claude", Why: raw.Why}
	default:
		r.V = replayVerdict{Verdict: "tie", Why: raw.Why}
	}
}

// appendErr joins error strings for a turn that failed in more than one place.
func appendErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// replayCounts tallies the judge's picks for one session's headline. Input: the replayed turns. Output: claude, tie, live counts over the turns that were judged.
func replayCounts(turns []replayTurnResult) (claude, tie, live int) {
	for _, t := range turns {
		switch t.V.Verdict {
		case "claude":
			claude++
		case "tie":
			tie++
		case "live":
			live++
		}
	}
	return
}

// writeReplayFile renders one session's replay as markdown: a headline table, then each turn side by side — USER / the live model's tool calls / LIVE / CLAUDE (with its tool choice) / the judge's pick. Input: the output directory and the replay. Output: the path written.
func writeReplayFile(dir string, r sessionReplay) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.Session.Start.Format("2006-01-02T15-04-05")+".md")

	var b strings.Builder
	fmt.Fprintf(&b, "# Counterfactual replay — session %s\n\n", r.Session.Start.Format("2006-01-02 15:04:05"))
	b.WriteString("Each turn: what the user said, what the live model (gemini) did and spoke, and what Claude would have done given the same system prompt shape, the same history, and the same tool results as evidence. The judge picks the better spoken reply per turn.\n\n")
	claude, tie, live := replayCounts(r.Turns)
	fmt.Fprintf(&b, "| Turns | Claude preferred | Tie | Live preferred |\n|---|---|---|---|\n| %d | %d | %d | %d |\n\n", len(r.Turns), claude, tie, live)

	for i, t := range r.Turns {
		u := r.Session.Events[t.UserIdx]
		fmt.Fprintf(&b, "## Turn %d — %s\n\n", i+1, u.At.Format("15:04:05"))
		fmt.Fprintf(&b, "**USER (%s):** %s\n\n", u.Mode, u.Text)
		tools := turnTools(r.Session, t.UserIdx)
		if len(tools) > 0 {
			b.WriteString("Live tool calls:\n\n")
			for _, e := range tools {
				fmt.Fprintf(&b, "- `%s %s` → %s\n", e.Tool, cell(e.Args), cell(truncateRunes(e.Result, 400)))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**LIVE (gemini):** %s\n\n", cmp.Or(liveReply(r.Session, t.UserIdx), "(no spoken reply logged)"))
		fmt.Fprintf(&b, "**CLAUDE** (tools: %s)**:** %s\n\n", cmp.Or(t.ClaudeTools, "?"), cmp.Or(t.ClaudeSpoken, "(no reply)"))
		if t.V.Verdict != "" {
			fmt.Fprintf(&b, "Verdict: **%s** — %s\n\n", t.V.Verdict, t.V.Why)
		}
		if t.Err != "" {
			fmt.Fprintf(&b, "Error: %s\n\n", t.Err)
		}
	}
	return path, os.WriteFile(path, []byte(b.String()), 0644)
}

// replaySystemPrompt renders the system prompt for a session, anchored at the session's start. The handshake context block and personal context were never logged, so a placeholder names that gap to the teacher model instead of pretending an empty day.
func replaySystemPrompt(s replaySession) string {
	return agent.SystemInstruction(s.Start,
		"",
		"  (this replay could not reconstruct the opening context block the live session had — rely on the conversation and the tool results below)")
}

// runTrack5 replays the selected sessions through the teacher brain, judges each turn, and writes one markdown file per session into outDir. Input: the judge, the teacher brain, the log path, the output directory, and an optional session-start prefix (e.g. "2026-08-30") — empty means the most recent 3 sessions. Output: a one-line headline across all replayed sessions.
func runTrack5(ctx context.Context, j *judge, teach brain.Brain, logPath, outDir, sessionPrefix string) (string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sessions := parseReplaySessions(sc)
	f.Close()
	if err := sc.Err(); err != nil {
		return "", err
	}

	sessions = selectSessions(sessions, sessionPrefix)
	if len(sessions) == 0 {
		return "", fmt.Errorf("no sessions with user turns matched %q", sessionPrefix)
	}

	var totalTurns, totalClaude, totalTie, totalLive int
	for _, s := range sessions {
		fmt.Printf("  session %s — %d events\n", s.Start.Format("2006-01-02 15:04:05"), len(s.Events))
		sys := replaySystemPrompt(s)
		rep := sessionReplay{Session: s}
		for i, userIdx := range userTurnIndexes(s) {
			r := replayTurnResult{UserIdx: userIdx}
			reply, err := teach(ctx, composeReplayPrompt(sys, s, userIdx))
			if err != nil {
				r.Err = "claude: " + err.Error()
				fmt.Printf("    [turn %2d] claude failed: %v\n", i+1, err)
				rep.Turns = append(rep.Turns, r)
				continue
			}
			r.ClaudeTools, r.ClaudeSpoken = parseReplayReply(reply)
			judgeReplayTurn(ctx, j, s, &r)
			fmt.Printf("    [turn %2d] %-6s  %.60s\n", i+1, cmp.Or(r.V.Verdict, "unjudged"), strings.ReplaceAll(r.ClaudeSpoken, "\n", " "))
			rep.Turns = append(rep.Turns, r)
		}
		path, err := writeReplayFile(outDir, rep)
		if err != nil {
			return "", err
		}
		fmt.Printf("    wrote %s\n", path)
		c, t, l := replayCounts(rep.Turns)
		totalTurns += len(rep.Turns)
		totalClaude += c
		totalTie += t
		totalLive += l
	}
	return fmt.Sprintf("replay: %d sessions, %d turns — claude preferred %d, tie %d, live %d",
		len(sessions), totalTurns, totalClaude, totalTie, totalLive), nil
}

// selectSessions picks which sessions to replay: those whose start time (RFC3339 local) begins with prefix, or the most recent 3 when the prefix is empty. Input: all sessions and the prefix. Output: the selection, oldest first.
func selectSessions(sessions []replaySession, prefix string) []replaySession {
	if prefix != "" {
		var out []replaySession
		for _, s := range sessions {
			if strings.HasPrefix(s.Start.Format(time.RFC3339), prefix) || strings.HasPrefix(s.Start.Format("2006-01-02T15-04-05"), prefix) {
				out = append(out, s)
			}
		}
		return out
	}
	sort.Slice(sessions, func(i, k int) bool { return sessions[i].Start.Before(sessions[k].Start) })
	if len(sessions) > 3 {
		sessions = sessions[len(sessions)-3:]
	}
	return sessions
}
