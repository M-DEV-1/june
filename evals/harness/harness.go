// harness gives every eval package the same store setup: a throwaway file-backed sqlite, or a snapshot of the user's real ora-db so questions can be grounded in actual memory.
package harness

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ora/internal/db"

	_ "modernc.org/sqlite"
)

// NewStore opens db.New under a tmp dir — the same WAL sqlite setup production uses, empty so a test can seed its own fixtures.
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

// ProductionDBPath is ora-db/db at the repo root, or ORA_DB if set. Empty string means the file isn't there.
func ProductionDBPath() string {
	if p := strings.TrimSpace(os.Getenv("ORA_DB")); p != "" {
		return p
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	return filepath.Join(root, "ora-db", "db")
}

// OpenSnapshot copies the production sqlite into a temp dir and opens that copy. Evals read (and may write) the copy, never the live ora-db. Skips the test when no production db is present.
func OpenSnapshot(t *testing.T) *db.Store {
	t.Helper()
	src := ProductionDBPath()
	if src == "" {
		t.Skip("harness: no production db path")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("harness: no production db at %s", src)
	}
	dst := filepath.Join(t.TempDir(), "ora-db", "db")
	if err := copySQLite(src, dst); err != nil {
		t.Fatalf("harness: snapshot %s -> %s: %v", src, dst, err)
	}
	store, err := db.New(dst)
	if err != nil {
		t.Fatalf("harness: open snapshot at %s: %v", dst, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// copySQLite writes a consistent copy of src to dst via VACUUM INTO (works while the live db is in WAL). Falls back to copying the db file plus WAL/SHM sidecars if VACUUM INTO fails.
func copySQLite(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	conn, err := sql.Open("sqlite", "file:"+srcAbs+"?mode=ro&_pragma=busy_timeout(8000)")
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Exec("VACUUM INTO " + sqliteQuote(dstAbs)); err == nil {
		return nil
	}
	if err := copyFile(srcAbs, dstAbs); err != nil {
		return err
	}
	for _, side := range []string{"-wal", "-shm"} {
		_ = copyFile(srcAbs+side, dstAbs+side)
	}
	return nil
}

func sqliteQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "''") + "'"
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0600)
}
