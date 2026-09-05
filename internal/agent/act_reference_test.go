package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// refNow is the clock every test in this file renders against, so the ages in the expected strings are fixed rather than whatever the machine says today.
var refNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// subtitlesMatch is a real successful run from the user's store — the one that found the subtitles button on a JioHotstar page — as SimilarActRuns hands it over.
func subtitlesMatch(ago time.Duration) db.ActMatch {
	return db.ActMatch{
		Score: 0.83,
		When:  refNow.Add(-ago),
		Run: db.ActRun{
			ID:       39,
			Question: "show me how to change subtitles on this page, or where",
			Outcome:  "ok",
			Steps: []db.ActStep{
				{Name: "observe_screen", Args: map[string]any{}, Result: "Brave Browser · Watch Family Guy S16 Episode 9 on JioHotstar - Brave\n[1] push button \"Minimize\" (51,20)"},
				{Name: "point_at", Args: map[string]any{"n": float64(28), "label": "subtitles"}, Result: `ringed [28] push button "Audio & Subtitles"`},
			},
		},
	}
}

// TestRenderActReferenceReadsBackTheQuestionThenTheSteps checks the one thing this rendering is for: the question that was asked, then what was done about it, in the order it was done, in words a person would say.
func TestRenderActReferenceReadsBackTheQuestionThenTheSteps(t *testing.T) {
	got := RenderActReference(subtitlesMatch(6*time.Hour), refNow)
	want := `6 hours ago, asked "show me how to change subtitles on this page, or where": looked at the screen, pointed at item 28 (subtitles).`
	if got != want {
		t.Errorf("RenderActReference =\n  %q\nwant\n  %q", got, want)
	}
}

// TestRenderActReferenceKeepsTheStepsInTheOrderTheyHappened checks the steps are not reordered or sorted: a reference whose steps are in the wrong order describes a way of working that never happened.
func TestRenderActReferenceKeepsTheStepsInTheOrderTheyHappened(t *testing.T) {
	m := db.ActMatch{
		When:  refNow.Add(-time.Hour),
		Score: 0.9,
		Run: db.ActRun{
			Question: "take me to the open tab where family guy is playing",
			Steps: []db.ActStep{
				{Name: "observe_screen", Result: "Brave Browser · clicky - Google Search - Brave"},
				{Name: "click", Args: map[string]any{"n": float64(84)}, Result: `clicked [84] page tab "Watch Family Guy S16 Episode 8 on JioHotstar" via doDefault`},
				{Name: "observe_screen", Result: "Brave Browser · Watch Family Guy S16 Episode 8 on JioHotstar - Brave"},
			},
		},
	}
	got := RenderActReference(m, refNow)
	steps := got[strings.Index(got, ": ")+2:]
	// The tab's name runs past the label cap and is cut at forty runes, the same cap the nightly notes stage puts on one.
	want := "looked at the screen, clicked item 84 (Watch Family Guy S16 Episode 8 on JioHot), looked at the screen."
	if steps != want {
		t.Errorf("steps rendered as\n  %q\nwant\n  %q", steps, want)
	}
}

// TestRenderActReferenceSaysHowLongAgoItWas checks the age the model reads so it can discount an old run itself, in the coarsest unit that is still informative.
func TestRenderActReferenceSaysHowLongAgoItWas(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "a moment ago"},
		{5 * time.Minute, "5 minutes ago"},
		{90 * time.Minute, "an hour ago"},
		{6 * time.Hour, "6 hours ago"},
		{25 * time.Hour, "yesterday"},
		{4 * 24 * time.Hour, "4 days ago"},
		{21 * 24 * time.Hour, "3 weeks ago"},
	}
	for _, c := range cases {
		got := RenderActReference(subtitlesMatch(c.ago), refNow)
		if !strings.HasPrefix(got, c.want+", asked ") {
			t.Errorf("a run from %v ago rendered as %q, want it to open with %q", c.ago, got, c.want)
		}
	}
	// A run whose time did not survive the store says nothing about its age rather than inventing one.
	m := subtitlesMatch(time.Hour)
	m.When = time.Time{}
	if got := RenderActReference(m, refNow); !strings.HasPrefix(got, `Asked "show me how`) {
		t.Errorf("a run with no time rendered as %q, want it to open with the question and no age", got)
	}
}

// TestRenderActReferenceHasNothingToSayAboutSomeRuns checks the two runs that are not worth showing: one whose steps render to nothing a person could read, and one so long it was feeling its way around the screen rather than following a way that worked — the same length rule the nightly notes stage applies before it will write a run up.
func TestRenderActReferenceHasNothingToSayAboutSomeRuns(t *testing.T) {
	empty := db.ActMatch{When: refNow, Score: 1, Run: db.ActRun{Question: "do the thing", Steps: []db.ActStep{{Name: "query_memory", Args: map[string]any{"query": "the thing"}}}}}
	if got := RenderActReference(empty, refNow); got != "" {
		t.Errorf("a run with no readable steps rendered as %q, want nothing", got)
	}

	long := db.ActMatch{When: refNow, Score: 1, Run: db.ActRun{Question: "please play season 16, episode 8"}}
	for i := 0; i < actReferenceStepCap+1; i++ {
		long.Run.Steps = append(long.Run.Steps, db.ActStep{Name: "observe_screen"})
	}
	if got := RenderActReference(long, refNow); got != "" {
		t.Errorf("a run of %d steps rendered as %q, want nothing", len(long.Run.Steps), got)
	}
}

// TestRenderActReferenceNeverQuotesWhatWasTyped checks the one thing that must never come back out: a step that typed says what it typed into, never what was typed, because what the user dictated can be a passphrase or a private message.
func TestRenderActReferenceNeverQuotesWhatWasTyped(t *testing.T) {
	m := db.ActMatch{
		When:  refNow.Add(-time.Hour),
		Score: 1,
		Run: db.ActRun{
			Question: "search for the thing",
			Steps: []db.ActStep{
				{Name: "click", Args: map[string]any{"n": float64(9)}, Result: `clicked [9] entry "Address and search bar" via activate`},
				// A row written before the storage layer started dropping it still carries the text, so the rendering must ignore it rather than trust it is gone.
				{Name: "type_text", Args: map[string]any{"enter": true, "text": "hunter2-my-password"}, Result: "typed 19 characters"},
			},
		},
	}
	got := RenderActReference(m, refNow)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("RenderActReference = %q, which carries what was typed", got)
	}
	if !strings.HasSuffix(got, "typed into the box in front and pressed Enter.") {
		t.Errorf("RenderActReference = %q, want it to end by saying what was typed into", got)
	}
}

// TestActReferenceBlockFramesTheRunsAsSomethingThatHappened is the wording rule in one test: the block must read as a record of what happened once, must say in words that it is not instructions, must say the screen may have changed, and must say the item numbers mean nothing now. A trace handed to a model as a script gets followed even when the screen has moved on, which is the failure this whole thing exists to prevent.
func TestActReferenceBlockFramesTheRunsAsSomethingThatHappened(t *testing.T) {
	block := ActReferenceBlock([]db.ActMatch{subtitlesMatch(6 * time.Hour)}, refNow)
	if block == "" {
		t.Fatal("ActReferenceBlock rendered nothing for one usable match")
	}
	for _, phrase := range []string{
		"not instructions",
		"may have changed",
		"look at it",
		"item numbers",
		"decide",
	} {
		if !strings.Contains(block, phrase) {
			t.Errorf("the block never says %q:\n%s", phrase, block)
		}
	}
	if !strings.Contains(block, `- 6 hours ago, asked "show me how to change subtitles on this page, or where": looked at the screen, pointed at item 28 (subtitles).`) {
		t.Errorf("the block does not carry the run's own line:\n%s", block)
	}
	// Fenced at both ends, the way turnContext fences quoted memory, because a rule stated only above the content can be argued away by the content.
	if !strings.HasPrefix(block, "[before]") || !strings.Contains(block, "[end before]") {
		t.Errorf("the block is not fenced at both ends:\n%s", block)
	}
}

// TestActReferenceBlockShowsNoMoreThanItsCap checks how much of the prompt this may take: it goes into every screen ask, so the number of runs shown is capped whatever the lookup returns, and the cap is the closest ones because the matches arrive closest first.
func TestActReferenceBlockShowsNoMoreThanItsCap(t *testing.T) {
	var matches []db.ActMatch
	for i := 0; i < actReferenceRunCap+2; i++ {
		m := subtitlesMatch(time.Duration(i+1) * time.Hour)
		m.Run.Question = strings.Repeat("wording ", i+1) + "change subtitles"
		matches = append(matches, m)
	}
	block := ActReferenceBlock(matches, refNow)
	// Two, written out rather than read from the constant: the cap is a claim about what this costs every screen ask, and a test that reads the constant would agree with any number someone put there.
	if lines := strings.Count(block, "\n- "); lines != 2 {
		t.Fatalf("the block carries %d run lines, want 2:\n%s", lines, block)
	}
	if !strings.Contains(block, "\n- an hour ago, asked \"wording change subtitles\"") {
		t.Errorf("the block dropped the closest run, which arrives first:\n%s", block)
	}
}

// TestActReferenceBlockIsNothingWhenThereIsNothingToShow checks the block adds not one token to a prompt when the lookup found nothing close enough, and the same when everything it found renders to nothing.
func TestActReferenceBlockIsNothingWhenThereIsNothingToShow(t *testing.T) {
	if got := ActReferenceBlock(nil, refNow); got != "" {
		t.Errorf("ActReferenceBlock(nil) = %q, want nothing", got)
	}
	unreadable := db.ActMatch{When: refNow, Score: 1, Run: db.ActRun{Question: "do the thing", Steps: []db.ActStep{{Name: "query_memory"}}}}
	if got := ActReferenceBlock([]db.ActMatch{unreadable}, refNow); got != "" {
		t.Errorf("ActReferenceBlock with nothing renderable = %q, want nothing", got)
	}
}

// actLookupBrain is a toolTestBrain that can also answer the act run lookup, standing in for the real store.
type actLookupBrain struct {
	*toolTestBrain
	matches []db.ActMatch
	err     error
	asked   string
	limit   int
}

func (b *actLookupBrain) SimilarActRuns(ctx context.Context, question string, limit int) ([]db.ActMatch, error) {
	b.asked, b.limit = question, limit
	return b.matches, b.err
}

// TestWithActReferencePutsTheBlockAheadOfWhatTheUserSaid checks the wiring an ask path uses: the reference arrives as a part of its own, behind the turn's context and in front of the user's own words, so nothing is folded into the user's text.
func TestWithActReferencePutsTheBlockAheadOfWhatTheUserSaid(t *testing.T) {
	brain := &actLookupBrain{toolTestBrain: &toolTestBrain{}, matches: []db.ActMatch{subtitlesMatch(6 * time.Hour)}}
	a := NewAgent(nil, nil, brain, nil, "")
	const question = "show me how to change subtitles"

	contents := a.WithActReference(context.Background(), refNow, question, buildTurnContent(refNow, nil, question))
	if len(contents) != 1 {
		t.Fatalf("WithActReference returned %d contents, want the one turn it was given", len(contents))
	}
	parts := contents[0].Parts
	if len(parts) != 3 {
		t.Fatalf("the turn has %d parts, want the context, the block and the question", len(parts))
	}
	if !strings.HasPrefix(parts[0].Text, "[context]") {
		t.Errorf("part 0 = %q, want the turn context first", parts[0].Text)
	}
	if !strings.HasPrefix(parts[1].Text, "[before]") {
		t.Errorf("part 1 = %q, want the reference block", parts[1].Text)
	}
	if parts[2].Text != question {
		t.Errorf("part 2 = %q, want the user's own words last and untouched", parts[2].Text)
	}
	if brain.asked != question || brain.limit != actReferenceMatchCap {
		t.Errorf("the lookup was asked %q with a limit of %d, want %q and %d", brain.asked, brain.limit, question, actReferenceMatchCap)
	}
}

// TestWithActReferenceLeavesTheTurnAloneWhenThereIsNoReference checks the three ways there is nothing to add — no match, a store that cannot look one up, and a store that failed — all leave the prompt exactly as it was, because a failed lookup must never cost an ask its answer.
func TestWithActReferenceLeavesTheTurnAloneWhenThereIsNoReference(t *testing.T) {
	const question = "what did i do yesterday"
	brains := map[string]ContextReader{
		"nothing close enough": &actLookupBrain{toolTestBrain: &toolTestBrain{}},
		// A store that errors part-way can hand back rows and an error together; the rows must be dropped with it.
		"the lookup failed":        &actLookupBrain{toolTestBrain: &toolTestBrain{}, matches: []db.ActMatch{subtitlesMatch(time.Hour)}, err: errNoLookup},
		"a store with no act runs": &toolTestBrain{},
	}
	for name, brain := range brains {
		t.Run(name, func(t *testing.T) {
			a := NewAgent(nil, nil, brain, nil, "")
			contents := a.WithActReference(context.Background(), refNow, question, buildTurnContent(refNow, nil, question))
			if len(contents[0].Parts) != 2 {
				t.Fatalf("the turn has %d parts, want the context and the question it started with", len(contents[0].Parts))
			}
			if contents[0].Parts[1].Text != question {
				t.Errorf("the user's words came back as %q, want %q", contents[0].Parts[1].Text, question)
			}
		})
	}
}

// errNoLookup stands in for a store that could not answer the lookup.
var errNoLookup = errors.New("the store is busy")

// TestRenderActReferenceSkipsARunThatOnlyLooked checks a run that did nothing but look at the screen is not offered as reference. Six of the twenty distinct questions in the user's store are answered by one observe_screen and nothing else — "what window is in front", "read the numbers you can see" — and a line saying only "looked at the screen" teaches the model nothing it does not already do, while still costing the whole block's framing. A run has to have pointed at, clicked, scrolled to or typed into something to be worth showing.
func TestRenderActReferenceSkipsARunThatOnlyLooked(t *testing.T) {
	onlyLooked := db.ActMatch{
		When:  refNow.Add(-3 * time.Hour),
		Score: 1,
		Run: db.ActRun{
			Question: "Read the numbers you can see in the window in front and list them in one line.",
			Steps: []db.ActStep{
				{Name: "observe_screen", Result: "Brave Browser · Desmos"},
				{Name: "show_marks", Result: "numbered 40 items"},
				{Name: "observe_screen", Result: "Brave Browser · Desmos"},
			},
		},
	}
	if got := RenderActReference(onlyLooked, refNow); got != "" {
		t.Errorf("a run that only looked at the screen rendered as %q, want nothing", got)
	}
	if got := ActReferenceBlock([]db.ActMatch{onlyLooked}, refNow); got != "" {
		t.Errorf("a block of nothing but looking rendered as %q, want nothing", got)
	}

	// One acting step among the looks is enough, since that step is the part worth reading.
	acted := onlyLooked
	acted.Run.Steps = append(acted.Run.Steps, db.ActStep{Name: "point_at", Args: map[string]any{"n": float64(7)}, Result: `ringed [7] text "34%"`})
	if got := RenderActReference(acted, refNow); got == "" {
		t.Error("a run that looked and then pointed at something rendered as nothing, want the line")
	}
}
