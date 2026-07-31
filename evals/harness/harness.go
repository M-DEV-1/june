// harness gives every eval package the same store setup.
package harness

import (
	"path/filepath"
	"testing"

	"ora/internal/db"
)

// prod opens db.New("ora-db/db"), a real file in WAL mode.
// we do the same here, just under a tmp dir.
// keeps evals on the same sqlite setup as the real thing instead of :memory:.
func NewStore(t *testing.T) *db.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ora-db", "db")
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("harness: open sqlite store at %s: %v", path, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
