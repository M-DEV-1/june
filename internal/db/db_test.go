package db_test

// tests are first class citizens

import (
	"context"
	"database/sql"
	"fmt"
	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore opens a throwaway store in a fresh temp directory that is closed when the test ends — a thin alias over dbtest.Open kept so every call site in this package doesn't need its own import.
func memStore(t *testing.T) *db.Store {
	t.Helper()
	return dbtest.Open(t)
}

// TestStore_LogNote_NormalizesCaseAndWhitespaceForDedup proves the fix for the "paraphrased restatement creates a duplicate row" problem: LogNote used to dedupe on an exact (content, kind) match only, so re-logging the same fact with different casing/whitespace created a second row instead of reconciling. Content is now normalized (trimmed, whitespace collapsed, lowercased) before the dedup check.
func TestStore_LogNote_NormalizesCaseAndWhitespaceForDedup(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id1, err := store.LogNote(ctx, "User likes Go", "fact")
	if err != nil {
		t.Fatalf("LogNote (first): %v", err)
	}

	id2, err := store.LogNote(ctx, "  user   likes   GO  ", "fact")
	if err != nil {
		t.Fatalf("LogNote (restated, different case/whitespace): %v", err)
	}
	if id2 != id1 {
		t.Errorf("expected paraphrased restatement to collapse to the same row, got id1=%d id2=%d", id1, id2)
	}

	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("want exactly 1 note after normalized dedup, got %d: %+v", len(notes), notes)
	}
	// storage is NOT normalized — the first-logged casing/whitespace wins and is what every subsequent paraphrased restatement resolves back to.
	if notes[0].Content != "User likes Go" {
		t.Errorf("expected original first-logged casing to survive in storage, got %q", notes[0].Content)
	}

	// A genuinely different fact must still get its own row — normalization must not over-collapse unrelated content.
	if _, err := store.LogNote(ctx, "User likes Python", "fact"); err != nil {
		t.Fatalf("LogNote (distinct fact): %v", err)
	}
	notes, err = store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes (after distinct fact): %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("want 2 notes (distinct facts must not collapse), got %d: %+v", len(notes), notes)
	}
}

// TestStore_GetImplicitContext_GatesIrrelevantNotes pins the fix for "bombarding notes with no point → agent spews bullshit with no context": implicit context must NOT dump identity notes unconditionally. A note unrelated to what the user is doing now stays out; the live thread matching current focus is what surfaces.
func TestStore_GetImplicitContext_GatesIrrelevantNotes(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	// current focus: debugging the Brightpath portal
	if err := store.SetWorkingState(ctx, "debugging the Brightpath Benchmarking Portal backend"); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}
	// a live thread that matches what the user is doing now
	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "Brightpath Benchmarking Portal",
		Kind:    "work",
		State:   "Monitoring CRD dashboard while debugging backend",
	}); err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}
	// a durable identity note with nothing to do with the current focus
	if _, err := store.LogNote(ctx, "user has an interest in vintage camera repair", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}
	joined := strings.Join(branch, "\n")

	// the live thread must surface — that's the useful recall
	if !strings.Contains(joined, "Brightpath Benchmarking Portal") {
		t.Errorf("expected live thread in context, got: %+v", branch)
	}
	// the irrelevant identity note must NOT be dumped in unconditionally
	if strings.Contains(joined, "vintage camera repair") {
		t.Errorf("irrelevant note leaked into context (unconditional note dump): %+v", branch)
	}
}

// TestStore_SearchMemory_NaturalLanguageQuery_ORofTerms verifies the fix for the whole-query phrase-quoting bug: MATCH used to wrap the entire query as one FTS5 phrase, which only matches content containing that exact contiguous run of words. Tokenizing into an OR-of-terms MATCH means any significant term (here "websocket"/"reconnect") is enough to recall the summary, even without a verbatim match.
func TestStore_SearchMemory_NaturalLanguageQuery_ORofTerms(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Voice Pipeline",
		Summary:  "Debugging WebSocket reconnect loop in Gemini Live session",
	})

	hits, err := store.SearchMemory(ctx, "what was I doing with the websocket reconnect")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected OR-of-terms match on a natural-language query that shares no verbatim phrase with stored content, got no hits")
	}
}

func TestStore_UpdateNote_AndFTSSync(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id, err := store.LogNote(ctx, "user prefers terse responses", "fact")
	if err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	// old content must be searchable before update
	hits, err := store.SearchMemory(ctx, "terse")
	if err != nil {
		t.Fatalf("SearchMemory pre-update: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected FTS hit for old content before update")
	}

	if err := store.UpdateNote(ctx, id, "user prefers terse and concise responses"); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}

	// new content must be searchable
	hits, err = store.SearchMemory(ctx, "concise")
	if err != nil {
		t.Fatalf("SearchMemory post-update new term: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS did not index new content after UpdateNote")
	}
	if !strings.Contains(hits[0].Content, "concise") {
		t.Errorf("unexpected FTS hit content: %s", hits[0].Content)
	}

	// notes table itself must reflect the new content
	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].Content != "user prefers terse and concise responses" {
		t.Errorf("GetNotes returned unexpected content: %+v", notes)
	}
}

// TestStore_GetImplicitContext_DoesNotLeakStaleTaskAcrossContexts guards against the missing time bound in GetImplicitContext's focus signal: it folds the last 2 task names in unconditionally, by id, so a stale task from days ago can still be "recent by id" and self-match its own summary back into context regardless of relevance. This reproduces the "filing facts bleed into an unrelated project" bug as a concrete test.
func TestStore_GetImplicitContext_DoesNotLeakStaleTaskAcrossContexts(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Quarterly Filing Review",
		Summary:  "Reviewed the Q1 filing line by line",
	}); err != nil {
		t.Fatalf("LogSemanticNode (old task): %v", err)
	}

	// backdate the old task node so it's genuinely stale, not just "not the most recent" — the bug is a missing time bound, not a missing id bound.
	staleTime := time.Now().Add(-72 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE nodes SET created_at = ? WHERE type = 'task' AND content = ?`,
		staleTime, "Quarterly Filing Review"); err != nil {
		t.Fatalf("backdate old task: %v", err)
	}

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Audio Pipeline Fix",
		Summary:  "Chasing a crackle in the Linux audio pipeline",
	}); err != nil {
		t.Fatalf("LogSemanticNode (new task): %v", err)
	}

	const state = "user is actively debugging the Linux audio pipeline and writing TDD tests"
	if err := store.SetWorkingState(ctx, state); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}
	for _, line := range branch {
		if strings.Contains(line, "Quarterly Filing") {
			t.Errorf("stale, unrelated task summary leaked into a fresh working-state context: %+v", branch)
		}
	}
}

// Two summaries written under different original parents can carry identical content — nothing stops two unrelated activities being written up in the same words. Reparenting both under the same new digest would give them the same (parent_id, type, content), which idx_nodes_unique forbids; the batch must survive that instead of failing the whole compaction and retrying forever.
func TestStore_ReplaceSummariesWithDigest_ToleratesDuplicateContentAmongSummaries(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)
	raw := store.DB()

	var userID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='user' LIMIT 1`).Scan(&userID); err != nil {
		t.Fatalf("find user node: %v", err)
	}
	var dayID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,?,?) RETURNING id`,
		userID, "day", "2026-08-01").Scan(&dayID); err != nil {
		t.Fatalf("insert day node: %v", err)
	}

	const dupContent = "fixed the flaky test uniquedup"
	var taskAID, taskBID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,?,?) RETURNING id`, dayID, "task", "Task A").Scan(&taskAID); err != nil {
		t.Fatalf("insert task A: %v", err)
	}
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,?,?) RETURNING id`, dayID, "task", "Task B").Scan(&taskBID); err != nil {
		t.Fatalf("insert task B: %v", err)
	}
	var sumA, sumB int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,'summary',?) RETURNING id`, taskAID, dupContent).Scan(&sumA); err != nil {
		t.Fatalf("insert summary A: %v", err)
	}
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,'summary',?) RETURNING id`, taskBID, dupContent).Scan(&sumB); err != nil {
		t.Fatalf("insert summary B: %v", err)
	}

	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{sumA, sumB}, "digest covering both tasks"); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest with duplicate summary content: %v", err)
	}

	var digestID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestID); err != nil {
		t.Fatalf("find digest node: %v", err)
	}

	var survivors int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='summary' AND parent_id=? AND content=?`, digestID, dupContent).Scan(&survivors); err != nil {
		t.Fatalf("count surviving summaries: %v", err)
	}
	if survivors != 1 {
		t.Errorf("expected exactly 1 surviving summary under the digest once the duplicate content is deduped, got %d", survivors)
	}

	hits, err := store.SearchMemory(ctx, "uniquedup")
	if err != nil {
		t.Fatalf("SearchMemory for the deduped summary term: %v", err)
	}
	if len(hits) == 0 {
		t.Error("FTS lost the deduped summary's term entirely")
	}
}

// A real store was found on 2026-09-05 with a day whose digest already existed but only some of its summaries had been reparented under it — the shape the pre-fix unique-index collision above left behind: the transaction's digest insert survived, its reparent update did not, on some batches. The next compaction pass for that day must not insert a second digest; it must find the one already there and finish reparenting whatever is still loose under it, including deduping a loose summary whose content already matches one already parented on the digest.
func TestStore_ReplaceSummariesWithDigest_ResumesADayWithAnExistingDigestAndLooseSummaries(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)
	raw := store.DB()

	var userID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='user' LIMIT 1`).Scan(&userID); err != nil {
		t.Fatalf("find user node: %v", err)
	}
	var dayID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,?,?) RETURNING id`,
		userID, "day", "2026-08-28").Scan(&dayID); err != nil {
		t.Fatalf("insert day node: %v", err)
	}

	// The digest a first, partial run already committed, with one summary already reparented under it.
	var digestID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,'digest',?) RETURNING id`, dayID, "first digest text").Scan(&digestID); err != nil {
		t.Fatalf("insert existing digest: %v", err)
	}
	var alreadyDone int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,'summary',?) RETURNING id`, digestID, "already reparented uniqueresume1").Scan(&alreadyDone); err != nil {
		t.Fatalf("insert already-reparented summary: %v", err)
	}

	// Two summaries still loose under a task, one of them a duplicate of what is already under the digest.
	var taskID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,?,?) RETURNING id`, dayID, "task", "Loose Task").Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	var loose1, loose2 int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,'summary',?) RETURNING id`, taskID, "already reparented uniqueresume1").Scan(&loose1); err != nil {
		t.Fatalf("insert loose duplicate summary: %v", err)
	}
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (?,'summary',?) RETURNING id`, taskID, "new work uniqueresume2").Scan(&loose2); err != nil {
		t.Fatalf("insert loose new summary: %v", err)
	}

	// The next compaction pass finds these two through the same query OldSummaryGroups runs (still under a task), and calls ReplaceSummariesWithDigest again for the same day.
	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{loose1, loose2}, "a fresh digest text covering the whole day"); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest resuming a day with an existing digest: %v", err)
	}

	var digestCount int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestCount); err != nil {
		t.Fatalf("count digests: %v", err)
	}
	if digestCount != 1 {
		t.Fatalf("expected exactly 1 digest for the day, got %d — a resumed compaction must reuse the existing digest rather than insert another", digestCount)
	}

	var digestContent string
	if err := raw.QueryRowContext(ctx, `SELECT content FROM nodes WHERE id=?`, digestID).Scan(&digestContent); err != nil {
		t.Fatalf("read digest content: %v", err)
	}
	if digestContent != "a fresh digest text covering the whole day" {
		t.Errorf("digest content = %q, want the text this pass generated — the reused digest is rewritten, not left frozen at what the first partial run said", digestContent)
	}

	var loose2Parent int64
	if err := raw.QueryRowContext(ctx, `SELECT parent_id FROM nodes WHERE id=?`, loose2).Scan(&loose2Parent); err != nil {
		t.Fatalf("find loose2: %v", err)
	}
	if loose2Parent != digestID {
		t.Errorf("loose2 parent = %d, want it reparented under the existing digest %d", loose2Parent, digestID)
	}

	var survivorsOfDup int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='summary' AND parent_id=? AND content=?`,
		digestID, "already reparented uniqueresume1").Scan(&survivorsOfDup); err != nil {
		t.Fatalf("count duplicate survivors: %v", err)
	}
	if survivorsOfDup != 1 {
		t.Errorf("expected exactly 1 surviving copy of the duplicate content under the digest, got %d", survivorsOfDup)
	}
}

// ─── Thread tests ─────────────────────────────────────────────────────────────

// ─── Relevance retrieval tests (B2) ───────────────────────────────────────────

// ─── Episode tests (Cycle 1: append-only episode storage) ────────────────────

// ─── Episode tests (Cycle 2: retrieval surfaces episodes) ────────────────────

// ─── Episode tests (Cycle 3: importance heuristic) ────────────────────────────

// ─── Episode tests (Cycle 4: ranking) ─────────────────────────────────────────

// ─── Consolidation retrieval (Cycle 1: temporal walk) ─────────────────────────

// TestStore_EpisodesInWindow_ChronologicalAndBounded seeds episodes at controlled timestamps spanning a day, plus one episode clearly outside the window, and verifies EpisodesInWindow returns only the in-window rows, ordered oldest-first (the "day arc"), and honors limit.
func TestStore_EpisodesInWindow_ChronologicalAndBounded(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)
	raw := store.DB()

	morningID, err := store.LogEpisode(ctx, "Mail", "Inbox", "reading morning emails")
	if err != nil {
		t.Fatalf("LogEpisode (morning): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-04 08:00:00' WHERE id = ?`, morningID); err != nil {
		t.Fatalf("backdate morning: %v", err)
	}

	noonID, err := store.LogEpisode(ctx, "VSCode", "main.go", "writing the consolidation layer")
	if err != nil {
		t.Fatalf("LogEpisode (noon): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-04 12:00:00' WHERE id = ?`, noonID); err != nil {
		t.Fatalf("backdate noon: %v", err)
	}

	eveningID, err := store.LogEpisode(ctx, "Firefox", "News", "reading the evening news")
	if err != nil {
		t.Fatalf("LogEpisode (evening): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-04 20:00:00' WHERE id = ?`, eveningID); err != nil {
		t.Fatalf("backdate evening: %v", err)
	}

	// clearly outside the window: the day before
	outsideID, err := store.LogEpisode(ctx, "Notes", "old memo", "yesterday's note")
	if err != nil {
		t.Fatalf("LogEpisode (outside): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET created_at = '2026-07-03 20:00:00' WHERE id = ?`, outsideID); err != nil {
		t.Fatalf("backdate outside: %v", err)
	}

	since := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 4, 23, 59, 59, 0, time.UTC)

	episodes, err := store.EpisodesInWindow(ctx, since, until, 10)
	if err != nil {
		t.Fatalf("EpisodesInWindow: %v", err)
	}
	if len(episodes) != 3 {
		t.Fatalf("expected 3 in-window episodes, got %d: %+v", len(episodes), episodes)
	}

	// chronological order: morning, noon, evening
	if episodes[0].ID != morningID || episodes[1].ID != noonID || episodes[2].ID != eveningID {
		t.Errorf("expected chronological order [morning,noon,evening], got ids [%d,%d,%d]",
			episodes[0].ID, episodes[1].ID, episodes[2].ID)
	}

	for _, e := range episodes {
		if e.ID == outsideID {
			t.Errorf("episode outside window must be excluded, got: %+v", e)
		}
	}

	// limit is honored
	limited, err := store.EpisodesInWindow(ctx, since, until, 2)
	if err != nil {
		t.Fatalf("EpisodesInWindow (limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected exactly 2 episodes with limit=2, got %d", len(limited))
	}
	if limited[0].ID != morningID || limited[1].ID != noonID {
		t.Errorf("expected limit to keep the earliest 2 in chronological order, got ids [%d,%d]", limited[0].ID, limited[1].ID)
	}
}

// ─── Consolidation retrieval (Cycle 2: MMR diversity) ─────────────────────────

// ─── Consolidation retrieval (Cycle 3: thread fusion) ─────────────────────────

// --- domain tagging + async embedding wiring ---
// Covers: the domain column migration, memory.Classify wiring into LogEpisode,
// LogEpisode's non-blocking async embed, and LogSemanticNode's majority-vote
// domain inheritance.

// fakeSlowEmbedder blocks in Embed until release is signaled, mirroring the blocking-tool-call pattern in internal/agent/connect_test.go's TestReceiveLoop_ToolCallDoesNotBlockReceivePath — used here to prove LogEpisode's async embed goroutine never makes the caller wait on it.
type fakeSlowEmbedder struct {
	release chan struct{}
	called  chan struct{} // closed once Embed is entered, for synchronization
}

func (f *fakeSlowEmbedder) Embed(ctx context.Context, task string, text string) ([]float32, error) {
	close(f.called)
	<-f.release
	return []float32{0.1, 0.2, 0.3}, nil
}

// fakeCountingVectorIndex is a minimal db.vectorIndex fake that just counts Add calls and returns canned Search results. embedder/vectorIndex are unexported interface types in hybrid.go, but Go's structural interface satisfaction lets this type work as an argument to Store.SetEmbedder/SetVectorIndex without ever naming them explicitly.
type fakeCountingVectorIndex struct {
	mu       sync.Mutex
	addCalls int
}

func (f *fakeCountingVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	f.mu.Lock()
	f.addCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeCountingVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	return nil, nil
}

func (f *fakeCountingVectorIndex) Delete(ctx context.Context, id string) error { return nil }

func (f *fakeCountingVectorIndex) IDs() []string { return nil }

// TestLogEpisode_DoesNotBlockOnSlowEmbedder proves LogEpisode returns immediately after its synchronous INSERT, even with a configured embedder that would block indefinitely — the async embed must run in its own goroutine, never inline on the caller's path.
func TestLogEpisode_DoesNotBlockOnSlowEmbedder(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	slow := &fakeSlowEmbedder{release: make(chan struct{}), called: make(chan struct{})}
	vidx := &fakeCountingVectorIndex{}
	store.SetEmbedder(slow)
	store.SetVectorIndex(vidx)

	done := make(chan struct{})
	go func() {
		if _, err := store.LogEpisode(ctx, "Code", "main.go", "writing the hybrid search layer"); err != nil {
			t.Errorf("LogEpisode: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("LogEpisode blocked on the slow embedder instead of returning immediately")
	}

	// Confirm the embed goroutine really did get started (so this test isn't vacuously true because the embedder was never invoked), then release it so it doesn't leak past the test.
	select {
	case <-slow.called:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the async embed goroutine to have started")
	}
	close(slow.release)
}

// TestNew_RestrictsDirectoryAndFilePermissions verifies db.New locks down the db directory to 0700 and the main db file to 0600 — the user's entire captured memory shouldn't default to world-readable (0755 dir / 0644 file) on a multi-user machine. POSIX permission bits don't map on Windows, so this is skipped there.
func TestNew_RestrictsDirectoryAndFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on Windows")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "db")

	store, err := db.New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Errorf("db directory permissions = %o, want 0700", got)
	}

	fileInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat db file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0600 {
		t.Errorf("db file permissions = %o, want 0600", got)
	}
}

// deleteRecordingVectorIndex records the vector ids a store asks it to delete, so a test can prove which notes' vectors were left alone.
type deleteRecordingVectorIndex struct {
	mu      sync.Mutex
	deleted []string
}

func (f *deleteRecordingVectorIndex) Add(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	return nil
}

func (f *deleteRecordingVectorIndex) Search(ctx context.Context, queryEmbedding []float32, n int, where map[string]string) ([]db.Result, error) {
	return nil, nil
}

func (f *deleteRecordingVectorIndex) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	f.mu.Unlock()
	return nil
}

func (f *deleteRecordingVectorIndex) IDs() []string { return nil }

func (f *deleteRecordingVectorIndex) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// fixedConsolidator is a NoteConsolidator that returns a canned canonical set and remembers what it was asked to consolidate.
type fixedConsolidator struct {
	out  []string
	seen []string
}

func (c *fixedConsolidator) ConsolidateNotes(ctx context.Context, notes []string) ([]string, error) {
	c.seen = notes
	return c.out, nil
}

// Note consolidation curates the model's facts about the user. Meeting minutes are filed in the same table under a different kind, and they are the only copy of what was said in a meeting, so a consolidation cycle must not show them to the model, must not delete their row, and must not delete their vector.
func TestNoteConsolidation_LeavesOtherKindsAlone(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	vidx := &deleteRecordingVectorIndex{}
	store.SetVectorIndex(vidx)

	// Twenty-five facts, so the compactor's "table is big enough to be worth curating" floor is cleared.
	var factIDs []int64
	for i := 0; i < 25; i++ {
		id, err := store.LogNote(ctx, fmt.Sprintf("the user knows fact number %d", i), "fact")
		if err != nil {
			t.Fatalf("LogNote: %v", err)
		}
		factIDs = append(factIDs, id)
	}
	const minutes = "# Meeting minutes\n\n- ship on friday"
	meetingID, err := store.LogNote(ctx, minutes, "meeting")
	if err != nil {
		t.Fatalf("LogNote(meeting): %v", err)
	}

	llm := &fixedConsolidator{out: []string{"the user knows a handful of things", "the user ships software"}}
	if err := db.NewNoteCompactor(llm, store).Compact(ctx); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	for _, seen := range llm.seen {
		if seen == minutes {
			t.Error("the meeting minutes were sent to the consolidation model, which is asked to drop non-facts")
		}
	}
	if len(llm.seen) != len(factIDs) {
		t.Errorf("the model saw %d notes, want the %d facts only", len(llm.seen), len(factIDs))
	}

	notes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	var survived bool
	for _, n := range notes {
		if n.ID == meetingID {
			survived = true
			if n.Content != minutes {
				t.Errorf("meeting note content = %q, want it byte-identical", n.Content)
			}
			if n.Kind != "meeting" {
				t.Errorf("meeting note kind = %q, want %q", n.Kind, "meeting")
			}
		}
	}
	if !survived {
		t.Fatalf("the meeting note (id %d) was deleted by consolidation; notes now: %+v", meetingID, notes)
	}

	// The old facts' vectors are deleted asynchronously, since consolidation renumbers them. Wait for that to finish before checking the meeting note's vector was spared.
	want := fmt.Sprintf("note:%d", meetingID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := vidx.deletedIDs()
		if len(got) >= len(factIDs) || time.Now().After(deadline) {
			for _, id := range got {
				if id == want {
					t.Fatalf("consolidation deleted the meeting note's vector (%s), orphaning its row", want)
				}
			}
			if len(got) < len(factIDs) {
				t.Errorf("only %d of %d replaced facts had their vectors deleted", len(got), len(factIDs))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// SummaryTimeline must carry each node's real creation time: the recall tier groups its lines by day, so a zeroed date collapses a whole week into one fake day (which is exactly what shipped the first time — every line read "[Jan 1]").
func TestSummaryTimeline_CarriesRealDates(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "route scoring", Summary: "adjusting vulnerability scores"}); err != nil {
		t.Fatalf("LogSemanticNode: %v", err)
	}
	sums, err := store.SummaryTimeline(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SummaryTimeline: %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("got %d summaries, want 1", len(sums))
	}
	if sums[0].CreatedAt.IsZero() || time.Since(sums[0].CreatedAt) > 5*time.Minute {
		t.Errorf("CreatedAt = %v, want the node's real creation time", sums[0].CreatedAt)
	}
}

// The tally table gained prompt_chars and reply_chars on 2026-09-01 in the create statement only, so a database created before that day failed every bump with "no column named prompt_chars". Opening such a database must add the columns.
func TestCreateSchema_TallyCharColumnsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := old.Exec(`CREATE TABLE tally (day TEXT NOT NULL, provider TEXT NOT NULL, calls INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0, total_ms INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (day, provider))`); err != nil {
		t.Fatalf("create old tally: %v", err)
	}
	old.Close()

	store, err := db.New(path)
	if err != nil {
		t.Fatalf("New over an old tally table: %v", err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`INSERT INTO tally (day, provider, calls, failures, total_ms, prompt_chars, reply_chars) VALUES ('2026-09-02', 'gemini', 1, 0, 5, 10, 20)`); err != nil {
		t.Errorf("tally still lacks its character columns after open: %v", err)
	}
}

// TestRetrieveRelevant_NoteExcerptSurvivesPastEpisodeCap checks the per-turn inject path gives a note the note budget rather than the 200-rune episode cap. Input: one note whose answer sits well past 200 runes. Output: the injected line still carries that answer.
func TestRetrieveRelevant_NoteExcerptSurvivesPastEpisodeCap(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	body := "Attendees: Zemna, Vexil. " + strings.Repeat("the payments team walked through the checkout flow again. ", 8) + "DECISION: ship the kubernetes migration on Friday."
	if _, err := store.LogNote(ctx, body, "meeting"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	lines, err := store.RetrieveRelevant(ctx, "kubernetes migration checkout", 4)
	if err != nil {
		t.Fatalf("RetrieveRelevant: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("expected the note to be retrieved at all")
	}
	found := false
	for _, l := range lines {
		if strings.Contains(l, "DECISION: ship the kubernetes migration on Friday.") {
			found = true
		}
	}
	if !found {
		t.Errorf("the note was cut to its heading — the inject path is still using the 200-rune episode cap: %+v", lines)
	}
}

// TestRelevantNotes_SurvivesCrossSourceLimit checks that the note filter runs in SQL before the row limit, not in Go after it. Input: more strongly-matching diary rows than the shared limit, plus one matching note. Output: the note is still returned.
func TestRelevantNotes_SurvivesCrossSourceLimit(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	for i := 0; i < 12; i++ {
		if err := store.SetDiaryEntry(ctx, fmt.Sprintf("2026-07-%02d", i+1), "day", "kubernetes migration payments"); err != nil {
			t.Fatalf("SetDiaryEntry: %v", err)
		}
	}
	if _, err := store.LogNote(ctx, "the user runs the kubernetes migration for the payments team on Fridays", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	notes, err := store.RelevantNotes(ctx, "kubernetes migration payments", 3)
	if err != nil {
		t.Fatalf("RelevantNotes: %v", err)
	}
	if len(notes) == 0 {
		t.Error("the matching note was filtered out after a cross-source limit had already spent every row on diary hits")
	}
}

// TestStore_GetImplicitContext_CarriesTheNightsUnderstanding is the sleep-time-compute wiring: the understanding doc the night rewrites is the one thing June computed while the user was away, and until now no live session read it. It must arrive first in the handshake context, before the live threads and the relevance hits, because it is the standing model everything else is read against.
func TestStore_GetImplicitContext_CarriesTheNightsUnderstanding(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	const doc = "He works on june most evenings and tests before he writes. His partner is Ada."
	if err := store.SetDiaryEntry(ctx, "", "understanding", doc); err != nil {
		t.Fatalf("SetDiaryEntry: %v", err)
	}
	if err := store.SetWorkingState(ctx, "user is reading the dream package"); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}
	if len(branch) == 0 {
		t.Fatal("GetImplicitContext returned nothing")
	}
	if !strings.HasPrefix(branch[0], "[understanding]") || !strings.Contains(branch[0], "tests before he writes") {
		t.Errorf("the night's understanding must lead the context, got first line %q in %+v", branch[0], branch)
	}
}
