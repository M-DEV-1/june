package db

import (
	"context"
	"database/sql"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"log/slog"
	"ora/internal/memory"
	"ora/internal/obs"
	"strings"
	"time"
)

// Thread and ThreadUpdate are the thread row and the compiler's attribution of a buffer slice to one. Aliases, not new types: they are defined in internal/memory, which owns the attribution logic that produces them, but they come back out of this package's own queries — so a caller reading db's API can name them db.Thread without importing memory for a type it only wants to spell. When these rows eventually move to their own vocabulary package, the alias absorbs the move and no caller changes.
type Thread = memory.Thread

// ThreadUpdate is one attributed slice, the input side of UpsertThread. See Thread above for why this is an alias.
type ThreadUpdate = memory.ThreadUpdate

// UpsertThread creates or refreshes a thread, returning its id. For an existing id it bumps salience/recency in place; for a new one it upserts on (subject, kind) so the same throughline is recognized over time.
func (s *Store) UpsertThread(ctx context.Context, u memory.ThreadUpdate) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpsertThread")
	defer span.End()

	if u.ID > 0 {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE threads SET state=?, last_seen_at=CURRENT_TIMESTAMP, times_seen=times_seen+1, salience=MIN(1.0, salience+0.05), status='active' WHERE id=?`,
			u.State, u.ID); err != nil {
			span.RecordError(err)
			return 0, fmt.Errorf("update thread: %w", err)
		}
		span.SetAttributes(attribute.Int64("db.thread_id", u.ID))
		return u.ID, nil
	}

	// new throughline: bias salience up slightly when the model flags it novel.
	salience := 0.5
	if u.Novel {
		salience = 0.6
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO threads(subject,kind,state,salience,times_seen) VALUES(?,?,?,?,1)
		 ON CONFLICT(subject,kind) DO UPDATE SET state=excluded.state, last_seen_at=CURRENT_TIMESTAMP, times_seen=threads.times_seen+1, salience=MIN(1.0, threads.salience+0.05), status='active'`,
		u.Subject, u.Kind, u.State, salience); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert thread: %w", err)
	}

	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM threads WHERE subject=? AND kind=?`,
		u.Subject, u.Kind).Scan(&id); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("read thread id: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.thread_id", id))
	return id, nil
}

// UpdateThreadState overwrites an existing thread's state — the one-line summary of where that throughline stands — leaving its subject and kind alone. This is the repair path for a thread whose summary merged two unrelated things or recorded a wrong fact; the model can see that from a "[thread#N]" hit but had no way to act on it, since update_note only reaches the notes table.
// The FTS5 mirror is kept in sync by the threads_au trigger. The stale vector is deleted and the corrected text re-embedded async/best-effort, same non-blocking pattern as UpdateNote — a vector-index error never fails the SQL update. The embed text is "subject — state", matching the threads_ai trigger so both halves of hybrid search see the same thread.
// Input: the thread's id and the corrected state. Output: an error if no thread carries that id.
func (s *Store) UpdateThreadState(ctx context.Context, id int64, state string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpdateThreadState")
	defer span.End()
	span.SetAttributes(attribute.Int64("db.thread_id", id))

	var subject string
	// A no-op UPDATE is not success: the model can hand us an id it invented, and reporting the fix as done throws the user's correction away. Reading the subject first both catches that and gives the re-embed its text.
	if err := s.db.QueryRowContext(ctx, `SELECT subject FROM threads WHERE id = ?`, id).Scan(&subject); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no thread with id %d", id)
		}
		span.RecordError(err)
		return fmt.Errorf("update thread state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE threads SET state = ?, last_seen_at = CURRENT_TIMESTAMP WHERE id = ?`, state, id); err != nil {
		span.RecordError(err)
		return fmt.Errorf("update thread state: %w", err)
	}

	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if vidx == nil {
		return nil
	}
	text := subject
	if strings.TrimSpace(state) != "" {
		text = subject + " — " + state
	}
	go func(id int64, text string) {
		vecCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		vecID := fmt.Sprintf("thread:%d", id)
		if err := vidx.Delete(vecCtx, vecID); err != nil {
			slog.Error("async thread vector delete (pre-update) failed", "thread_id", id, "error", err)
		}
		if emb == nil {
			return
		}
		vec, err := emb.Embed(vecCtx, "RETRIEVAL_DOCUMENT", text)
		if err != nil {
			slog.Error("async thread re-embed failed", "thread_id", id, "error", err)
			return
		}
		meta := map[string]string{
			"source":     "thread",
			"kind":       string(memory.KindArc),
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		if err := vidx.Add(vecCtx, vecID, text, vec, meta); err != nil {
			slog.Error("async thread vector re-add failed", "thread_id", id, "error", err)
		}
	}(id, text)
	return nil
}

// GetLiveThreads returns threads touched in the last 2 days, newest-first. This is the "what's going on in their life right now" view; concurrent threads coexist here.
func (s *Store) GetLiveThreads(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetLiveThreads")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE last_seen_at >= datetime('now','-2 days') ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// ActiveThreads returns up to limit status='active' threads, most recently seen first. No recency window on purpose: the dreaming loop wants the standing picture of what is going on in the user's life, not just the last two days of it.
func (s *Store) ActiveThreads(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ActiveThreads")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE status = 'active' ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// ThreadsForAttribution returns threads touched in the last 14 days, newest-first. Wider window than GetLiveThreads so the compiler can reattach to a throughline the user picked back up after a few days away.
func (s *Store) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ThreadsForAttribution")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE last_seen_at >= datetime('now','-14 days') ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// queryThreads runs a thread SELECT and scans rows. last_seen_at scans into a time.Time, matching how GetNotes scans Note.CreatedAt.
func (s *Store) queryThreads(ctx context.Context, span trace.Span, query string, limit int) ([]memory.Thread, error) {
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query threads: %w", err)
	}
	defer rows.Close()

	var out []memory.Thread
	for rows.Next() {
		var t memory.Thread
		if err := rows.Scan(&t.ID, &t.Subject, &t.Kind, &t.State, &t.Salience, &t.TimesSeen, &t.LastSeen, &t.Status); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate threads: %w", err)
	}
	span.SetAttributes(attribute.Int("db.thread_count", len(out)))
	return out, nil
}

// LinkEpisodesToThread records that every episode captured between since and until belongs to one ongoing thread. The compiler calls it after attributing a flushed buffer, which is the moment — and the only moment — that the connection is known.
// The window is used rather than a list of ids because the compiler buffers activities before they have ids, and the buffer's own span is already exactly the stretch the attribution covered. Re-linking the same pair is a no-op, so a re-run or an overlapping window costs nothing.
// ponytail: every episode in the window is linked to every thread the flush produced. When one stretch of work genuinely covered two threads at once, both get all of it — truthful at the level the attribution was made, but coarser than tagging each episode individually. Per-episode precision means carrying episode ids through the compiler's buffer; do that if the joined evidence turns out to be too noisy to read.
func (s *Store) LinkEpisodesToThread(ctx context.Context, threadID int64, since, until time.Time) error {
	if threadID <= 0 {
		return fmt.Errorf("link episodes: thread id must be positive, got %d", threadID)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO episode_threads(episode_id, thread_id)
		SELECT id, ? FROM episodes WHERE created_at >= ? AND created_at <= ?`,
		threadID, sqliteUTC(since), sqliteUTC(until))
	if err != nil {
		return fmt.Errorf("link episodes to thread %d: %w", threadID, err)
	}
	return nil
}

// EpisodesForThread returns the captures behind one thread, newest first. This is the walk a thread's summary could never support on its own: "reviewed the code, eleven findings" leads here, to the screens the findings were actually on.
func (s *Store) EpisodesForThread(ctx context.Context, threadID int64, limit int) ([]Episode, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.created_at, e.app, e.title, e.screen_text, e.importance, e.domain,
		       e.user_activity, e.visible_text, e.image_path
		FROM episodes e
		JOIN episode_threads et ON et.episode_id = e.id
		WHERE et.thread_id = ?
		ORDER BY e.created_at DESC
		LIMIT ?`, threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("episodes for thread %d: %w", threadID, err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.App, &e.Title, &e.ScreenText, &e.Importance,
			&e.Domain, &e.UserActivity, &e.VisibleText, &e.ImagePath); err != nil {
			return nil, fmt.Errorf("scan episode for thread: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
