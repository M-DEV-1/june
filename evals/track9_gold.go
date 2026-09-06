package main

// Track 9 asks one gold set of questions of several models through Ora's own memory tools and writes one comparison sheet.
//
// Every arm runs on one harness: track 7's runTrajTurn loop, the system instruction agent.HandshakePrompt builds (the one a live voice turn opens with), the same tool declarations, the same tool executor, the same history and the same tool-round cap. What differs is only the model and the wire it speaks over — the Gemini arm uses real function calling, the three command-line arms describe the tools in the prompt and parse a TOOL:/SPOKEN: line back out.
//
// The arms are made equal by taking the acting away rather than by handing it to everyone: the read tools (query_memory, query_store, recall, action_items) really run against a VACUUM INTO snapshot of the store, and every write tool and every screen tool returns track 7's stub for every arm, because a scored comparison in which one arm can save notes and click and type on the user's real desktop while the other only pretends to is not comparing like with like and cannot be left running unattended.
//
// The pass/fail column is mechanical: every "must" substring present, no "must_not" substring present, case-insensitively. It is a first pass to sort the sheet, not a judgement — the sheet carries a tick column per arm for a person to disagree in.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/genai"

	"ora/internal/agent"
	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
)

// The track 9 flags live here rather than in main.go so adding the track cost main.go six lines. flag.Parse in main picks them up either way.
var (
	goldPath        = flag.String("gold", "evals/gold/questions.jsonl", "track 9: the gold question set")
	goldArmNames    = flag.String("arms", "gemini-text,claude,grok,agy", "track 9: which arms to run, comma separated")
	goldGeminiModel = flag.String("gemini-model", config.TextModel, "track 9: the model the gemini-text arm runs on")
	goldGrokModel   = flag.String("grok-model", "", "track 9: the model the grok arm runs on, empty means the CLI's own default")
	goldAgyModel    = flag.String("agy-model", "", "track 9: the model the agy arm runs on, empty means the CLI's own default")
	goldHTMLPath    = flag.String("gold-html", "", "track 9: also write the sheet as self-contained HTML here")
	goldResume      = flag.String("gold-resume", "", "track 9: a previous run's raw trace; every answer in it that did not error is reused instead of asked again")
)

// goldToolCap is the safety cap on tool calls in one turn. There is no smaller budget than this: an arm may look things up as many times as it likes, and the cap only stops a model that has decided to search forever.
const goldToolCap = 12

// goldTurnTimeout is the wall clock one arm gets for one question, tool calls included.
const goldTurnTimeout = 3 * time.Minute

// goldTurnSpec is one question in the gold set, with the answer and the substrings that mechanically mark a right or a wrong answer.
type goldTurnSpec struct {
	Q       string   `json:"q"`
	Gold    string   `json:"gold"`
	Must    []string `json:"must"`
	MustNot []string `json:"must_not"`
}

// goldItem is one line of the gold file: a single question, or one conversation whose turns are asked in order with the answers so far as history.
type goldItem struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`
	Tags  []string       `json:"tags"`
	Turns []goldTurnSpec `json:"turns"`
	// Grade says what a pass on this row actually measures. See goldTruth and goldRetrieval; a row with no grade is read as goldRetrieval.
	Grade  string `json:"grade,omitempty"`
	Source string `json:"source"`
	Note   string `json:"note"`
}

// goldTruth and goldRetrieval are the two things a row can measure. A truth row's gold answer came from the user, so a pass says the arm was right; most of those exist because the store itself was wrong. A retrieval row's gold answer was read off the store the arms are being asked to search, so a pass only says the arm found that row — a wrong note scores as the right answer. Keeping the two apart is the difference between "the system is right 3 times out of 4" and "the system found the row 28 times out of 36".
const (
	goldTruth     = "truth"
	goldRetrieval = "retrieval"
)

// goldGrades resolves every item's grade, defaulting a row with none to goldRetrieval: store-derived is what a row is until somebody confirms it. Input: the gold items. Output: item id to grade.
func goldGrades(items []goldItem) map[string]string {
	out := make(map[string]string, len(items))
	for _, it := range items {
		if it.Grade == goldTruth {
			out[it.ID] = goldTruth
			continue
		}
		out[it.ID] = goldRetrieval
	}
	return out
}

// goldTallyGraded sums one run's answers per arm over one grade's rows alone. Input: every answer, the item id to grade map, and the grade wanted. Output: the same per-arm stats goldTally returns, over that group.
func goldTallyGraded(answers []goldAnswer, grades map[string]string, grade string) map[string]goldStat {
	var kept []goldAnswer
	for _, a := range answers {
		if grades[a.ID] == grade {
			kept = append(kept, a)
		}
	}
	return goldTally(kept)
}

// goldGradedLine renders one grade's pass rate per arm, as one sentence for the sheet and the run note. Input: the arm names in run order, every answer, the grades, and the grade to report. Output: "truth (answers the user confirmed): claude 3/4, grok 2/4", or "" when the set holds no row of that grade.
func goldGradedLine(arms []string, answers []goldAnswer, grades map[string]string, grade string) string {
	tally := goldTallyGraded(answers, grades, grade)
	var parts []string
	total := 0
	for _, arm := range arms {
		st := tally[arm]
		n := st.Pass + st.Fail + st.Errors
		total += n
		parts = append(parts, fmt.Sprintf("%s %d/%d", arm, st.Pass, n))
	}
	if total == 0 {
		return ""
	}
	what := "answers the user confirmed, so a pass means the arm was right"
	if grade == goldRetrieval {
		what = "answers read off the store the arms search, so a pass means the arm found the row, not that the row is true"
	}
	return fmt.Sprintf("%s (%s): %s", grade, what, strings.Join(parts, ", "))
}

// goldHop is one tool call an arm made, as it goes into the raw trace.
type goldHop struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Result string         `json:"result"`
}

// goldAnswer is one arm's go at one turn: what it said, what it ran, how long it took, and whether the mechanical check passed.
type goldAnswer struct {
	ID       string    `json:"id"`
	Turn     int       `json:"turn"`
	Arm      string    `json:"arm"`
	Question string    `json:"question"`
	Answer   string    `json:"answer"`
	Hops     []goldHop `json:"hops"`
	Seconds  float64   `json:"seconds"`
	Err      string    `json:"error,omitempty"`
	Pass     bool      `json:"pass"`
}

// goldStat is one arm's column of the summary table.
type goldStat struct {
	Answered int
	Pass     int
	Fail     int
	Errors   int
	Calls    int
	Median   float64
}

// goldRun is everything one run produced, held until the files are written so an arm that dies late does not lose the arms that finished.
type goldRun struct {
	Started time.Time
	SHA     string
	Arms    []string
	Items   []goldItem
	// Ans[arm][item id] is that arm's answers for that item, one per turn in order.
	Ans   map[string]map[string][]goldAnswer
	Notes []string
}

// loadGold reads the gold set. Input: the path to the jsonl file. Output: the items in file order.
func loadGold(path string) ([]goldItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []goldItem
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var it goldItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n+1, err)
		}
		items = append(items, it)
	}
	return items, nil
}

// goldTurnCount is how many questions the set holds, counting each turn of a multi-turn item as one.
func goldTurnCount(items []goldItem) int {
	n := 0
	for _, it := range items {
		n += len(it.Turns)
	}
	return n
}

// goldFold reduces text to the words in it: lower case, with every run of characters that is not a letter or a digit turned into one space, including a run at either end. "AI_value_chain" and "AI value chain" both fold to "ai value chain"; "$" folds to the empty string, because it holds no word at all.
func goldFold(s string) string {
	var b strings.Builder
	pending := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pending {
				b.WriteByte(' ')
			}
			pending = false
			b.WriteRune(r)
			continue
		}
		pending = true
	}
	if pending && b.Len() > 0 {
		b.WriteByte(' ')
	}
	return b.String()
}

// goldMatch says whether one must or must_not substring is in an answer. It tries the plain case-insensitive substring first and then the same test on both sides folded to words, so an answer that spells an identifier out in prose matches a check written as the identifier: "AI value chain" matches a must of "AI_value_chain", "ND GAIN" matches "ND-GAIN", "country ranking change" matches "country_ranking". Those three items already carried a note saying the spelled-out form was to be accepted, and the plain substring did not accept it. The answer is padded with a space at each end before folding, so a check written with a trailing space — "Var ", meaning the name and not the start of Varoon — matches the name at the very end of a sentence too. A check that folds away to nothing, like "$", keeps only its plain form.
func goldMatch(answer, needle string) bool {
	if strings.Contains(strings.ToLower(answer), strings.ToLower(needle)) {
		return true
	}
	folded := goldFold(needle)
	if folded == "" {
		return false
	}
	return strings.Contains(" "+goldFold(answer)+" ", folded)
}

// goldPass is the mechanical first pass: every must substring present and no must_not substring present, all case-insensitively and insensitive to the punctuation a spoken answer drops. An empty answer never passes.
func goldPass(answer string, spec goldTurnSpec) bool {
	if strings.TrimSpace(answer) == "" {
		return false
	}
	for _, m := range spec.Must {
		if !goldMatch(answer, m) {
			return false
		}
	}
	for _, m := range spec.MustNot {
		if goldMatch(answer, m) {
			return false
		}
	}
	return true
}

// goldUnscored names the turns a machine cannot score at all: no must and no must_not, so any answer that is not empty passes. Input: the gold set. Output: their "id.turn" labels in file order, for the note the sheet opens with.
func goldUnscored(items []goldItem) []string {
	var out []string
	for _, it := range items {
		for i, spec := range it.Turns {
			if len(spec.Must) == 0 && len(spec.MustNot) == 0 {
				out = append(out, fmt.Sprintf("%s.%d", it.ID, i+1))
			}
		}
	}
	return out
}

// medianSeconds is the middle value, or the mean of the middle two. Input: the per-question seconds in any order. Output: 0 when there were none.
func medianSeconds(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// goldTally sums one run's answers per arm. A turn that errored is not counted as answered, but its seconds still count toward the median, because a three-minute timeout is part of what that arm costs.
func goldTally(answers []goldAnswer) map[string]goldStat {
	secs := map[string][]float64{}
	out := map[string]goldStat{}
	for _, a := range answers {
		st := out[a.Arm]
		st.Calls += len(a.Hops)
		switch {
		case a.Err != "":
			st.Errors++
		case a.Pass:
			st.Answered++
			st.Pass++
		default:
			st.Answered++
			st.Fail++
		}
		out[a.Arm] = st
		secs[a.Arm] = append(secs[a.Arm], a.Seconds)
	}
	for name, st := range out {
		st.Median = medianSeconds(secs[name])
		out[name] = st
	}
	return out
}

// --- the arms ---

// goldArmFn answers one question with the conversation so far. Input: the finished turns of this item for this arm, and the new question. Output: the answer, the tool calls made on this turn, and an error.
type goldArmFn func(ctx context.Context, prior []trajTurn, q string) (string, []trajCall, error)

// goldResultBudget caps every tool result, in runes, before any arm reads it. It is applied once in the executor rather than left to each wire's prompt builder, because track 7's text prompt cuts a result to historyToolBudget once its turn is in the past while a Gemini function response is never cut, and an arm reasoning from a shorter record of its own lookups is not being scored on the same evidence as the other. It has to stay under the 10000 runes that prompt allows the turn in progress, so that cut is a no-op and the one number here is the whole budget.
const goldResultBudget = 9000

// goldExecFn runs one tool call for whichever arm asked. Input: the tool name and its arguments. Output: the result string the arm reads.
type goldExecFn func(ctx context.Context, name string, args map[string]any) string

// goldExec is the one tool executor every arm runs. The read tools go through track 7's trajExec to the daemon's own ExecuteTool against the snapshot; every write tool and every screen tool returns trajExec's stub, so no arm writes to the store or drives the desktop during a scored run. Input: the agent holding the snapshot store. Output: the executor, with every result cut to goldResultBudget so no arm reads more of one lookup than another.
func goldExec(ag *agent.Agent) goldExecFn {
	return func(ctx context.Context, name string, args map[string]any) string {
		return truncateRunes(trajExec(ag, ctx, name, args), goldResultBudget)
	}
}

// goldPrior rebuilds the finished turns as the history every arm is given: the user's message and the arm's reply, with the tool calls of those turns dropped. Gemini signs each function call with a thought signature and rejects a history that hands one back without it, so the Gemini arm cannot be given its own past calls, and dropping them for the command-line arms too is what keeps the two histories the same shape. Input: the finished turns. Output: the same turns with their calls removed.
func goldPrior(prior []trajTurn) []trajTurn {
	out := make([]trajTurn, 0, len(prior)+1)
	for _, t := range prior {
		out = append(out, trajTurn{User: t.User, Reply: t.Reply})
	}
	return out
}

// goldArm puts one model wire on the shared harness: track 7's runTrajTurn, the same system instruction, the same executor, the same history and the same tool-round cap, so the only thing left different between two arms is the model and how it names a tool call. Input: the system instruction, the arm's model step, and the shared executor. Output: the arm.
func goldArm(sys string, step armStep, exec goldExecFn) goldArmFn {
	return func(ctx context.Context, prior []trajTurn, q string) (string, []trajCall, error) {
		turns := append(goldPrior(prior), trajTurn{User: q})
		done := runTrajTurn(ctx, sys, step, exec, turns, goldToolCap)
		if done.Reply == "" && done.Err != "" {
			return "", done.Calls, errors.New(done.Err)
		}
		return done.Reply, done.Calls, nil
	}
}

// buildGoldArms puts every model wire on the shared harness. It is the only place a track 9 arm is made, so an arm cannot be added later that runs on a different executor or a different history. Input: the system instruction, the wires by arm name, and the shared executor. Output: the arms under the same names.
func buildGoldArms(sys string, steps map[string]armStep, exec goldExecFn) map[string]goldArmFn {
	out := make(map[string]goldArmFn, len(steps))
	for name, step := range steps {
		out[name] = goldArm(sys, step, exec)
	}
	return out
}

// goldArmSteps builds one model wire per arm over the same declarations: Gemini's own function calling for the gemini-text arm, and track 7's text tool protocol for the three command-line arms. A wire only turns a conversation into either a tool call or a reply; running the tool is the harness's job, which is why every arm ends up with the same one. Input: the Gemini client, the pacer the Gemini calls share, and the declarations. Output: the wires by arm name.
func goldArmSteps(client *genai.Client, pace *trajPacer, decls []*genai.FunctionDeclaration) map[string]armStep {
	return map[string]armStep{
		"gemini-text": geminiArm(client, *goldGeminiModel, decls, pace),
		"claude":      claudeArm(brain.ClaudeCLI("claude", "sonnet", 180), decls, goldToolCap),
		"grok":        claudeArm(brain.GrokCLI("grok", 180, *goldGrokModel), decls, goldToolCap),
		"agy":         claudeArm(brain.AgyCLI("agy", 180, *goldAgyModel), decls, goldToolCap),
	}
}

// goldRateLimitWait is how long an arm waits before its one retry of a rate-limited question. Retrying at once is guaranteed to hit the same spent minute.
const goldRateLimitWait = 30 * time.Second

// goldRetiredArms names the arms track 9 no longer runs and why, so asking for one gets the reason rather than "no arm called live".
var goldRetiredArms = map[string]string{
	"live": "the Live API voice arm answered through the agent's own ask path, which runs every tool for real: it wrote to the snapshot with save_note and revise and drove the desktop with click and type_text while the other arms got stubs, so its scores were never comparable with theirs. The agent offers no way to substitute that executor from outside its package, and the Live session cannot speak the text tool protocol the other arms use, so the arm is retired until the agent grows an executor seam",
}

// goldGeminiArms names the arms that share one Gemini API quota. They run in one lane, one question at a time, while each command-line arm gets its own lane and goldWorkers workers, because each of those is a separate subscription.
var goldGeminiArms = map[string]bool{"gemini-text": true}

// goldRateLimited says whether an arm's failure was the subscription saying no rather than the code being wrong. A rate-limited arm is retried once and then stood down for the rest of the run, so a spent Claude budget does not burn an hour producing identical errors.
func goldRateLimited(msg string) bool {
	s := strings.ToLower(msg)
	for _, m := range []string{"rate limit", "rate_limit", "429", "quota", "usage limit", "resource_exhausted", "too many requests", "limit reached"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// goldKey names one arm's go at one turn, which is what a resumed run looks up.
type goldKey struct {
	Arm  string
	ID   string
	Turn int
}

// goldDone reads a previous run's raw trace and returns the answers worth reusing: the ones that did not error. An errored answer — a spent quota, a timeout — is left out so the resumed run asks it again. Input: the path to a gold-<date>.jsonl. Output: the reusable answers by arm, item and turn.
func goldDone(path string) (map[goldKey]goldAnswer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[goldKey]goldAnswer{}
	for n, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var a goldAnswer
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n+1, err)
		}
		if a.Err != "" {
			continue
		}
		out[goldKey{a.Arm, a.ID, a.Turn}] = a
	}
	return out, nil
}

// --- the run ---

// goldHalt is one arm's rate-limit state, shared by that arm's workers. Once the subscription has said no twice, every remaining question for that arm is recorded as skipped rather than asked again, so a spent budget does not burn an hour producing identical errors.
type goldHalt struct {
	mu     sync.Mutex
	reason string
}

func (h *goldHalt) stopped() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

func (h *goldHalt) stop(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		h.reason = reason
	}
}

// goldWorkers is how many questions one CLI arm is asked at once. The command-line models take between twenty seconds and three minutes to answer, so asking them one at a time would put the run past three hours; three at a time keeps each arm's own subscription well short of anything that looks like abuse. Items never overlap, so no arm's conversation is interleaved with another of its own.
const goldWorkers = 3

// runGoldArm asks one arm every question in the set, one worker per item at a time. Input: the arm's name and function, the items, how many to run at once, and the arm's shared rate-limit state. Output: one answer per turn, in no particular order (the caller sorts).
func runGoldArm(ctx context.Context, name string, fn goldArmFn, items []goldItem, workers int, halt *goldHalt, done map[goldKey]goldAnswer) []goldAnswer {
	jobs := make(chan goldItem)
	var mu sync.Mutex
	var out []goldAnswer
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				answers := runGoldItem(ctx, name, fn, it, halt, done)
				mu.Lock()
				out = append(out, answers...)
				mu.Unlock()
			}
		}()
	}
	for _, it := range items {
		jobs <- it
	}
	close(jobs)
	wg.Wait()
	return out
}

// runGoldItem asks one arm the turns of one item in order, keeping the answers so far as that arm's history for the next turn. A rate-limited turn is retried once; a second rate limit stands the whole arm down. Input: the arm, the item, the arm's rate-limit state. Output: one answer per turn, in order.
func runGoldItem(ctx context.Context, name string, fn goldArmFn, it goldItem, halt *goldHalt, done map[goldKey]goldAnswer) []goldAnswer {
	var out []goldAnswer
	var prior []trajTurn
	for i, spec := range it.Turns {
		ans := goldAnswer{ID: it.ID, Turn: i + 1, Arm: name, Question: spec.Q}
		if prev, ok := done[goldKey{name, it.ID, i + 1}]; ok && prev.Question == spec.Q {
			// The mechanical check is recomputed rather than trusted, so a gold answer edited between the two runs is scored against the version in the file now. The question itself has to match, though: an id and turn number say where an answer sits in the file, not what was asked, so a question edited between the two runs is asked again rather than scored against an answer to the old one.
			prev.Pass = goldPass(prev.Answer, spec)
			out = append(out, prev)
			prior = append(prior, trajTurn{User: spec.Q, Reply: prev.Answer})
			continue
		}
		if stopped := halt.stopped(); stopped != "" {
			ans.Err = stopped
			out = append(out, ans)
			fmt.Printf("  [%s %s.%d] SKIP %s\n", name, it.ID, i+1, truncateRunes(stopped, 90))
			continue
		}
		start := time.Now()
		reply, calls, err := goldAsk(ctx, fn, prior, spec.Q)
		if err != nil && goldRateLimited(err.Error()) {
			fmt.Printf("  [%s %s.%d] rate limited, one retry in %s\n", name, it.ID, i+1, goldRateLimitWait)
			select {
			case <-time.After(goldRateLimitWait):
			case <-ctx.Done():
			}
			reply, calls, err = goldAsk(ctx, fn, prior, spec.Q)
			if err != nil && goldRateLimited(err.Error()) {
				halt.stop("arm stood down after a second rate limit: " + err.Error())
			}
		}
		ans.Seconds = time.Since(start).Seconds()
		ans.Answer = reply
		for _, c := range calls {
			ans.Hops = append(ans.Hops, goldHop{Name: c.Name, Args: c.Args, Result: truncateRunes(c.Result, 2048)})
		}
		if err != nil {
			ans.Err = err.Error()
		}
		ans.Pass = ans.Err == "" && goldPass(reply, spec)
		out = append(out, ans)
		prior = append(prior, trajTurn{User: spec.Q, Reply: reply})
		fmt.Printf("  [%s %s.%d] %s %d tools %5.1fs %.60s\n", name, it.ID, i+1, goldVerdict(ans), len(ans.Hops), ans.Seconds,
			strings.ReplaceAll(orText(orText(ans.Answer, ans.Err), "(nothing)"), "\n", " "))
	}
	return out
}

// goldAsk runs one question under the per-turn wall clock.
func goldAsk(ctx context.Context, fn goldArmFn, prior []trajTurn, q string) (string, []trajCall, error) {
	turnCtx, cancel := context.WithTimeout(ctx, goldTurnTimeout)
	defer cancel()
	return fn(turnCtx, prior, q)
}

// goldVerdict is the mechanical call on one answer, as one word.
func goldVerdict(a goldAnswer) string {
	switch {
	case a.Err != "":
		return "ERR "
	case a.Pass:
		return "pass"
	default:
		return "fail"
	}
}

// runTrack9 snapshots the store, builds every selected arm on the one harness — the same handshake instruction, tool surface, executor and history — runs them, and writes the sheet. Arms run in parallel lanes: the Gemini arms share one lane because they share one API quota, and each command-line arm gets its own because each is a different subscription. Input: the data directory, the API key, the question file, the arm list and the output directory. Output: a one-line headline for the scorecard.
func runTrack9(ctx context.Context, dataDir, apiKey, questions, arms, outDir string) (string, error) {
	items, err := loadGold(questions)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", fmt.Errorf("%s holds no questions", questions)
	}

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

	done := map[goldKey]goldAnswer{}
	if *goldResume != "" {
		var err error
		if done, err = goldDone(*goldResume); err != nil {
			return "", err
		}
	}

	run := goldRun{Started: time.Now(), SHA: gitSHA(), Items: items, Ans: map[string]map[string][]goldAnswer{}}
	if len(done) > 0 {
		run.Notes = append(run.Notes, fmt.Sprintf("%d answers were reused from %s rather than asked again; every one of them errored nowhere and its mechanical check was recomputed against the gold file as it stands now", len(done), *goldResume))
	}
	if un := goldUnscored(items); len(un) > 0 {
		run.Notes = append(run.Notes, fmt.Sprintf("%d of the %d turns carry neither a must nor a must_not (%s), so nothing about them is scored: every arm passes them on any answer that is not empty, and the pass column is that much higher than what was measured", len(un), goldTurnCount(items), strings.Join(un, ", ")))
	}
	if _, err := embedder.Embed(ctx, "RETRIEVAL_QUERY", "probe"); err != nil {
		run.Notes = append(run.Notes, fmt.Sprintf("daemon /embed unreachable (%v) — every arm searched lexical-only, the same handicap for all of them but not what a live session sees", err))
	}

	// One agent, holding the snapshot store, is all the harness needs: it is reached only through the executor, and the executor never writes.
	toolAgent := agent.NewAgent(nil, nil, store, nil, apiKey)
	exec := goldExec(toolAgent)
	decls := textDecls(agent.ToolDeclarations())
	sys, _ := toolAgent.HandshakePrompt(ctx, run.Started)

	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return "", fmt.Errorf("gemini client: %w", err)
	}
	// One pacer for the Gemini arm, which waits judgeInterval between API calls rather than a fixed gap between questions. A question is several calls, one per tool round, and the free tier counts every one of them; the first run fired them back to back, spent the minute's quota and stood the Gemini arm down at question 6.
	build := buildGoldArms(sys, goldArmSteps(client, &trajPacer{}, decls), exec)

	var lanes [][]string
	var gemini []string
	for _, name := range strings.Split(arms, ",") {
		name = strings.TrimSpace(name)
		if why, retired := goldRetiredArms[name]; retired {
			return "", fmt.Errorf("the %s arm was retired: %s", name, why)
		}
		if build[name] == nil {
			return "", fmt.Errorf("no arm called %q", name)
		}
		run.Arms = append(run.Arms, name)
		if goldGeminiArms[name] {
			gemini = append(gemini, name)
			continue
		}
		lanes = append(lanes, []string{name})
	}
	if len(gemini) > 0 {
		lanes = append([][]string{gemini}, lanes...)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var all []goldAnswer
	for _, lane := range lanes {
		wg.Add(1)
		go func(lane []string) {
			defer wg.Done()
			for _, name := range lane {
				// The Gemini arm answers in seconds and its pacer is one clock, so it runs one question at a time; each command-line arm is a separate subscription and takes minutes, so it runs three.
				workers := goldWorkers
				if goldGeminiArms[name] {
					workers = 1
				}
				answers := runGoldArm(ctx, name, build[name], items, workers, &goldHalt{}, done)
				mu.Lock()
				all = append(all, answers...)
				byItem := map[string][]goldAnswer{}
				for _, a := range answers {
					byItem[a.ID] = append(byItem[a.ID], a)
				}
				run.Ans[name] = byItem
				mu.Unlock()
			}
		}(lane)
	}
	wg.Wait()

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ID != all[j].ID {
			return all[i].ID < all[j].ID
		}
		if all[i].Turn != all[j].Turn {
			return all[i].Turn < all[j].Turn
		}
		return all[i].Arm < all[j].Arm
	})

	stem := goldStem(run.Started, run.SHA)
	tracePath := filepath.Join(outDir, stem+".jsonl")
	if err := writeGoldTrace(tracePath, all); err != nil {
		return "", err
	}
	sheetPath := filepath.Join(outDir, fmt.Sprintf("%s-%s.md", stem, strings.ReplaceAll(arms, ",", "+")))
	if err := os.WriteFile(sheetPath, []byte(goldMarkdown(run)), 0644); err != nil {
		return "", err
	}
	if *goldHTMLPath != "" {
		if err := os.WriteFile(*goldHTMLPath, []byte(goldHTML(run)), 0644); err != nil {
			return "", err
		}
	}

	grades := goldGrades(items)
	fmt.Printf("    wrote %s\n    wrote %s\n", sheetPath, tracePath)
	note := fmt.Sprintf("gold set: %d questions over %d items — mechanical pass, %s", goldTurnCount(items), len(items), goldGradedLine(run.Arms, all, grades, goldTruth))
	if retrieval := goldGradedLine(run.Arms, all, grades, goldRetrieval); retrieval != "" {
		note += "; " + retrieval
	}
	return note, nil
}

// goldStem names a run's two output files. The clock is to the minute and the commit is in the name for the same reason track 8's frozen file carries both: two runs of one day is the normal case when a fix is being measured, and a day-only name silently overwrote the earlier of the two — the "before" half of exactly that comparison, and the trace a -gold-resume run was reading. Input: the run's start time and the commit it measured. Output: the shared stem of the trace and the sheet.
func goldStem(started time.Time, sha string) string {
	return fmt.Sprintf("gold-%s-%s", started.Format("2006-01-02-1504"), sha)
}

// writeGoldTrace dumps every answer as one json line, tool results already truncated to 2 KB.
func writeGoldTrace(path string, answers []goldAnswer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, a := range answers {
		if err := enc.Encode(a); err != nil {
			return err
		}
	}
	return nil
}

// --- the sheet ---

// goldPreamble is the paragraph both renderings open with, so nobody reads the pass column as a score.
const goldPreamble = "The pass/fail column is mechanical and is a first pass only: it says every `must` substring is present and no `must_not` substring is, case-insensitively. It cannot tell a right answer said differently from a wrong one, so read the answers. The tick column beside each arm is for your own verdict."

// goldCall renders one tool call the way it belongs in a cell: name(args).
func goldCall(h goldHop) string {
	return fmt.Sprintf("%s(%s)", h.Name, argsJSON(h.Args))
}

// goldAnswerFor finds one arm's answer to one turn of one item.
func goldAnswerFor(r goldRun, arm, id string, turn int) (goldAnswer, bool) {
	for _, a := range r.Ans[arm][id] {
		if a.Turn == turn {
			return a, true
		}
	}
	return goldAnswer{}, false
}

func goldMarkdown(r goldRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Gold set — %s (%s)\n\n", r.Started.Format("2006-01-02 15:04"), r.SHA)
	fmt.Fprintf(&b, "%s\n\n", goldPreamble)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "> Note: %s\n\n", n)
	}

	var all []goldAnswer
	for _, arm := range r.Arms {
		for _, as := range r.Ans[arm] {
			all = append(all, as...)
		}
	}
	tally := goldTally(all)
	// The truth rows are the headline: they are the only ones whose gold answer did not come out of the store being searched.
	grades := goldGrades(r.Items)
	for _, grade := range []string{goldTruth, goldRetrieval} {
		if line := goldGradedLine(r.Arms, all, grades, grade); line != "" {
			fmt.Fprintf(&b, "**%s**\n\n", line)
		}
	}
	b.WriteString("| Arm | Answered | Pass | Fail | Error/timeout | Tool calls | Median s |\n|---|---|---|---|---|---|---|\n")
	for _, arm := range r.Arms {
		st := tally[arm]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %.1f |\n", arm, st.Answered, st.Pass, st.Fail, st.Errors, st.Calls, st.Median)
	}
	b.WriteString("\n")

	header := "| id | question | gold |"
	rule := "|---|---|---|"
	for _, arm := range r.Arms {
		header += fmt.Sprintf(" %s | ✓ |", arm)
		rule += "---|---|"
	}
	b.WriteString(header + "\n" + rule + "\n")
	for _, it := range r.Items {
		for i, spec := range it.Turns {
			id := it.ID
			if len(it.Turns) > 1 {
				id = fmt.Sprintf("%s.%d", it.ID, i+1)
			}
			row := fmt.Sprintf("| %s<br>%s<br>%s | %s | %s |", id, it.Kind, strings.Join(it.Tags, ", "), cell(spec.Q), cell(spec.Gold))
			for _, arm := range r.Arms {
				a, _ := goldAnswerFor(r, arm, it.ID, i+1)
				var calls []string
				for _, h := range a.Hops {
					calls = append(calls, "`"+goldCall(h)+"`")
				}
				body := cell(orText(a.Answer, "(nothing)"))
				if a.Err != "" {
					body += "<br>**error:** " + cell(truncateRunes(a.Err, 300))
				}
				if len(calls) > 0 {
					body += "<br>" + cell(strings.Join(calls, "<br>"))
				}
				row += fmt.Sprintf(" %s<br>%.1fs — **%s** |  |", body, a.Seconds, strings.TrimSpace(goldVerdict(a)))
			}
			b.WriteString(row + "\n")
		}
	}
	return b.String()
}

func goldHTML(r goldRun) string {
	var b strings.Builder
	var all []goldAnswer
	for _, arm := range r.Arms {
		for _, as := range r.Ans[arm] {
			all = append(all, as...)
		}
	}
	tally := goldTally(all)

	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Ora gold set</title>
<link rel="preconnect" href="https://fonts.googleapis.com"><link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Source+Serif+4:opsz,wght@8..60,400;8..60,600&family=JetBrains+Mono:wght@400&display=swap" rel="stylesheet">
<style>
:root{--ink:#1b1b1b;--dim:#6b6b6b;--rule:#e0ddd6;--bg:#fbfaf7;--pass:#1f7a3f;--fail:#b0281f;--err:#7a6a20}
body{margin:0;padding:32px;background:var(--bg);color:var(--ink);font:16px/1.5 "Source Serif 4",Georgia,serif}
h1{font-size:24px;margin:0 0 4px}
p.lede{max-width:60em;color:var(--dim);margin:0 0 24px}
table{border-collapse:collapse;width:100%;margin-bottom:32px}
th,td{border:1px solid var(--rule);padding:8px 10px;vertical-align:top;text-align:left;font-size:14px}
th{background:#f2efe8;font-weight:600;position:sticky;top:0}
.wrap{overflow-x:auto}
td.q{min-width:220px}td.gold{min-width:240px;color:var(--dim)}td.arm{min-width:300px}
td.id{white-space:nowrap;font-size:13px;color:var(--dim)}
td.tick{width:34px;text-align:center}
.multi td{background:#f7f4ec}
.calls{font:12px/1.45 "JetBrains Mono",monospace;color:var(--dim);margin-top:6px;white-space:pre-wrap;word-break:break-word}
.meta{font-size:12px;color:var(--dim);margin-top:6px}
.err{color:var(--fail);font-size:12px;margin-top:6px;white-space:pre-wrap}
.pill{display:inline-block;padding:1px 8px;border-radius:9px;color:#fff;font-size:11px;letter-spacing:.04em;text-transform:uppercase}
.pill.pass{background:var(--pass)}.pill.fail{background:var(--fail)}.pill.err{background:var(--err)}
.note{background:#fff;border-left:3px solid var(--rule);padding:8px 12px;margin:0 0 16px;font-size:14px}
</style></head><body>`)
	fmt.Fprintf(&b, "<h1>Gold set — %s <span style=\"color:var(--dim);font-weight:400\">(%s)</span></h1>", html.EscapeString(r.Started.Format("2006-01-02 15:04")), html.EscapeString(r.SHA))
	fmt.Fprintf(&b, "<p class=lede>%s</p>", html.EscapeString(strings.ReplaceAll(goldPreamble, "`", "")))
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "<p class=note>%s</p>", html.EscapeString(n))
	}

	htmlGrades := goldGrades(r.Items)
	for _, grade := range []string{goldTruth, goldRetrieval} {
		if line := goldGradedLine(r.Arms, all, htmlGrades, grade); line != "" {
			fmt.Fprintf(&b, "<p class=note><b>%s</b></p>", html.EscapeString(line))
		}
	}
	b.WriteString(`<div class=wrap><table><tr><th>Arm</th><th>Answered</th><th>Pass</th><th>Fail</th><th>Error/timeout</th><th>Tool calls</th><th>Median s</th></tr>`)
	for _, arm := range r.Arms {
		st := tally[arm]
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%.1f</td></tr>",
			html.EscapeString(arm), st.Answered, st.Pass, st.Fail, st.Errors, st.Calls, st.Median)
	}
	b.WriteString("</table></div>")

	b.WriteString(`<div class=wrap><table><tr><th>id</th><th>question</th><th>gold</th>`)
	for _, arm := range r.Arms {
		fmt.Fprintf(&b, "<th>%s</th><th>✓</th>", html.EscapeString(arm))
	}
	b.WriteString("</tr>")
	for _, it := range r.Items {
		for i, spec := range it.Turns {
			class := ""
			if len(it.Turns) > 1 {
				class = " class=multi"
			}
			id := it.ID
			if len(it.Turns) > 1 {
				id = fmt.Sprintf("%s <b>turn %d/%d</b>", it.ID, i+1, len(it.Turns))
			}
			fmt.Fprintf(&b, "<tr%s><td class=id>%s<div class=meta>%s<br>%s<br>%s</div></td><td class=q>%s</td><td class=gold>%s</td>",
				class, id, html.EscapeString(it.Kind), html.EscapeString(strings.Join(it.Tags, ", ")),
				html.EscapeString(it.Source), html.EscapeString(spec.Q), html.EscapeString(spec.Gold))
			for _, arm := range r.Arms {
				a, _ := goldAnswerFor(r, arm, it.ID, i+1)
				pill := "err"
				switch {
				case a.Err != "":
				case a.Pass:
					pill = "pass"
				default:
					pill = "fail"
				}
				fmt.Fprintf(&b, "<td class=arm>%s", html.EscapeString(orText(a.Answer, "(nothing)")))
				if a.Err != "" {
					fmt.Fprintf(&b, "<div class=err>%s</div>", html.EscapeString(truncateRunes(a.Err, 400)))
				}
				if len(a.Hops) > 0 {
					b.WriteString("<div class=calls>")
					for _, h := range a.Hops {
						fmt.Fprintf(&b, "%s\n", html.EscapeString(goldCall(h)))
					}
					b.WriteString("</div>")
				}
				fmt.Fprintf(&b, "<div class=meta>%.1fs · <span class=\"pill %s\">%s</span></div></td><td class=tick></td>",
					a.Seconds, pill, pill)
			}
			b.WriteString("</tr>")
		}
	}
	b.WriteString("</table></div></body></html>")
	return b.String()
}
