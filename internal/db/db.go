package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // blank import
)

// store to hold db conn
type Store struct {
	db              *sql.DB
	currentParentID int64 // bookmark
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

	s.currentParentID = sessionID // bookmark

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
	var err error

	if parentID == 0 {
		// ROOT, i.e. user
		query := `SELECT id FROM nodes WHERE parent_id IS NULL AND type = ? AND content = ?`
		err = s.db.QueryRow(query, nodeType, content).Scan(&id)
	} else {
		// CHILD, i.e. day, session or activity
		query := `SELECT id FROM nodes WHERE parent_id = ? AND type = ? AND content = ?`
		err = s.db.QueryRow(query, parentID, nodeType, content).Scan(&id)
	}

	if err == sql.ErrNoRows {
		var res sql.Result
		var insertErr error
		if parentID == 0 {
			// fmt.Println("Existing user not found, intializing new user...")
			query := `INSERT INTO nodes (type, content) VALUES (?, ?)`
			res, insertErr = s.db.Exec(query, nodeType, content)
		} else {
			query := `INSERT INTO nodes (parent_id, type, content) VALUES (?, ?, ?)`
			res, insertErr = s.db.Exec(query, parentID, nodeType, content)
		}

		if insertErr != nil {
			return 0, fmt.Errorf("failed to ensure %s node: %w", nodeType, insertErr)
		}

		id, err = res.LastInsertId()

		if err != nil {
			return 0, fmt.Errorf("failed to get last insert id: %w", err)
		}
	} else if err != nil {
		return 0, err
	}

	return id, nil
}

// logs current user activity
func (s *Store) LogActivity(ctx context.Context, app, title string) error {
	// temporary app + title placeholder

	content := fmt.Sprintf("%s | %s", app, title)
	query := `INSERT INTO nodes (parent_id, type, content) VALUES (?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query, s.currentParentID, "activity", content)

	return err
}

func (s *Store) GetImplicitContext(ctx context.Context) ([]string, error) {
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

	rows, err := s.db.QueryContext(ctx, query, s.currentParentID)

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

	return branch, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}
