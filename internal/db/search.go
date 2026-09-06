package db

import (
	"context"
	"encoding/json"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"math"
	"ora/internal/obs"
	"ora/internal/util"
	"sort"
	"strings"
	"time"
	"unicode"
)

// MemoryHit is one FTS5 row — either a summary or a note.
type MemoryHit struct {
	Content   string
	Source    string // "episode" | "summary" | "digest" | "note" | "thread"
	RefID     int64
	Domain    string    // work | personal | "" — set when known
	CreatedAt time.Time // zero if unknown
	App       string    // episode framing; empty for non-episodes
	Title     string
	ImagePath string // relative JPEG path when vision captured a frame
}

// excerptContent caps content to maxRunes (defaulting to maxEpisodeExcerpt when maxRunes <= 0) — shared by FormatHit and FormatNoteHit so every caller excerpts identically instead of each re-implementing the rune cap.
func excerptContent(content string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = maxEpisodeExcerpt
	}
	return util.Runes(content, maxRunes)
}

// FormatHit renders a hit for the model in the established tool/inject shape:
//
//	[note] …
//	[summary] …
//	[episode] App — Title: …
//
// Source labels stay as stored (episode/note/summary/…) — kind-aware ranking happens in HybridSearch/RankedEpisodes, not in the prefix.
// Episode hits carry App/Title provenance (which window/app the capture came from) — without it the model can't tell two episodes from different, unrelated projects apart, which is exactly how cross-project confabulation happens (see systemInstructionText's synthesis-rule comment, WP12 Part C). Other sources are left as they were: threads/summaries/digests already carry their own framing (subject, kind) baked into content itself.
// Content is excerpted for inject budget; full content still lives in the DB.
func FormatHit(h MemoryHit, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = excerptBudget(h.Source)
	}
	content := excerptContent(h.Content, maxRunes)
	if h.Source == "note" {
		return fmt.Sprintf("[note] %s", content)
	}
	src := h.Source
	if src == "" {
		src = "unknown"
	}
	// Threads carry their ref id for the same reason notes do: a thread's summary can be wrong, the model can see that it is, and revise needs an id to name. Nothing else here has a repair tool.
	if h.Source == "thread" && h.RefID > 0 {
		src = fmt.Sprintf("thread#%d", h.RefID)
	}
	label := src
	// The age carries the calendar date past the first day, because "which day was that" is a question the rows themselves have to be able to answer — a row that only says "4d ago" makes the model do date arithmetic it reliably gets wrong out loud.
	if age := formatRelativeAge(h.CreatedAt); age != "" {
		if time.Since(h.CreatedAt) >= 24*time.Hour {
			// The clock time rides along with the date. Without it a whole class of question cannot be answered from any number of rows: "when do I usually stop working" was refused by every model arm while the rows in front of them plainly described wrapping up for the night, because each one said only "Fri Aug 28" and the hour had been dropped here.
			age = h.CreatedAt.Local().Format("Mon Jan 2 15:04") + ", " + age
		}
		label = fmt.Sprintf("%s (%s)", src, age)
	}
	// Content leads and the window provenance trails in a parenthetical: the model reads the row left to right, so what it should say comes first and the machine names it should not say come last (taste audit T1 — the provenance still guards cross-project confabulation, it just stops being the headline).
	if h.Source == "episode" && (h.App != "" || h.Title != "") {
		line := fmt.Sprintf("[%s] %s (%s — %s)", label, content, h.App, h.Title)
		if h.ImagePath != "" {
			line += " [img]"
		}
		return line
	}
	return fmt.Sprintf("[%s] %s", label, content)
}

// formatRelativeAge renders how long ago t was, in the coarsest unit that's still informative ("3d ago", "5h ago", "2m ago") — "" for a zero/unknown time, so FormatHit can skip the suffix entirely rather than print a meaningless age.
func formatRelativeAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	age := time.Since(t)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Hour:
		m := int(age.Minutes())
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("%dm ago", m)
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

// EvidenceSource is a stable pointer back to the stored row one hit line came from: which kind of row (mirrors MemoryHit.Source — "note", "episode", "summary", "thread", …), its own row id, a title when the row has one, and when it happened (RFC3339, "" if unknown). A caller can show this to the user as the evidence behind an answer, or use it to look the row back up.
type EvidenceSource struct {
	Kind  string `json:"kind"`
	ID    int64  `json:"id"`
	Title string `json:"title"`
	When  string `json:"when"`
}

// evidenceSourceOf builds the EvidenceSource for one hit straight from its own fields.
func evidenceSourceOf(h MemoryHit) EvidenceSource {
	when := ""
	if !h.CreatedAt.IsZero() {
		when = h.CreatedAt.Format(time.RFC3339)
	}
	return EvidenceSource{Kind: h.Source, ID: h.RefID, Title: h.Title, When: when}
}

// withSourceTag appends a machine-parseable `{"source":{...}}` suffix to an already-rendered hit line, so a caller can trace the line back to the stored row it came from without re-querying. Falls back to the line unchanged if marshalling somehow fails — the tag is provenance, not the content, so its absence must not break the line it was decorating.
func withSourceTag(line string, h MemoryHit) string {
	tag, err := json.Marshal(struct {
		Source EvidenceSource `json:"source"`
	}{evidenceSourceOf(h)})
	if err != nil {
		return line
	}
	return line + " " + string(tag)
}

// FormatHitWithSource is FormatHit plus a source tag (see withSourceTag) — used by tool results that feed the caller's Evidence trail (query_memory, recall). Plain FormatHit stays untouched for context that can reach a spoken reply (handshake inject, focus lookup), which must never carry raw JSON into what gets read aloud.
func FormatHitWithSource(h MemoryHit, maxRunes int) string {
	return withSourceTag(FormatHit(h, maxRunes), h)
}

// FormatNoteHitWithSource is FormatNoteHit plus a source tag (see withSourceTag), for the same reason FormatHitWithSource exists.
func FormatNoteHitWithSource(h MemoryHit, maxRunes int) string {
	return withSourceTag(FormatNoteHit(h, maxRunes), h)
}

// FormatNoteHit renders a note hit with its ref_id in the "[note#N] …" shape — notes are the only source with a revise follow-up tool, so a caller (query_memory) needs the id in hand to act on a correction. Content is excerpted identically to FormatHit.
func FormatNoteHit(h MemoryHit, maxRunes int) string {
	if maxRunes <= 0 {
		// Same budget FormatHit gives a note, and for the same reason. query_memory routes note hits here and everything else to FormatHit, so leaving this on the default meant meeting minutes reached the live agent at 200 runes while the eval — which formats every source through FormatHit — reported the excerpt fix as working.
		maxRunes = excerptBudget(h.Source)
	}
	return fmt.Sprintf("[note#%d] %s", h.RefID, excerptContent(h.Content, maxRunes))
}

// ftsStopwords are short function words dropped when tokenizing a query for buildFTSMatch — they carry no retrieval signal and just add noise to the OR-matched term set.
var ftsStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"were": true, "what": true, "when": true, "where": true, "did": true,
	"do": true, "does": true, "i": true, "you": true, "my": true, "your": true,
	"it": true, "its": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "that": true, "this": true,
}

// ftsQuotePhrase wraps a single FTS5 phrase in quotes, doubling any embedded quotes so it can't break out of the phrase.
func ftsQuotePhrase(phrase string) string {
	return `"` + strings.ReplaceAll(phrase, `"`, `""`) + `"`
}

// tokenizeQuery splits query into significant terms: whitespace/punctuation-separated, lowercased, deduped, with short function words (ftsStopwords) dropped — they carry no retrieval signal. Shared by buildFTSMatch (which ORs the terms into an FTS5 MATCH expression) and HybridSearch's lexical relevance floor (which counts how many of them a candidate's content actually contains).
func tokenizeQuery(query string) []string {
	raw := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := make(map[string]bool, len(raw))
	var terms []string
	for _, term := range raw {
		term = strings.ToLower(term)
		if term == "" || ftsStopwords[term] || seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
	}
	return terms
}

// buildFTSMatch tokenizes query (see tokenizeQuery) and joins the terms into an FTS5 OR-of-terms MATCH expression (e.g. `"foo" OR "bar"`). Natural-language questions rarely share a verbatim contiguous phrase with stored prose, so matching on any significant term recalls far better than treating the whole query as one phrase.
// Falls back to the original whole-query phrase when tokenization leaves no terms (e.g. an all-stopword query), so the call never MATCHes on an empty/invalid expression.
func buildFTSMatch(query string) string {
	terms := tokenizeQuery(query)
	if len(terms) == 0 {
		return ftsQuotePhrase(query)
	}
	parts := make([]string, len(terms))
	for i, term := range terms {
		parts[i] = ftsQuotePhrase(term)
	}
	return strings.Join(parts, " OR ")
}

// SearchMemory runs FTS5 over summaries + notes. Returns top 10 by rank.
// Empty query -> empty result, no error.
func (s *Store) SearchMemory(ctx context.Context, query string) ([]MemoryHit, error) {
	return s.searchMemoryWindow(ctx, query, "", time.Time{}, time.Time{}, 10)
}

// sqliteUTC renders t the way every timestamp column in this store is written (UTC "YYYY-MM-DD HH:MM:SS"), so bound parameters compare correctly against stored values.
func sqliteUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// ftsRowTime is the SQL expression for a memory_fts row's timestamp — the fts table itself carries none, so it is looked up in the row's source table: notes.created_at, threads.last_seen_at (a thread's meaningful time is when it was last touched — same choice hitCreatedAt makes), or nodes.created_at for summary/digest. NULL for a dangling ref, which any comparison then excludes.
const ftsRowTime = `(CASE source
	WHEN 'note' THEN (SELECT created_at FROM notes WHERE id = ref_id)
	WHEN 'thread' THEN (SELECT last_seen_at FROM threads WHERE id = ref_id)
	WHEN 'diary' THEN (SELECT updated_at FROM diary WHERE id = ref_id)
	ELSE (SELECT created_at FROM nodes WHERE id = ref_id)
END)`

// searchMemoryWindow is SearchMemory constrained to rows whose timestamp falls in [since, until] and, when source is not "", to that one source; limit caps the rows returned. Both the window and the source are part of the WHERE clause, before the LIMIT, so a sparse window or a single-source caller still yields its rows instead of being crowded out by rows that rank higher — filtering after a cross-source LIMIT 10 returned nothing whenever ten other rows outranked the best note.
func (s *Store) searchMemoryWindow(ctx context.Context, query, source string, since, until time.Time, limit int) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SearchMemory")
	defer span.End()

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// MATCH wants tokens, not one whole-query phrase; OR the significant terms.
	safe := buildFTSMatch(query)

	where := "memory_fts MATCH ?"
	args := []any{safe}
	if source != "" {
		where += " AND source = ?"
		args = append(args, source)
	}
	if !since.IsZero() {
		where += " AND " + ftsRowTime + " >= ?"
		args = append(args, sqliteUTC(since))
	}
	if !until.IsZero() {
		where += " AND " + ftsRowTime + " <= ?"
		args = append(args, sqliteUTC(until))
	}

	// Summary rows carry the whole marshalled TaskSummary in content (see LogSemanticNode), so the plain summary text is pulled back out here — the model reads a hit's Content verbatim and a raw JSON blob is unreadable. Digest rows and any summary whose content isn't JSON fall through unchanged.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			CASE WHEN source IN ('summary','digest') AND json_valid(content)
				THEN IFNULL(NULLIF(json_extract(content, '$.summary'), ''), content)
				ELSE content
			END,
			source, ref_id
		FROM memory_fts
		WHERE `+where+`
		ORDER BY rank
		LIMIT ?
	`, append(args, limit)...)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fts5 search: %w", err)
	}
	defer rows.Close()

	var out []MemoryHit
	for rows.Next() {
		var h MemoryHit
		if err := rows.Scan(&h.Content, &h.Source, &h.RefID); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan fts5 row: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate fts5 rows: %w", err)
	}
	rows.Close()

	// A second pass, after the FTS cursor above is fully closed — a follow-up query issued while it's still open can silently fail depending on connection-pool state, leaving CreatedAt at its zero value.
	for i := range out {
		out[i].CreatedAt = s.hitCreatedAt(ctx, out[i].Source, out[i].RefID)
	}

	span.SetAttributes(attribute.Int("db.search_results", len(out)))
	return out, nil
}

// hitCreatedAt looks up when a memory_fts-backed hit's row was created — memory_fts itself carries no timestamp column, so this is a follow-up query per row (bounded: SearchMemory caps at 10 rows). "thread" and any other unrecognized source return the zero value; FormatHit already treats a zero CreatedAt as "no age known."
func (s *Store) hitCreatedAt(ctx context.Context, source string, refID int64) time.Time {
	var created string
	switch source {
	case "summary", "digest":
		_ = s.db.QueryRowContext(ctx, `SELECT created_at FROM nodes WHERE id = ?`, refID).Scan(&created)
	case "note":
		_ = s.db.QueryRowContext(ctx, `SELECT created_at FROM notes WHERE id = ?`, refID).Scan(&created)
	case "thread":
		// last_seen_at, not created_at: a thread's age that matters is when it was last touched, which is also what MemoryAsOf reports for one.
		_ = s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM threads WHERE id = ?`, refID).Scan(&created)
	case "diary":
		// updated_at, not created_at: the understanding doc is one row rewritten in place, and its meaningful age is the last rewrite.
		_ = s.db.QueryRowContext(ctx, `SELECT updated_at FROM diary WHERE id = ?`, refID).Scan(&created)
	default:
		return time.Time{}
	}
	return parseSQLiteTime(created)
}

// maxEpisodeExcerpt caps how much of an episode's screen_text is surfaced in RetrieveRelevant output — this is context meant to orient the model, not a full transcript.
const maxEpisodeExcerpt = 200

// A hit's excerpt is budgeted by what kind of thing it is, because these sources differ in length by more than an order of magnitude and one cap cannot serve them all.
// maxNoteExcerpt covers a document. Meeting minutes average 4,400 characters and run to 7,300; the cap that used to apply to them was 200, which showed the model a heading and the first two attendees while the answer sat in the body — a question about a demo failed on every model arm with the right document retrieved three times, because the word never crossed into view.
// maxSummaryExcerpt covers a paragraph. Task summaries average about 350 characters, so 200 was cutting roughly two fifths off a typical one, and threads run to 330.
const (
	maxNoteExcerpt    = 4000
	maxSummaryExcerpt = 700
)

// excerptBudget is how many runes one hit of this source is worth showing when the caller has no opinion. Episodes keep the original cap: a screen capture is a window title and a scrap of text, and 200 runes is genuinely enough for one.
// ponytail: a fixed budget per source, not a budget shared across the whole result set. Ten note hits at the note cap is a large prompt; if that ever bites, the fix is to divide a total allowance across the hits actually returned rather than to shrink this constant back down.
func excerptBudget(source string) int {
	switch source {
	case "note", "diary":
		return maxNoteExcerpt
	case "summary", "digest", "thread":
		return maxSummaryExcerpt
	default:
		return maxEpisodeExcerpt
	}
}

// Ranking weights for RankedEpisodes. All three terms are min-max normalized to [0,1] across the candidate set before weighting, so these are directly comparable "how much each signal counts" knobs. Equal by default (combined score ranges [0,3]) — tune here if one signal should dominate.
const (
	rankWeightRecency    = 1.0
	rankWeightImportance = 1.0
	rankWeightRelevance  = 1.0
)

// recencyHalfLifeFactor is the per-hour exponential decay base for recency scoring: recency = recencyHalfLifeFactor^hoursSinceCreated. Closer to 1.0 means slower decay (0.995 ≈ half over ~138 hours / ~5.75 days).
const recencyHalfLifeFactor = 0.995

// rankCandidatePoolSize bounds how many FTS matches are pulled from episodes_fts before re-ranking in Go — wider than the final `limit` so the weighted formula, not raw FTS rank alone, decides the final order.
const rankCandidatePoolSize = 50

// rankedEpisodeCandidate holds the raw per-episode signals needed to compute a combined ranking score before normalization.
type rankedEpisodeCandidate struct {
	id         int64
	content    string
	createdAt  time.Time
	importance float64
	bm25       float64 // raw FTS5 bm25 score; more negative = better match
}

// RankedEpisodes scores episodes matching focus by a weighted blend of recency, importance, and FTS relevance, and returns the top `limit` as MemoryHit (Source="episode"). Each term is min-max normalized to [0,1] across the candidate pool before weighting:
//
//	score = rankWeightRecency*recency + rankWeightImportance*importance + rankWeightRelevance*relevance
//
// recency = recencyHalfLifeFactor^hoursSinceCreated (higher = more recent), importance is the stored episodes.importance column, and relevance is the FTS5 bm25 score inverted (bm25 is "lower is better") then normalized so the best match in the pool scores 1.0.
func (s *Store) RankedEpisodes(ctx context.Context, focus string, limit int) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.RankedEpisodes")
	defer span.End()

	focus = strings.TrimSpace(focus)
	if focus == "" || limit <= 0 {
		return nil, nil
	}
	// Tokenised and ORed like every other query in this file, not quoted as one contiguous phrase: a natural-language subject rarely appears verbatim in stored prose, so "Riddler project" found nothing while "Riddler" found the episode.
	safe := buildFTSMatch(focus)

	rows, err := s.db.QueryContext(ctx, `
		SELECT episodes.id, episodes.screen_text, episodes.created_at, episodes.importance,
		       episodes.app, episodes.title, episodes.domain, bm25(episodes_fts)
		FROM episodes_fts
		JOIN episodes ON episodes.id = episodes_fts.rowid
		WHERE episodes_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, safe, rankCandidatePoolSize)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("fts5 ranked episode candidates: %w", err)
	}
	defer rows.Close()

	type rankedRow struct {
		rankedEpisodeCandidate
		app, title, domain string
	}
	var candidates []rankedRow
	for rows.Next() {
		var c rankedRow
		if err := rows.Scan(&c.id, &c.content, &c.createdAt, &c.importance, &c.app, &c.title, &c.domain, &c.bm25); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan ranked episode candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate ranked episode candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	now := time.Now()
	recencyRaw := make([]float64, len(candidates))
	relevanceRaw := make([]float64, len(candidates))
	for i, c := range candidates {
		hours := now.Sub(c.createdAt).Hours()
		if hours < 0 {
			hours = 0
		}
		recencyRaw[i] = math.Pow(recencyHalfLifeFactor, hours)
		relevanceRaw[i] = -c.bm25 // invert: higher = more relevant
	}

	recencyNorm := minMaxNormalize(recencyRaw)
	relevanceNorm := minMaxNormalize(relevanceRaw)
	importanceRaw := make([]float64, len(candidates))
	for i, c := range candidates {
		importanceRaw[i] = c.importance
	}
	importanceNorm := minMaxNormalize(importanceRaw)

	type scored struct {
		hit   MemoryHit
		score float64
	}
	results := make([]scored, len(candidates))
	for i, c := range candidates {
		results[i] = scored{
			hit: MemoryHit{
				Content:   c.content,
				Source:    "episode",
				RefID:     c.id,
				App:       c.app,
				Title:     c.title,
				Domain:    c.domain,
				CreatedAt: c.createdAt,
			},
			score: rankWeightRecency*recencyNorm[i] + rankWeightImportance*importanceNorm[i] + rankWeightRelevance*relevanceNorm[i],
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if limit < len(results) {
		results = results[:limit]
	}

	out := make([]MemoryHit, len(results))
	for i, r := range results {
		out[i] = r.hit
	}
	span.SetAttributes(attribute.Int("db.ranked_episode_count", len(out)))
	return out, nil
}

// minMaxNormalize scales vals to [0,1]. When all values are equal (max==min), every element normalizes to 1.0 so that term contributes its full weight uniformly rather than collapsing the whole score to 0.
func minMaxNormalize(vals []float64) []float64 {
	out := make([]float64, len(vals))
	if len(vals) == 0 {
		return out
	}
	min, max := vals[0], vals[0]
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if max == min {
		for i := range out {
			out[i] = 1.0
		}
		return out
	}
	for i, v := range vals {
		out[i] = (v - min) / (max - min)
	}
	return out
}

// RetrieveRelevant returns up to maxItems relevance-ranked lines for unsolicited inject. Uses HybridSearch so moments get recency decay, facts/periods do not, and every line is FormatHit (content + context).
// If maxItems <= 0, defaults to 4 (stingy inject budget). An empty focus returns nil directly — there's no real query to run relevance search against, and substituting a placeholder phrase would just search for those literal words.
func (s *Store) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	focus = strings.TrimSpace(focus)
	if focus == "" {
		return nil, nil
	}
	if maxItems <= 0 {
		maxItems = 4
	}
	hits, err := s.HybridSearch(ctx, focus, "", maxItems)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		// Budget 0 means "let excerptBudget decide per source", the same call formatFocusHits and query_memory make. Passing maxEpisodeExcerpt here cut a note to 200 runes on the path that injects memory into every turn, which showed the model a heading where the answer was in the body.
		out = append(out, FormatHit(h, 0))
	}
	return out, nil
}

func (s *Store) GetImplicitContext(ctx context.Context) ([]string, error) {
	// init tracer to db module
	tracer := obs.GetTracer(ctx, "ora.db")

	// starts the span, and will inherit a trace id from context
	ctx, span := tracer.Start(ctx, "GetImplicitContext")
	defer span.End()

	// recursive common table expression, kind of like a while loop
	// we grab the last 5 summaries and walk them up to the root (user/day/session)
	query := `WITH RECURSIVE branch AS (
		-- Anchor: The last 5 summaries under the current session
		SELECT * FROM (
			SELECT id, parent_id, type, content 
			FROM nodes 
			WHERE type = 'summary' AND parent_id IN (
				SELECT id FROM nodes WHERE parent_id = ? AND type = 'task'
			)
			ORDER BY id DESC LIMIT 5
		)
		
		UNION ALL	
		
		-- recursively walk up the tree
		SELECT n.id, n.parent_id, n.type, n.content 
		FROM nodes n
		JOIN branch b ON n.id = b.parent_id
	)
	-- DISTINCT to avoid repeating common ancestors (Session, Day, User)
	SELECT DISTINCT type, content FROM branch ORDER BY id ASC`

	s.mu.RLock()
	parentID := s.currentParentID
	s.mu.RUnlock()

	var branch []string
	// Identity notes are NOT dumped unconditionally anymore — a wall of generic "[about] user likes X" facts buried the useful live threads. Notes now surface only through relevance retrieval below, gated by what the user is actually doing now: build a focus signal from working state + recent task nodes, then surface only the notes/summaries/threads that match it.
	workingState, workingStateErr := s.GetWorkingState(ctx)
	var focusSignal string
	if workingStateErr == nil && workingState != "" {
		focusSignal = workingState
	}
	// Bounded by recency, not just by id: an old task is still "last 2 by id" in a lightly-used store, and folding its name into focusSignal would self-match its own summary back into context indefinitely regardless of whether it's actually current.
	recentSince := time.Now().Add(-recentTaskWindow).UTC().Format("2006-01-02 15:04:05")
	if trows, err := s.db.QueryContext(ctx, `SELECT content FROM nodes WHERE type='task' AND created_at >= ? ORDER BY id DESC LIMIT 2`, recentSince); err == nil && trows != nil {
		for trows.Next() {
			var tsk string
			_ = trows.Scan(&tsk)
			if tsk != "" {
				focusSignal += " " + tsk
			}
		}
		trows.Close()
	}
	// A cold-start store (no working state yet, no recent task) has no real focus signal — skip relevance search entirely instead of running it against a placeholder phrase (RetrieveRelevant now also returns nil on empty focus, but the point here is to never construct a fake non-empty one in the first place).
	if focusSignal = strings.TrimSpace(focusSignal); focusSignal != "" {
		const maxRel = 6
		if rel, rerr := s.RetrieveRelevant(ctx, focusSignal, maxRel); rerr == nil {
			branch = append(branch, rel...)
		}
	}
	// live threads: what's going on in their life right now (recency = relevance). Concurrent threads coexist here — watching + coding both surface.
	threads, terr := s.GetLiveThreads(ctx, 6)
	if terr == nil {
		for _, t := range threads {
			if t.State != "" {
				branch = append(branch, fmt.Sprintf("[thread:%s] %s — %s", t.Kind, t.Subject, t.State))
			} else {
				branch = append(branch, fmt.Sprintf("[thread:%s] %s", t.Kind, t.Subject))
			}
		}
	}
	// synthesized "right now" (reuses the working-state fetched above for focusSignal — nothing between there and here mutates it).
	if workingStateErr == nil && workingState != "" {
		branch = append(branch, fmt.Sprintf("[now] %s", workingState))
	}
	if len(branch) > 0 {
		span.SetAttributes(attribute.Int("db.node_count", len(branch)))
		return branch, nil
	}

	// fallback (cold start, nothing synthesized yet): existing recursive summary walk.
	rows, err := s.db.QueryContext(ctx, query, parentID)

	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("failed to query context: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var nodeType, content string

		if err := rows.Scan(&nodeType, &content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		nodeString := fmt.Sprintf("[%s] %s", nodeType, content)
		branch = append(branch, nodeString)
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	span.SetAttributes(attribute.Int("db.node_count", len(branch)))
	return branch, nil
}
