package db

import (
	"context"
	"fmt"
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
	Count() int
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

// hybridVectorPoolSize bounds how many nearest-neighbor results are pulled from the vector index before fusion — wider than the final `limit` so RRF, not raw vector similarity alone, decides the final order.
const hybridVectorPoolSize = 50

// splitCandidateID parses an rrfCandidate/vector-result id of the form "<source>:<refID>" (e.g. "episode:42", "note:3") into its parts. refID is 0 if the numeric suffix doesn't parse cleanly (e.g. a vector-only hit whose id shape doesn't map to a nodes/episodes row) — best effort, not an error, since MemoryHit.RefID is informational.
func splitCandidateID(id string) (source string, refID int64) {
	idx := strings.LastIndex(id, ":")
	if idx < 0 {
		return id, 0
	}
	source = id[:idx]
	refID, _ = strconv.ParseInt(id[idx+1:], 10, 64)
	return source, refID
}

// candidateDomain looks up the domain tag for a lexical candidate. Only episodes and nodes (summary/digest) carry a domain column — notes and threads report "" (untagged) here.
func (s *Store) candidateDomain(ctx context.Context, source string, refID int64) string {
	var domain string
	switch source {
	case "episode":
		_ = s.db.QueryRowContext(ctx, `SELECT domain FROM episodes WHERE id = ?`, refID).Scan(&domain)
	case "summary", "digest":
		_ = s.db.QueryRowContext(ctx, `SELECT domain FROM nodes WHERE id = ?`, refID).Scan(&domain)
	}
	return domain
}

// currentDomain returns the domain tag of the most recently logged episode — the store's best guess at "what domain is the user in right now", used to boost (not filter) fused results when the caller didn't pass an explicit domainFilter.
func (s *Store) currentDomain(ctx context.Context) string {
	var domain string
	_ = s.db.QueryRowContext(ctx, `SELECT domain FROM episodes ORDER BY id DESC LIMIT 1`).Scan(&domain)
	return domain
}

// HybridSearch fuses lexical (FTS5) and vector search over episodes, summaries, notes, and threads via reciprocalRankFusion, and returns the top `limit` as MemoryHit.
// If domainFilter is "work" or "personal", results are hard-filtered to that domain (both the lexical SQL query and the vector search's `where` clause). If domainFilter is "", no hard filter is applied, but candidates matching the store's inferred current domain (see currentDomain) get domainBoostFactor applied to their fused score before the final sort.
// If the Store has no embedder/vector index configured (both nil — the default for a bare db.New(":memory:")), HybridSearch degrades gracefully to lexical-only fusion rather than erroring.
func (s *Store) HybridSearch(ctx context.Context, query, domainFilter string, limit int) ([]MemoryHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// lexical candidates: reuse the existing FTS5 paths, don't reimplement.
	memHits, err := s.SearchMemory(ctx, query)
	if err != nil {
		return nil, err
	}
	episodeHits, err := s.SearchEpisodes(ctx, query)
	if err != nil {
		return nil, err
	}

	// domain is looked up for every lexical candidate up front (not just when domainFilter is set) — it backs both the hard-filter below and the same-domain boost applied later when domainFilter == "". Doing this only inside the filter branch would leave every lexical/FTS5 hit's domain permanently "" for the common no-filter case, silently defeating the boost.
	lexical := make([]rrfCandidate, 0, len(memHits)+len(episodeHits))
	for _, h := range memHits {
		domain := h.Domain
		if domain == "" {
			domain = s.candidateDomain(ctx, h.Source, h.RefID)
		}
		lexical = append(lexical, rrfCandidate{
			id:      fmt.Sprintf("%s:%d", h.Source, h.RefID),
			content: h.Content,
			source:  h.Source,
			domain:  domain,
		})
	}
	for _, h := range episodeHits {
		domain := h.Domain
		if domain == "" {
			domain = s.candidateDomain(ctx, h.Source, h.RefID)
		}
		lexical = append(lexical, rrfCandidate{
			id:        fmt.Sprintf("%s:%d", h.Source, h.RefID),
			content:   h.Content,
			source:    h.Source,
			domain:    domain,
			app:       h.App,
			title:     h.Title,
			createdAt: h.CreatedAt,
		})
	}

	if domainFilter != "" {
		filtered := lexical[:0:0]
		for _, c := range lexical {
			if c.domain == domainFilter {
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

	var vector []rrfCandidate
	if emb != nil && vidx != nil {
		queryVec, err := emb.Embed(ctx, "RETRIEVAL_QUERY", query)
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}

		var where map[string]string
		if domainFilter != "" {
			where = map[string]string{"domain": domainFilter}
		}

		results, err := vidx.Search(ctx, queryVec, hybridVectorPoolSize, where)
		if err != nil {
			return nil, fmt.Errorf("vector search: %w", err)
		}

		vector = make([]rrfCandidate, 0, len(results))
		for _, r := range results {
			source, _ := splitCandidateID(r.ID)
			createdAt := time.Time{}
			if ts := r.Metadata["created_at"]; ts != "" {
				createdAt = parseSQLiteTime(ts)
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

	var lists [][]rrfCandidate
	if len(lexical) > 0 {
		lists = append(lists, lexical)
	}
	if len(vector) > 0 {
		lists = append(lists, vector)
	}
	fused := reciprocalRankFusion(rrfK, lists...)

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
