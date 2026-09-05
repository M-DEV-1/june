package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ora/internal/agent"
)

func TestLoadGold_ReadsTheSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "questions.jsonl")
	os.WriteFile(path, []byte(`{"id":"G01","kind":"single","tags":["meeting"],"turns":[{"q":"who was on the call?","gold":"Karan.","must":["Karan"],"must_not":["Rakesh"]}],"source":"note#196","note":"one fact"}

{"id":"M01","kind":"multi","tags":["follow-up"],"turns":[{"q":"what did I demo?","gold":"Sonar.","must":["Sonar"]},{"q":"and who asked about it?","gold":"Ro.","must":["Ro"]}],"source":"note#178"}
`), 0644)

	items, err := loadGold(path)
	if err != nil {
		t.Fatalf("loadGold: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	if items[0].ID != "G01" || items[0].Turns[0].MustNot[0] != "Rakesh" {
		t.Errorf("first item read wrong: %+v", items[0])
	}
	if items[1].Kind != "multi" || len(items[1].Turns) != 2 {
		t.Errorf("multi item read wrong: %+v", items[1])
	}
	if got := goldTurnCount(items); got != 3 {
		t.Errorf("want 3 turns across both items, got %d", got)
	}
}

func TestGoldPass_MechanicalCheck(t *testing.T) {
	spec := goldTurnSpec{Must: []string{"Karan"}, MustNot: []string{"Rakesh", "Var "}}
	cases := []struct {
		answer string
		want   bool
	}{
		{"Karan Mehta was on it.", true},
		{"karan mehta was on it.", true},
		{"Karan and Rakesh were on it.", false},
		{"Karan and Var were on it.", false},
		{"Karan and Varoon were on it.", true},
		{"Nobody I can see.", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := goldPass(c.answer, spec); got != c.want {
			t.Errorf("goldPass(%q) = %v, want %v", c.answer, got, c.want)
		}
	}
	if !goldPass("anything at all", goldTurnSpec{}) {
		t.Error("a turn with no musts and no must_nots passes on any non-empty answer")
	}
}

func TestMedianSeconds(t *testing.T) {
	if got := medianSeconds(nil); got != 0 {
		t.Errorf("median of nothing = %v, want 0", got)
	}
	if got := medianSeconds([]float64{3, 1, 2}); got != 2 {
		t.Errorf("odd-length median = %v, want 2", got)
	}
	if got := medianSeconds([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("even-length median = %v, want 2.5", got)
	}
}

func TestGoldTally_CountsPassFailAndError(t *testing.T) {
	got := goldTally([]goldAnswer{
		{Arm: "live", Pass: true, Seconds: 2},
		{Arm: "live", Pass: false, Seconds: 4},
		{Arm: "live", Err: "rate limited", Seconds: 1},
		{Arm: "claude", Pass: true, Seconds: 9, Hops: []goldHop{{Name: "recall"}, {Name: "query_memory"}}},
	})
	if got["live"].Answered != 2 || got["live"].Pass != 1 || got["live"].Fail != 1 || got["live"].Errors != 1 {
		t.Errorf("live tally wrong: %+v", got["live"])
	}
	// Seconds 2, 4 and 1: the median is 2 only because the errored turn's second counts; drop it and the median of 2 and 4 would be 3.
	if got["live"].Median != 2 {
		t.Errorf("live median = %v, want 2 (errored turns count toward time)", got["live"].Median)
	}
	if got["claude"].Calls != 2 {
		t.Errorf("claude tool calls = %d, want 2", got["claude"].Calls)
	}
}

// The gold runner gives an arm a twelve-tool safety cap where track 7 gives four, so the cap has to be the caller's to choose.
func TestRunTrajTurn_HonoursACallerSuppliedCap(t *testing.T) {
	arm := func(ctx context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
		return &trajCall{Name: "query_memory"}, "partial answer", nil
	}
	exec := func(context.Context, string, map[string]any) string { return "a row" }
	got := runTrajTurn(context.Background(), "SYS", arm, exec, []trajTurn{{User: "hey"}}, 2)
	if len(got.Calls) != 2 {
		t.Fatalf("want 2 calls under a cap of 2, got %d", len(got.Calls))
	}
	if !strings.Contains(got.Err, "2 tool-round cap") {
		t.Errorf("the turn should say which cap stopped it, got %q", got.Err)
	}
	if !strings.Contains(claudeArmPrompt("SYS", "tools", []trajTurn{{User: "hey"}}, 12), "at most 12 tools") {
		t.Error("the prompt has to tell the model the cap it is actually under")
	}
}

// Grok runs its narration and the protocol line together with no newline between them, so a TOOL: marker has to be found anywhere in the reply, the way SPOKEN: already is. Before this it fell through to "the whole text is the answer" and the arm ran no tools at all.
func TestParseArmReply_FindsAToolCallMidLine(t *testing.T) {
	call, _, err := parseArmReply(`I'll look at the full request first.I'll pull this week's meetings.TOOL: recall {"since": "2026-08-31", "until": "2026-09-04"}`)
	if err != nil {
		t.Fatalf("parseArmReply: %v", err)
	}
	if call == nil || call.Name != "recall" {
		t.Fatalf("want a recall call, got %+v", call)
	}
	if call.Args["since"] != "2026-08-31" {
		t.Errorf("args = %v", call.Args)
	}
	if _, spoken, _ := parseArmReply("SPOKEN: nothing to look up here"); spoken != "nothing to look up here" {
		t.Errorf("a plain spoken reply still has to win: %q", spoken)
	}
}

// The Live API reports a spent quota by closing the socket with 1011 and the quota text in the close reason, not as a tidy 429, and that is the string that has to stand an arm down.
func TestGoldRateLimited_RecognisesBothWireShapes(t *testing.T) {
	yes := []string{
		"ask text: generate (iteration 0): Error 429, Message: You exceeded your current quota",
		"ask voice: receive: websocket: close 1011 (internal server error): You exceeded your current quota, please check your plan",
		"claude -p failed (error): usage limit reached",
	}
	for _, s := range yes {
		if !goldRateLimited(s) {
			t.Errorf("should be read as a rate limit: %q", s)
		}
	}
	no := []string{
		"ask voice: receive: websocket: close 1011 (internal server error)",
		"agy timed out after 3m0s",
		"could not read the args for recall",
	}
	for _, s := range no {
		if goldRateLimited(s) {
			t.Errorf("should not be read as a rate limit: %q", s)
		}
	}
}

// A run that lost two arms to a spent API quota should not cost the three arms that finished a second time. The resume path reads a previous run's raw trace and re-asks only what is missing or errored.
func TestGoldDone_KeepsGoodAnswersAndDropsErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := writeGoldTrace(path, []goldAnswer{
		{ID: "G01", Turn: 1, Arm: "claude", Answer: "Brightpath Health", Seconds: 3},
		{ID: "G01", Turn: 1, Arm: "live", Err: "429 quota"},
		{ID: "M01", Turn: 2, Arm: "claude", Answer: "Ro asked"},
	}); err != nil {
		t.Fatalf("writeGoldTrace: %v", err)
	}
	done, err := goldDone(path)
	if err != nil {
		t.Fatalf("goldDone: %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("want 2 reusable answers, got %d: %v", len(done), done)
	}
	if got := done[goldKey{"claude", "G01", 1}].Answer; got != "Brightpath Health" {
		t.Errorf("claude G01.1 = %q", got)
	}
	if _, ok := done[goldKey{"live", "G01", 1}]; ok {
		t.Error("an errored answer must be re-asked, not reused")
	}
	if _, ok := done[goldKey{"claude", "M01", 2}]; !ok {
		t.Error("the second turn of a multi-turn item must be keyed separately")
	}
}

// The gold answer is the answer a person wrote down as right, so it has to pass the check written beside it. Five turns did not: G02 and G16 name the wrong name in order to correct it while listing that name as a must_not, G08 quotes the store's "PSP" typo, G17's gold opens with "not in paper notebooks any more" against a must_not of "paper", and M04's third gold says the rebase item is closed against a must_not of "rebase". Each of those marked the best possible answer a fail, so the pass column punished the arms that answered well.
func TestGoldFixtures_TheGoldAnswerPassesItsOwnCheck(t *testing.T) {
	for _, path := range []string{"gold/questions.jsonl", "gold/sample.jsonl"} {
		items, err := loadGold(path)
		if err != nil {
			t.Fatalf("loadGold(%s): %v", path, err)
		}
		if len(items) == 0 {
			t.Fatalf("%s holds no questions", path)
		}
		for _, it := range items {
			for i, spec := range it.Turns {
				if !goldPass(spec.Gold, spec) {
					t.Errorf("%s %s.%d: the gold answer fails its own check\n  gold: %s\n  must: %q\n  must_not: %q", path, it.ID, i+1, spec.Gold, spec.Must, spec.MustNot)
				}
			}
		}
	}
}

// A turn with neither a must nor a must_not passes on any answer that is not empty, so it is a free point for every arm. The scored set must not hold one; the sheet says so out loud for any set that does.
func TestGoldUnscored_NamesTheTurnsNothingCanScore(t *testing.T) {
	items, err := loadGold("gold/questions.jsonl")
	if err != nil {
		t.Fatalf("loadGold: %v", err)
	}
	if un := goldUnscored(items); len(un) > 0 {
		t.Errorf("the scored gold set has %d turns that cannot be scored: %v", len(un), un)
	}
	got := goldUnscored([]goldItem{
		{ID: "A", Turns: []goldTurnSpec{{Must: []string{"x"}}, {}}},
		{ID: "B", Turns: []goldTurnSpec{{MustNot: []string{"y"}}}},
	})
	if len(got) != 1 || got[0] != "A.2" {
		t.Errorf("goldUnscored = %v, want [A.2]", got)
	}
	if !goldPass("anything at all", goldTurnSpec{}) {
		t.Error("a turn with no musts and no must_nots still passes on any non-empty answer, which is what the note warns about")
	}
}

// A model answering in prose writes "AI value chain", not "AI_value_chain". Checked as plain substrings, the three checks written as identifiers marked those answers wrong for saying exactly the right thing — and all three items already carried a note saying that form was to be accepted.
func TestGoldPass_MatchesAnAnswerSpelledOutInProse(t *testing.T) {
	spoken := []struct {
		answer string
		must   string
	}{
		{"You said you'd turn on AI value chain in prod.", "AI_value_chain"},
		{"The ND GAIN formula, from Notre Dame.", "ND-GAIN"},
		{"You were in country ranking change dot xlsx.", "country_ranking"},
		{"Everything's under local share ora in your home directory.", ".local/share/ora"},
	}
	for _, c := range spoken {
		if !goldPass(c.answer, goldTurnSpec{Must: []string{c.must}}) {
			t.Errorf("must %q should match the spoken answer %q", c.must, c.answer)
		}
	}
	// Folding words together must not turn a must_not that is pure punctuation into an empty substring, which would match everything and fail every answer.
	if goldPass("I don't know that.", goldTurnSpec{MustNot: []string{"$"}}) == false {
		t.Error("a must_not of \"$\" must not fire on an answer that holds no dollar sign")
	}
	if goldPass("About $400.", goldTurnSpec{MustNot: []string{"$"}}) {
		t.Error("a must_not of \"$\" still has to catch a dollar amount")
	}
}

// "Var " is written with a trailing space to mean the name and not the start of Varoon. Padding the answer before folding is what makes that hold at the end of a sentence, where there is no space after the name.
func TestGoldPass_ATrailingSpaceStillMeansTheWholeWord(t *testing.T) {
	spec := goldTurnSpec{MustNot: []string{"Var "}}
	cases := []struct {
		answer string
		want   bool
	}{
		{"Karan and Varoon were on it.", true},
		{"Nobody else was around.", true},
		{"On the call: Priya and Var.", false},
		{"On the call: Priya and Var", false},
		{"Var was there.", false},
	}
	for _, c := range cases {
		if got := goldPass(c.answer, spec); got != c.want {
			t.Errorf("goldPass(%q) = %v, want %v", c.answer, got, c.want)
		}
	}
}

// A resumed run looks an answer up by arm, item id and turn number. None of those say what was asked, so editing a question in place — the ordinary way a gold set is sharpened between runs — silently scored the old answer against the new question. The question text has to match or the turn is asked again.
func TestRunGoldItem_ReAsksAQuestionThatWasEditedSinceTheTrace(t *testing.T) {
	item := goldItem{ID: "G01", Turns: []goldTurnSpec{{Q: "who was on the 12:35 call?", Must: []string{"Priya"}}}}
	asked := 0
	fn := func(ctx context.Context, prior []trajTurn, q string) (string, []trajCall, error) {
		asked++
		return "Priya was, and nobody else.", nil, nil
	}

	stale := map[goldKey]goldAnswer{{Arm: "claude", ID: "G01", Turn: 1}: {
		ID: "G01", Turn: 1, Arm: "claude", Question: "who was on the call?", Answer: "Priya.",
	}}
	got := runGoldItem(context.Background(), "claude", fn, item, &goldHalt{}, stale)
	if asked != 1 {
		t.Fatalf("an answer to a different question was reused: the arm was asked %d times, want 1", asked)
	}
	if len(got) != 1 || got[0].Question != item.Turns[0].Q {
		t.Fatalf("the answer has to carry the question that was actually asked, got %+v", got)
	}

	fresh := map[goldKey]goldAnswer{{Arm: "claude", ID: "G01", Turn: 1}: {
		ID: "G01", Turn: 1, Arm: "claude", Question: item.Turns[0].Q, Answer: "Priya, nobody else.",
	}}
	asked = 0
	got = runGoldItem(context.Background(), "claude", fn, item, &goldHalt{}, fresh)
	if asked != 0 {
		t.Errorf("an answer to the same question should be reused, not asked again")
	}
	if len(got) != 1 || !got[0].Pass {
		t.Errorf("the reused answer's check is recomputed against the file as it stands: %+v", got)
	}
}

// Two runs of a day is the normal case when a fix is being measured. Named by the day alone, the second run's trace and sheet overwrote the first's — including the trace a -gold-resume run had just read its answers out of. Track 8's frozen file already carries the minute and the commit for this reason.
func TestGoldStem_NamesEachRunAndNotEachDay(t *testing.T) {
	morning := time.Date(2026, 9, 4, 9, 15, 0, 0, time.UTC)
	evening := time.Date(2026, 9, 4, 21, 40, 0, 0, time.UTC)
	if goldStem(morning, "abc1234") == goldStem(evening, "abc1234") {
		t.Fatalf("two runs on one day share the file name %q", goldStem(morning, "abc1234"))
	}
	if goldStem(evening, "abc1234") == goldStem(evening, "def5678") {
		t.Error("two commits measured at the same minute share a file name")
	}
	if got := goldStem(morning, "abc1234"); got != "gold-2026-09-04-0915-abc1234" {
		t.Errorf("stem = %q", got)
	}
}

// No arm may write to the store or touch the desktop during a scored comparison. The Gemini arms used to answer through the agent's own ask path with eval writes turned on, which really ran save_note and revise against the snapshot and really drove the screen with click and type_text, while the command-line arms got stubs for all of it. Every arm now shares one executor, and this pins what that executor does with each of those tools.
func TestGoldExec_NoArmWritesOrTouchesTheDesktop(t *testing.T) {
	exec := goldExec(agent.NewAgent(nil, nil, nil, nil, ""))
	for _, name := range []string{"save_note", "revise", "personal_context", "click", "type_text", "scroll_to", "observe_screen", "point_at", "show_marks", "shell_exec", "read_file", "open_url"} {
		got := exec(context.Background(), name, map[string]any{})
		if got != trajStubs[name] {
			t.Errorf("%s must come back as its stub %q, got %q", name, trajStubs[name], got)
		}
	}
	// A result longer than the shared budget is cut in the executor, so the cap is one number applied once rather than each wire's prompt builder cutting to its own.
	if n := len([]rune(truncateRunes(strings.Repeat("x", goldResultBudget+500), goldResultBudget))); n <= goldResultBudget {
		t.Errorf("truncateRunes should mark a cut result, got %d runes", n)
	}
}

// Both arms have to be handed the same executor and the same history, or their scores are not comparable: an arm that can really act, or that reads a shorter record of its own lookups, is answering a different question. This walks every arm buildGoldArms makes with a fake model wire, so an arm added later that skips the shared harness fails here.
func TestBuildGoldArms_EveryArmRunsOnTheSameHarness(t *testing.T) {
	var mu sync.Mutex
	var execCalls []string
	exec := goldExecFn(func(_ context.Context, name string, _ map[string]any) string {
		mu.Lock()
		defer mu.Unlock()
		execCalls = append(execCalls, name)
		return "the one result " + name
	})

	names := []string{"gemini-text", "claude", "grok", "agy"}
	sawResult := map[string]string{}
	sawPastCalls := map[string]int{}
	sawSys := map[string]string{}
	steps := map[string]armStep{}
	for _, name := range names {
		steps[name] = func(_ context.Context, sys string, turns []trajTurn) (*trajCall, string, error) {
			cur := turns[len(turns)-1]
			if len(cur.Calls) == 0 {
				return &trajCall{Name: "save_note", Args: map[string]any{"text": "x"}}, "", nil
			}
			mu.Lock()
			defer mu.Unlock()
			sawSys[name] = sys
			sawResult[name] = cur.Calls[0].Result
			for _, t := range turns[:len(turns)-1] {
				sawPastCalls[name] += len(t.Calls)
			}
			return nil, "answered", nil
		}
	}

	arms := buildGoldArms("SYS", steps, exec)
	if len(arms) != len(names) {
		t.Fatalf("want an arm per wire, got %d of %d", len(arms), len(names))
	}
	prior := []trajTurn{{User: "earlier", Reply: "earlier answer", Calls: []trajCall{{Name: "recall", Result: "an earlier lookup"}}}}
	for _, name := range names {
		fn := arms[name]
		if fn == nil {
			t.Fatalf("no arm built for %s", name)
		}
		reply, calls, err := fn(context.Background(), prior, "and now?")
		if err != nil || reply != "answered" || len(calls) != 1 {
			t.Fatalf("%s: reply %q, %d calls, err %v", name, reply, len(calls), err)
		}
	}
	if len(execCalls) != len(names) {
		t.Errorf("every arm's tool call must go through the one executor, got %d for %d arms", len(execCalls), len(names))
	}
	for _, name := range names {
		if sawResult[name] != "the one result save_note" {
			t.Errorf("%s read %q, want the shared executor's result", name, sawResult[name])
		}
		if sawSys[name] != "SYS" {
			t.Errorf("%s got system instruction %q, want the shared one", name, sawSys[name])
		}
		if sawPastCalls[name] != 0 {
			t.Errorf("%s carried %d tool calls out of a past turn; no arm may, because the Gemini wire cannot", name, sawPastCalls[name])
		}
	}
}

// The two wires render one conversation into two different prompts, and the history budget has to survive both: track 7's text prompt cuts a tool result to historyToolBudget once its turn is in the past, while a Gemini function response is never cut. Track 9 keeps them equal by dropping every past turn's calls and by capping the result once in the executor, so the same call reads the same way on both wires.
func TestGoldArm_BothWiresCarryTheSameHistory(t *testing.T) {
	long := strings.Repeat("z", goldResultBudget+2000)
	capped := truncateRunes(long, goldResultBudget)
	prior := []trajTurn{{User: "first", Reply: "first answer", Calls: []trajCall{{Name: "recall", Result: long}}}}
	turns := append(goldPrior(prior), trajTurn{User: "second", Calls: []trajCall{{Name: "query_memory", Args: map[string]any{"q": "x"}, Result: capped}}})

	prompt := claudeArmPrompt("SYS", "tools", turns, goldToolCap)
	if !strings.Contains(prompt, capped) {
		t.Error("the text wire must carry the executor's result whole; the shared budget has to stay under the budget that prompt applies itself")
	}
	if strings.Contains(prompt, "TOOL recall") {
		t.Error("the text wire must not carry a past turn's tool call, because the Gemini wire cannot be given one")
	}

	var responses, pastCalls int
	for _, c := range geminiContents(turns) {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				responses++
				if got, _ := p.FunctionResponse.Response["output"].(string); got != capped {
					t.Errorf("the Gemini wire carried %d runes of the result, the text wire carried %d", len([]rune(got)), len([]rune(capped)))
				}
			}
			if p.FunctionCall != nil && p.FunctionCall.Name == "recall" {
				pastCalls++
			}
		}
	}
	if responses != 1 {
		t.Errorf("want the one in-progress tool result on the Gemini wire, got %d", responses)
	}
	if pastCalls != 0 {
		t.Errorf("the Gemini wire carried %d past tool calls", pastCalls)
	}
}

// Track 9 must not reach for the agent's own ask path again. That path executes every tool for real — it wrote to the snapshot and drove the desktop through click and type_text — and there is no way to substitute its executor from outside the agent package, so an arm built on it cannot be put on the shared harness.
func TestTrack9_NoArmAnswersThroughTheWritingHarness(t *testing.T) {
	src, err := os.ReadFile("track9_gold.go")
	if err != nil {
		t.Fatalf("read track9_gold.go: %v", err)
	}
	for _, banned := range []string{"AllowEvalWrites", "AskWith"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("track 9 uses %s again, which lets one arm act on the real machine while the others are stubbed", banned)
		}
	}
	if _, ok := goldRetiredArms["live"]; !ok {
		t.Error("the live arm ran on that path and has to stay named as retired, with the reason, so asking for it does not just say there is no such arm")
	}
	if steps := goldArmSteps(nil, &trajPacer{}, nil); steps["live"] != nil {
		t.Error("no arm may be built on the Live API session until its executor can be substituted")
	}
}
