package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"june/internal/memory"
	"june/internal/obs"
	"log/slog"
	"strings"
	"time"
)

// SetWorkingState upserts the single-row working-state cache. Content is synthesized by the daemon's state deriver and overwritten in full each cadence tick — no history is kept.
func (s *Store) SetWorkingState(ctx context.Context, content string) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.SetWorkingState")
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO working_state(id, content, updated_at) VALUES(1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET content = excluded.content, updated_at = CURRENT_TIMESTAMP`,
		content)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("set working state: %w", err)
	}
	return nil
}

// GetWorkingState returns the cached working-state content, or ("", nil) when no row has been written yet.
func (s *Store) GetWorkingState(ctx context.Context) (string, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.GetWorkingState")
	defer span.End()

	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM working_state WHERE id = 1`).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		span.RecordError(err)
		return "", fmt.Errorf("get working state: %w", err)
	}
	return content, nil
}

// RecentSummaries returns the content of the most recent summary and digest nodes, newest-first, up to limit rows. Used by the state deriver to build the synthesis prompt without walking the full ancestor tree.
func (s *Store) RecentSummaries(ctx context.Context, limit int) ([]string, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.RecentSummaries")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT content FROM nodes WHERE type IN ('summary','digest') ORDER BY id DESC LIMIT ?`,
		limit)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("recent summaries: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan recent summary: %w", err)
		}
		out = append(out, content)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate recent summaries: %w", err)
	}
	span.SetAttributes(attribute.Int("db.recent_count", len(out)))
	return out, nil
}

// WindowSummary is one summary or digest node inside a recall window: when it was written and its content (a task summary is the compiler's JSON, a digest plain prose).
type WindowSummary struct {
	CreatedAt time.Time
	Content   string
}

// SummaryTimeline returns the summary and digest nodes whose created_at falls in [since, until], oldest first. This is the tier recall reads when a window holds more episodes than fit in one tool result: the summaries are bounded per day by construction, so a whole day or week comes back with every stretch of it represented.
func (s *Store) SummaryTimeline(ctx context.Context, since, until time.Time) ([]WindowSummary, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.SummaryTimeline")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT created_at, content FROM nodes WHERE type IN ('summary','digest') AND created_at >= ? AND created_at <= ? ORDER BY created_at ASC`,
		sqliteUTC(since), sqliteUTC(until))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("summary timeline: %w", err)
	}
	defer rows.Close()

	var out []WindowSummary
	for rows.Next() {
		var w WindowSummary
		// The driver converts the DATETIME column itself; scanning through a string re-parses its formatting instead of the stored value and zeroed every date the first time this ran.
		if err := rows.Scan(&w.CreatedAt, &w.Content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan summary timeline: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate summary timeline: %w", err)
	}
	span.SetAttributes(attribute.Int("db.window_count", len(out)))
	return out, nil
}

// CountSummariesSince returns the number of summary and digest nodes created after since. Used as a cost guard so the state deriver skips recomputation when nothing new has been written.
// since is formatted as UTC "2006-01-02 15:04:05" to match SQLite's CURRENT_TIMESTAMP storage format, which has no sub-second component.
func (s *Store) CountSummariesSince(ctx context.Context, since time.Time) (int, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.CountSummariesSince")
	defer span.End()

	// Truncate to second precision and use >= so rows inserted within the same clock-second as since are included — safe because production callers set since right after a derive run, so those rows should trigger a re-derive anyway.
	sinceStr := since.UTC().Truncate(time.Second).Format("2006-01-02 15:04:05")
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE type IN ('summary','digest') AND created_at >= ?`,
		sinceStr).Scan(&n)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("count summaries since: %w", err)
	}
	span.SetAttributes(attribute.Int("db.count", n))
	return n, nil
}

// recentTaskWindow bounds how far back GetImplicitContext looks when folding recent task names into its focus signal. Without a time bound, "last 2 tasks by id" in a lightly-used store can still be a stale task from days ago that self-matches its own summary back into context regardless of relevance.
const recentTaskWindow = 2 * time.Hour

// get or create
func (s *Store) ensureNode(ctx context.Context, parentID int64, nodeType, content string) (int64, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	_, span := tracer.Start(ctx, "DB.EnsureNode")
	span.SetAttributes(attribute.String("node.type", nodeType))
	defer span.End()

	var id int64

	// wrap in a transaction to ensure atomicity
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if parentID == 0 {
		// ROOT, i.e. user
		// insert if not there already, or ignore and move on
		query := `INSERT OR IGNORE INTO nodes (type, content) VALUES (?, ?)`
		_, err = tx.Exec(query, nodeType, content)
		if err == nil {
			query = `SELECT id FROM nodes WHERE parent_id IS NULL AND type = ? AND content = ?`
			err = tx.QueryRow(query, nodeType, content).Scan(&id)
		}
	} else {
		// CHILD, i.e. day, session or activity
		query := `INSERT OR IGNORE INTO nodes (parent_id, type, content) VALUES (?, ?, ?)`
		_, err = tx.Exec(query, parentID, nodeType, content)
		if err == nil {
			query = `SELECT id FROM nodes WHERE parent_id = ? AND type = ? AND content = ?`
			err = tx.QueryRow(query, parentID, nodeType, content).Scan(&id)
		}
	}

	if err != nil {
		return 0, fmt.Errorf("failed to ensure %s node: %w", nodeType, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return id, nil
}

// SummaryGroup is one day's summary nodes awaiting compaction, NodeRef one node within it, and TaskSummary the compiler's structured summary of a work slice. Aliases of the memory types for the reason given on db.Thread.
type SummaryGroup = memory.SummaryGroup

// NodeRef is one summary node inside a SummaryGroup.
type NodeRef = memory.NodeRef

// TaskSummary is the compiler's structured summary of one flushed buffer, the input side of LogSemanticNode.
type TaskSummary = memory.TaskSummary

// MaxSummariesPerGroup caps how many of one day's summaries a single OldSummaryGroups call returns, and MaxDayGroupsPerRun caps how many days it returns at all. Both exist because the compaction job puts a group's summaries into one prompt: an uncapped day (a store where a week of summaries landed under one day node would hold hundreds) builds a prompt past the model's input limit, the call fails, and the day is skipped on every run forever. What is left behind is picked up by the next run, since a compacted summary is reparented under the digest and drops out of this query.
const (
	MaxSummariesPerGroup = 200
	MaxDayGroupsPerRun   = 20
)

// OldSummaryGroups returns summary nodes older than olderThan, grouped by their ancestor DAY node, capped at MaxSummariesPerGroup rows per day and MaxDayGroupsPerRun days. Used by the compaction job to decide which days are ready to roll up.
func (s *Store) OldSummaryGroups(ctx context.Context, olderThan time.Duration) ([]memory.SummaryGroup, error) {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.OldSummaryGroups")
	defer span.End()

	secs := int64(olderThan.Seconds())
	rows, err := s.db.QueryContext(ctx, `
		SELECT day_id, day_content, sm_id, sm_content FROM (
			SELECT day.id AS day_id, day.content AS day_content, sm.id AS sm_id, sm.content AS sm_content,
			       ROW_NUMBER() OVER (PARTITION BY day.id ORDER BY sm.id) AS row_in_day,
			       DENSE_RANK() OVER (ORDER BY day.id) AS day_rank
			FROM nodes sm
			JOIN nodes task ON sm.parent_id = task.id AND task.type = 'task'
			JOIN nodes sess ON task.parent_id = sess.id AND sess.type = 'session'
			JOIN nodes day  ON sess.parent_id = day.id  AND day.type = 'day'
			WHERE sm.type = 'summary'
			  AND sm.created_at < datetime('now', '-' || ? || ' seconds')
		)
		WHERE row_in_day <= ? AND day_rank <= ?
		ORDER BY day_id, sm_id
	`, secs, MaxSummariesPerGroup, MaxDayGroupsPerRun)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("old summary groups query: %w", err)
	}
	defer rows.Close()

	// build groups in insertion order while preserving day grouping
	var groups []memory.SummaryGroup
	index := make(map[int64]int) // dayID → index into groups slice

	for rows.Next() {
		var dayID int64
		var dayContent string
		var smID int64
		var smContent string
		if err := rows.Scan(&dayID, &dayContent, &smID, &smContent); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan summary group row: %w", err)
		}

		idx, seen := index[dayID]
		if !seen {
			groups = append(groups, memory.SummaryGroup{DayID: dayID, Day: dayContent})
			idx = len(groups) - 1
			index[dayID] = idx
		}
		groups[idx].Summaries = append(groups[idx].Summaries, memory.NodeRef{ID: smID, Content: smContent})
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate summary group rows: %w", err)
	}

	span.SetAttributes(attribute.Int("db.group_count", len(groups)))
	return groups, nil
}

// inClause builds a "... WHERE id IN (?,?,...)" query and its argument slice for a set of int64 ids. Input: the SQL up to and including the open paren (such as "DELETE FROM nodes WHERE id IN ("), and the ids. Output: the finished SQL and the ids as an arg slice, in the same order.
func inClause(prefix string, ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return prefix + strings.Join(placeholders, ",") + ")", args
}

// existingDigestID returns the digest node already filed under dayID, if any. Input: ctx, the open transaction, and the day's node id. Output: the digest's id and true, or 0 and false when the day has none yet, or an error from the read.
func (s *Store) existingDigestID(ctx context.Context, tx *sql.Tx, dayID int64) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type = 'digest' AND parent_id = ?`, dayID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find existing digest: %w", err)
	}
	return id, true, nil
}

// ExistingDigest returns the text of the digest already filed under dayID, or "" when the day has none. The compaction job reads it so the model merging a day's summaries is shown what its earlier digest of that day already said, instead of writing a digest that covers only the batch it happens to be holding. Input: ctx and the day node's id. Output: the digest text, or "" plus any read error.
func (s *Store) ExistingDigest(ctx context.Context, dayID int64) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM nodes WHERE type = 'digest' AND parent_id = ?`, dayID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("existing digest: %w", err)
	}
	return content, nil
}

// digestCreatedAt returns the timestamp the day's digest should carry: the newest created_at among the summaries it covers, or midnight of the day node's own date when none of them resolve. Input: ctx, the open transaction, the day node's id, and the summary ids being rolled up. Output: the timestamp in SQLite's stored format, or "" when neither source yields one, which tells the caller to leave the column default alone.
//
// It exists because a digest inserted with CURRENT_TIMESTAMP is dated the moment compaction ran, and every reader that selects summaries and digests by created_at — SummaryTimeline, RecentSummaries, the evening close, the dream — then narrates last week's work as today's.
func (s *Store) digestCreatedAt(ctx context.Context, tx *sql.Tx, dayID int64, summaryIDs []int64) string {
	if len(summaryIDs) > 0 {
		selectSQL, args := inClause("SELECT MAX(created_at) FROM nodes WHERE id IN (", summaryIDs)
		var newest sql.NullString
		if err := tx.QueryRowContext(ctx, selectSQL, args...).Scan(&newest); err == nil && newest.Valid && newest.String != "" {
			return newest.String
		}
	}
	var day string
	if err := tx.QueryRowContext(ctx, `SELECT content FROM nodes WHERE id = ?`, dayID).Scan(&day); err == nil {
		if _, perr := time.Parse("2006-01-02", day); perr == nil {
			return day + " 00:00:00"
		}
	}
	return ""
}

// dedupeSummaryContent reads back the content of every id in summaryIDs and splits them into keepIDs, one id per distinct content (the first seen), and dropIDs, every later id whose content repeats one already kept or one already parented on digestID. Input: ctx, the open transaction, the digest the batch is about to be reparented under, and the candidate ids. Output: the two sets, covering every input id between them once, or an error from the read.
//
// Seeding with digestID's own children is what makes a resumed compaction safe: a day whose digest already exists from an earlier, partial run can have a summary still sitting under its original task that happens to repeat the content of one already moved under the digest, and reparenting it now would hit the exact same idx_nodes_unique collision a plain duplicate within the batch would.
func (s *Store) dedupeSummaryContent(ctx context.Context, tx *sql.Tx, digestID int64, summaryIDs []int64) (keepIDs, dropIDs []int64, err error) {
	seen := make(map[string]bool, len(summaryIDs))
	existing, err := tx.QueryContext(ctx, `SELECT content FROM nodes WHERE parent_id = ? AND type = 'summary'`, digestID)
	if err != nil {
		return nil, nil, fmt.Errorf("read digest's existing summaries: %w", err)
	}
	for existing.Next() {
		var c string
		if err := existing.Scan(&c); err != nil {
			existing.Close()
			return nil, nil, fmt.Errorf("scan digest's existing summary: %w", err)
		}
		seen[c] = true
	}
	if err := existing.Err(); err != nil {
		existing.Close()
		return nil, nil, fmt.Errorf("iterate digest's existing summaries: %w", err)
	}
	existing.Close()

	selectSQL, args := inClause("SELECT id, content FROM nodes WHERE id IN (", summaryIDs)
	rows, err := tx.QueryContext(ctx, selectSQL, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read summary content: %w", err)
	}
	defer rows.Close()

	content := make(map[int64]string, len(summaryIDs))
	for rows.Next() {
		var id int64
		var c string
		if err := rows.Scan(&id, &c); err != nil {
			return nil, nil, fmt.Errorf("scan summary content: %w", err)
		}
		content[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate summary content: %w", err)
	}

	for _, id := range summaryIDs {
		c, ok := content[id]
		if !ok {
			// Already gone by the time this ran; nothing to reparent or drop.
			continue
		}
		if seen[c] {
			dropIDs = append(dropIDs, id)
			continue
		}
		seen[c] = true
		keepIDs = append(keepIDs, id)
	}
	return keepIDs, dropIDs, nil
}

// ReplaceSummariesWithDigest reparents the given summary nodes under dayID's digest, in a single transaction — the surviving summaries are kept, not deleted, so the day's raw source material survives compaction. The exception is a summary whose content exactly duplicates one already kept (see dedupeSummaryContent below): that node, and its FTS5 and vector-index entries, are deleted, since keeping both would collide on idx_nodes_unique. Every surviving summary's row is left alone, so its existing memory_fts entry is untouched.
//
// A day with no digest yet gets one written now, from digest, and the digest insert is what is picked up by the nodes_ai_summary trigger and (see below) embedded. A day that already has a digest — a day compacted before, whether because an earlier call reparented only some of its summaries or because more summaries crossed the age cutoff since — reuses that node and rewrites its content with digest, so the digest the caller just paid a model call for is the one the day ends up with rather than being discarded. The rewrite fires the nodes_au_summary trigger for FTS and is re-embedded below.
// The digest is dated the day it covers, not the moment compaction ran; see digestCreatedAt.
// If the digest insert fails the reparenting never happens — summaries are never left orphaned.
func (s *Store) ReplaceSummariesWithDigest(ctx context.Context, dayID int64, summaryIDs []int64, digest string) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "DB.ReplaceSummariesWithDigest")
	defer span.End()

	if len(summaryIDs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("begin replace tx: %w", err)
	}
	defer tx.Rollback()

	digestID, reused, err := s.existingDigestID(ctx, tx, dayID)
	if err != nil {
		span.RecordError(err)
		return err
	}
	createdAt := s.digestCreatedAt(ctx, tx, dayID, summaryIDs)
	// digestWritten says whether this call put digest's text into the row, which is what decides below whether the text is worth embedding.
	digestWritten := strings.TrimSpace(digest) != ""
	switch {
	case !reused:
		// insert digest node — triggers nodes_ai_summary which indexes into FTS5
		var res sql.Result
		var err error
		if createdAt != "" {
			res, err = tx.ExecContext(ctx,
				`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?, 'digest', ?, ?)`,
				dayID, digest, createdAt)
		} else {
			res, err = tx.ExecContext(ctx,
				`INSERT INTO nodes (parent_id, type, content) VALUES (?, 'digest', ?)`,
				dayID, digest)
		}
		if err != nil {
			span.RecordError(err)
			return fmt.Errorf("insert digest node: %w", err)
		}
		digestID, err = res.LastInsertId()
		if err != nil {
			span.RecordError(err)
			return fmt.Errorf("digest node id: %w", err)
		}
	case digestWritten:
		// The day was digested before and is being digested again with more of its summaries. Overwrite the old text rather than dropping the new one on the floor, and move created_at forward to cover the newly included summaries.
		if createdAt != "" {
			_, err = tx.ExecContext(ctx,
				`UPDATE nodes SET content = ?, created_at = MAX(created_at, ?) WHERE id = ?`,
				digest, createdAt, digestID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE nodes SET content = ? WHERE id = ?`, digest, digestID)
		}
		if err != nil {
			span.RecordError(err)
			return fmt.Errorf("update digest node: %w", err)
		}
	}

	// Two summaries written under different original parents can carry identical content, and reparenting both under the same digest would give them the same (parent_id, type, content) — exactly what idx_nodes_unique forbids. Read each summary's content back and drop every id past the first that shares content with one already kept or with one already parented on this digest, so the reparent below only ever moves one row per distinct content. Nothing is lost: the dropped row's content is a byte-for-byte duplicate of the one that survives beside it under the digest.
	keepIDs, dropIDs, err := s.dedupeSummaryContent(ctx, tx, digestID, summaryIDs)
	if err != nil {
		span.RecordError(err)
		return err
	}
	if len(dropIDs) > 0 {
		dropSQL, dropArgs := inClause("DELETE FROM nodes WHERE id IN (", dropIDs)
		if _, err := tx.ExecContext(ctx, dropSQL, dropArgs...); err != nil {
			span.RecordError(err)
			return fmt.Errorf("drop duplicate summary nodes: %w", err)
		}
	}

	// reparent the surviving summaries under the new digest instead of deleting them — they stay searchable via their existing memory_fts rows, which this UPDATE never touches. keepIDs can be empty if every summary named was already gone by the time this ran, in which case there is nothing left to reparent.
	if len(keepIDs) > 0 {
		reparentSQL, reparentArgs := inClause("UPDATE nodes SET parent_id = ? WHERE id IN (", keepIDs)
		reparentArgs = append([]any{digestID}, reparentArgs...)
		if _, err := tx.ExecContext(ctx, reparentSQL, reparentArgs...); err != nil {
			span.RecordError(err)
			return fmt.Errorf("reparent summary nodes: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("commit replace tx: %w", err)
	}

	span.SetAttributes(
		attribute.Int64("db.day_id", dayID),
		attribute.Int("db.summaries_replaced", len(summaryIDs)),
	)

	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()

	// The reparented (surviving) summaries' vectors are left in place — those nodes are reparented, not deleted, so their embeddings still point at live rows. The dropped duplicates are a different story: their nodes are gone (deleted above), so their vector entries would otherwise sit orphaned in the index forever, the same problem DeleteNote and DeleteThread already guard against. Deleted async/best-effort, same non-blocking pattern as those, so a vector-index error here never fails the reparent the caller is waiting on.
	if vidx != nil {
		for _, id := range dropIDs {
			go func(id int64) {
				delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := vidx.Delete(delCtx, fmt.Sprintf("summary:%d", id)); err != nil {
					slog.Error("async dropped-summary vector delete failed", "summary_id", id, "error", err)
				}
			}(id)
		}
	}

	// Async, best-effort embedding of the digest text this call wrote, same non-blocking pattern as LogSemanticNode's summary embed goroutine. Without this the digest is FTS-only forever — the whole point of a digest is to still answer "what did I do that day" through the semantic half of HybridSearch. A rewritten digest is re-embedded under the same key, so the vector says what the row now says.
	if digestWritten && emb != nil && vidx != nil {
		// The vector's created_at is the day the digest covers, matching the row, so recency ranking does not treat a week-old day as today.
		stamp := nowStamp()
		if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
			stamp = t.UTC().Format(time.RFC3339)
		}
		go func(id int64, text, stamp string) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async digest embed failed", "digest_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"domain":     "", // digest nodes don't carry a domain today; key stays present so chromem's exact-match where doesn't drop this vector from domain-filtered searches
				"source":     "digest",
				"kind":       string(memory.KindPeriod),
				"created_at": stamp,
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("digest:%d", id), text, vec, meta); err != nil {
				slog.Error("async digest vector index add failed", "digest_id", id, "error", err)
			}
		}(digestID, digest, stamp)
	}
	return nil
}

// LogSemanticNode implements the memory.Storage interface: it creates task nodes and summary leaf nodes in the temporal tree.
func (s *Store) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	tracer := obs.GetTracer(ctx, "june.db")
	ctx, span := tracer.Start(ctx, "LogSemanticNode")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Every summary writer funnels through here, so strip object replacement characters once at the door: window titles carry U+FFFC into the compiler's raw-activity fallback and occasionally into LLM summaries.
	summary.TaskName = memory.StripObjectChars(summary.TaskName)
	summary.Summary = memory.StripObjectChars(summary.Summary)

	// Resolve the day this write belongs to rather than the one the process started on, so a daemon that runs past local midnight files tomorrow's summaries under tomorrow's day node.
	sessionID, err := s.sessionForDay(ctx, s.now())
	if err != nil {
		span.RecordError(err)
		return err
	}
	if sessionID != s.currentParentID {
		s.currentParentID = sessionID
		// The bookmarked task hangs off the previous day's session and cannot be continued under this one.
		s.currentTaskID = 0
		s.currentTaskName = ""
	}

	// The task node comes from this summary's own task name, every call. SameTask only says the caller believes it is continuing the thread it named, and is honoured just as a way to skip the re-ensure when the name has not changed — with several threads interleaved (the case the attribution call exists for), trusting SameTask alone filed each thread's summary under whichever task was written last.
	if !summary.SameTask || s.currentTaskID == 0 || summary.TaskName != s.currentTaskName {
		taskID, err := s.ensureNode(ctx, s.currentParentID, "task", summary.TaskName)
		if err != nil {
			span.RecordError(err)
			return err
		}
		s.currentTaskID = taskID
		s.currentTaskName = summary.TaskName
	}

	// log summary as child to task node
	payload, _ := json.Marshal(summary)
	nodeID, err := s.ensureNode(ctx, s.currentTaskID, "summary", string(payload))
	if err != nil {
		span.RecordError(err)
		return err
	}

	// Domain: majority vote over the episodes that fed this summary, not a single Classify(app,title) call — a task can span many app switches, so "the domain of this task" is whichever domain dominated those episodes. Ties broken by domain string for determinism.
	// The window starts at the later of the task node's created_at and summary.Since, the start of the flush this summary came from. Without that floor a task node that is days old (a long-running thread, or one rehydrated at startup) made every summary vote over days of unrelated activity.
	floor := "0000-01-01 00:00:00"
	if !summary.Since.IsZero() {
		floor = sqliteUTC(summary.Since)
	}
	var domain string
	if derr := s.db.QueryRowContext(ctx, `
		SELECT domain FROM episodes
		WHERE created_at >= MAX((SELECT created_at FROM nodes WHERE id = ?), ?)
		GROUP BY domain
		ORDER BY COUNT(*) DESC, domain
		LIMIT 1
	`, s.currentTaskID, floor).Scan(&domain); derr != nil && derr != sql.ErrNoRows {
		span.RecordError(derr)
		// non-fatal: domain tagging is best-effort, the summary node itself is already committed above.
	}
	if domain != "" {
		if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET domain = ? WHERE id = ?`, domain, nodeID); err != nil {
			span.RecordError(err)
		}
	}

	span.SetAttributes(
		attribute.String("db.task_name", summary.TaskName),
		attribute.Bool("db.same_task", summary.SameTask),
		attribute.String("db.summary_domain", domain),
	)

	// Async, best-effort embedding of the summary text (same non-blocking pattern as LogEpisode — see its doc comment for why this goroutine owns its own context rather than reusing ctx).
	// Snapshot under the write lock already held above — do NOT RLock again (sync.RWMutex isn't reentrant; nested RLock while holding Lock deadlocks).
	emb, vidx := s.embedder, s.vectorIndex
	if emb != nil && vidx != nil && strings.TrimSpace(summary.Summary) != "" {
		go func(id int64, text string, domain string) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async summary embed failed", "node_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"domain":     domain,
				"source":     "summary",
				"kind":       string(memory.KindPeriod),
				"created_at": nowStamp(),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("summary:%d", id), text, vec, meta); err != nil {
				slog.Error("async summary vector index add failed", "node_id", id, "error", err)
			}
		}(nodeID, summary.Summary, domain)
	}

	return nil
}

// SummaryText pulls the prose out of a summary node's content — task summaries are stored as marshalled JSON and the model should read the sentence, not the blob. Non-JSON content passes through unchanged.
func SummaryText(content string) string {
	var t struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &t); err == nil && strings.TrimSpace(t.Summary) != "" {
		return t.Summary
	}
	return content
}
