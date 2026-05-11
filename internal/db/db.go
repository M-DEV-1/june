package db

import (
	"context"
	"database/sql"
	"fmt"
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
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)" // data source name
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

	userID, err := s.ensureUser("default_user") // make this configurable to system user
	if err != nil {
		return nil, fmt.Errorf("failed to ensure user: %w", err)
	}

	today := time.Now().Format("2006-01-02") // YYYY-MM-DD
	var dayID int64

	err = s.db.QueryRow("SELECT id FROM nodes WHERE type = 'day' AND content = ? AND parent_id = ?", today, userID).Scan(&dayID)
	if err == sql.ErrNoRows {
		res, insertErr := s.db.Exec("INSERT INTO nodes (parent_id, type, content) VALUES (?, ?, ?)", userID, "day", today)
		if insertErr != nil {
			return nil, fmt.Errorf("failed to insert day node: %w", insertErr)
		}
		dayID, _ = res.LastInsertId()
	} else if err != nil {
		return nil, fmt.Errorf("failed to query for day node: %w", err)
	}

	s.currentParentID = dayID

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
	_, err := s.db.Exec(query)
	return err
}

// get or create
func (s *Store) ensureUser(username string) (int64, error) {
	var id int64

	// get user id
	err := s.db.QueryRow("SELECT id FROM nodes WHERE type = 'user' AND content = ?", username).Scan(&id)

	// create user if not found, return created user id
	if err == sql.ErrNoRows {
		fmt.Println("Existing user not found, initializing new user...")
		res, err := s.db.Exec("INSERT INTO nodes (type, content) VALUES (?, ?)", "user", username)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
		// returns both id and err
	}

	return id, err
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
		WHERE id = (SELECT MAX(id) FROM nodes)

		UNION ALL	

		-- recursively join to parent
		SELECT n.id, n.parent_id, n.type, n.content 
		FROM nodes n
		JOIN branch b ON n.id = b.parent_id
	)
	-- select in ascending order
	SELECT type, content FROM branch ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query)

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
