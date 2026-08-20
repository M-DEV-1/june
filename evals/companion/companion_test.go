package companion

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"ora/evals/dataset"
	"ora/evals/harness"
	"ora/internal/agent"
)

func TestPickDiagnostic_HandshakeThenCacheMiss(t *testing.T) {
	qs, err := dataset.LoadQuestions()
	if err != nil {
		t.Fatal(err)
	}
	got := pickDiagnostic(qs, 4)
	if len(got) != 4 {
		t.Fatalf("len=%d", len(got))
	}
	want := []string{"current-work", "pune", "tui", "suits"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("got[%d]=%s want %s", i, got[i].ID, id)
		}
	}
}

func TestGrade_StaleNeedsAWhen(t *testing.T) {
	q := dataset.Question{
		ID:       "suits",
		Kind:     "thread",
		Horizon:  dataset.HorizonStale,
		AgeLabel: "8d ago",
		Expect:   []string{"Tiny Violin"},
	}
	undated := Grade(q, agent.TurnTrace{Answer: "You left off on Season 7 Episode 15, Tiny Violin."})
	if !undated.AnswerHit {
		t.Fatal("expected fact hit")
	}
	if undated.TimeOK {
		t.Fatal("stale answer with no date should fail recency")
	}

	dated := Grade(q, agent.TurnTrace{Answer: "Last week you left off on Season 7 Episode 15, Tiny Violin."})
	if !dated.AnswerHit || !dated.TimeOK {
		t.Fatalf("dated stale recall should pass recency, timeOK=%v note=%s", dated.TimeOK, dated.RecencyNote)
	}

	present := Grade(q, agent.TurnTrace{Answer: "You've been watching Tiny Violin recently."})
	if !present.PresentSlip {
		t.Fatal("calling an 8d-old episode recent should be a present-slip")
	}
}

func TestGrade_DurablePresentTenseOK(t *testing.T) {
	q := dataset.Question{ID: "pune", Kind: "identity", Horizon: dataset.HorizonDurable, Expect: []string{"Hadapsar", "Mundhwa"}}
	s := Grade(q, agent.TurnTrace{Answer: "Hadapsar and Mundhwa."})
	if !s.AnswerHit || !s.TimeOK || s.PresentSlip {
		t.Fatalf("durable identity should allow present tense, %+v", s)
	}

	slip := Grade(q, agent.TurnTrace{
		Answer: "You've been recently looking at Hadapsar and Mundhwa.",
		ToolHops: []agent.ToolHop{{
			Name:   "query_memory",
			Result: "[thread (25d ago)] Pune Location & Geography Research — Google Maps around Mundhwa",
		}},
	})
	if !slip.PresentSlip {
		t.Fatal("durable fact plus a 25d hit called 'recently' should present-slip")
	}
}

func TestGrade_SplitsAnswerFromThought(t *testing.T) {
	q := dataset.Question{ID: "pune", Expect: []string{"Hadapsar", "Mundhwa"}}
	tr := agent.TurnTrace{
		Answer:    "You were looking at Hadapsar and Mundhwa.",
		Thoughts:  []string{"Need to recall the Pune note."},
		Handshake: []string{"[now] climate risk"},
	}
	s := Grade(q, tr)
	if !s.AnswerHit {
		t.Fatal("expected answer hit")
	}
	if s.ThoughtHit {
		t.Fatal("thought should not contain the needles")
	}
	if s.HandshakeHit {
		t.Fatal("handshake should not contain the needles")
	}

	quiet := agent.TurnTrace{
		Answer:   "Yeah I remember the neighborhoods.",
		Thoughts: []string{"Hadapsar and Mundhwa are the ones."},
	}
	s = Grade(q, quiet)
	if s.AnswerHit {
		t.Fatal("short answer should miss")
	}
	if !s.ThoughtHit {
		t.Fatal("thought had the fact — this is the not-talkative case")
	}
	if got := strings.Join(s.Missing, ","); got != "Hadapsar,Mundhwa" && got != "Hadapsar, Mundhwa" {
		if len(s.Missing) != 2 {
			t.Fatalf("missing = %v", s.Missing)
		}
	}
}

func TestProbeCache_HandshakeVsTools(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)
	if err := store.SetWorkingState(ctx, "Driving Climate Risk Statement Builder ASRS for Opal HealthCare at Tingira Hills."); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(ctx, "User has a professional and personal interest in Pune real estate, specifically Hadapsar and Mundhwa.", "fact"); err != nil {
		t.Fatal(err)
	}

	qs := []dataset.Question{
		{ID: "current-work", Kind: "current", Ask: "What am I working on right now?", Expect: []string{"climate", "ASRS", "Opal"}},
		{ID: "pune", Kind: "identity", Ask: "Which Pune neighborhoods am I looking at for real estate?", Expect: []string{"Hadapsar", "Mundhwa"}},
	}
	probes := ProbeCache(ctx, store, qs)
	if len(probes) != 2 {
		t.Fatalf("got %d probes", len(probes))
	}
	if !probes[0].HandshakeHas {
		t.Fatalf("current work should be in handshake, diagnosis=%s handshake=%q", probes[0].Diagnosis, probes[0].Handshake)
	}
	if probes[1].HandshakeHas {
		t.Fatalf("pune should not ride along in climate-focused handshake, got %q", probes[1].Handshake)
	}
	if !probes[1].QueryHas && !probes[1].RetrieveHas {
		t.Fatalf("pune should be findable via query_memory or retrieve, query=%q retrieve=%q diagnosis=%s", probes[1].Query, probes[1].Retrieve, probes[1].Diagnosis)
	}
}

func TestCacheProbe_ProductionSnapshot(t *testing.T) {
	ctx := context.Background()
	store := harness.OpenSnapshot(t)
	qs, err := dataset.FromStore(ctx, store)
	if err != nil {
		t.Fatalf("FromStore: %v", err)
	}
	if len(qs) == 0 {
		t.Fatal("no questions")
	}
	probes := ProbeCache(ctx, store, qs)
	html := RenderHTML(probes, nil)
	out := filepath.Join(os.TempDir(), "ora-companion-eval.html")
	if err := os.WriteFile(out, []byte(html), 0600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("wrote %s (%d questions)", out, len(probes))

	var hs, ret, tool, miss int
	for _, p := range probes {
		t.Logf("%-18s %-8s %-8s hs=%-5v ret=%-5v q=%-5v rec=%-5v  %s  %q", p.Question.ID, p.Question.Horizon, p.Question.AgeLabel, p.HandshakeHas, p.RetrieveHas, p.QueryHas, p.RecentHas, p.Diagnosis, p.Question.Ask)
		if p.HandshakeHas {
			hs++
		}
		if p.RetrieveHas {
			ret++
		}
		if p.QueryHas || p.RecentHas {
			tool++
		}
		if !p.HandshakeHas && !p.RetrieveHas && !p.QueryHas && !p.RecentHas {
			miss++
		}
	}
	t.Logf("handshake %d / retrieve %d / tools %d / miss %d  (of %d)", hs, ret, tool, miss, len(probes))
	if hs == 0 {
		t.Fatal("expected at least the current-work question to hit handshake on production working_state")
	}
}

func TestVoiceVsText_LiveAPI(t *testing.T) {
	if os.Getenv("ORA_EVALS") != "1" {
		t.Skip("set ORA_EVALS=1 to run live voice vs text API evals")
	}
	loadDotenv()
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set")
	}

	ctx := context.Background()
	store := harness.OpenSnapshot(t)
	qs, err := dataset.LoadQuestions()
	if err != nil {
		t.Fatal(err)
	}
	qs = pickDiagnostic(qs, evalLimit())
	qs, err = dataset.Stamp(ctx, store, qs)
	if err != nil {
		t.Fatal(err)
	}

	a := agent.NewAgent(nil, nil, store, nil, key)
	probes := ProbeCache(ctx, store, qs)
	pairs := make([]Pair, 0, len(qs))

	for _, q := range qs {
		t.Logf("--- %s [%s %s]: %s", q.ID, q.Horizon, q.AgeLabel, q.Ask)
		pair := Pair{}

		voiceCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		voice, verr := a.AskVoice(voiceCtx, q.Ask)
		cancel()
		if verr != nil {
			t.Logf("voice error: %v", verr)
		}
		pair.Voice = voice
		pair.VoiceScore = Grade(q, voice)
		t.Logf("voice answerHit=%v timeOK=%v slip=%v tools=%v thoughts=%d dur=%s note=%s answer=%q", pair.VoiceScore.AnswerHit, pair.VoiceScore.TimeOK, pair.VoiceScore.PresentSlip, pair.VoiceScore.ToolNames, len(voice.Thoughts), voice.Duration, pair.VoiceScore.RecencyNote, clip(voice.Answer, 400))
		for i, th := range voice.Thoughts {
			t.Logf("voice thought[%d]: %s", i, clip(th, 240))
		}

		textCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		text, terr := a.AskText(textCtx, q.Ask)
		cancel()
		if terr != nil {
			t.Logf("text error: %v", terr)
		}
		pair.Text = text
		pair.TextScore = Grade(q, text)
		t.Logf("text  answerHit=%v tools=%v thoughts=%d dur=%s answer=%q", pair.TextScore.AnswerHit, pair.TextScore.ToolNames, len(text.Thoughts), text.Duration, clip(text.Answer, 180))
		for i, th := range text.Thoughts {
			t.Logf("text thought[%d]: %s", i, clip(th, 240))
		}

		pairs = append(pairs, pair)
	}

	html := RenderHTML(probes, pairs)
	out := filepath.Join(os.TempDir(), "ora-companion-eval.html")
	if err := os.WriteFile(out, []byte(html), 0600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("wrote %s", out)

	var voiceHits, textHits int
	for _, p := range pairs {
		if p.VoiceScore.AnswerHit {
			voiceHits++
		}
		if p.TextScore.AnswerHit {
			textHits++
		}
	}
	t.Logf("voice %d/%d  text %d/%d", voiceHits, len(pairs), textHits, len(pairs))
}

func TestQuestionsFromStore_UsesWorkingState(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)
	if err := store.SetWorkingState(ctx, "Driving Climate Risk Statement Builder ASRS for Opal HealthCare."); err != nil {
		t.Fatal(err)
	}
	qs, err := dataset.FromStore(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	var hasCurated, hasAuto bool
	for _, q := range qs {
		if q.ID == "current-work" {
			hasCurated = true
			if q.Horizon != dataset.HorizonNow {
				t.Fatalf("current-work horizon=%s want now", q.Horizon)
			}
		}
		if q.ID == "auto-now" {
			hasAuto = true
			if len(q.Expect) == 0 {
				t.Fatal("auto-now has no expect tokens")
			}
		}
	}
	if !hasCurated || !hasAuto {
		t.Fatalf("expected current-work and auto-now, got %#v", qs)
	}
}

func loadDotenv() {
	_ = godotenv.Load()
	if root := repoRoot(); root != "" {
		_ = godotenv.Load(filepath.Join(root, ".env"))
	}
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func evalLimit() int {
	n, _ := strconv.Atoi(os.Getenv("ORA_EVALS_LIMIT"))
	return n
}

// pickDiagnostic prefers one question per failure mode (handshake hit, retrieve-inject, tools-only, store-miss) so a short live run still covers the cache hypothesis instead of the first N climate questions.
func pickDiagnostic(qs []dataset.Question, n int) []dataset.Question {
	prefer := []string{"current-work", "pune", "tui", "suits", "nietzsche", "tdd", "scrapped-pet", "just-now"}
	byID := make(map[string]dataset.Question, len(qs))
	for _, q := range qs {
		byID[q.ID] = q
	}
	var out []dataset.Question
	seen := map[string]bool{}
	for _, id := range prefer {
		q, ok := byID[id]
		if !ok || seen[id] {
			continue
		}
		out = append(out, q)
		seen[id] = true
	}
	for _, q := range qs {
		if seen[q.ID] {
			continue
		}
		out = append(out, q)
	}
	if n > 0 && n < len(out) {
		return out[:n]
	}
	return out
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
