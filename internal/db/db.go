package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // blank import
)

// store to hold db conn
type Store struct {
	db *sql.DB
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

	return &Store{db: db}, err
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

// methods over struct Store
func (s *Store) LogActivity(ctx context.Context, app, title string) error {
	return ctx.Err()
}

func (s *Store) GetImplicitContext(ctx context.Context) ([]string, error) {
	return []string{"mock context"}, nil
}

func (s *Store) Close() error {
	return nil
}
