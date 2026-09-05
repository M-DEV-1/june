// This file splits a capture into passages that can each be embedded whole.
//
// Until this existed, an episode was one vector. The embedder caps input at maxEmbedRunes (4,000) because EmbeddingGemma's context is 2,048 tokens and llama-server rejects rather than truncates a longer document — so a 96,061-character screen was represented by its first 4,000 characters and the other 92,000 had no semantic representation at all. Measured across the live store: 7,393,940 characters captured, 3,676,245 embedded, 50.3% never reachable by meaning.
//
// The second problem is subtler and applies even under the cap: one vector for a whole screen is an average of everything on it. A code-review page embeds mostly as navigation and chrome, and the eleven specific findings on it are a rounding error in that average. Splitting first means a passage about the findings is its own vector and can be matched on its own terms.
package db

import (
	"fmt"
	"strconv"
	"strings"
)

// chunkRunes is how much text goes into one passage, and chunkOverlap is how much of the previous passage each one repeats.
// The size sits well under maxEmbedRunes so a chunk is never itself truncated, and it is large enough that the average capture (1,528 runes in this store) stays a single chunk — the common case pays nothing.
// The overlap exists because a split lands wherever the text runs out of room, which is as likely to be mid-sentence as anywhere: without it, an answer straddling a boundary belongs to neither passage and is findable in neither.
const (
	chunkRunes   = 1500
	chunkOverlap = 200
)

// chunkText splits s into overlapping passages of at most size runes. Input: the capture and the passage geometry. Output: the passages in order, or nothing at all for text that is empty or only whitespace, since the embedder rejects those.
// Splits prefer a line break and then a space near the end of the passage, so a passage tends to end where the text does rather than mid-word. Text shorter than size comes back as one passage, unchanged.
func chunkText(s string, size, overlap int) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	runes := []rune(s)
	// A size of zero or less has no window to advance: the loop below appended an empty passage and then reset start to where it already was, growing the output until memory ran out, and a negative size sliced runes[start:start-1] and panicked. There is no sensible split at that geometry, so the whole text comes back as one passage.
	if size <= 0 || len(runes) <= size {
		return []string{s}
	}
	if overlap >= size {
		overlap = size / 4
	}

	var out []string
	for start := 0; start < len(runes); {
		end := start + size
		if end >= len(runes) {
			out = append(out, string(runes[start:]))
			break
		}
		// Back up to a natural boundary, but only within the last quarter of the passage — searching further would produce passages far shorter than the budget.
		cut := end
		for i := end - 1; i > end-size/4 && i > start; i-- {
			if runes[i] == '\n' {
				cut = i + 1
				break
			}
			if runes[i] == ' ' && cut == end {
				cut = i + 1
			}
		}
		out = append(out, string(runes[start:cut]))
		next := cut - overlap
		if next <= start {
			next = cut // never go backwards, however the boundary search landed
		}
		start = next
	}
	return out
}

// chunkVectorID names one passage's vector. The first passage keeps the plain "source:id" the index has always used, so every vector already stored stays valid and simply becomes chunk zero — no deletion, no re-index of the common case, and reconciliation only has to add the passages that were never there.
func chunkVectorID(source string, id int64, chunk int) string {
	if chunk == 0 {
		return fmt.Sprintf("%s:%d", source, id)
	}
	return fmt.Sprintf("%s:%d#%d", source, id, chunk)
}

// episodeIDFromVectorID recovers the episode a vector belongs to, whichever passage it is. Reconciliation needs it to tell "this episode has no vectors at all" from "this episode has its first passage and is missing the rest".
func episodeIDFromVectorID(vid string) (int64, bool) {
	rest, ok := strings.CutPrefix(vid, "episode:")
	if !ok {
		return 0, false
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil
}

// bestPassagePerRow collapses a fused candidate list so each underlying row appears once, represented by its highest-scoring passage. Input: candidates in fused order. Output: the same order with later passages of an already-seen row dropped.
// Without this a screen that matches a query in three places spends three of the ten rows the model gets, and the ten rows stop covering ten different moments — which is the budget the excerpt work exists to protect.
func bestPassagePerRow(cands []rrfCandidate) []rrfCandidate {
	out := make([]rrfCandidate, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		source, refID := splitCandidateID(c.id)
		// A row id of 0 means the id did not name a row at all; those are kept as-is rather than collapsed together.
		key := fmt.Sprintf("%s:%d", source, refID)
		if refID != 0 {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, c)
	}
	return out
}
