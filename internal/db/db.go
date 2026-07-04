package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"ora/internal/memory"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ora/internal/obs"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	_ "modernc.org/sqlite" // blank import
)

// store to hold db conn
type Store struct {
	db              *sql.DB
	mu              sync.RWMutex
	currentParentID int64 // bookmark for session
	currentTaskID   int64 // bookmark for task
}

// constructor, return pointer to struct and err
func New(path string) (*Store, error) {
	// write-ahead logging (multi tasking)
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)" // data source name
	// filenames multi-os needs to be managed

	if path != ":memory:" {
		dir := filepath.Dir(path)

		// 0755 is octal for 755 chmod with owner 421 full access, group 401 read and enter only, others 401
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn)

	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.createSchema(); err != nil {
		return nil, err
	}

	// USER --> DAY --> SESSION --> ACTIVITY
	// check for user node
	userID, err := s.ensureNode(context.Background(), 0, "user", "default_user") // make this configurable to system user
	if err != nil {
		return nil, fmt.Errorf("failed to ensure user: %w", err)
	}

	// check for day node
	today := time.Now().Format("2006-01-02") // YYYY-MM-DD
	dayID, err := s.ensureNode(context.Background(), userID, "day", today)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure day: %w", err)
	}

	// check for session node
	sessionID, err := s.ensureNode(context.Background(), dayID, "session", "Active Session")
	// TODO: semantic session naming
	if err != nil {
		return nil, fmt.Errorf("failed to ensure session: %w", err)
	}

	s.mu.Lock()
	s.currentParentID = sessionID // bookmark

	// rehydrate the latest task ID for continuity
	var taskID int64
	err = db.QueryRow("SELECT id FROM nodes WHERE parent_id = ? AND type = 'task' ORDER BY id DESC LIMIT 1", sessionID).Scan(&taskID)
	if err == nil {
		s.currentTaskID = taskID
	}
	s.mu.Unlock()

	return s, nil
}

func (s *Store) createSchema() error {
	query := `
	CREATE TABLE IF NOT EXISTS nodes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id INTEGER REFERENCES nodes(id),
		type TEXT NOT NULL,
		content TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_unique ON nodes(IFNULL(parent_id, 0), type, content);
	CREATE INDEX IF NOT EXISTS idx_parent_id ON nodes(parent_id);

	-- notes: explicit user-stated facts. always-on, small, forever.
	-- separate from nodes/tree because they're not temporal events.
	CREATE TABLE IF NOT EXISTS notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'fact',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_notes_unique ON notes(content, kind);

	-- single-row cache: synthesized "what is the user doing right now" summary.
	-- recomputed on a cadence by the daemon, disposable, replaced in full each time.
	CREATE TABLE IF NOT EXISTS working_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		content TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	-- FTS5 over summary content + note content.
	-- triggers below keep it in sync. tokenizer 'unicode61' is FTS5 default.
	CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content,
		source UNINDEXED,
		ref_id UNINDEXED,
		tokenize = 'unicode61'
	);

	-- Drop the old trigger so existing dev DBs pick up the updated WHEN clause.
	DROP TRIGGER IF EXISTS nodes_ai_summary;
	CREATE TRIGGER IF NOT EXISTS nodes_ai_summary AFTER INSERT ON nodes
	WHEN NEW.type IN ('summary','digest')
	BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, NEW.type, NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS nodes_ad_summary AFTER DELETE ON nodes
	WHEN OLD.type IN ('summary','digest')
	BEGIN
		DELETE FROM memory_fts WHERE source IN ('summary','digest') AND ref_id = OLD.id;
	END;

	CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes
	BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, 'note', NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes
	BEGIN
		DELETE FROM memory_fts WHERE source = 'note' AND ref_id = OLD.id;
	END;

	CREATE TRIGGER IF NOT EXISTS notes_au AFTER UPDATE ON notes
	BEGIN
		DELETE FROM memory_fts WHERE source = 'note' AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, 'note', NEW.id);
	END;

	-- threads: ongoing throughlines in the user's life (a show, a project, a
	-- person), each with a current state = where the user is *within* it.
	-- concurrent threads coexist; they are never collapsed into one another.
	CREATE TABLE IF NOT EXISTS threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		subject TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'work',
		state TEXT,
		salience REAL NOT NULL DEFAULT 0.5,
		times_seen INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		status TEXT NOT NULL DEFAULT 'active'
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_threads_subject ON threads(subject, kind);
	CREATE INDEX IF NOT EXISTS idx_threads_last_seen ON threads(last_seen_at);

	CREATE TRIGGER IF NOT EXISTS threads_ai AFTER INSERT ON threads BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.subject || ' — ' || IFNULL(NEW.state,''), 'thread', NEW.id);
	END;
	CREATE TRIGGER IF NOT EXISTS threads_ad AFTER DELETE ON threads BEGIN
		DELETE FROM memory_fts WHERE source='thread' AND ref_id = OLD.id;
	END;
	CREATE TRIGGER IF NOT EXISTS threads_au AFTER UPDATE ON threads BEGIN
		DELETE FROM memory_fts WHERE source='thread' AND ref_id = OLD.id;
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.subject || ' — ' || IFNULL(NEW.state,''), 'thread', NEW.id);
	END;
	`
	// db struc: USER --> DAY --> SESSION --> ACTIVITY
	// TODO: salience score to prioritize important activities and not track menial activities
	// i.e. what do we choose to remember
	_, err := s.db.Exec(query)
	return err
}

// Note is a stable, user-stated fact. Always-on in implicit context.
type Note struct {
	ID        int64
	Content   string
	Kind      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// LogNote inserts a note. Idempotent on (content, kind) — returns existing id.
func (s *Store) LogNote(ctx context.Context, content, kind string) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.LogNote")
	defer span.End()

	if kind == "" {
		kind = "fact"
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO notes (content, kind) VALUES (?, ?)`,
		content, kind); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert note: %w", err)
	}

	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM notes WHERE content = ? AND kind = ?`,
		content, kind).Scan(&id); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("read note id: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.note_id", id))
	return id, nil
}

// GetNotes returns all notes ordered newest first.
func (s *Store) GetNotes(ctx context.Context) ([]Note, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetNotes")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, kind, created_at, updated_at FROM notes ORDER BY id DESC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query notes: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Content, &n.Kind, &n.CreatedAt, &n.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note: %w", err)
		}
		out = append(out, n)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// SetWorkingState upserts the single-row working-state cache.
// Content is synthesized by the daemon's state deriver; it is
// overwritten in full each cadence tick — no history is kept.
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

// GetWorkingState returns the cached working-state content, or ("", nil)
// when no row has been written yet.
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

// RecentSummaries returns the content of the most recent summary and digest
// nodes, newest-first, up to limit rows. Used by the state deriver to build
// the synthesis prompt without walking the full ancestor tree.
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

// CountSummariesSince returns the number of summary and digest nodes created
// after since. Used as a cost guard so the state deriver skips recomputation
// when nothing new has been written.
// since is formatted as UTC "2006-01-02 15:04:05" to match SQLite's
// CURRENT_TIMESTAMP storage format, which has no sub-second component.
func (s *Store) CountSummariesSince(ctx context.Context, since time.Time) (int, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.CountSummariesSince")
	defer span.End()

	// Truncate to second precision and use >= so rows inserted within the
	// same clock-second as since are included. Production callers set since
	// from time.Now() right after a derive run; >= is safe because those rows
	// were written in the same tick and should trigger a re-derive.
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

// MemoryHit is one FTS5 row — either a summary or a note.
type MemoryHit struct {
	Content string
	Source  string // "summary" | "note"
	RefID   int64
}

// Retriever defines a minimal interface for relevance-based memory retrieval.
// FTS5 (via SearchMemory) backs it now; vector stores can implement later
// without changing callers like GetImplicitContext.
type Retriever interface {
	RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error)
}

// SearchMemory runs FTS5 over summaries + notes. Returns top 10 by rank.
// Empty query -> empty result, no error.
func (s *Store) SearchMemory(ctx context.Context, query string) ([]MemoryHit, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SearchMemory")
	defer span.End()

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// MATCH wants tokens, not raw user text. quote it so special chars
	// don't break the FTS5 parser; FTS5 phrase search is fine for v1.
	safe := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`

	rows, err := s.db.QueryContext(ctx, `
		SELECT content, source, ref_id
		FROM memory_fts
		WHERE memory_fts MATCH ?
		ORDER BY rank
		LIMIT 10
	`, safe)
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
	span.SetAttributes(attribute.Int("db.search_results", len(out)))
	return out, nil
}

// RetrieveRelevant returns up to maxItems relevance-ranked strings (notes/summaries/
// threads) by calling SearchMemory (FTS5) with sanitized focus (or fallback "recent
// context"). Formats as [note]/[summary]/[<source>]. Satisfies Retriever.
// If maxItems <= 0 all hits (up to Search limit) are returned.
func (s *Store) RetrieveRelevant(ctx context.Context, focus string, maxItems int) ([]string, error) {
	focus = strings.TrimSpace(focus)
	if focus == "" {
		focus = "recent context"
	}
	hits, err := s.SearchMemory(ctx, focus)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	var out []string
	for i, h := range hits {
		if maxItems > 0 && i >= maxItems {
			break
		}
		if h.Source == "note" {
			out = append(out, fmt.Sprintf("[note] %s", h.Content))
		} else {
			out = append(out, fmt.Sprintf("[%s] %s", h.Source, h.Content))
		}
	}
	return out, nil
}

// DeleteNote removes a note by id. FTS5 mirror is dropped via trigger.
func (s *Store) DeleteNote(ctx context.Context, id int64) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DeleteNote")
	defer span.End()

	_, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, id)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// UpdateNote overwrites the content of an existing note. FTS5 mirror is kept in
// sync via the notes_au trigger. updated_at is refreshed atomically.
func (s *Store) UpdateNote(ctx context.Context, id int64, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpdateNote")
	defer span.End()

	_, err := s.db.ExecContext(ctx,
		`UPDATE notes SET content = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		content, id)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("update note: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.note_id", id))
	return nil
}

// ExistingNotes returns id+content for every stored note. Used by the memory
// compiler to feed the reconciliation LLM call.
func (s *Store) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ExistingNotes")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM notes ORDER BY id ASC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query existing notes: %w", err)
	}
	defer rows.Close()

	var out []memory.NoteRef
	for rows.Next() {
		var n memory.NoteRef
		if err := rows.Scan(&n.ID, &n.Content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note ref: %w", err)
		}
		out = append(out, n)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// UpsertThread creates or refreshes a thread, returning its id. For an existing
// id it bumps salience/recency in place; for a new one it upserts on
// (subject, kind) so the same throughline is recognized over time.
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

// GetLiveThreads returns threads touched in the last 2 days, newest-first. This
// is the "what's going on in their life right now" view; concurrent threads
// coexist here.
func (s *Store) GetLiveThreads(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetLiveThreads")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE last_seen_at >= datetime('now','-2 days') ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// ThreadsForAttribution returns threads touched in the last 14 days, newest-first.
// Wider window than GetLiveThreads so the compiler can reattach to a throughline
// the user picked back up after a few days away.
func (s *Store) ThreadsForAttribution(ctx context.Context, limit int) ([]memory.Thread, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ThreadsForAttribution")
	defer span.End()

	return s.queryThreads(ctx, span,
		`SELECT id,subject,kind,IFNULL(state,''),salience,times_seen,last_seen_at,status FROM threads WHERE last_seen_at >= datetime('now','-14 days') ORDER BY last_seen_at DESC LIMIT ?`,
		limit)
}

// queryThreads runs a thread SELECT and scans rows. last_seen_at scans into a
// time.Time, matching how GetNotes scans Note.CreatedAt.
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

// ReplaceAllNotes atomically swaps the entire notes table for a curated set,
// used by periodic note consolidation. All notes are rewritten as kind "fact".
// FTS5 mirror stays in sync via the per-row notes_ad / notes_ai triggers.
// The caller must guarantee contents is non-empty — an empty swap would wipe
// the table — but we defend against it here too.
func (s *Store) ReplaceAllNotes(ctx context.Context, contents []string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ReplaceAllNotes")
	defer span.End()

	if len(contents) == 0 {
		return fmt.Errorf("replace all notes: refusing to wipe table with empty set")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, `DELETE FROM notes`); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: clear: %w", err)
	}

	for _, c := range contents {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO notes (content, kind) VALUES (?, 'fact')`, c); err != nil {
			span.RecordError(err)
			return fmt.Errorf("replace notes: insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: commit: %w", err)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(contents)))
	return nil
}

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

// logs current user activity
func (s *Store) LogActivity(ctx context.Context, app, title string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "LogActivity")
	defer span.End()

	// temporary app + title placeholder
	span.SetAttributes(
		attribute.String("db.app", app),
		attribute.String("db.window_title", title),
	)

	s.mu.RLock()
	parentID := s.currentParentID
	s.mu.RUnlock()

	content := fmt.Sprintf("%s | %s", app, title)
	query := `INSERT OR IGNORE INTO nodes (parent_id, type, content) VALUES (?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query, parentID, "activity", content)

	if err != nil {
		span.RecordError(err)
	}

	return err
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
	// NOTE: identity notes are NOT dumped unconditionally anymore. A wall of
	// generic "[about] user likes X" facts buried the useful live threads and
	// poisoned the model's context. Notes now surface only through the relevance
	// path below (RetrieveRelevant), gated by what the user is actually doing now.
	//
	// relevance-filtered retrieval: rather than dumping every summary, build a
	// focus signal from working state + recent task nodes and surface only the
	// notes/summaries/threads that actually match what the user is doing now.
	focusSignal := "recent context"
	if st, serr := s.GetWorkingState(ctx); serr == nil && st != "" {
		focusSignal = st
	}
	if trows, err := s.db.QueryContext(ctx, `SELECT content FROM nodes WHERE type='task' ORDER BY id DESC LIMIT 2`); err == nil && trows != nil {
		for trows.Next() {
			var tsk string
			_ = trows.Scan(&tsk)
			if tsk != "" {
				focusSignal += " " + tsk
			}
		}
		trows.Close()
	}
	const maxRel = 6
	if rel, rerr := s.RetrieveRelevant(ctx, focusSignal, maxRel); rerr == nil {
		branch = append(branch, rel...)
	}
	// live threads: what's going on in their life right now (recency = relevance).
	// concurrent threads coexist here — watching + coding both surface.
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
	// synthesized "right now".
	state, serr := s.GetWorkingState(ctx)
	if serr == nil && state != "" {
		branch = append(branch, fmt.Sprintf("[now] %s", state))
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

// OldSummaryGroups returns summary nodes older than olderThan, grouped by
// their ancestor DAY node. Used by the compaction job to decide which days
// are ready to roll up.
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

// ReplaceSummariesWithDigest writes a digest node under dayID and deletes the
// constituent summary nodes in a single transaction. FTS5 stays correct via the
// nodes_ai_summary (insert) and nodes_ad_summary (delete) triggers.
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?, 'digest', ?)`,
		dayID, digest); err != nil {
		span.RecordError(err)
		return fmt.Errorf("insert digest node: %w", err)
	}

	// delete summaries — triggers nodes_ad_summary which removes from FTS5.
	// Build a parameterized IN clause manually; the driver does not support
	// []int64 expansion, so we construct the placeholders as a string.
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
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// implements the memory.Storage interface.
// It creates task nodes and summary leaf nodes in the temporal tree.
func (s *Store) LogSemanticNode(ctx context.Context, summary memory.TaskSummary) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "LogSemanticNode")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

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
	_, err := s.ensureNode(ctx, s.currentTaskID, "summary", string(payload))
	if err != nil {
		span.RecordError(err)
		return err
	}

	span.SetAttributes(
		attribute.String("db.task_name", summary.TaskName),
		attribute.Bool("db.same_task", summary.SameTask),
	)

	return nil
}

// DB exposes the underlying connection for test-only raw queries.
func (s *Store) DB() *sql.DB { return s.db }

// CullRawActivities deletes activity nodes older than olderThan. Summaries,
// tasks, sessions, days, users, and notes are never touched.
// Returns the number of rows deleted.
func (s *Store) CullRawActivities(ctx context.Context, olderThan time.Duration) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.CullRawActivities")
	defer span.End()

	secs := int64(olderThan.Seconds())
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM nodes WHERE type = 'activity' AND created_at < datetime('now', '-' || ? || ' seconds')`,
		secs,
	)
	if err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("cull activities: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.culled_rows", n))
	return n, nil
}
