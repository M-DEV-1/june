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

	-- FTS5 over summary content + note content.
	-- triggers below keep it in sync. tokenizer 'unicode61' is FTS5 default.
	CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content,
		source UNINDEXED,
		ref_id UNINDEXED,
		tokenize = 'unicode61'
	);

	CREATE TRIGGER IF NOT EXISTS nodes_ai_summary AFTER INSERT ON nodes
	WHEN NEW.type = 'summary'
	BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, 'summary', NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes
	BEGIN
		INSERT INTO memory_fts(content, source, ref_id) VALUES (NEW.content, 'note', NEW.id);
	END;

	CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes
	BEGIN
		DELETE FROM memory_fts WHERE source = 'note' AND ref_id = OLD.id;
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

// MemoryHit is one FTS5 row — either a summary or a note.
type MemoryHit struct {
	Content string
	Source  string // "summary" | "note"
	RefID   int64
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

	// notes go first — they're "who is this user" context, always on top.
	// fetch BEFORE opening CTE rows; sqlite `:memory:` per-connection
	// isolation means an interleaved query would see an empty schema.
	notes, nerr := s.GetNotes(ctx)
	if nerr == nil {
		for _, n := range notes {
			branch = append(branch, fmt.Sprintf("[note:%s] %s", n.Kind, n.Content))
		}
	}

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

// searches historic summaries
func (s *Store) QueryMemory(ctx context.Context, query string) ([]string, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "QueryMemory")
	defer span.End()

	// simple LIKE query on summary nodes
	sqlQuery := `
		SELECT content 
		FROM nodes 
		WHERE type = 'summary' AND content LIKE ?
		ORDER BY id DESC 
		LIMIT 10
	`

	rows, err := s.db.QueryContext(ctx, sqlQuery, "%"+query+"%")
	// wildcard search i.e. "any length"
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("failed to search memory: %w", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, content)
	}

	span.SetAttributes(attribute.Int("db.search_results", len(results)))
	return results, nil
}
