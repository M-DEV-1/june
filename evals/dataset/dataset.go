// dataset holds eval fixtures: retrieval facts (seeded into an empty store) and companion questions (asked of a production snapshot).
package dataset

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"ora/internal/db"
)

//go:embed facts.json questions.json
var files embed.FS

// Probe = one query + the substrings we expect to see back (case-insensitive).
type Probe struct {
	Query          string   `json:"query"`
	ExpectContains []string `json:"expect_contains"`
}

// Fact = one real thing ORA should remember, used by the retrieval-tool scorecard.
type Fact struct {
	ID            string  `json:"id"`
	SourceSession string  `json:"source_session"`
	Kind          string  `json:"kind"` // "note" or "thread"
	Content       string  `json:"content"`
	Probes        []Probe `json:"probes"`
}

// Question is one companion prompt grounded in the user's own sqlite, asked of both the voice Live API and the text GenerateContent API.
type Question struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"` // identity | current | timeline | thread | synthesis | negative | specific
	Ask      string    `json:"ask"`
	Expect   []string  `json:"expect"`
	Source   string    `json:"source,omitempty"`
	Horizon  string    `json:"horizon,omitempty"` // durable | now | today | recent | stale — filled by Stamp from sqlite time, not wall-clock
	AsOf     time.Time `json:"-"`
	AgeLabel string    `json:"-"`
}

// Load reads facts.json (embedded at build time) into []Fact.
func Load() ([]Fact, error) {
	raw, err := files.ReadFile("facts.json")
	if err != nil {
		return nil, err
	}
	var facts []Fact
	if err := json.Unmarshal(raw, &facts); err != nil {
		return nil, err
	}
	return facts, nil
}

// LoadQuestions reads the curated companion questions (grounded in this machine's production notes/threads/working state).
func LoadQuestions() ([]Question, error) {
	raw, err := files.ReadFile("questions.json")
	if err != nil {
		return nil, err
	}
	var qs []Question
	if err := json.Unmarshal(raw, &qs); err != nil {
		return nil, err
	}
	return qs, nil
}

// FromStore appends questions derived from the live sqlite (working state, live threads, recent moments) onto the curated set. IDs already present in curated are not duplicated.
func FromStore(ctx context.Context, store *db.Store) ([]Question, error) {
	curated, err := LoadQuestions()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(curated))
	for _, q := range curated {
		seen[q.ID] = true
	}
	out := append([]Question{}, curated...)

	if state, err := store.GetWorkingState(ctx); err == nil && strings.TrimSpace(state) != "" {
		id := "auto-now"
		if !seen[id] {
			expect := distinctive(state, 6)
			if len(expect) > 0 {
				out = append(out, Question{
					ID:     id,
					Kind:   "current",
					Ask:    "What am I working on right now?",
					Expect: expect,
					Source: "working_state",
				})
				seen[id] = true
			}
		}
	}

	clock, err := store.LatestMemoryTime(ctx)
	if err != nil {
		return out, fmt.Errorf("store clock: %w", err)
	}
	if clock.IsZero() {
		clock = time.Now()
	}

	threads, err := store.GetLiveThreads(ctx, 8)
	if err != nil {
		return out, fmt.Errorf("live threads: %w", err)
	}
	for _, th := range threads {
		id := fmt.Sprintf("auto-thread-%d", th.ID)
		if seen[id] || strings.TrimSpace(th.Subject) == "" {
			continue
		}
		if !th.LastSeen.IsZero() && clock.Sub(th.LastSeen) > recentWindow {
			continue
		}
		expect := distinctive(th.State, 3)
		if len(expect) == 0 {
			expect = distinctive(th.Subject, 2)
		}
		if len(expect) == 0 {
			continue
		}
		out = append(out, Question{
			ID:     id,
			Kind:   "thread",
			Ask:    fmt.Sprintf("What's the latest on %s?", th.Subject),
			Expect: expect,
			Source: fmt.Sprintf("thread:%d", th.ID),
		})
		seen[id] = true
	}

	episodes, err := store.ListEpisodes(ctx, db.EpisodeQuery{Limit: 5, NewestFirst: true})
	if err != nil {
		return out, fmt.Errorf("recent episodes: %w", err)
	}
	if len(episodes) > 0 {
		id := "auto-recent"
		if !seen[id] {
			e := episodes[0]
			blob := strings.TrimSpace(e.UserActivity + " " + e.Title + " " + e.App)
			expect := distinctive(blob, 3)
			if len(expect) > 0 {
				out = append(out, Question{
					ID:     id,
					Kind:   "timeline",
					Ask:    "What was I just looking at?",
					Expect: expect,
					Source: fmt.Sprintf("episode:%d", e.ID),
				})
			}
		}
	}
	return Stamp(ctx, store, out)
}

var stopwords = map[string]bool{
	"about": true, "after": true, "along": true, "also": true, "been": true, "being": true,
	"from": true, "have": true, "into": true, "just": true, "like": true, "that": true,
	"their": true, "them": true, "this": true, "with": true, "your": true, "what": true,
	"when": true, "where": true, "which": true, "while": true, "user": true, "using": true,
	"currently": true, "reviewing": true, "viewing": true, "reading": true, "working": true,
	"actively": true, "focused": true, "alongside": true, "recent": true, "activity": true,
}

// distinctive returns up to n content-bearing tokens from s, longest first, for use as expect_contains needles. Input: free text. Output: lowercased tokens of length >= 4 that aren't stopwords.
func distinctive(s string, n int) []string {
	seen := map[string]bool{}
	var toks []string
	var buf strings.Builder
	flush := func() {
		w := strings.ToLower(buf.String())
		buf.Reset()
		if len(w) < 4 || stopwords[w] || seen[w] {
			return
		}
		seen[w] = true
		toks = append(toks, w)
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	// longest first so "tingira" beats leftover short fragments if we later cap harder
	for i := 0; i < len(toks); i++ {
		for j := i + 1; j < len(toks); j++ {
			if len(toks[j]) > len(toks[i]) {
				toks[i], toks[j] = toks[j], toks[i]
			}
		}
	}
	if n > 0 && len(toks) > n {
		toks = toks[:n]
	}
	return toks
}
