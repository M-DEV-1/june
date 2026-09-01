package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"ora/internal/memory"
	"ora/internal/obs"
	"strings"
	"time"
)

// SetWorkingState upserts the single-row working-state cache. Content is synthesized by the daemon's state deriver and overwritten in full each cadence tick — no history is kept.
func (s *Store) SetWorkingState(ctx context.Context, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
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
	tracer := obs.GetTracer(ctx, "ora.db")
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
	tracer := obs.GetTracer(ctx, "ora.db")
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
	tracer := obs.GetTracer(ctx, "ora.db")
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
	tracer := obs.GetTracer(ctx, "ora.db")
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
	tracer := obs.GetTracer(ctx, "ora.db")
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

// OldSummaryGroups returns summary nodes older than olderThan, grouped by their ancestor DAY node. Used by the compaction job to decide which days are ready to roll up.
func (s *Store) OldSummaryGroups(ctx context.Context, olderThan time.Duration) ([]memory.SummaryGroup, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.OldSummaryGroups")
	defer span.End()

	secs := int64(olderThan.Seconds())
	rows, err := s.db.QueryContext(ctx, `
		SELECT day.id, day.content, sm.id, sm.content
		FROM nodes sm
		JOIN nodes task ON sm.parent_id = task.id AND task.type = 'task'
		JOIN nodes sess ON task.parent_id = sess.id AND sess.type = 'session'
		JOIN nodes day  ON sess.parent_id = day.id  AND day.type = 'day'
		WHERE sm.type = 'summary'
		  AND sm.created_at < datetime('now', '-' || ? || ' seconds')
		ORDER BY day.id, sm.id
	`, secs)
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

// ReplaceSummariesWithDigest writes a digest node under dayID and deletes the constituent summary nodes in a single transaction. FTS5 stays correct via the nodes_ai_summary (insert) and nodes_ad_summary (delete) triggers.
// If the insert fails the deletes never happen — summaries are never lost.
func (s *Store) ReplaceSummariesWithDigest(ctx context.Context, dayID int64, summaryIDs []int64, digest string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
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

	// insert digest node — triggers nodes_ai_summary which indexes into FTS5
	res, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?, 'digest', ?)`,
		dayID, digest)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("insert digest node: %w", err)
	}
	digestID, err := res.LastInsertId()
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("digest node id: %w", err)
	}

	// delete summaries — triggers nodes_ad_summary which removes from FTS5.
	// Build a parameterized IN clause manually since the driver doesn't support []int64 expansion.
	placeholders := make([]string, len(summaryIDs))
	args := make([]any, len(summaryIDs))
	for i, id := range summaryIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	deleteSQL := "DELETE FROM nodes WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	if _, err := tx.ExecContext(ctx, deleteSQL, args...); err != nil {
		span.RecordError(err)
		return fmt.Errorf("delete summary nodes: %w", err)
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
	if vidx != nil {
		go func(ids []int64) {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, id := range ids {
				if err := vidx.Delete(delCtx, fmt.Sprintf("summary:%d", id)); err != nil {
					slog.Error("async summary vector delete failed", "summary_id", id, "error", err)
				}
			}
		}(summaryIDs)
	}

	// Async, best-effort embedding of the new digest, same non-blocking pattern as LogSemanticNode's summary embed goroutine. Without this the digest is FTS-only forever — the whole point of a digest is to still answer "what did I do that day" through the semantic half of HybridSearch.
	if emb != nil && vidx != nil && strings.TrimSpace(digest) != "" {
		go func(id int64, text string) {
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
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("digest:%d", id), text, vec, meta); err != nil {
				slog.Error("async digest vector index add failed", "digest_id", id, "error", err)
			}
		}(digestID, digest)
	}
	return nil
}

// LogSemanticNode implements the memory.Storage interface: it creates task nodes and summary leaf nodes in the temporal tree.
func (s *Store) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "LogSemanticNode")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Every summary writer funnels through here, so strip object replacement characters once at the door: window titles carry U+FFFC into the compiler's raw-activity fallback and occasionally into LLM summaries.
	summary.TaskName = memory.StripObjectChars(summary.TaskName)
	summary.Summary = memory.StripObjectChars(summary.Summary)

	// if new task, and no task id - create task node
	if !summary.SameTask || s.currentTaskID == 0 {
		taskID, err := s.ensureNode(ctx, s.currentParentID, "task", summary.TaskName)
		if err != nil {
			span.RecordError(err)
			return err
		}
		s.currentTaskID = taskID
	}

	// log summary as child to task node
	payload, _ := json.Marshal(summary)
	nodeID, err := s.ensureNode(ctx, s.currentTaskID, "summary", string(payload))
	if err != nil {
		span.RecordError(err)
		return err
	}

	// Domain: majority vote over episodes logged since the current task started (task.created_at), not a single Classify(app,title) call — a task can span many app switches, so "the domain of this task" is whichever domain dominated the episodes that fed it. Ties broken by domain string for determinism.
	var domain string
	if derr := s.db.QueryRowContext(ctx, `
		SELECT domain FROM episodes
		WHERE created_at >= (SELECT created_at FROM nodes WHERE id = ?)
		GROUP BY domain
		ORDER BY COUNT(*) DESC, domain
		LIMIT 1
	`, s.currentTaskID).Scan(&domain); derr != nil && derr != sql.ErrNoRows {
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
				"created_at": time.Now().UTC().Format(time.RFC3339),
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
