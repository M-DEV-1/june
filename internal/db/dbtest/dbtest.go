// Package dbtest holds the one store constructor every other package's tests need, so each package stops hand-rolling its own copy.
package dbtest

import (
	"path/filepath"
	"testing"

	"june/internal/db"
)

// Open creates a file-backed store in a fresh temp directory that is closed automatically when the test ends.
// Input: t, the test or benchmark controller that owns the temp directory and the cleanup. Output: a ready *db.Store, or the test is failed immediately if the store cannot be created.
// It is file-backed rather than ":memory:" because a second connection against ":memory:" sees an empty, unrelated database in database/sql plus modernc/sqlite, which breaks any test that queries concurrently or reopens the store.
func Open(t testing.TB) *db.Store {
	t.Helper()
	store, err := db.New(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
