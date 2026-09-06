package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"ora/internal/memory"
)

// rrfK is Reciprocal Rank Fusion's smoothing constant. ORA's small corpus (thousands, not millions, of items) favors a lower k (~40-60) than the textbook default (60-100) — sharpens the gap between an item both retrievers agree on and one only a single retriever found. See the worked example in docs/ora-memory-deck.html §02.
const rrfK = 50

// domainBoostFactor multiplies the fused score of a candidate whose domain matches the inferred current domain, applied only when the caller didn't pass an explicit domain filter. A mis-tagged/unset item still surfaces, just lower — boost, not a wall.
const domainBoostFactor = 1.15

// momentRecencyHalfLife is the exponential half-life applied only to moments (episodes) after RRF. Facts/arcs/periods are not decayed — durable or tree-structured memory shouldn't fade like ambient screen observations.
const momentRecencyHalfLife = 7 * 24 * time.Hour

// rrfCandidate is one item in a pre-ranked list going into fusion.
type rrfCandidate struct {
	id        string // stable identity across lists, e.g. "episode:42", "summary:7", "note:3", "thread:1"
	content   string
	source    string // "episode" | "summary" | "note" | "thread"
	domain    string // "" if untagged
	app       string
	title     string
	createdAt time.Time

	// score is the fused RRF score, populated by reciprocalRankFusion. Lives on rrfCandidate (not a parallel slice) because HybridSearch needs to read it back per-candidate to apply domainBoostFactor / moment recency before the final sort.
	score float64
}

// reciprocalRankFusion merges any number of pre-ranked lists (rank 1 = best within that list) into one fused ranking. Every candidate's score is Σ 1/(k+rank) summed across every list it appears in (matched by id) — score, not the raw ranks, decides final order; ties broken by id for determinism. Output is sorted by fused score descending.
func reciprocalRankFusion(k int, lists ...[]rrfCandidate) []rrfCandidate {
	if len(lists) == 0 {
		return nil
	}

	merged := make(map[string]*rrfCandidate)
	var order []string // first-seen order, for a stable starting point before sort

	for _, list := range lists {
		for i, cand := range list {
			rank := i + 1
			contribution := 1.0 / float64(k+rank)

			existing, ok := merged[cand.id]
			if !ok {
				c := cand // copy; first-seen list seeds metadata
				c.score = contribution
				merged[cand.id] = &c
				order = append(order, cand.id)
				continue
			}
			existing.score += contribution
			// Fill gaps from later lists (e.g. vector hit lacks app/title that lexical has, or the reverse for created_at on vector metadata).
			if existing.domain == "" && cand.domain != "" {
				existing.domain = cand.domain
			}
			if existing.app == "" && cand.app != "" {
				existing.app = cand.app
			}
			if existing.title == "" && cand.title != "" {
				existing.title = cand.title
			}
			if existing.createdAt.IsZero() && !cand.createdAt.IsZero() {
				existing.createdAt = cand.createdAt
			}
			if existing.content == "" && cand.content != "" {
				existing.content = cand.content
			}
		}
	}

	out := make([]rrfCandidate, 0, len(order))
	for _, id := range order {
		out = append(out, *merged[id])
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})

	return out
}

// embedder and vectorIndex are the minimal interfaces Store depends on for hybrid search — defined here rather than imported from internal/embed/internal/vector so db_test.go can supply fakes without a real network/chromem dependency, and internal/db carries no import-time dependency on those packages. internal/embed.Embedder and internal/vector.Index (or *ChromemIndex) satisfy these structurally.
type embedder interface {
	Embed(ctx context.Context, task string, text string) ([]float32, error)
}
type vectorIndex interface {
	Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error
	Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]Result, error)
	Delete(ctx context.Context, id string) error
	IDs() []string
}

// Result mirrors internal/vector.Result — duplicated here rather than imported, same reason as embedder/vectorIndex above: internal/db must not gain an import-time dependency on internal/vector.
type Result struct {
	ID         string
	Content    string
	Metadata   map[string]string
	Similarity float32
}

// SetEmbedder/SetVectorIndex wire in the hybrid search layer. Both nilable (the zero value) — a Store with neither configured runs lexical-only. Optional, not constructor arguments, so existing db.New(":memory:") callers throughout db_test.go keep working unmodified.
func (s *Store) SetEmbedder(e embedder) {
	s.mu.Lock()
	s.embedder = e
	s.mu.Unlock()
}

func (s *Store) SetVectorIndex(v vectorIndex) {
	s.mu.Lock()
	s.vectorIndex = v
	s.mu.Unlock()
}

// SetEmbedsAreFree tells the Store that its embedder costs CPU rather than metered API calls (the local EmbeddingGemma engine). The only thing this changes is reconciliation's episode age window: with a metered embedder, backfill covers only the last reconcileEpisodeWindow of episodes so a dirty store can't run up a bill; with a free one, every episode that still has text is eligible, which is what brings back the old episodes that lost their vectors to API failures.
func (s *Store) SetEmbedsAreFree(free bool) {
	s.mu.Lock()
	s.embedsAreFree = free
	s.mu.Unlock()
}

// SetVectorSimilarityFloor overrides the absolute cosine floor a vector hit must clear to enter fusion. minVectorSimilarity's default was measured against Gemini's similarity range; a different embedding model scores the same genuinely-relevant documents on a different scale, and leaving the floor where it is would drop every vector candidate and reduce hybrid search to lexical-only without saying so. Values of zero or below are ignored, so the floor can never be turned off entirely.
func (s *Store) SetVectorSimilarityFloor(floor float32) {
	if floor <= 0 {
		return
	}
	s.mu.Lock()
	s.vectorSimilarityFloor = floor
	s.mu.Unlock()
}

// vectorFloor is the absolute cosine floor in force, defaulting to minVectorSimilarity when nothing has overridden it.
func (s *Store) vectorFloor() float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.vectorSimilarityFloor > 0 {
		return s.vectorSimilarityFloor
	}
	return float32(minVectorSimilarity)
}

// hybridVectorPoolSize bounds how many nearest-neighbor results are pulled from the vector index before fusion — wider than the final `limit` so RRF, not raw vector similarity alone, decides the final order.
const hybridVectorPoolSize = 50

// minVectorSimilarity is the cosine-similarity floor a vector hit must clear to enter fusion at all — chromem's Search always returns its n nearest neighbors regardless of how weakly related they actually are, so without a floor a query about a topic absent from the store still gets padded with barely-related rows. Unvalidated starting point; tune against real queries.
const minVectorSimilarity = 0.55

// vectorSimilarityBand is how far below the best hit's cosine similarity a vector hit may sit and still enter fusion. Real similarities on this store span roughly 0.58-0.69, so the absolute minVectorSimilarity floor alone drops nothing and junk rides in alongside genuine matches; the effective floor is max(minVectorSimilarity, topSimilarity-vectorSimilarityBand). Tune against real queries.
const vectorSimilarityBand = 0.06

// splitCandidateID parses an rrfCandidate/vector-result id of the form "<source>:<refID>" (e.g. "episode:42", "note:3") into its parts. refID is 0 if the numeric suffix doesn't parse cleanly (e.g. a vector-only hit whose id shape doesn't map to a nodes/episodes row) — best effort, not an error, since MemoryHit.RefID is informational.
func splitCandidateID(id string) (source string, refID int64) {
	idx := strings.LastIndex(id, ":")
	if idx < 0 {
		return id, 0
	}
	source = id[:idx]
	// A passage's id carries a "#N" suffix naming which chunk of the row it is. The row is the same one either way, and everything downstream — the domain lookup, the app/title on a formatted hit — wants the row.
	num := id[idx+1:]
	if h := strings.IndexByte(num, '#'); h >= 0 {
		num = num[:h]
	}
	refID, _ = strconv.ParseInt(num, 10, 64)
	return source, refID
}

// inPlaceholders renders the "?, ?, ?" list and the matching argument slice for an IN clause over n ids. Input: the ids. Output: the placeholder text and the ids as query arguments, so one query can replace a lookup run once per candidate.
func inPlaceholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// nodeDomains looks up the domain tag of every node named in ids with one query. Input: the node ids of the summary and digest candidates in one fused result. Output: a map from id to domain, holding only the nodes that exist and carry a non-empty domain; an empty id list runs no query at all.
// This replaces a SELECT run once per candidate: a search filling both FTS limits carries up to twenty lexical candidates, so the per-candidate form issued up to twenty round trips where one now serves.
func (s *Store) nodeDomains(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	placeholders, args := inPlaceholders(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT id, domain FROM nodes WHERE id IN (`+placeholders+`) AND domain <> ''`, args...)
	if err != nil {
		slog.Warn("hybrid search: looking up candidate domains failed, candidates stay untagged", "error", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var domain string
		if err := rows.Scan(&id, &domain); err != nil {
			continue
		}
		out[id] = domain
	}
	return out
}

// currentDomain returns the domain tag of the most recently logged episode — the store's best guess at "what domain is the user in right now", used to boost (not filter) fused results when the caller didn't pass an explicit domainFilter.
func (s *Store) currentDomain(ctx context.Context) string {
	var domain string
	_ = s.db.QueryRowContext(ctx, `SELECT domain FROM episodes ORDER BY id DESC LIMIT 1`).Scan(&domain)
	return domain
}

// lexicalTermOverlap counts how many distinct queryTerms appear (case-insensitive substring match) in content — used by HybridSearch's multi-term relevance floor to distinguish a real match from a single-term FTS5 coincidence.
func lexicalTermOverlap(content string, queryTerms []string) int {
	lower := strings.ToLower(content)
	n := 0
	for _, term := range queryTerms {
		if strings.Contains(lower, term) {
			n++
		}
	}
	return n
}

// domainTaggedSources are the hit sources whose backing table has a domain column, so a domain filter can genuinely narrow them. A note, thread or diary row has no domain at all, and filtering those on exact equality drops every one of them rather than narrowing them.
var domainTaggedSources = map[string]bool{"episode": true, "summary": true, "digest": true}

// domainMatches reports whether a candidate survives a domain filter. Input: the candidate's source and domain, and the requested domain. Output: true when the source carries no domain to filter on, or when its domain is the requested one.
func domainMatches(source, domain, filter string) bool {
	if !domainTaggedSources[source] {
		return true
	}
	return domain == filter
}

// HybridSearch fuses lexical (FTS5) and vector search over episodes, summaries, notes, and threads via reciprocalRankFusion, and returns the top `limit` as MemoryHit.
// If domainFilter is "work" or "personal", results are hard-filtered to that domain (both the lexical SQL query and the vector search's `where` clause). If domainFilter is "", no hard filter is applied, but candidates matching the store's inferred current domain (see currentDomain) get domainBoostFactor applied to their fused score before the final sort.
// If the Store has no embedder/vector index configured (both nil — the default for a bare db.New(":memory:")), HybridSearch degrades gracefully to lexical-only fusion rather than erroring.
func (s *Store) HybridSearch(ctx context.Context, query, domainFilter string, limit int) ([]MemoryHit, error) {
	return s.HybridSearchWindow(ctx, query, domainFilter, time.Time{}, time.Time{}, limit)
}

// HybridSearchWindow is HybridSearch constrained to items whose timestamp falls in [since, until]. A zero bound is open on that side; both zero means no time constraint at all.
// The window is applied inside the lexical SQL (before each FTS query's LIMIT) and to the vector candidate pool before fusion — never to the fused top-k after ranking, which for a sparse window (the whole point of a dated question) usually leaves nothing. A window containing nothing returns an honestly empty result, not a fallback to unwindowed matches.
// Under a window, a vector candidate with no readable created_at metadata is dropped: its membership in the window can't be shown. Unwindowed search keeps such candidates.
func (s *Store) HybridSearchWindow(ctx context.Context, query, domainFilter string, since, until time.Time, limit int) ([]MemoryHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	windowed := !since.IsZero() || !until.IsZero()

	// lexical candidates: reuse the existing FTS5 paths, don't reimplement.
	memHits, err := s.searchMemoryWindow(ctx, query, "", since, until, 10)
	if err != nil {
		return nil, err
	}
	episodeHits, err := s.searchEpisodesWindow(ctx, query, since, until)
	if err != nil {
		return nil, err
	}

	// domain is looked up for every lexical candidate up front (not just when domainFilter is set) — it backs both the hard-filter below and the same-domain boost applied later when domainFilter == "". Doing this only inside the filter branch would leave every lexical/FTS5 hit's domain permanently "" for the common no-filter case, silently defeating the boost.
	// Only summary and digest hits need the lookup: searchMemoryWindow does not select the nodes.domain column, while searchEpisodesWindow already selects episodes.domain, so re-reading an episode's domain could only ever return the empty string it already carries.
	var nodeIDs []int64
	for _, h := range memHits {
		if h.Domain == "" && (h.Source == "summary" || h.Source == "digest") {
			nodeIDs = append(nodeIDs, h.RefID)
		}
	}
	domains := s.nodeDomains(ctx, nodeIDs)

	lexical := make([]rrfCandidate, 0, len(memHits)+len(episodeHits))
	for _, h := range memHits {
		domain := h.Domain
		if domain == "" {
			domain = domains[h.RefID]
		}
		lexical = append(lexical, rrfCandidate{
			id:        fmt.Sprintf("%s:%d", h.Source, h.RefID),
			content:   h.Content,
			source:    h.Source,
			domain:    domain,
			createdAt: h.CreatedAt,
		})
	}
	for _, h := range episodeHits {
		lexical = append(lexical, rrfCandidate{
			id:        fmt.Sprintf("%s:%d", h.Source, h.RefID),
			content:   h.Content,
			source:    h.Source,
			domain:    h.Domain,
			app:       h.App,
			title:     h.Title,
			createdAt: h.CreatedAt,
		})
	}

	// Relevance floor for multi-term queries: buildFTSMatch ORs every significant term together, so FTS5 alone returns any row matching even one of them. A candidate matching a small fraction of a long query is coincidental noise, not a real match — this is what "10 junk rows for an absent-topic query" traces back to.
	// The floor is half the query's terms, capped at two, so it scales with the question instead of turning short queries into a strict AND: two-term queries need one term (unchanged from plain FTS5, since "ora daemon" must still find "the daemon crashed"), three or more need two.
	if queryTerms := tokenizeQuery(query); len(queryTerms) >= 3 {
		need := (len(queryTerms) + 1) / 2
		if need > 2 {
			need = 2
		}
		filtered := lexical[:0:0]
		for _, c := range lexical {
			if lexicalTermOverlap(c.content, queryTerms) >= need {
				filtered = append(filtered, c)
			}
		}
		lexical = filtered
	}

	if domainFilter != "" {
		filtered := lexical[:0:0]
		for _, c := range lexical {
			if domainMatches(c.source, c.domain, domainFilter) {
				filtered = append(filtered, c)
			}
		}
		lexical = filtered
	}

	// --- vector candidates: only if both an embedder and a vector index are wired. ---
	s.mu.RLock()
	emb := s.embedder
	vidx := s.vectorIndex
	s.mu.RUnlock()

	// An embed or vector-search failure degrades to lexical-only fusion (logged, not returned) rather than failing the whole call — with the client depending on daemon IPC for the vector half, "daemon down" must not take query_memory down with it.
	var vector []rrfCandidate
	if emb != nil && vidx != nil {
		queryVec, err := emb.Embed(ctx, "RETRIEVAL_QUERY", query)
		if err != nil {
			slog.Warn("hybrid search: embed query failed, degrading to lexical-only", "error", err)
			queryVec = nil
		}

		if queryVec != nil {
			// The domain filter is applied in Go below rather than through chromem's where clause, for the same reason the time window is: exact-match metadata would drop every note and thread, which are written with no domain key at all.
			results, err := vidx.Search(ctx, queryVec, hybridVectorPoolSize, nil)
			if err != nil {
				slog.Warn("hybrid search: vector search failed, degrading to lexical-only", "error", err)
			} else {
				// The floor is relative to the best hit in this result set, with the embedder's absolute floor as its lower bound. Computed from the results rather than assuming they arrive sorted.
				floor := s.vectorFloor()
				for _, r := range results {
					if band := r.Similarity - vectorSimilarityBand; band > floor {
						floor = band
					}
				}

				vector = make([]rrfCandidate, 0, len(results))
				for _, r := range results {
					if r.Similarity < floor {
						continue
					}
					source, _ := splitCandidateID(r.ID)
					if domainFilter != "" && !domainMatches(source, r.Metadata["domain"], domainFilter) {
						continue
					}
					createdAt := time.Time{}
					if ts := r.Metadata["created_at"]; ts != "" {
						createdAt = parseSQLiteTime(ts)
					}
					// chromem's where clause is exact-match only, so the time window is applied here on the candidate pool (hybridVectorPoolSize wide, not the final top-k).
					if windowed {
						if createdAt.IsZero() ||
							(!since.IsZero() && createdAt.Before(since)) ||
							(!until.IsZero() && createdAt.After(until)) {
							continue
						}
					}
					vector = append(vector, rrfCandidate{
						id:        r.ID,
						content:   r.Content,
						source:    source,
						domain:    r.Metadata["domain"],
						createdAt: createdAt,
					})
				}
			}
		}
	}

	var lists [][]rrfCandidate
	if len(lexical) > 0 {
		lists = append(lists, lexical)
	}
	if len(vector) > 0 {
		lists = append(lists, vector)
	}
	// Collapsed to one hit per row before anything downstream spends its budget: a screen matching in three passages is three vectors but one moment, and the ten rows the model gets should cover ten moments.
	fused := bestPassagePerRow(reciprocalRankFusion(rrfK, lists...))
	// A summary whose day was already rolled into a digest survives compaction (ReplaceSummariesWithDigest reparents rather than deletes it), so it can still surface here alongside its own digest — same day's content counted twice. Drop it when its parent digest is also in this result set; keep it when the digest isn't, since then it's the only record of that day.
	fused = s.dropSummariesShadowedByDigest(ctx, fused)

	// Kind-aware score shaping after fusion:
	//  - same-domain soft boost when no explicit domain filter
	//  - exponential recency decay ONLY for moments (episodes)
	//  - facts / arcs / periods keep fused score (tree + durable lanes)
	now := time.Now()
	needRescore := false
	if domainFilter == "" {
		if current := s.currentDomain(ctx); current != "" {
			for i := range fused {
				if fused[i].domain == current {
					fused[i].score *= domainBoostFactor
					needRescore = true
				}
			}
		}
	}
	for i := range fused {
		if memory.KindOf(fused[i].source) != memory.KindMoment {
			continue
		}
		at := fused[i].createdAt
		if at.IsZero() {
			continue
		}
		age := now.Sub(at)
		if age < 0 {
			age = 0
		}
		// boost = 2^(-age/halfLife); never zero, so old moments can still match an explicit topical query, just rank lower than recent ones.
		fused[i].score *= math.Exp2(-age.Hours() / momentRecencyHalfLife.Hours())
		needRescore = true
	}
	if needRescore {
		sort.Slice(fused, func(i, j int) bool {
			if fused[i].score != fused[j].score {
				return fused[i].score > fused[j].score
			}
			return fused[i].id < fused[j].id
		})
	}

	if limit > 0 && len(fused) > limit {
		fused = fused[:limit]
	}

	s.recordVectorContribution(fused, vector)

	out := make([]MemoryHit, len(fused))
	for i, c := range fused {
		source, refID := splitCandidateID(c.id)
		if c.source != "" {
			source = c.source
		}
		out[i] = MemoryHit{
			Content:   c.content,
			Source:    source,
			RefID:     refID,
			Domain:    c.domain,
			App:       c.app,
			Title:     c.title,
			CreatedAt: c.createdAt,
		}
	}
	return out, nil
}

// dropSummariesShadowedByDigest removes a "summary" candidate from cands when its parent node is a "digest" that is also present in cands — the reparenting in ReplaceSummariesWithDigest keeps a compacted day's summaries alive as children of their digest, so both can otherwise reach the same fused result and describe the same day twice. A summary whose parent digest isn't in this result set is left alone.
func (s *Store) dropSummariesShadowedByDigest(ctx context.Context, cands []rrfCandidate) []rrfCandidate {
	digestIDs := make(map[int64]bool)
	for _, c := range cands {
		if c.source == "digest" {
			_, refID := splitCandidateID(c.id)
			digestIDs[refID] = true
		}
	}
	if len(digestIDs) == 0 {
		return cands
	}

	var summaryIDs []int64
	for _, c := range cands {
		if c.source == "summary" {
			_, refID := splitCandidateID(c.id)
			summaryIDs = append(summaryIDs, refID)
		}
	}
	parents := s.summaryParents(ctx, summaryIDs)

	out := cands[:0:0]
	for _, c := range cands {
		if c.source == "summary" {
			_, refID := splitCandidateID(c.id)
			if parent, ok := parents[refID]; ok && digestIDs[parent] {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// summaryParents looks up the parent node of every summary named in ids with one query. Input: the node ids of the summary candidates in one fused result. Output: a map from summary id to parent id, holding only the summaries that have a parent; an empty id list runs no query at all.
// This replaces a SELECT run once per summary candidate, which on a fused result made mostly of summaries was one round trip per row returned.
func (s *Store) summaryParents(ctx context.Context, ids []int64) map[int64]int64 {
	out := map[int64]int64{}
	if len(ids) == 0 {
		return out
	}
	placeholders, args := inPlaceholders(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT id, parent_id FROM nodes WHERE id IN (`+placeholders+`) AND parent_id IS NOT NULL`, args...)
	if err != nil {
		slog.Warn("hybrid search: looking up summary parents failed, no summary is dropped as shadowed", "error", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var parent sql.NullInt64
		if err := rows.Scan(&id, &parent); err != nil {
			continue
		}
		if parent.Valid {
			out[id] = parent.Int64
		}
	}
	return out
}

// recordVectorContribution is the vector-arm contribution counter: for one HybridSearchWindow call, did any of the vector-search candidates survive fusion into the final (post-limit) top-k, and how many. Recorded into the tally table (see db.go's schema comment) as two rows dated today (local): "vector-queries" ticks once per call regardless of outcome, and "vector-hits" ticks once, with the survivor count folded into its total_ms column, only when at least one did. Best-effort — a tally write failure is logged and never surfaces to the caller, same discipline every other accounting write in this package follows.
func (s *Store) recordVectorContribution(fused, vector []rrfCandidate) {
	day := time.Now().Format(tallyDayFormat)
	if err := s.bumpTally(day, "vector-queries", 1, 0, 0); err != nil {
		slog.Warn("tally: recording vector-queries failed", "error", err)
	}
	if len(vector) == 0 {
		return
	}
	fromVector := make(map[string]bool, len(vector))
	for _, c := range vector {
		fromVector[c.id] = true
	}
	survivors := 0
	for _, c := range fused {
		if fromVector[c.id] {
			survivors++
		}
	}
	if survivors == 0 {
		return
	}
	if err := s.bumpTally(day, "vector-hits", 1, 0, int64(survivors)); err != nil {
		slog.Warn("tally: recording vector-hits failed", "error", err)
	}
}
