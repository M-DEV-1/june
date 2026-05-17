package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/json"
	"fmt"
	"ora/internal/memory"
	"os"
	"path/filepath"
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
	userID, err := s.ensureNode(0, "user", "default_user") // make this configurable to system user
	if err != nil {
		return nil, fmt.Errorf("failed to ensure user: %w", err)
	}

	// check for day node
	today := time.Now().Format("2006-01-02") // YYYY-MM-DD
	dayID, err := s.ensureNode(userID, "day", today)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure day: %w", err)
	}

	// check for session node
	sessionID, err := s.ensureNode(dayID, "session", "Active Session")
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
	`
	// db struc: USER --> DAY --> SESSION --> ACTIVITY
	// TODO: salience score to prioritize important activities and not track menial activities
	// i.e. what do we choose to remember
	_, err := s.db.Exec(query)
	return err
}

// get or create
func (s *Store) ensureNode(parentID int64, nodeType, content string) (int64, error) {
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
	// bottom to top search of db, flips it around for readability in the end
	query := `WITH RECURSIVE branch AS (

		-- most recent activity
		SELECT id, parent_id, type, content 
		FROM nodes 
		WHERE id = (
			SELECT MAX(id) FROM nodes
			WHERE type = 'activity' AND parent_id = ?
		)
		UNION ALL	

		-- recursively join to parent
		SELECT n.id, n.parent_id, n.type, n.content 
		FROM nodes n
		JOIN branch b ON n.id = b.parent_id
	)
	-- select in ascending order
	SELECT type, content FROM branch ORDER BY id ASC`

	s.mu.RLock()
	parentID := s.currentParentID
	s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query, parentID)

	if err != nil {
		return nil, fmt.Errorf("failed to query context: %w", err)
	}

	defer rows.Close()

	var branch []string

	for rows.Next() {
		var nodeType, content string

		if err := rows.Scan(&nodeType, &content); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		nodeString := fmt.Sprintf("[%s] %s", nodeType, content)
		branch = append(branch, nodeString)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	// some db specific metadata
	// this span will return exact no of nodes returned for a specific request
	span.SetAttributes(attribute.Int("db.node_count", len(branch)))
	return branch, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}
