package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ora/internal/memory"
)

// ReconcileReport counts what one ReconcileVectors sweep did — logged by the caller (cmd/daemon.go), not returned as an error since a sweep is a maintenance pass, not a request that can fail outright.
type ReconcileReport struct {
	Deleted    int
	Backfilled int
}

// ReconcileVectors walks every id currently in the vector index and deletes any whose backing SQL row is gone (or, for episodes, already thinned to empty screen_text) — this is what heals the orphaning left by consolidation (ReplaceAllNotes renumbering notes) for a store that predates targeted vector deletes, without a manual rebuild. ReplaceSummariesWithDigest no longer causes this: it reparents summaries under their digest instead of deleting them, so their vectors stay live.
// A Store with no vector index configured is a no-op (zero report, no error).
func (s *Store) ReconcileVectors(ctx context.Context, embedCap int) (ReconcileReport, error) {
	var report ReconcileReport

	s.mu.RLock()
	vidx := s.vectorIndex
	s.mu.RUnlock()
	if vidx == nil {
		return report, nil
	}

	// Backing rows are looked up once per source rather than once per vector: since chunking landed one episode is one vector per passage, so an index holding tens of thousands of ids used to mean tens of thousands of round trips (each episode one pulling its whole screen_text into Go) to answer "does this row still exist".
	ids := vidx.IDs()
	refsBySource := map[string][]int64{}
	seen := map[string]bool{}
	for _, id := range ids {
		source, refID := splitCandidateID(id)
		key := fmt.Sprintf("%s:%d", source, refID)
		if seen[key] {
			continue
		}
		seen[key] = true
		refsBySource[source] = append(refsBySource[source], refID)
	}
	live := map[string]map[int64]bool{}
	for source, refs := range refsBySource {
		live[source] = s.liveVectorRefs(ctx, source, refs)
	}

	existing := make(map[string]bool)
	for _, id := range ids {
		source, refID := splitCandidateID(id)
		if alive := live[source]; alive == nil || alive[refID] {
			existing[id] = true
			continue
		}
		if err := vidx.Delete(ctx, id); err != nil {
			slog.Error("reconcile: delete orphaned vector failed", "id", id, "error", err)
			existing[id] = true // delete failed — still there, don't try to re-add over it below
			continue
		}
		report.Deleted++
	}

	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil || embedCap <= 0 {
		return report, nil
	}

	// The candidate builder already drops what the index has for the sources it bounds; this covers the rest, so nothing that already has a vector is embedded twice.
	batchAdder, hasBatch := vidx.(batchVectorIndex)
	var batchIDs []string
	var batchContents []string
	var batchEmbeddings [][]float32
	var batchMetadatas []map[string]string

	flushBatch := func() {
		if len(batchIDs) == 0 {
			return
		}
		if err := batchAdder.AddBatch(ctx, batchIDs, batchContents, batchEmbeddings, batchMetadatas); err != nil {
			// The embeddings in a failed batch have already been paid for, so each document is offered again on its own rather than dropped. This also closes the hole a half-applied batch leaves in a *vector.ChromemIndex, where a document the batch added but never stamped with a createdAt is invisible to eviction: the single Add stamps it.
			slog.Error("reconcile: backfill batch add failed, retrying one at a time", "count", len(batchIDs), "error", err)
			for i, id := range batchIDs {
				if err := vidx.Add(ctx, id, batchContents[i], batchEmbeddings[i], batchMetadatas[i]); err != nil {
					slog.Error("reconcile: backfill add after a failed batch failed", "id", id, "error", err)
					continue
				}
				report.Backfilled++
			}
		} else {
			report.Backfilled += len(batchIDs)
		}
		batchIDs = batchIDs[:0]
		batchContents = batchContents[:0]
		batchEmbeddings = batchEmbeddings[:0]
		batchMetadatas = batchMetadatas[:0]
	}

	for _, c := range s.reconcileBackfillCandidates(ctx, existing, embedCap) {
		if existing[c.id] {
			continue
		}
		if report.Backfilled+len(batchIDs) >= embedCap {
			break
		}
		vec, err := emb.Embed(ctx, "RETRIEVAL_DOCUMENT", c.content)
		if err != nil {
			// One bad document is skipped, but an engine that has gone away refuses every candidate, and refuses in microseconds with no I/O. Carrying on through the rest of the list then costs one ERROR line per candidate at several thousand a second: on shutdown, when the root context is cancelled and the engine closed while a sweep started at boot is still running, that was 29,763 of the log's 29,941 error lines in eight days. Nothing is lost by stopping — the next sweep re-derives whatever still has no vector.
			if engineGone(ctx, err) {
				slog.Warn("reconcile: backfill stopped, the embedding engine is gone", "backfilled", report.Backfilled, "error", err)
				if hasBatch {
					flushBatch()
				}
				return report, err
			}
			slog.Error("reconcile: backfill embed failed", "id", c.id, "error", err)
			continue
		}
		if hasBatch {
			batchIDs = append(batchIDs, c.id)
			batchContents = append(batchContents, c.content)
			batchEmbeddings = append(batchEmbeddings, vec)
			batchMetadatas = append(batchMetadatas, c.metadata)
			if len(batchIDs) >= 50 {
				flushBatch()
			}
		} else {
			if err := vidx.Add(ctx, c.id, c.content, vec, c.metadata); err != nil {
				slog.Error("reconcile: backfill add failed", "id", c.id, "error", err)
				continue
			}
			report.Backfilled++
		}
	}
	if hasBatch {
		flushBatch()
	}

	return report, nil
}

// reconcileCandidate is one item eligible to be backfilled into the vector index.
type reconcileCandidate struct {
	id       string
	content  string
	metadata map[string]string
}

// reconcileCandidateBound is how many multiples of one sweep's embed cap the episode scan may collect before it stops. Above one so a sweep whose first candidates fail to embed still has others to fall back on, small enough that the list stays a few hundred rows rather than the whole capture history.
const reconcileCandidateBound = 4

// reconcileEpisodeWindow bounds how far back an episode is still eligible for vector backfill — matches the "recent" framing the rest of the memory system uses (e.g. RankedEpisodes' recency decay); re-embedding arbitrarily old raw captures indefinitely isn't worth the API cost.
const reconcileEpisodeWindow = 10 * 24 * time.Hour

// reconcileBackfillCandidates returns notes, live (non-digested) summary and digest nodes, threads, and episodes (within reconcileEpisodeWindow unless embeds are free) — every source whose vector lifecycle ReconcileVectors owns. Metadata and embed text are built to match each source's own write-path goroutine exactly (LogNote/LogSemanticNode/LogEpisode) — a backfilled vector missing e.g. domain would silently drop out of every domain-filtered search (chromem's exact-match where fails on a missing key), and a missing created_at defeats HybridSearch's moment recency decay for episodes. Query errors are logged and treated as "no candidates from this source" rather than failing the whole sweep.
func (s *Store) reconcileBackfillCandidates(ctx context.Context, existing map[string]bool, embedCap int) []reconcileCandidate {
	var out []reconcileCandidate

	noteRows, err := s.db.QueryContext(ctx, `SELECT id, content, created_at FROM notes`)
	if err != nil {
		slog.Error("reconcile: query notes for backfill failed", "error", err)
	} else {
		for noteRows.Next() {
			var id int64
			var content, created string
			if err := noteRows.Scan(&id, &content, &created); err != nil {
				slog.Error("reconcile: scan note for backfill failed", "error", err)
				continue
			}
			out = append(out, reconcileCandidate{
				id:      fmt.Sprintf("note:%d", id),
				content: content,
				metadata: map[string]string{
					"source":     "note",
					"kind":       string(memory.KindFact),
					"created_at": created,
				},
			})
		}
		noteRows.Close()
	}

	// nodes.content for a summary is the full JSON-marshaled TaskSummary (see LogSemanticNode), not the plain summary text — LogSemanticNode's own embed goroutine embeds only summary.Summary, so backfill must extract the same field rather than embedding the raw JSON blob.
	// digest rows are included alongside summaries (both are "live" period nodes whose vector lifecycle this sweep owns) — a digest's content is already plain text, so the JSON-unmarshal below just fails and falls through to using it as-is.
	summaryRows, err := s.db.QueryContext(ctx, `SELECT id, type, content, domain, created_at FROM nodes WHERE type IN ('summary', 'digest')`)
	if err != nil {
		slog.Error("reconcile: query summaries for backfill failed", "error", err)
	} else {
		for summaryRows.Next() {
			var id int64
			var nodeType, payload, domain, created string
			if err := summaryRows.Scan(&id, &nodeType, &payload, &domain, &created); err != nil {
				slog.Error("reconcile: scan summary for backfill failed", "error", err)
				continue
			}
			var ts memory.TaskSummary
			text := payload
			if err := json.Unmarshal([]byte(payload), &ts); err == nil && ts.Summary != "" {
				text = ts.Summary
			}
			out = append(out, reconcileCandidate{
				id:      fmt.Sprintf("%s:%d", nodeType, id),
				content: text,
				metadata: map[string]string{
					"domain":     domain,
					"source":     nodeType,
					"kind":       string(memory.KindPeriod),
					"created_at": created,
				},
			})
		}
		summaryRows.Close()
	}

	// Threads carry the arc layer — the weeks-long throughlines. No write path has ever embedded them, so until this sweep picks them up the whole layer is reachable only through the lexical half of hybrid search. The embed text matches the threads_ai FTS trigger's ("subject — state") so both halves see the same thread.
	threadRows, err := s.db.QueryContext(ctx, `SELECT id, subject, IFNULL(state,''), created_at FROM threads`)
	if err != nil {
		slog.Error("reconcile: query threads for backfill failed", "error", err)
	} else {
		for threadRows.Next() {
			var id int64
			var subject, state, created string
			if err := threadRows.Scan(&id, &subject, &state, &created); err != nil {
				slog.Error("reconcile: scan thread for backfill failed", "error", err)
				continue
			}
			text := subject
			if state != "" {
				text = subject + " — " + state
			}
			out = append(out, reconcileCandidate{
				id:      fmt.Sprintf("thread:%d", id),
				content: text,
				metadata: map[string]string{
					"source":     "thread",
					"kind":       string(memory.KindArc),
					"created_at": created,
				},
			})
		}
		threadRows.Close()
	}

	// A metered embedder only re-embeds episodes inside reconcileEpisodeWindow, so a large dirty store can't run up an API bill. A local embedder costs CPU, so every episode that still has text is eligible — which is what brings back the roughly a thousand older episodes whose vectors were lost to API failures and which the window would otherwise exclude permanently.
	s.mu.RLock()
	free := s.embedsAreFree
	s.mu.RUnlock()

	episodeQuery := `SELECT id, app, title, screen_text, domain, created_at FROM episodes WHERE created_at >= datetime('now', '-' || ? || ' seconds') AND screen_text != ''`
	args := []any{int64(reconcileEpisodeWindow.Seconds())}
	if free {
		episodeQuery = `SELECT id, app, title, screen_text, domain, created_at FROM episodes WHERE screen_text != ''`
		args = nil
	}
	episodeCandidates := 0
	episodeRows, err := s.db.QueryContext(ctx, episodeQuery, args...)
	if err != nil {
		slog.Error("reconcile: query episodes for backfill failed", "error", err)
	} else {
		for episodeRows.Next() {
			// Episodes are the one unbounded source here: the store holds every capture ever taken, and with a free embedder there is no age window either, so reading them all in would materialise every screen capture's full text for a sweep that can only ever embed embedCap of them. Stopping the scan a few multiples past that cap keeps the memory proportional to the work.
			// The already-indexed ids are skipped here rather than after the list is built, so the bounded budget goes to episodes that actually need a vector — and since the scan is in rowid order, each sweep resumes on the oldest ones still missing and the backlog drains instead of the same rows being rebuilt every time.
			if episodeCandidates >= reconcileCandidateBound*embedCap {
				break
			}
			var id int64
			var app, title, screenText, domain, created string
			if err := episodeRows.Scan(&id, &app, &title, &screenText, &domain, &created); err != nil {
				slog.Error("reconcile: scan episode for backfill failed", "error", err)
				continue
			}
			// Same context-framed document LogEpisode embeds (app/title header + content), not bare screen_text — Normalize re-running on already-clean stored text is safe (it's idempotent chrome-stripping/capping on content that's already clean).
			doc := memory.NormalizeFull(app, title, screenText).Document()
			if strings.TrimSpace(doc) == "" {
				doc = screenText
			}
			meta := map[string]string{
				"domain":     domain,
				"source":     "episode",
				"kind":       string(memory.KindMoment),
				"created_at": created,
			}
			// Per passage, and each checked separately. An episode indexed before chunking existed has its first passage and none of the others, which is exactly the 50% of captured text that had no vector at all — this is the sweep that brings it back.
			for i, chunk := range chunkText(doc, chunkRunes, chunkOverlap) {
				vid := chunkVectorID("episode", id, i)
				if existing[vid] {
					continue
				}
				episodeCandidates++
				out = append(out, reconcileCandidate{id: vid, content: chunk, metadata: meta})
			}
		}
		episodeRows.Close()
	}

	return out
}

// vectorBackingChunk is how many ids one backing-existence query asks about at a time, keeping the bound-parameter count well inside SQLite's limit for an index holding tens of thousands of vectors.
const vectorBackingChunk = 500

// liveVectorRefs reports which of one source's ref ids still have a backing row worth keeping a vector for. Input: the source name and the ref ids the vector index holds for it. Output: the set of those ids that are still alive — for an episode, alive also requires a non-empty screen_text, since a thinned capture's vector describes text the row no longer holds. A source whose vector lifecycle this sweep does not own returns nil, and the caller then leaves every one of its vectors alone.
// A query error reports the whole chunk alive: a sweep that cannot read the backing table must not take that as licence to delete.
func (s *Store) liveVectorRefs(ctx context.Context, source string, refs []int64) map[int64]bool {
	var query string
	var extra []any
	switch source {
	case "note":
		query = `SELECT id FROM notes WHERE id IN (%s)`
	case "summary", "digest":
		query = `SELECT id FROM nodes WHERE id IN (%s) AND type = ?`
		extra = []any{source}
	case "thread":
		query = `SELECT id FROM threads WHERE id IN (%s)`
	case "episode":
		query = `SELECT id FROM episodes WHERE id IN (%s) AND screen_text <> ''`
	default:
		return nil
	}

	live := make(map[int64]bool, len(refs))
	for start := 0; start < len(refs); start += vectorBackingChunk {
		end := start + vectorBackingChunk
		if end > len(refs) {
			end = len(refs)
		}
		chunk := refs[start:end]
		placeholders, args := inPlaceholders(chunk)
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(query, placeholders), append(args, extra...)...)
		if err != nil {
			slog.Error("reconcile: backing-row lookup failed, keeping this chunk's vectors", "source", source, "error", err)
			for _, id := range chunk {
				live[id] = true
			}
			continue
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				live[id] = true
			}
		}
		rows.Close()
	}
	return live
}

// engineGone reports whether an embed failure means the engine itself has stopped answering, as against one document it could not embed. Input: the sweep's context and the error the embedder returned. Output: true when carrying on would only repeat the same failure for every remaining candidate.
// The shapes are the ways the engine stops answering every caller at once: a shutdown cancels the root context first, so calls fail on the way out to the server, and closes the engine a few steps later, after which it refuses in memory; a whisper decode takes the GPU for its whole run, and the engine refuses in memory for that whole time too. All three refuse instantly, which is what turns "keep going" into thousands of log lines a second.
func engineGone(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, ": closed") || strings.Contains(msg, "the GPU is held")
}
