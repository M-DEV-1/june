// This file is the guard on PRAGMA foreign_keys. SQLite defaults the pragma to off and applies it per connection, not per database, so every ON DELETE CASCADE in the schema is inert until each connection the pool opens turns it on. These tests are file-backed on purpose: an in-memory store pins the pool to one connection (see New) and so could not tell a pragma set on every connection from one set once by hand.
package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// newFileStore opens a throwaway store backed by a real file, closed when the test ends. The file matters: only a file-backed store keeps the full connection pool, which is what the per-connection pragma has to be proved against.
func newFileStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "june.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestForeignKeysOnEveryPooledConnection holds several connections from the pool open at once and reads PRAGMA foreign_keys on each. Holding them simultaneously is what forces the pool to open distinct connections: a pragma set once after sql.Open would show as on for the first connection and off for every later one, which is the bug this guards.
func TestForeignKeysOnEveryPooledConnection(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	const connections = 4
	var held []*sql.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()

	for i := 0; i < connections; i++ {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("open connection %d: %v", i, err)
		}
		held = append(held, conn)

		var on int
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
			t.Fatalf("read foreign_keys on connection %d: %v", i, err)
		}
		if on != 1 {
			t.Errorf("connection %d has foreign_keys = %d, want 1", i, on)
		}
	}
}
