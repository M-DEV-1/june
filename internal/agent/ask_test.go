package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/util"

	"google.golang.org/genai"
)

func TestEvalExecute_BlocksNonMemoryTools(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	got := a.evalExecute(t.Context(), "shell_exec", map[string]any{"command": "rm -rf /"})
	if !strings.Contains(got, "not available in an ask") {
		t.Fatalf("expected shell_exec to be blocked, got %q", got)
	}
	// read_file stays blocked too: askAllowedTools admits save_note and the other store-writing tools a real /ask turn needs, but not the file tools.
	got = a.evalExecute(t.Context(), "read_file", map[string]any{"path": "/etc/passwd"})
	if !strings.Contains(got, "not available in an ask") {
		t.Fatalf("expected read_file to be blocked, got %q", got)
	}
}

// TestExecuteTool_NilBrainNeverPanics builds an agent with a nil brain — the shape NewAgent(nil, nil, nil, nil, "") produces, and the shape a not-yet-connected daemon can hand a tool call — and runs every tool the ask gate or the subtask gate can reach through it. Widening askAllowedTools to admit more memory tools exposed a path where such a call reached a.brain.SomeMethod(...) and panicked on the nil interface's own method dispatch instead of failing like every other executeTool error path does. None of these calls may panic, and each must come back as a plain "error: ..." string, the same shape every other failure path in executeTool already uses.
func TestExecuteTool_NilBrainNeverPanics(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, "")
	names := make(map[string]bool, len(subtaskAllowedTools)+len(askAllowedTools))
	for name := range subtaskAllowedTools {
		names[name] = true
	}
	for name := range askAllowedTools {
		names[name] = true
	}
	for name := range names {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("executeTool(%q) panicked with a nil brain: %v", name, r)
				}
			}()
			got := a.executeTool(context.Background(), name, nil)
			if got == "" {
				t.Fatalf("executeTool(%q) with a nil brain returned an empty result", name)
			}
			// Only the tools that actually read or write through a.brain must fail: the rest (observe_screen and the other screen tools, open_url) don't touch it at all, and can come back with a real result when this test runs on a desktop that has a screen to look at.
			if brainRequiredTools[name] && !strings.HasPrefix(got, "error:") {
				t.Fatalf("executeTool(%q) touches a nil brain and must fail as \"error: ...\", got %q", name, got)
			}
		})
	}
}

// textOf joins the text of a history content's parts, so a test can read back what one prior turn carried.
func textOf(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// A thread of a few very long turns is under the count cap and still too big to re-send, so the size cap drops the oldest until what is left fits, and one turn longer than the whole per-turn cap is truncated rather than dropped.
func TestHistoryFromTurns_DropsOldestWhenTheThreadIsTooBig(t *testing.T) {
	// Six turns of a full per-turn cap each are 24 KB, half again over the size cap, and well under the count cap, so the size cap is what does the cutting here.
	big := strings.Repeat("x", maxHistoryTurnBytes)
	var turns []db.Turn
	for i, label := range []string{"oldest", "second", "third", "fourth", "fifth", "newest"} {
		role := "you"
		if i%2 == 1 {
			role = "june"
		}
		turns = append(turns, db.Turn{Role: role, Text: label + " " + big, Kind: "ask"})
	}
	got := HistoryFromTurns(turns)
	if len(got) == 0 || len(got) >= len(turns) {
		t.Fatalf("got %d history turns, want the oldest of %d dropped", len(got), len(turns))
	}
	total := 0
	for _, c := range got {
		total += len(textOf(c))
	}
	if total > maxHistoryBytes {
		t.Errorf("history is %d bytes, over the cap of %d", total, maxHistoryBytes)
	}
	if !strings.HasPrefix(textOf(got[len(got)-1]), "newest") {
		t.Errorf("the newest turn must survive, last kept = %q", util.Runes(textOf(got[len(got)-1]), 20))
	}
	if strings.HasPrefix(textOf(got[0]), "oldest") {
		t.Errorf("the oldest turn should have been dropped first, first kept = %q", util.Runes(textOf(got[0]), 20))
	}

	long := HistoryFromTurns([]db.Turn{{Role: "june", Text: strings.Repeat("y", maxHistoryTurnBytes*2), Kind: "ask"}})
	if len(long) != 1 {
		t.Fatalf("one overlong turn must be kept, got %d", len(long))
	}
	if n := len(textOf(long[0])); n != maxHistoryTurnBytes {
		t.Errorf("overlong turn kept %d bytes, want it truncated to %d", n, maxHistoryTurnBytes)
	}
}

// The Gemini text path must put the thread in the request itself: the prior question and its answer come before this turn's context and question, in that order. A question asked with no history carries none of it, so the existing single-question entry point sends what it always sent.
func TestAskTextWith_SendsThePriorTurnsBeforeTheQuestion(t *testing.T) {
	var bodies []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Ringed the Pause button again."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	history := HistoryFromTurns([]db.Turn{
		{Role: "you", Text: "draw a ring around any one button you can see", Kind: "ask"},
		{Role: "june", Text: "I ringed the Pause button.", Kind: "ask"},
	})
	tr, err := a.AskTextWith(t.Context(), history, "do it again")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Answer != "Ringed the Pause button again." {
		t.Errorf("answer = %q", tr.Answer)
	}
	if len(bodies) != 1 {
		t.Fatalf("%d requests, want 1", len(bodies))
	}
	asked := strings.Index(bodies[0], "draw a ring around any one button you can see")
	answered := strings.Index(bodies[0], "I ringed the Pause button.")
	now := strings.Index(bodies[0], "do it again")
	if asked < 0 || answered < 0 || now < 0 {
		t.Fatalf("request carried question/answer/new question at %d/%d/%d", asked, answered, now)
	}
	if !(asked < answered && answered < now) {
		t.Errorf("the thread must be in order, got question at %d, answer at %d, new question at %d", asked, answered, now)
	}

	if _, err := a.AskText(t.Context(), "do it again"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	if strings.Contains(bodies[1], "I ringed the Pause button.") {
		t.Error("a question asked with no history must not carry a prior answer")
	}
}

// TestAskTools_OfferNothingTheGateWouldRefuse checks the three ask paths hand the model only tools evalExecute will actually run. The failing run called shell_exec and branch, was refused by the gate both times, and spent two of its twelve rounds learning that tools it had been offered do not work — a tool on the list that can only ever come back refused is a trap the prompt cannot talk the model out of.
func TestAskTools_OfferNothingTheGateWouldRefuse(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	offered := map[string]bool{}
	for _, decl := range a.askToolDeclarations() {
		offered[decl.Name] = true
		if !askAllowedTools[decl.Name] && !subtaskAllowedTools[decl.Name] {
			t.Errorf("tool %q is offered but the ask gate refuses it", decl.Name)
		}
	}
	for _, want := range []string{"observe_screen", "click", "scroll_to", "query_memory"} {
		if !offered[want] {
			t.Errorf("tool %q must stay on the ask's list", want)
		}
	}
	for _, tool := range a.textAskTools() {
		for _, decl := range tool.FunctionDeclarations {
			if !offered[decl.Name] {
				t.Errorf("the text channel offers %q, which the declaration list does not", decl.Name)
			}
		}
	}
	// A stress run against a snapshot lifts the gate, so it must also get the full tool surface back rather than the narrowed one.
	a.AllowEvalWrites()
	if len(a.askToolDeclarations()) != len(ToolDeclarations()) {
		t.Errorf("with the gate lifted the ask offers %d tools, want all %d", len(a.askToolDeclarations()), len(ToolDeclarations()))
	}
}

// TestAskText_RepeatedLooksDoNotSpendTheCap runs the whole text loop against a model that does nothing but call observe_screen. Every look comes back with the same window, so none of them moves the task on: the loop must keep going past maxAskIterations rather than stopping at the step cap, must still stop at the hard round bound rather than spinning forever, and must end by naming the window it was looking at all along.
func TestAskText_RepeatedLooksDoNotSpendTheCap(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _ := observingAgent(t)
	tr, err := a.askText(t.Context(), "gemini-test", nil, "play the eighth one")
	if err == nil {
		t.Fatal("a loop that never answers must come back as an error")
	}
	if len(tr.ToolHops) <= maxAskIterations {
		t.Errorf("%d looks, want more than %d: identical looks must not spend the cap", len(tr.ToolHops), maxAskIterations)
	}
	if len(tr.ToolHops) != maxAskRounds {
		t.Errorf("%d looks, want the hard bound of %d to stop it", len(tr.ToolHops), maxAskRounds)
	}
	if !strings.Contains(err.Error(), "PR #13 · GitHub") {
		t.Errorf("err = %q, want it to name the window it last saw", err)
	}
}

// TestAskText_StopsAtTheStepCapWithCapError runs the whole text loop against a model that keeps calling a real tool forever, each call distinct (a fresh add_task title every round), so every round spends one of the ask's maxAskIterations steps rather than being absorbed by sameScreenAgain or onlyAnnotated. The loop must stop the moment it reaches the cap and hand back capError, not run on to the round-based safety net (maxAskRounds).
func TestAskText_StopsAtTheStepCapWithCapError(t *testing.T) {
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"add_task","args":{"title":"step %d"}}}]}}]}`, n)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	tr, err := a.askText(t.Context(), "gemini-test", nil, "file tasks forever")
	if err == nil {
		t.Fatal("a loop that never stops calling tools must come back as an error once the cap is spent")
	}
	if len(tr.ToolHops) != maxAskIterations {
		t.Errorf("%d tool hops, want exactly the cap of %d", len(tr.ToolHops), maxAskIterations)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", maxAskIterations)) {
		t.Errorf("err = %q, want capError to name how many steps it ran out after", err)
	}
}

// The one hard stop on askText's loop alongside the step cap is the wall clock (askWallClock): a turn that keeps calling tools still has to end once that much time has passed, so a runaway loop cannot hang the daemon. The test shrinks the wall clock rather than waiting twelve minutes for the real one to prove itself.
func TestAskText_StopsAtTheWallClock(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"add_task","args":{"title":"x"}}}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	old := askWallClock
	askWallClock = 20 * time.Millisecond
	t.Cleanup(func() { askWallClock = old })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	start := time.Now()
	_, err := a.askText(t.Context(), "gemini-test", nil, "keep filing tasks forever")
	if err == nil {
		t.Fatal("a loop that never stops calling tools must come back as an error once the wall clock passes")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("askText took %s to stop after a %s wall clock", elapsed, askWallClock)
	}
}

// A click the stop line refuses ends the turn on the spot: the tool's own "Stopped before ..." result becomes the answer, with no further round given a chance to click the same button a different way or talk around the refusal.
func TestAskText_StopsBeforeIrreversibleClickAndEndsTheTurn(t *testing.T) {
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"click","args":{"n":1}}}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, f, _ := guardedClickAgent(t)
	tr, err := a.askText(t.Context(), "gemini-test", nil, "click send")
	if err != nil {
		t.Fatalf("askText: %v", err)
	}
	if len(f.clicked) != 0 {
		t.Errorf("clicked = %v, want the click refused", f.clicked)
	}
	if !strings.HasPrefix(tr.Answer, "Stopped before ") {
		t.Errorf("answer = %q, want it to begin \"Stopped before \"", tr.Answer)
	}
	if !strings.Contains(tr.Answer, `"yes, send it"`) {
		t.Errorf("answer = %q, want the one-line question naming the phrase to say", tr.Answer)
	}
	if n != 2 {
		t.Errorf("rounds = %d, want exactly 2 — the turn must end the moment the stop line fires, not spend a third round on it", n)
	}
}

// A look is only worth taking if the model is actually shown the picture. The Gemini path sends it as inline data in a user turn straight after the tool result that produced it, so the answer to "what is in this video" is read off the pixels rather than guessed.
func TestAskText_SendsTheLookPictureToTheModel(t *testing.T) {
	var bodies []string
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"look","args":{}}}]}}]}`)
			return
		}
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Stewie is on the left."}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _, _ := lookingAgent(t)
	tr, err := a.askText(t.Context(), "gemini-test", nil, "who is who on screen")
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d rounds, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], base64.StdEncoding.EncodeToString([]byte("fake-jpeg-bytes"))) {
		t.Error("the round after a look must carry the picture's own bytes")
	}
	if !strings.Contains(bodies[1], `"image/jpeg"`) {
		t.Error("the picture must be sent with its media type")
	}
	if tr.ImageTokens != lookTokenCost(1280, 704) {
		t.Errorf("ImageTokens = %d, want %d", tr.ImageTokens, lookTokenCost(1280, 704))
	}
}

// refusingGate is a request gate that turns every Gemini request away with the error it was given.
type refusingGate struct{ err error }

func (g refusingGate) Allow(string) error { return g.err }

// An interactive ask counts against the same daily Gemini allowance as the nightly jobs: when the gate refuses, no request reaches Gemini, and the refusal is the 429 shape the Codex hand-over already recognises, so the ask moves on instead of failing.
func TestAskText_GateRefusalReachesNoBackendAndReadsAsQuota(t *testing.T) {
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"never"}]}}]}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "test-key")
	a.SetRequestGate(refusingGate{err: genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "daily request quota of 20 reached"}})
	_, err := a.askText(t.Context(), "gemini-3.5-flash", nil, "what is in front of me")
	if err == nil {
		t.Fatal("askText succeeded with a refusing gate")
	}
	if !geminiCannotAnswer(err) {
		t.Errorf("error %v is not the quota shape the hand-over looks for", err)
	}
	if requests != 0 {
		t.Errorf("%d requests reached the backend, want 0", requests)
	}
}

// A turn that only read the screen may still be handed to Codex or Claude when Gemini's allowance runs out: repeating observe_screen on the other provider changes nothing, where repeating a click would click twice. Counting every hop instead — which is what AskTextWith used to pass — refused the hand-over to any turn that had merely looked, and that is how a routine returned a raw 429 to the user on 2026-09-05.
func TestAskTextWith_HandsOnAfterAReadOnlyToolButNotAfterAnAction(t *testing.T) {
	round := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"observe_screen","args":{}}}]}}]}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`)
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a, _ := observingAgent(t)
	// askText is the call AskTextWith makes for the Gemini provider; what the router then decides on what came back is checked here rather than by letting the test run the Codex backend or the Claude command line for real.
	tr, err := a.askText(t.Context(), "gemini-test", nil, "what is on my screen")
	if err == nil {
		t.Fatal("the ask must fail once the model answers 429")
	}
	if len(tr.ToolHops) != 1 || tr.ToolHops[0].Name != "observe_screen" {
		t.Fatalf("tool hops = %+v, want the one read-only look the model made", tr.ToolHops)
	}
	if !ProviderSpent(err) {
		t.Error("a 429 must count as a spent allowance, so the question is handed on")
	}
	if actionHops(tr.ToolHops) != 0 {
		t.Error("a turn that only looked at the screen has taken no action, so it may still be handed on")
	}
	acted := append(tr.ToolHops, ToolHop{Name: "click", Result: "clicked [1] push button \"Merge\""})
	if actionHops(acted) == 0 {
		t.Error("a turn that clicked something must count as having acted, so it is never replayed on another provider")
	}
}

// TestLeanPrompt_CarriesTheStandingUnderstanding pins the bug from conversation 263 on 2026-10-07: asked for recommendations "based on my workstyle", June answered "I don't know your workstyle yet" while the diary's understanding row, rewritten every night by dreaming, already said how he works. The voice handshake had it through GetImplicitContext's leading "[understanding]" line; the text ask carried no context at all. The lean prompt now carries the doc once, in the tail, so the cacheable prefix stays unchanged, and the handshake is left alone so it does not carry it twice.
func TestLeanPrompt_CarriesTheStandingUnderstanding(t *testing.T) {
	b := &toolTestBrain{understanding: "He runs several projects in parallel and expects direct, unembellished output."}
	a := NewAgent(nil, nil, b, nil, "")
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.Local)
	lean := a.LeanPrompt(t.Context(), now)
	if n := strings.Count(lean, b.understanding); n != 1 {
		t.Errorf("lean prompt carries the understanding doc %d times, want once", n)
	}
	if i := strings.Index(lean, b.understanding); i >= 0 && i < strings.Index(lean, "<safety_rules>") {
		t.Error("lean prompt puts the understanding ahead of the stable prefix")
	}
	if voice, _ := a.HandshakePrompt(t.Context(), now); strings.Contains(voice, "Your standing understanding") {
		t.Error("handshake carries the lean block on top of the [understanding] context line")
	}
	// An empty doc, the state before the first dream, leaves nothing behind in the prompt.
	b.understanding = ""
	if strings.Contains(a.LeanPrompt(t.Context(), now), "understanding of them") {
		t.Error("lean prompt carries an understanding block with no doc to put in it")
	}
}

// The instruction of a round that has not touched the screen leaves out the screen blocks, still carries the stop line and the understanding doc, and states the number of tools that round is actually offered. Claude and agy have no round that swaps the instruction, so LeanPrompt keeps the screen blocks for them.
func TestPlainAskPrompt_LeavesOutTheScreenBlocks(t *testing.T) {
	b := &toolTestBrain{understanding: "He prefers short answers."}
	a := NewAgent(nil, nil, b, nil, "")
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.Local)
	plain := a.plainAskPrompt(t.Context(), now)
	for _, gone := range []string{"<screen_interaction>", screenTaskGuidance} {
		if strings.Contains(plain, gone) {
			t.Errorf("a plain ask's instruction carries %.40q...", gone)
		}
	}
	for _, kept := range []string{"<memory_guidelines>", stopLineText, b.understanding} {
		if !strings.Contains(plain, kept) {
			t.Errorf("a plain ask's instruction lost %.40q...", kept)
		}
	}
	want := fmt.Sprintf("You have %d tools", len(withoutDeclarations(a.askToolDeclarations(), screenActionTools)))
	if !strings.Contains(plain, want) {
		t.Errorf("a plain ask's instruction does not say %q", want)
	}
	lean := a.LeanPrompt(t.Context(), now)
	if !strings.Contains(lean, "<screen_interaction>") || !strings.Contains(lean, screenTaskGuidance) {
		t.Error("LeanPrompt must keep the screen blocks for the providers that cannot swap their instruction mid-ask")
	}
}

// do and branch are offered to the live voice session only, so the guidance about them is in its prompt and in no text ask's, whatever the provider.
func TestTextPrompts_LeaveOutTheGuidanceForToolsOnlyVoiceHas(t *testing.T) {
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "")
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.Local)
	handshake, _ := a.HandshakePrompt(t.Context(), now)
	for name, text := range map[string]string{"plain": a.plainAskPrompt(t.Context(), now), "lean": a.LeanPrompt(t.Context(), now), "handshake": handshake} {
		if strings.Contains(text, jobToolsGuidance) {
			t.Errorf("the %s text prompt tells the model to call do or branch, which an ask refuses", name)
		}
	}
	if voice := systemInstructionText(now, "linux", "amd64", "bash", "", "", 0); !strings.Contains(voice, jobToolsGuidance) {
		t.Error("the voice prompt lost its guidance on do and branch")
	}
	for _, name := range []string{"do", "branch"} {
		if askAllowedTools[name] || subtaskAllowedTools[name] {
			t.Errorf("%q is now allowed in an ask, so the text prompts need its guidance back", name)
		}
	}
}

// offeredToolNames reads the names of the function declarations out of one request body the Gemini text path sent.
func offeredToolNames(t *testing.T, body string) map[string]bool {
	t.Helper()
	var req struct {
		Tools []struct {
			FunctionDeclarations []struct{ Name string } `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range req.Tools {
		for _, d := range tool.FunctionDeclarations {
			names[d.Name] = true
		}
	}
	return names
}

// A question that does not name a screen task is offered no tool that acts on the screen, so each ask does not pay to declare them. The ask that then calls a screen tool must get them from the next round on, and a thread that has already called a memory tool must get them too, because the model may still turn to the screen after it.
func TestAskText_PlainAskWidensToScreenToolsAfterALook(t *testing.T) {
	calls := func(names ...string) []string {
		var replies []string
		for _, name := range names {
			replies = append(replies, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"`+name+`","args":{}}}]}}]}`)
		}
		return append(replies, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]}}]}`)
	}
	run := func(replies []string) []map[string]bool {
		var seen []map[string]bool
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			round := len(seen)
			seen = append(seen, offeredToolNames(t, string(body)))
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, replies[round])
		}))
		defer backend.Close()
		geminiBaseURL = backend.URL
		t.Cleanup(func() { geminiBaseURL = "" })
		a, _ := observingAgent(t)
		if _, err := a.askText(t.Context(), "gemini-test", nil, "who owns Project X"); err != nil {
			t.Fatal(err)
		}
		return seen
	}

	rounds := run(calls("observe_screen"))
	if len(rounds) != 2 {
		t.Fatalf("%d requests, want 2", len(rounds))
	}
	for _, name := range []string{"query_memory", "observe_screen", "look", "open_app", "open_url", "save_note"} {
		if !rounds[0][name] {
			t.Errorf("a plain ask's first round must offer %q", name)
		}
	}
	for name := range screenActionTools {
		if rounds[0][name] {
			t.Errorf("a plain ask's first round offers %q before any look at the screen", name)
		}
		if !rounds[1][name] {
			t.Errorf("the round after observe_screen must offer %q", name)
		}
	}

	rounds = run(calls("query_memory", "observe_screen"))
	for name := range screenActionTools {
		if !rounds[2][name] {
			t.Errorf("the round after a memory call and a look must offer %q", name)
		}
	}
	if !rounds[2]["query_memory"] {
		t.Error("a thread that already used a memory tool keeps the full list once it has looked at the screen")
	}
}
