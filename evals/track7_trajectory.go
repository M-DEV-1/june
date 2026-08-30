package main

// Track 7 is the interactive trajectory eval. Two live text conversations run in parallel against the same memory: one with Gemini on Ora's real harness, one with Claude on the identical harness. A third model roleplays the user and writes the next message separately for each arm, so the two conversations diverge exactly as far as the two assistants earn.
//
// This is the fix for track 5's blind spot. Track 5 froze the live session's tool results and handed them to the counterfactual model as evidence, which punished it whenever its own tool plan differed from the live one — there were no results for the plan it actually wanted. Here both arms run a real agentic loop with real tool execution, so a different tool choice produces different evidence and the whole trajectory is what gets compared.
//
// Everything runs against a VACUUM INTO snapshot of the live store. The read tools (query_memory, recall, get_recent) are the daemon's own, reached through agent.ExecuteTool exactly as the model's function calls reach them. Every other tool is stubbed: the call is recorded in the trajectory and a plausible success goes back to the model, so an arm that decides to save a note is scored on the decision while the store stays byte-identical.
//
// What this eval does not measure, because the harness diverges from the live daemon here: there is no voice or ASR (both arms are text), no handshake "[working]" activity block or focus hits (the eval has no tracker buffer), no Gemini native web search (it has no declaration to give the Claude arm, so neither arm gets it), and the Gemini arm runs generateContent on a text model rather than the Live API's native-audio model, which cannot do text function calling.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"ora/internal/agent"
	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
)

// trajToolRounds is how many tools one arm may run before it has to answer. Four covers the real pattern (a lookup, a narrower second lookup, a save) with room to spare, and stops a model that has decided to search forever from spending the run's whole quota on one turn.
const trajToolRounds = 4

// trajCall is one tool call an arm made during a turn, with the result the executor gave back.
type trajCall struct {
	Name   string
	Args   map[string]any
	Result string
	// Raw is the Gemini arm's own model turn for this call, kept verbatim. Gemini 3 signs each function call with a thought_signature and rejects the next request if the history hands the call back without it, so the call cannot be rebuilt from Name and Args — it has to be replayed exactly as it came. The Claude arm leaves this nil.
	Raw *genai.Content
}

// trajTurn is one exchange in one arm's conversation: what the roleplay user said, the tools the arm ran, and what it replied.
type trajTurn struct {
	User  string
	Calls []trajCall
	Reply string
	Err   string
}

// armStep is the only place the two arms differ. Given the system prompt and the conversation so far (whose last turn is the one in progress, with the tool calls already run on it), it returns either the next tool call or the spoken reply. Both arms are stateless and rebuild everything from the turns each time, so neither carries hidden state the other lacks.
type armStep func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error)

// trajStubs are the tools track 7 records but never runs. Three reasons a tool is here: it writes to the store (save_note, update_note, delete_note, fix_thread, personal_context), it changes the machine or reaches the network (shell_exec, open_url, read_file, list_files, read_clipboard), or it would need a live session to work at all (branch spawns a sub-agent against the Live API). shell_exec and a sensitive read_file also block on human approval through a channel nothing is draining in the eval, so stubbing them is what keeps the loop from hanging.
var trajStubs = map[string]string{
	"save_note":        "saved",
	"update_note":      "note updated",
	"delete_note":      "note deleted",
	"fix_thread":       "thread updated",
	"personal_context": "personal context updated",
	"shell_exec":       "error: no shell in this eval — answer without running anything",
	"open_url":         "opened in the browser",
	"read_file":        "error: no filesystem in this eval — answer without reading files",
	"list_files":       "error: no filesystem in this eval — answer without reading files",
	"read_clipboard":   "error: no clipboard in this eval",
	"branch":           "error: no sub-agent in this eval — use query_memory or recall directly",
}

// trajExec runs one tool call. The three read tools go through the daemon's own ExecuteTool against the snapshot store, so their semantics are the live ones down to the wording of an empty result; everything else returns its stub. Input: the agent holding the snapshot store, the tool name and its args. Output: the result string the arm sees.
func trajExec(ag *agent.Agent, ctx context.Context, name string, args map[string]any) string {
	switch name {
	case "query_memory", "recall", "get_recent":
		return ag.ExecuteTool(ctx, name, args)
	}
	if msg, ok := trajStubs[name]; ok {
		return msg
	}
	return "error: there is no tool called " + name
}

// runTrajTurn drives one arm through one user turn: call the model, run whatever tool it asks for, feed the result back, repeat until it answers or the tool-round cap stops it. The turn in progress is the last element of turns and is updated in place, so the arm sees its own tool results on the next step. Input: the system prompt, the arm, the tool executor, and the conversation whose last turn holds the new user message. Output: the finished turn.
func runTrajTurn(ctx context.Context, sys string, arm armStep, exec func(context.Context, string, map[string]any) string, turns []trajTurn) trajTurn {
	cur := &turns[len(turns)-1]
	for round := 0; ; round++ {
		call, spoken, err := arm(ctx, sys, turns)
		if err != nil {
			cur.Err = appendErr(cur.Err, err.Error())
			return *cur
		}
		if call == nil {
			cur.Reply = spoken
			return *cur
		}
		if round >= trajToolRounds {
			cur.Reply = spoken
			cur.Err = appendErr(cur.Err, fmt.Sprintf("hit the %d tool-round cap; dropped a further %s call and took whatever it had said", trajToolRounds, call.Name))
			return *cur
		}
		call.Result = exec(ctx, call.Name, call.Args)
		cur.Calls = append(cur.Calls, *call)
	}
}

// --- the Gemini arm ---

// geminiArm answers with one generateContent call carrying the real tool declarations. Temperature is zero so a rerun moves only when the memory or the prompt moved. Input: a client, the model name, and the declarations. Output: the arm.
func geminiArm(client *genai.Client, model string, decls []*genai.FunctionDeclaration, pace *trajPacer) armStep {
	return func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
		var temp float32
		cfg := &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Role: "system", Parts: []*genai.Part{{Text: sys}}},
			Tools:             []*genai.Tool{{FunctionDeclarations: decls}},
			Temperature:       &temp,
		}
		contents := geminiContents(turns)
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			pace.wait(ctx, attempt)
			resp, err := client.Models.GenerateContent(ctx, model, contents, cfg)
			if err != nil {
				lastErr = err
				continue
			}
			if fcs := resp.FunctionCalls(); len(fcs) > 0 {
				call := &trajCall{Name: fcs[0].Name, Args: fcs[0].Args}
				if len(resp.Candidates) > 0 {
					call.Raw = resp.Candidates[0].Content
				}
				return call, "", nil
			}
			return nil, strings.TrimSpace(resp.Text()), nil
		}
		return nil, "", lastErr
	}
}

// geminiContents renders the conversation as the SDK's Contents: a user text part per user message, a model part per function call, a user part per function response, and a model text part per reply. The in-progress turn ends with its last tool response, which is what makes the model produce the next step. Input: the conversation. Output: the contents in order.
func geminiContents(turns []trajTurn) []*genai.Content {
	var out []*genai.Content
	for _, t := range turns {
		out = append(out, genai.NewContentFromText(t.User, genai.RoleUser))
		for _, c := range t.Calls {
			if c.Raw != nil {
				out = append(out, c.Raw)
			} else {
				out = append(out, &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{Name: c.Name, Args: c.Args},
				}}})
			}
			out = append(out, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{Name: c.Name, Response: map[string]any{"output": c.Result}},
			}}})
		}
		if t.Reply != "" {
			out = append(out, genai.NewContentFromText(t.Reply, genai.RoleModel))
		}
	}
	return out
}

// textDecls strips the NON_BLOCKING behavior off the live declarations. That flag is a Live API contract about when a result is folded back into a spoken turn; generateContent has no such notion and rejects it. Nothing else about the declarations changes, so both arms see the same names, descriptions and schemas the voice session sees. Input: the live declarations. Output: copies safe for generateContent.
func textDecls(decls []*genai.FunctionDeclaration) []*genai.FunctionDeclaration {
	out := make([]*genai.FunctionDeclaration, 0, len(decls))
	for _, d := range decls {
		c := *d
		c.Behavior = ""
		out = append(out, &c)
	}
	return out
}

// --- the Claude arm ---

// claudeArm answers through the machine's claude CLI login. It has no function-calling wire, so the tools are described in the prompt and the reply is one TOOL: or SPOKEN: line. Input: the brain and the declarations to describe. Output: the arm.
func claudeArm(b brain.Brain, decls []*genai.FunctionDeclaration) armStep {
	doc := describeTools(decls)
	return func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
		out, err := b(ctx, claudeArmPrompt(sys, doc, turns))
		if err != nil {
			return nil, "", err
		}
		return parseArmReply(out)
	}
}

// describeTools renders the declarations as one line each: name, parameter names, description. This is the Claude arm's whole tool surface, and it is built from the same declarations the Gemini arm gets over the wire, so neither arm knows about a tool the other does not.
func describeTools(decls []*genai.FunctionDeclaration) string {
	var b strings.Builder
	for _, d := range decls {
		var params []string
		if d.Parameters != nil {
			for name := range d.Parameters.Properties {
				params = append(params, name)
			}
		}
		sortStrings(params)
		fmt.Fprintf(&b, "- %s(%s) — %s\n", d.Name, strings.Join(params, ", "), strings.TrimSpace(d.Description))
	}
	return b.String()
}

// sortStrings keeps the parameter list stable across runs, since a Go map iterates in a different order every time and an unstable prompt makes two runs incomparable.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for k := i; k > 0 && s[k] < s[k-1]; k-- {
			s[k], s[k-1] = s[k-1], s[k]
		}
	}
}

// claudeArmPrompt is the Claude arm's whole input: the real system prompt, the tool surface, the reply protocol, and the conversation so far including the tool results from the turn in progress.
func claudeArmPrompt(sys, toolDoc string, turns []trajTurn) string {
	var b strings.Builder
	b.WriteString(sys)
	b.WriteString("\n\n---\nThis is a text conversation, not a voice one. Everything above about how you talk still holds — brief, warm, no markdown, no lists read out mechanically — you are just typing it instead of saying it.\n\nYour tools:\n")
	b.WriteString(toolDoc)
	fmt.Fprintf(&b, "\nReply with exactly one line and nothing else, in one of these two forms:\nTOOL: <name> <arguments as one line of JSON>\nSPOKEN: <what you say to the user>\n\nA tool result comes back and you get another go. You may run at most %d tools before you have to answer.\n\nThe conversation so far:\n\n", trajToolRounds)
	for i, t := range turns {
		current := i == len(turns)-1
		fmt.Fprintf(&b, "USER: %s\n", t.User)
		for _, c := range t.Calls {
			budget := historyToolBudget
			if current {
				budget = 10000
			}
			fmt.Fprintf(&b, "TOOL %s %s -> %s\n", c.Name, argsJSON(c.Args), truncateRunes(c.Result, budget))
		}
		if t.Reply != "" {
			fmt.Fprintf(&b, "YOU: %s\n", t.Reply)
		}
	}
	b.WriteString("\nYour move:\n")
	return b.String()
}

// parseArmReply reads one text-protocol reply. A TOOL: line wins over anything else in the text, since a model that narrates before calling still meant to call. No marker at all means the whole text is the reply, so a formatting slip costs a protocol line rather than the turn. Input: the model's raw text. Output: a tool call, or the spoken reply.
func parseArmReply(text string) (*trajCall, string, error) {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "TOOL:")
		if !ok {
			continue
		}
		name, raw, _ := strings.Cut(strings.TrimSpace(rest), " ")
		name = strings.TrimSpace(strings.Trim(name, "(:,"))
		args := map[string]any{}
		if raw = strings.TrimSpace(raw); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return nil, "", fmt.Errorf("could not read the args for %s (%.120s): %w", name, raw, err)
			}
		}
		return &trajCall{Name: name, Args: args}, "", nil
	}
	if i := strings.Index(text, "SPOKEN:"); i >= 0 {
		return nil, strings.TrimSpace(text[i+len("SPOKEN:"):]), nil
	}
	return nil, strings.TrimSpace(text), nil
}

// argsJSON renders tool arguments compactly for the transcript and the report.
func argsJSON(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}

// --- the roleplay user ---

const trajRoleplayBrief = `You are roleplaying the owner of this laptop, texting the memory companion that has been watching your screen all day. You are the user, never the assistant.

Over this conversation you will: ask about your own recent past (yesterday, a project you are in the middle of, a person, a meeting), follow up on whatever the assistant actually said rather than on a script, push back once when something it says sounds wrong or vague, ask one question your memory probably cannot answer, and finish by asking it to remember one new fact about you.

Write like a person texting: short, direct, lowercase is fine, no greetings after the first message, no stage directions, no quotation marks around your message. Write only the message itself.`

// nextUserMessage asks the roleplay model for the next thing the user says, given what this arm actually replied. It runs separately per arm, which is the point: the two conversations follow the two assistants. Input: the roleplay brain, the grounding rollup, the conversation so far, and which message this is out of how many. Output: the user's next message.
func nextUserMessage(ctx context.Context, b brain.Brain, grounding string, turns []trajTurn, msgNo, total int) (string, error) {
	var p strings.Builder
	p.WriteString(trajRoleplayBrief)
	p.WriteString("\n\nWhat your memory actually holds from the last few days — this is your life, ask about the real things in it:\n\n")
	p.WriteString(grounding)
	p.WriteString("\n\nThe conversation so far:\n\n")
	if len(turns) == 0 {
		p.WriteString("(nothing yet — this is your opening message)\n")
	}
	for _, t := range turns {
		fmt.Fprintf(&p, "YOU: %s\n", t.User)
		fmt.Fprintf(&p, "ASSISTANT: %s\n", orText(t.Reply, "(said nothing)"))
	}
	fmt.Fprintf(&p, "\nThis is message %d of %d. %s\n\nWrite only your next message.", msgNo, total, roleplayNudge(msgNo, total))
	out, err := b(ctx, p.String())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(out), `"`)), nil
}

// roleplayNudge is the one instruction that changes per message, so the arc in the brief actually happens instead of ten interchangeable questions.
func roleplayNudge(msgNo, total int) string {
	switch {
	case msgNo == 1:
		return "Open by asking about something from your own recent past."
	case msgNo == total:
		return "This is your last message: tell it one new fact about yourself and ask it to remember that."
	case msgNo == total-1:
		return "Ask something your memory probably cannot answer, and see how it handles that."
	case msgNo == total/2:
		return "Push back on something it said — say it sounds wrong or too vague, and ask it to be specific."
	default:
		return "Follow up on what it just said."
	}
}

// trajGrounding fetches what the roleplay user is allowed to know: the last three days out of the snapshot store, through the same recall tool the arms use. A store with nothing in that window comes back saying so, which is honest grounding for a quiet machine rather than an invitation to invent a week.
func trajGrounding(ctx context.Context, ag *agent.Agent, now time.Time) string {
	out := trajExec(ag, ctx, "recall", map[string]any{
		"since": now.AddDate(0, 0, -3).Format("2006-01-02"),
		"until": now.Format("2006-01-02"),
	})
	return truncateRunes(strings.TrimSpace(out), 6000)
}

// --- the judge ---

const track7PairInstruction = `You are comparing two AI memory companions, A and B. Each has been watching the same user's screen and has the same memory and the same tools; each is holding its own conversation with the user, so their messages have drifted apart. For one matched point in those conversations you get what the user said to each, every tool each one ran with the result it got, and what each replied.

Judge decision quality, in this order: did it reach for the right tool at the right moment (and not skip a lookup it needed, or run one it did not); is the reply grounded in what its own tools actually returned rather than invented; when memory came back empty did it say so plainly and offer something instead of stonewalling or making something up; did it work out what the user meant from the conversation rather than guessing wildly or retreating into a safe refusal.

Reply with JSON only: {"verdict":"A","why":"..."} where verdict is "A", "B", or "tie", and why is one clause under 25 words.`

const track7OverallInstruction = `You are reading one whole text conversation between a user and their AI memory companion, with the tool calls it made along the way.

Judge coherence across the whole conversation: does it hold the thread from turn to turn, does it remember what it already said and already looked up, does it repeat itself or re-run the same search, does it get better or worse as the conversation goes on, and does the user end up with what they came for.

Reply with JSON only: {"grade":"strong","why":"..."} where grade is "strong", "mixed", or "weak", and why is under 30 words.`

// trajPairVerdict is the judge's pick for one matched turn index, already mapped from A/B to the arm names.
type trajPairVerdict struct {
	Turn    int
	Verdict string // "gemini" | "claude" | "tie"
	Why     string
	Err     string
}

// trajGrade is one arm's whole-conversation coherence verdict.
type trajGrade struct {
	Grade string
	Why   string
	Err   string
}

// trajRun is everything one track 7 run produced, held until the markdown is written so a judge failure late does not lose the trajectories.
type trajRun struct {
	Started time.Time
	Model   string
	Gemini  []trajTurn
	Claude  []trajTurn
	Pairs   []trajPairVerdict
	Grades  map[string]trajGrade
	Notes   []string
}

// judgeTrajBudget caps a tool result quoted to the judge: enough to check that a reply is grounded in it, not the whole thing.
const judgeTrajBudget = 1500

// renderTurnForJudge is one arm's side of a matched pair: what it was asked, what it ran, what it said.
func renderTurnForJudge(t trajTurn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user said: %s\n", t.User)
	if len(t.Calls) == 0 {
		b.WriteString("Tools run: none\n")
	}
	for _, c := range t.Calls {
		fmt.Fprintf(&b, "Tool %s %s -> %s\n", c.Name, argsJSON(c.Args), truncateRunes(c.Result, judgeTrajBudget))
	}
	fmt.Fprintf(&b, "Reply: %s\n", orText(t.Reply, "(said nothing)"))
	return b.String()
}

// judgeTrajPair asks the judge which arm handled its turn better. A is always Gemini and B always Claude. ponytail: fixed A/B ordering carries a position bias; shuffle and re-map if the verdicts ever look suspiciously one-sided toward a slot.
func judgeTrajPair(ctx context.Context, j *judge, turn int, g, c trajTurn) trajPairVerdict {
	material := fmt.Sprintf("Assistant A:\n%s\nAssistant B:\n%s", renderTurnForJudge(g), renderTurnForJudge(c))
	var raw struct {
		Verdict string `json:"verdict"`
		Why     string `json:"why"`
	}
	if err := j.ask(ctx, track7PairInstruction, material, &raw); err != nil {
		return trajPairVerdict{Turn: turn, Err: err.Error()}
	}
	v := trajPairVerdict{Turn: turn, Why: raw.Why}
	switch strings.ToUpper(strings.TrimSpace(raw.Verdict)) {
	case "A":
		v.Verdict = "gemini"
	case "B":
		v.Verdict = "claude"
	default:
		v.Verdict = "tie"
	}
	return v
}

// judgeTrajOverall grades one arm's whole conversation for coherence.
func judgeTrajOverall(ctx context.Context, j *judge, turns []trajTurn) trajGrade {
	var b strings.Builder
	for i, t := range turns {
		fmt.Fprintf(&b, "Turn %d\n%s\n", i+1, renderTurnForJudge(t))
	}
	var raw struct {
		Grade string `json:"grade"`
		Why   string `json:"why"`
	}
	if err := j.ask(ctx, track7OverallInstruction, b.String(), &raw); err != nil {
		return trajGrade{Err: err.Error()}
	}
	return trajGrade{Grade: strings.ToLower(strings.TrimSpace(raw.Grade)), Why: raw.Why}
}

// trajCounts tallies the matched-pair verdicts for the headline. Input: the pair verdicts. Output: gemini, tie, claude counts over the pairs that were judged.
func trajCounts(pairs []trajPairVerdict) (gemini, tie, claude int) {
	for _, p := range pairs {
		switch p.Verdict {
		case "gemini":
			gemini++
		case "tie":
			tie++
		case "claude":
			claude++
		}
	}
	return
}

// --- the report ---

// writeTrajectoryFile renders one run as markdown: the headline counts, the two whole-conversation grades, then each matched turn with both arms side by side — user message, tool calls with truncated results, reply, verdict. Input: the output directory and the run. Output: the path written.
func writeTrajectoryFile(dir string, r trajRun) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.Started.Format("2006-01-02T15-04-05")+".md")

	var b strings.Builder
	fmt.Fprintf(&b, "# Trajectory eval — %s\n\n", r.Started.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Two live text conversations against the same snapshot of memory: Gemini (%s) and Claude, both on Ora's real system prompt and real read tools, with a roleplay user writing the next message separately for each arm. Write-side tools are recorded and stubbed; the store never moves.\n\n", r.Model)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "> Note: %s\n\n", n)
	}
	gemini, tie, claude := trajCounts(r.Pairs)
	fmt.Fprintf(&b, "| Matched turns | Gemini preferred | Tie | Claude preferred |\n|---|---|---|---|\n| %d | %d | %d | %d |\n\n", len(r.Pairs), gemini, tie, claude)

	b.WriteString("| Arm | Turns | Tool calls | Coherence | Why |\n|---|---|---|---|---|\n")
	for _, arm := range []struct {
		name  string
		turns []trajTurn
	}{{"gemini", r.Gemini}, {"claude", r.Claude}} {
		g := r.Grades[arm.name]
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s |\n", arm.name, len(arm.turns), countCalls(arm.turns), orText(g.Grade, "?"), cell(orText(g.Why, g.Err)))
	}
	b.WriteString("\n")

	n := len(r.Gemini)
	if len(r.Claude) > n {
		n = len(r.Claude)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "## Turn %d\n\n", i+1)
		writeTrajArm(&b, "GEMINI", r.Gemini, i)
		writeTrajArm(&b, "CLAUDE", r.Claude, i)
		for _, p := range r.Pairs {
			if p.Turn != i {
				continue
			}
			if p.Err != "" {
				fmt.Fprintf(&b, "Judge error: %s\n\n", p.Err)
				continue
			}
			fmt.Fprintf(&b, "Verdict: **%s** — %s\n\n", p.Verdict, p.Why)
		}
	}
	return path, os.WriteFile(path, []byte(b.String()), 0644)
}

// writeTrajArm renders one arm's turn i, or nothing when that arm's conversation ended earlier.
func writeTrajArm(b *strings.Builder, label string, turns []trajTurn, i int) {
	if i >= len(turns) {
		return
	}
	t := turns[i]
	fmt.Fprintf(b, "**USER → %s:** %s\n\n", label, t.User)
	for _, c := range t.Calls {
		fmt.Fprintf(b, "- `%s %s` → %s\n", c.Name, cell(argsJSON(c.Args)), cell(truncateRunes(c.Result, 400)))
	}
	if len(t.Calls) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "**%s:** %s\n\n", label, orText(t.Reply, "(said nothing)"))
	if t.Err != "" {
		fmt.Fprintf(b, "Error: %s\n\n", t.Err)
	}
}

// countCalls is how many tools one arm ran across its whole conversation.
func countCalls(turns []trajTurn) int {
	n := 0
	for _, t := range turns {
		n += len(t.Calls)
	}
	return n
}

// --- pacing ---

// trajPacer keeps every Gemini call track 7 makes under the free tier's requests-per-minute cap, for the same reason judge.ask paces itself: firing a run's calls back to back spends the minute's quota in twenty seconds and 429s the rest of the run into blank rows.
// ponytail: a fixed interval, not a token bucket. On a paid key, delete the pacing rather than tuning it.
type trajPacer struct{ last time.Time }

// wait blocks until the next call is allowed. A retry attempt waits out the whole quota window instead, because a failure at this point is almost always a 429.
func (p *trajPacer) wait(ctx context.Context, attempt int) {
	d := judgeInterval - time.Since(p.last)
	if attempt > 0 {
		d = time.Duration(attempt) * 25 * time.Second
	}
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	p.last = time.Now()
}

// --- the run ---

// runTrack7 snapshots the live store, opens it with the daemon's embedder and vector index, and runs the two conversations turn by turn, judging each matched pair and each whole trajectory at the end. Input: the judge, the API key, the data directory, the Gemini arm's model, how many user messages to run, and the output directory. Output: a one-line headline for the scorecard.
func runTrack7(ctx context.Context, j *judge, apiKey, dataDir, model string, turns int, outDir string) (string, error) {
	snapshot, err := snapshotDB(filepath.Join(dataDir, "db"))
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(filepath.Dir(snapshot))

	store, err := db.New(snapshot)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	defer store.Close()
	embedder, index := newDaemonClients()
	store.SetEmbedder(embedder)
	store.SetVectorIndex(index)
	store.SetVectorSimilarityFloor(float32(config.LoadConfig().Embed.Floor()))

	run := trajRun{Started: time.Now(), Model: model, Grades: map[string]trajGrade{}}
	if _, err := embedder.Embed(ctx, "RETRIEVAL_QUERY", "probe"); err != nil {
		run.Notes = append(run.Notes, fmt.Sprintf("daemon /embed unreachable (%v) — both arms searched lexical-only, which is the same handicap for both but not what the live session sees", err))
		fmt.Println("  WARNING: /embed unreachable, both arms degrade to lexical-only search")
	}

	// The agent handle exists only to reach ExecuteTool against the snapshot: no mic, no speaker, no compiler, no live session.
	ag := agent.NewAgent(nil, nil, store, nil, apiKey)
	exec := func(c context.Context, name string, args map[string]any) string { return trajExec(ag, c, name, args) }

	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return "", fmt.Errorf("gemini client: %w", err)
	}
	pace := &trajPacer{}
	decls := textDecls(agent.ToolDeclarations())

	arms := map[string]armStep{
		"gemini": geminiArm(client, model, decls, pace),
		"claude": claudeArm(teacherBrain(), decls),
	}
	roleplay := pacedBrain(brain.GeminiAPI(apiKey, config.TextModel), pace)

	now := time.Now()
	sys := trajSystemPrompt(ctx, store, now)
	grounding := trajGrounding(ctx, ag, now)
	run.Notes = append(run.Notes, fmt.Sprintf("roleplay grounding: %d runes of the last three days out of the snapshot, fetched with the recall tool", len([]rune(grounding))))

	opening, err := nextUserMessage(ctx, roleplay, grounding, nil, 1, turns)
	if err != nil {
		return "", fmt.Errorf("roleplay opening: %w", err)
	}
	fmt.Printf("  opening (both arms): %s\n", opening)

	convo := map[string][]trajTurn{"gemini": nil, "claude": nil}
	for i := 0; i < turns; i++ {
		for _, name := range []string{"gemini", "claude"} {
			msg := opening
			if i > 0 {
				msg, err = nextUserMessage(ctx, roleplay, grounding, convo[name], i+1, turns)
				if err != nil {
					fmt.Printf("    [%s turn %2d] roleplay failed: %v\n", name, i+1, err)
					continue
				}
			}
			convo[name] = append(convo[name], trajTurn{User: msg})
			convo[name][i] = runTrajTurn(ctx, sys, arms[name], exec, convo[name])
			t := convo[name][i]
			fmt.Printf("    [%s turn %2d] %d tools  %.70s\n", name, i+1, len(t.Calls), strings.ReplaceAll(orText(t.Reply, "(nothing)"), "\n", " "))
		}
	}
	run.Gemini, run.Claude = convo["gemini"], convo["claude"]

	for i := 0; i < len(run.Gemini) && i < len(run.Claude); i++ {
		v := judgeTrajPair(ctx, j, i, run.Gemini[i], run.Claude[i])
		fmt.Printf("    [judge turn %2d] %s — %s\n", i+1, orText(v.Verdict, "error"), orText(v.Why, v.Err))
		run.Pairs = append(run.Pairs, v)
	}
	run.Grades["gemini"] = judgeTrajOverall(ctx, j, run.Gemini)
	run.Grades["claude"] = judgeTrajOverall(ctx, j, run.Claude)

	path, err := writeTrajectoryFile(outDir, run)
	if err != nil {
		return "", err
	}
	fmt.Printf("    wrote %s\n", path)

	gemini, tie, claude := trajCounts(run.Pairs)
	return fmt.Sprintf("trajectory: %d matched turns — gemini preferred %d, tie %d, claude preferred %d; coherence gemini %s, claude %s",
		len(run.Pairs), gemini, tie, claude,
		orText(run.Grades["gemini"].Grade, "?"), orText(run.Grades["claude"].Grade, "?")), nil
}

// pacedBrain wraps a Gemini brain so the roleplay user shares the same per-minute budget the Ora arm and the judge are spending.
func pacedBrain(b brain.Brain, pace *trajPacer) brain.Brain {
	return func(ctx context.Context, prompt string) (string, error) {
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			pace.wait(ctx, attempt)
			out, err := b(ctx, prompt)
			if err == nil {
				return out, nil
			}
			lastErr = err
		}
		return "", lastErr
	}
}

// trajSystemPrompt renders the identical system prompt both arms get: the real one, anchored at now, with the real personal context. The context block names what the eval cannot reconstruct — there is no tracker buffer here, so the "[working]" lines and the focus hits the live handshake assembles are simply absent, and saying so is better than handing the model an empty day it might read as a quiet one.
func trajSystemPrompt(ctx context.Context, store *db.Store, now time.Time) string {
	personal := ""
	if entries, err := store.PersonalContext(ctx); err == nil {
		personal = agent.PersonalContextBlock(entries)
	}
	return agent.SystemInstruction(now, personal,
		"  (this eval has no live activity tracker, so the current-screen lines and focus hits a real session opens with are missing — everything you know about their day has to come from the memory tools)")
}
