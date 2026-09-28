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

// TestTurnAgainstMissingConversationIsRefused writes straight to conversation_turns, past AddTurn's own existence check, with a conversation id that names nothing. With foreign keys enforced the database itself refuses the row.
func TestTurnAgainstMissingConversationIsRefused(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	_, err := store.db.ExecContext(ctx,
		`INSERT INTO conversation_turns (conversation_id, role, text, kind) VALUES (?, 'you', 'into the void', 'ask')`, 999999)
	if err == nil {
		t.Fatalf("a turn against a conversation that does not exist was stored")
	}

	var stored int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_turns`).Scan(&stored); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if stored != 0 {
		t.Errorf("the refused insert left %d rows in conversation_turns, want none", stored)
	}
}

// TestDeletingAConversationCascadesItsTurns deletes the conversation row alone, without the explicit turn delete DeleteConversation does, and expects the declared ON DELETE CASCADE to take the turns with it.
func TestDeletingAConversationCascadesItsTurns(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "when does it leave", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "you", "when does it leave", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}

	var left int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_turns WHERE conversation_id = ?`, id).Scan(&left); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if left != 0 {
		t.Errorf("deleting the conversation left %d of its turns behind, want none", left)
	}
}

// TestDeletingAnEpisodeCascadesItsThreadEdges deletes one episode and expects its rows in episode_threads to go with it, since an edge to an episode that no longer exists is not a fact about anything.
func TestDeletingAnEpisodeCascadesItsThreadEdges(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	episodeID, err := store.LogEpisode(ctx, "Brave", "pricing page", "seat pricing")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	res, err := store.db.ExecContext(ctx, `INSERT INTO threads (subject, kind, state) VALUES ('pricing', 'work', 'reading the seat tiers')`)
	if err != nil {
		t.Fatalf("insert thread: %v", err)
	}
	threadID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("thread id: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO episode_threads (episode_id, thread_id) VALUES (?, ?)`, episodeID, threadID); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM episodes WHERE id = ?`, episodeID); err != nil {
		t.Fatalf("delete episode: %v", err)
	}

	var edges int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM episode_threads WHERE episode_id = ?`, episodeID).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 0 {
		t.Errorf("deleting the episode left %d dangling edges in episode_threads, want none", edges)
	}
}

// TestEdgeToAMissingThreadIsRefused writes an edge naming a thread that does not exist, which is what LinkEpisodesToThread would do if it were handed a stale thread id.
func TestEdgeToAMissingThreadIsRefused(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	episodeID, err := store.LogEpisode(ctx, "Brave", "pricing page", "seat pricing")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO episode_threads (episode_id, thread_id) VALUES (?, ?)`, episodeID, 424242); err != nil {
		return
	}
	t.Fatalf("an edge to a thread that does not exist was stored")
}
