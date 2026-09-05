package dream

// This file runs the actual nightly pruning stage against the user's own store, using his own config file, and reports exactly what it would remove and keep. It never opens his real db: it asks the sqlite3 CLI to make a `.backup` copy first — safe against a database that a live daemon still has open in WAL mode — and only ever reads and prunes that copy. Skipped, not failed, wherever the real store or the sqlite3 binary is not there to find: this is a diagnostic against one specific machine's data, not a portability test.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ora/internal/config"
	"ora/internal/db"
)

// TestPruneStage_AgainstTheRealStore backs up the user's real store, runs the same two passes the nightly pruning stage runs (internal/dream/prune.go's pruneStage), and logs what each one removed and what it held back, against his real config's retention numbers. It asserts nothing about the counts beyond that the calls succeed: the point of this test is the number it prints, not a fixed expectation that would go stale the day his store legitimately grows past the retention cap.
func TestPruneStage_AgainstTheRealStore(t *testing.T) {
	src := filepath.Join(config.DataDir(), "db")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no real store at %s: %v", src, err)
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skipf("no sqlite3 binary to take a safe backup copy: %v", err)
	}

	copyPath := filepath.Join(t.TempDir(), "ora-real-copy.db")
	if out, err := exec.Command("sqlite3", src, fmt.Sprintf(".backup '%s'", copyPath)).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 .backup %s: %v: %s", src, err, out)
	}

	store, err := db.New(copyPath)
	if err != nil {
		t.Fatalf("open the backup copy: %v", err)
	}
	defer store.Close()

	cfg := config.LoadConfig()
	keep := cfg.ActRunsKept()
	failedGrace := time.Duration(cfg.FailedActRunsKeptDays()) * 24 * time.Hour

	ctx := context.Background()
	protectedConvos, err := store.ProtectedConversations(ctx, db.EmptyConversationAge)
	if err != nil {
		t.Fatalf("ProtectedConversations: %v", err)
	}
	keptForNotes, keptFailedYoung, err := store.ProtectedActRuns(ctx, failedGrace)
	if err != nil {
		t.Fatalf("ProtectedActRuns: %v", err)
	}
	removedConvos, err := store.PruneEmptyConversations(ctx, db.EmptyConversationAge)
	if err != nil {
		t.Fatalf("PruneEmptyConversations: %v", err)
	}
	removedRuns, err := store.PruneActRuns(ctx, keep, failedGrace)
	if err != nil {
		t.Fatalf("PruneActRuns: %v", err)
	}

	t.Logf("retention: act_run_keep=%d failed_grace=%v", keep, failedGrace)
	t.Logf("empty conversations: removed=%d kept(has turns or a task)=%d", removedConvos, protectedConvos)
	t.Logf("act runs: removed=%d kept(has a procedure note)=%d kept(failed, within grace)=%d", removedRuns, keptForNotes, keptFailedYoung)
}
