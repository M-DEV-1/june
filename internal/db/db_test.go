package db_test

// tests are first class citizens

import (
	"context"
	"fmt"
	"ora/internal/db"
	"ora/internal/memory"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// t param is test controller. object to provide methods to control the flow of the test + reporting
func TestStore_Notes_DeleteAndDedupe(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id, _ := store.LogNote(ctx, "user works at Credibl ESG", "fact")

	// re-logging same content + kind is a no-op (idempotent)
	id2, err := store.LogNote(ctx, "user works at Credibl ESG", "fact")
	if err != nil {
		t.Fatalf("LogNote dedupe: %v", err)
	}
	if id2 != id {
		t.Errorf("expected idempotent insert to return same id, got %d != %d", id2, id)
	}

	if err := store.DeleteNote(ctx, id); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}

	notes, _ := store.GetNotes(ctx)
	if len(notes) != 0 {
		t.Fatalf("want 0 notes after delete, got %d", len(notes))
	}
}

// TestStore_LogNote_NormalizesCaseAndWhitespaceForDedup proves the fix for the "paraphrased restatement creates a duplicate row" problem: LogNote used to dedupe on an exact (content, kind) match only, so re-logging the same fact with different casing/whitespace created a second row instead of reconciling. Content is now normalized (trimmed, whitespace collapsed, lowercased) before the dedup check.
func TestStore_LogNote_NormalizesCaseAndWhitespaceForDedup(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

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
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// current focus: debugging the ESG portal
	if err := store.SetWorkingState(ctx, "debugging the ESG Benchmarking Portal backend"); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}
	// a live thread that matches what the user is doing now
	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ESG Benchmarking Portal",
		Kind:    "work",
		State:   "Monitoring CRD dashboard while debugging backend",
	}); err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}
	// a durable identity note with nothing to do with the current focus
	if _, err := store.LogNote(ctx, "user has an interest in Pune real estate", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}
	joined := strings.Join(branch, "\n")

	// the live thread must surface — that's the useful recall
	if !strings.Contains(joined, "ESG Benchmarking Portal") {
		t.Errorf("expected live thread in context, got: %+v", branch)
	}
	// the irrelevant identity note must NOT be dumped in unconditionally
	if strings.Contains(joined, "Pune real estate") {
		t.Errorf("irrelevant note leaked into context (unconditional note dump): %+v", branch)
	}
}

func TestStore_SearchMemory_FTS5(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary and a note
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Voice Pipeline",
		Summary:  "Debugging WebSocket reconnect loop in Gemini Live session",
	})
	_, _ = store.LogNote(ctx, "user works at Credibl ESG as an intern", "fact")

	// FTS5 should find the summary by a tokenized word
	hits, err := store.SearchMemory(ctx, "WebSocket")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS5 returned no hits for 'WebSocket'")
	}
	if !strings.Contains(hits[0].Content, "WebSocket") {
		t.Errorf("expected hit to mention WebSocket: %s", hits[0].Content)
	}
	if hits[0].Source != "summary" {
		t.Errorf("expected source=summary, got %s", hits[0].Source)
	}

	// FTS5 should also surface notes
	noteHits, err := store.SearchMemory(ctx, "Credibl")
	if err != nil {
		t.Fatalf("SearchMemory notes: %v", err)
	}
	if len(noteHits) == 0 {
		t.Fatal("FTS5 returned no hits for 'Credibl'")
	}
	if noteHits[0].Source != "note" {
		t.Errorf("expected source=note, got %s", noteHits[0].Source)
	}
}

// TestStore_SearchMemory_NaturalLanguageQuery_ORofTerms verifies the fix for the whole-query phrase-quoting bug: MATCH used to wrap the entire query as one FTS5 phrase, which only matches content containing that exact contiguous run of words. Tokenizing into an OR-of-terms MATCH means any significant term (here "websocket"/"reconnect") is enough to recall the summary, even without a verbatim match.
func TestStore_SearchMemory_NaturalLanguageQuery_ORofTerms(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

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

// TestStore_SearchMemory_AllStopwordQuery_FallsBackWithoutError verifies that when tokenization drops every term (an all-stopword query), buildFTSMatch falls back to the original whole-query phrase rather than handing FTS5 an empty/invalid MATCH expression that would error.
func TestStore_SearchMemory_AllStopwordQuery_FallsBackWithoutError(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	_, _ = store.LogNote(ctx, "user prefers dark mode", "preference")

	if _, err := store.SearchMemory(ctx, "is are was"); err != nil {
		t.Fatalf("expected all-stopword query to fall back to whole-query phrase without error, got: %v", err)
	}
}

// TestStore_SearchEpisodes_NaturalLanguageQuery_ORofTerms is the SearchEpisodes analogue of the SearchMemory OR-of-terms fix — same whole-query phrase-quoting bug, same shared buildFTSMatch fix.
func TestStore_SearchEpisodes_NaturalLanguageQuery_ORofTerms(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	if _, err := store.LogEpisode(ctx, "Firefox", "Gotham News", "Breaking: Commissioner Gordon holds press conference about the Riddler's latest scheme downtown"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	hits, err := store.SearchEpisodes(ctx, "what did the Riddler do downtown")
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected OR-of-terms match on a natural-language query that shares no verbatim phrase with the episode, got no hits")
	}
}

func TestStore_UpdateNote_AndFTSSync(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

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

func TestStore_ExistingNotes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id1, _ := store.LogNote(ctx, "user is a Go developer", "fact")
	id2, _ := store.LogNote(ctx, "user prefers dark mode", "preference")

	refs, err := store.ExistingNotes(ctx)
	if err != nil {
		t.Fatalf("ExistingNotes: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	// ordered by id ASC
	if refs[0].ID != id1 || refs[0].Content != "user is a Go developer" {
		t.Errorf("unexpected first ref: %+v", refs[0])
	}
	if refs[1].ID != id2 || refs[1].Content != "user prefers dark mode" {
		t.Errorf("unexpected second ref: %+v", refs[1])
	}
}

// seedOldTree inserts user→day→session→task→summary nodes using explicit old
// timestamps so OldSummaryGroups can find them. Returns the day node id.
func seedOldTree(t *testing.T, ctx context.Context, store *db.Store, dayContent string, summaries []string) int64 {
	t.Helper()
	raw := store.DB()

	var userID int64
	if err := raw.QueryRowContext(ctx, `SELECT id FROM nodes WHERE type='user' LIMIT 1`).Scan(&userID); err != nil {
		t.Fatalf("find user node: %v", err)
	}

	var dayID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days')) RETURNING id`,
		userID, "day", dayContent).Scan(&dayID); err != nil {
		t.Fatalf("insert day node: %v", err)
	}

	var sessID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days')) RETURNING id`,
		dayID, "session", "Old Session").Scan(&sessID); err != nil {
		t.Fatalf("insert session node: %v", err)
	}

	var taskID int64
	if err := raw.QueryRowContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days')) RETURNING id`,
		sessID, "task", "Old Task").Scan(&taskID); err != nil {
		t.Fatalf("insert task node: %v", err)
	}

	for _, s := range summaries {
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (?,?,?,datetime('now','-30 days'))`,
			taskID, "summary", s); err != nil {
			t.Fatalf("insert summary node: %v", err)
		}
	}

	return dayID
}

func TestStore_OldSummaryGroups_ReturnsGroupedByDay(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed two separate old days with summaries
	dayID1 := seedOldTree(t, ctx, store, "2026-05-01", []string{"summary-alpha", "summary-beta"})
	dayID2 := seedOldTree(t, ctx, store, "2026-05-02", []string{"summary-gamma"})

	// a fresh summary (created now) must NOT appear
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Current Task",
		Summary:  "very recent summary should not appear",
	})

	groups, err := store.OldSummaryGroups(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("OldSummaryGroups: %v", err)
	}

	// build a map by dayID for assertion order independence
	byDay := make(map[int64]memory.SummaryGroup)
	for _, g := range groups {
		byDay[g.DayID] = g
	}

	g1, ok := byDay[dayID1]
	if !ok {
		t.Fatalf("group for dayID %d not found; got groups: %+v", dayID1, groups)
	}
	if len(g1.Summaries) != 2 {
		t.Errorf("expected 2 summaries in day1 group, got %d", len(g1.Summaries))
	}

	g2, ok := byDay[dayID2]
	if !ok {
		t.Fatalf("group for dayID %d not found", dayID2)
	}
	if len(g2.Summaries) != 1 {
		t.Errorf("expected 1 summary in day2 group, got %d", len(g2.Summaries))
	}
}

func TestStore_GetImplicitContext_WithWorkingState(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary so the tree has raw material
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Baseline Task",
		Summary:  "Writing baseline summary for context test",
	})

	// before working state is set, there must be no [now] line
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext (no state): %v", err)
	}
	for _, line := range branch {
		if strings.Contains(line, "[now]") {
			t.Errorf("did not expect [now] line before working state is set: %s", line)
		}
	}

	// once set, working state appears as [now]. (This used to also assert the raw summary dump was replaced entirely, but the summary's own task name is always folded into the focus signal, so relevance retrieval legitimately re-surfaces it now — that's the FTS5 phrase-quoting fix working as intended, not a leak.)
	const state = "user is actively debugging the Linux audio pipeline and writing TDD tests"
	if err := store.SetWorkingState(ctx, state); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch2, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext (with state): %v", err)
	}

	var foundState bool
	for _, line := range branch2 {
		if strings.Contains(line, "[now]") && strings.Contains(line, state) {
			foundState = true
		}
	}
	if !foundState {
		t.Errorf("[now] line missing from context: %+v", branch2)
	}
}

// TestStore_GetImplicitContext_DoesNotLeakStaleTaskAcrossContexts guards against the missing time bound in GetImplicitContext's focus signal: it folds the last 2 task names in unconditionally, by id, so a stale task from days ago can still be "recent by id" and self-match its own summary back into context regardless of relevance. This reproduces the "ESG facts bleed into an unrelated project" bug as a concrete test.
func TestStore_GetImplicitContext_DoesNotLeakStaleTaskAcrossContexts(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "ESG Report Review",
		Summary:  "Reviewed the Q1 ESG compliance report line by line",
	}); err != nil {
		t.Fatalf("LogSemanticNode (old task): %v", err)
	}

	// backdate the old task node so it's genuinely stale, not just "not the most recent" — the bug is a missing time bound, not a missing id bound.
	staleTime := time.Now().Add(-72 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE nodes SET created_at = ? WHERE type = 'task' AND content = ?`,
		staleTime, "ESG Report Review"); err != nil {
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
		if strings.Contains(line, "ESG") {
			t.Errorf("stale, unrelated task summary leaked into a fresh working-state context: %+v", branch)
		}
	}
}

func TestStore_GetImplicitContext_FallbackWhenNoState(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed a summary only. With no identity notes, no live threads, and no working state, GetImplicitContext falls back to the recursive summary walk.
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Fallback Task",
		Summary:  "Checking fallback behavior works correctly",
	})

	// cold start: nothing synthesized → fallback path returns the summary tree
	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext fallback: %v", err)
	}

	var hasSummary bool
	for _, line := range branch {
		if strings.Contains(line, "Checking fallback behavior") {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Errorf("expected summary in fallback context: %+v", branch)
	}
}

func TestStore_RecentSummaries(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// seed 3 summaries
	for _, name := range []string{"first-summary-alpha", "second-summary-beta", "third-summary-gamma"} {
		_ = store.LogSemanticNode(ctx, memory.TaskSummary{
			SameTask: false,
			TaskName: name,
			Summary:  name + " content",
		})
	}

	// seed a digest via direct insert (should also appear)
	_, err = store.DB().ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content) VALUES (
			(SELECT id FROM nodes WHERE type='day' LIMIT 1),
			'digest', 'digest-delta-content'
		)`)
	if err != nil {
		t.Fatalf("insert digest: %v", err)
	}

	recent, err := store.RecentSummaries(ctx, 10)
	if err != nil {
		t.Fatalf("RecentSummaries: %v", err)
	}

	// must include both summary and digest rows
	if len(recent) < 4 {
		t.Fatalf("expected at least 4 rows (3 summaries + 1 digest), got %d: %+v", len(recent), recent)
	}

	// newest-first: digest was inserted last, so it must appear first
	if !strings.Contains(recent[0], "digest-delta-content") {
		t.Errorf("expected digest to be first (newest), got: %s", recent[0])
	}

	// limit should be respected
	limited, err := store.RecentSummaries(ctx, 2)
	if err != nil {
		t.Fatalf("RecentSummaries (limited): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected exactly 2 rows with limit=2, got %d", len(limited))
	}
}

func TestStore_CountSummariesSince(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	before := time.Now()

	// seed 2 summaries AFTER the mark
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "New Task 1",
		Summary:  "new work alpha",
	})
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: true,
		TaskName: "New Task 1",
		Summary:  "new work beta",
	})

	// seed 1 old summary via direct insert
	_, err = store.DB().ExecContext(ctx,
		`INSERT INTO nodes (parent_id, type, content, created_at) VALUES (
			(SELECT id FROM nodes WHERE type='session' LIMIT 1),
			'summary', 'old-summary-content', datetime('now', '-2 hours')
		)`)
	if err != nil {
		t.Fatalf("insert old summary: %v", err)
	}

	count, err := store.CountSummariesSince(ctx, before)
	if err != nil {
		t.Fatalf("CountSummariesSince: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 summaries after 'before', got %d", count)
	}

	// counting from the future should return 0
	future := time.Now().Add(time.Hour)
	count2, err := store.CountSummariesSince(ctx, future)
	if err != nil {
		t.Fatalf("CountSummariesSince (future): %v", err)
	}
	if count2 != 0 {
		t.Errorf("expected 0 summaries in the future, got %d", count2)
	}
}

func TestStore_ReplaceSummariesWithDigest_TransactionAndFTS(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	dayID := seedOldTree(t, ctx, store, "2026-05-10", []string{
		"user debugged the xgb tracker uniquetoken1",
		"user reviewed PR for audio pipeline uniquetoken2",
	})

	// grab the summary IDs just inserted
	rows, err := store.DB().QueryContext(ctx,
		`SELECT id FROM nodes WHERE type='summary' ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("query summaries: %v", err)
	}
	var summaryIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		summaryIDs = append(summaryIDs, id)
	}
	rows.Close()

	if len(summaryIDs) != 2 {
		t.Fatalf("expected 2 summary IDs before replace, got %d", len(summaryIDs))
	}

	digest := "user spent the day debugging the tracker and reviewing the audio PR uniquetoken3"
	if err := store.ReplaceSummariesWithDigest(ctx, dayID, summaryIDs, digest); err != nil {
		t.Fatalf("ReplaceSummariesWithDigest: %v", err)
	}

	// summaries must be gone
	var sumCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='summary'`).Scan(&sumCount); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if sumCount != 0 {
		t.Errorf("expected 0 summaries after replace, got %d", sumCount)
	}

	// digest must exist under dayID
	var digestContent string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT content FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestContent); err != nil {
		t.Fatalf("find digest node: %v", err)
	}
	if digestContent != digest {
		t.Errorf("digest content mismatch: got %q", digestContent)
	}

	// FTS: digest term must be searchable
	hits, err := store.SearchMemory(ctx, "uniquetoken3")
	if err != nil {
		t.Fatalf("SearchMemory for digest term: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS returned no hits for digest term — digest insert trigger not firing")
	}
	if hits[0].Source != "digest" {
		t.Errorf("expected source=digest, got %s", hits[0].Source)
	}

	// FTS: terms unique to deleted summaries must be gone
	oldHits1, err := store.SearchMemory(ctx, "uniquetoken1")
	if err != nil {
		t.Fatalf("SearchMemory for deleted summary term: %v", err)
	}
	if len(oldHits1) != 0 {
		t.Errorf("FTS still returns deleted summary term 'uniquetoken1' — delete trigger not firing")
	}

	oldHits2, err := store.SearchMemory(ctx, "uniquetoken2")
	if err != nil {
		t.Fatalf("SearchMemory for deleted summary term: %v", err)
	}
	if len(oldHits2) != 0 {
		t.Errorf("FTS still returns deleted summary term 'uniquetoken2' — delete trigger not firing")
	}
}

// ─── Thread tests ─────────────────────────────────────────────────────────────

// TestStore_UpsertThread_NewThread verifies that a zero-ID upsert creates a new row with the right initial salience (0.5 when Novel=false, 0.6 when Novel=true), times_seen=1, and status='active'.
func TestStore_UpsertThread_NewThread(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	cases := []struct {
		novel        bool
		wantSalience float64
	}{
		{novel: false, wantSalience: 0.5},
		{novel: true, wantSalience: 0.6},
	}

	for _, tc := range cases {
		subject := fmt.Sprintf("project-novel-%v", tc.novel)
		id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: subject,
			Kind:    "work",
			State:   "working on it",
			Novel:   tc.novel,
		})
		if err != nil {
			t.Fatalf("UpsertThread (novel=%v): %v", tc.novel, err)
		}
		if id == 0 {
			t.Fatalf("novel=%v: expected non-zero id", tc.novel)
		}

		var sal float64
		var timesSeen int
		var status string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT salience, times_seen, status FROM threads WHERE id = ?`, id).
			Scan(&sal, &timesSeen, &status); err != nil {
			t.Fatalf("query thread (novel=%v): %v", tc.novel, err)
		}
		if sal != tc.wantSalience {
			t.Errorf("novel=%v: want salience %v, got %v", tc.novel, tc.wantSalience, sal)
		}
		if timesSeen != 1 {
			t.Errorf("novel=%v: want times_seen=1, got %d", tc.novel, timesSeen)
		}
		if status != "active" {
			t.Errorf("novel=%v: want status='active', got %q", tc.novel, status)
		}
	}
}

// TestStore_UpsertThread_Conflict verifies that a second zero-ID upsert with the same (subject, kind) updates state, bumps times_seen to 2, raises salience by ~0.05 (capped at ≤1.0), and returns the same id with exactly one row.
func TestStore_UpsertThread_Conflict(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	id1, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "initial state",
		Novel:   false,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	var sal1 float64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT salience FROM threads WHERE id = ?`, id1).Scan(&sal1); err != nil {
		t.Fatalf("query salience before conflict: %v", err)
	}

	// second upsert with same subject+kind, different state
	id2, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "writing more tests",
		Novel:   false,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id2 != id1 {
		t.Errorf("conflict must return same id: got %d, want %d", id2, id1)
	}

	var sal2 float64
	var timesSeen int
	var state string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT salience, times_seen, state FROM threads WHERE id = ?`, id1).
		Scan(&sal2, &timesSeen, &state); err != nil {
		t.Fatalf("query after conflict: %v", err)
	}
	if sal2 <= sal1 {
		t.Errorf("salience should increase on conflict: before=%v after=%v", sal1, sal2)
	}
	if sal2 > 1.0 {
		t.Errorf("salience must not exceed 1.0, got %v", sal2)
	}
	if timesSeen != 2 {
		t.Errorf("times_seen should be 2 after conflict, got %d", timesSeen)
	}
	if state != "writing more tests" {
		t.Errorf("state should be updated to new value, got %q", state)
	}

	// must stay at exactly one row
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM threads WHERE subject = 'ORA project' AND kind = 'work'`).Scan(&count); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if count != 1 {
		t.Errorf("conflict must keep single row, got %d rows", count)
	}
}

// TestStore_UpsertThread_SalienceCap verifies that salience never exceeds 1.0
// regardless of how many times the same thread is upserted.
func TestStore_UpsertThread_SalienceCap(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 30; i++ {
		if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: "recurring thread",
			Kind:    "work",
			State:   fmt.Sprintf("iteration %d", i),
			Novel:   true,
		}); err != nil {
			t.Fatalf("upsert iter %d: %v", i, err)
		}
	}

	var sal float64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT salience FROM threads WHERE subject = 'recurring thread' AND kind = 'work'`).Scan(&sal); err != nil {
		t.Fatalf("query salience: %v", err)
	}
	if sal > 1.0 {
		t.Errorf("salience must not exceed 1.0 after many upserts, got %v", sal)
	}
}

// TestStore_UpsertThread_ExplicitID verifies the ID>0 update path: state is replaced, times_seen is incremented, and the same id is returned.
func TestStore_UpsertThread_ExplicitID(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "initial",
		Novel:   false,
	})
	if err != nil {
		t.Fatalf("initial insert: %v", err)
	}

	var timesSeen1 int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT times_seen FROM threads WHERE id = ?`, id).Scan(&timesSeen1); err != nil {
		t.Fatalf("query times_seen before explicit upsert: %v", err)
	}

	returnedID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		ID:    id,
		State: "explicit-state-update",
	})
	if err != nil {
		t.Fatalf("explicit-id upsert: %v", err)
	}
	if returnedID != id {
		t.Errorf("explicit-id upsert must return same id: got %d, want %d", returnedID, id)
	}

	var state string
	var timesSeen2 int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state, times_seen FROM threads WHERE id = ?`, id).
		Scan(&state, &timesSeen2); err != nil {
		t.Fatalf("query after explicit upsert: %v", err)
	}
	if state != "explicit-state-update" {
		t.Errorf("state should be replaced: got %q", state)
	}
	if timesSeen2 != timesSeen1+1 {
		t.Errorf("times_seen should increment: got %d, want %d", timesSeen2, timesSeen1+1)
	}
}

// TestStore_GetLiveThreads_RecencyWindow verifies the 2-day cutoff: a thread last_seen within 2 days is returned; one older than 2 days is not. Also checks newest-first ordering and that the limit parameter is honored.
func TestStore_GetLiveThreads_RecencyWindow(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	// recent thread (last_seen = now)
	recentID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "recent-work",
		Kind:    "work",
		State:   "in progress",
	})
	if err != nil {
		t.Fatalf("insert recent thread: %v", err)
	}

	// day-old thread (still within 2-day window)
	dayOldID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "day-old-work",
		Kind:    "work",
		State:   "ongoing",
	})
	if err != nil {
		t.Fatalf("insert day-old thread: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-1 day') WHERE id = ?`, dayOldID); err != nil {
		t.Fatalf("backdate day-old thread: %v", err)
	}

	// thread older than 2 days (must NOT appear)
	oldID, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "old-work",
		Kind:    "work",
		State:   "stale",
	})
	if err != nil {
		t.Fatalf("insert old thread: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-3 days') WHERE id = ?`, oldID); err != nil {
		t.Fatalf("backdate old thread: %v", err)
	}

	threads, err := store.GetLiveThreads(ctx, 10)
	if err != nil {
		t.Fatalf("GetLiveThreads: %v", err)
	}

	byID := make(map[int64]int) // id → index in result
	for i, th := range threads {
		byID[th.ID] = i
	}
	if _, ok := byID[recentID]; !ok {
		t.Error("recent thread should appear in live threads")
	}
	if _, ok := byID[dayOldID]; !ok {
		t.Error("1-day-old thread should appear in live threads (within 2-day window)")
	}
	if _, ok := byID[oldID]; ok {
		t.Error("3-day-old thread must NOT appear in live threads")
	}

	// ordering: newest-first; recent must precede day-old
	recentIdx, dayOldIdx := byID[recentID], byID[dayOldID]
	if recentIdx > dayOldIdx {
		t.Errorf("recent thread (idx %d) should come before day-old thread (idx %d)", recentIdx, dayOldIdx)
	}

	// limit is honored
	limited, err := store.GetLiveThreads(ctx, 1)
	if err != nil {
		t.Fatalf("GetLiveThreads (limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 should return exactly 1 thread, got %d", len(limited))
	}
}

// TestStore_ThreadsForAttribution_Window verifies the 14-day cutoff. Threads touched 3 days ago appear here (but not in GetLiveThreads); threads touched 15 days ago appear in neither.
func TestStore_ThreadsForAttribution_Window(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	recentID, _ := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "recent", Kind: "work", State: "s"})

	mid3dID, _ := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "mid-3d", Kind: "work", State: "s"})
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-3 days') WHERE id = ?`, mid3dID); err != nil {
		t.Fatalf("backdate 3d thread: %v", err)
	}

	tooOldID, _ := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "too-old", Kind: "work", State: "s"})
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE threads SET last_seen_at = datetime('now', '-15 days') WHERE id = ?`, tooOldID); err != nil {
		t.Fatalf("backdate 15d thread: %v", err)
	}

	attrThreads, err := store.ThreadsForAttribution(ctx, 50)
	if err != nil {
		t.Fatalf("ThreadsForAttribution: %v", err)
	}

	attrByID := make(map[int64]bool)
	for _, th := range attrThreads {
		attrByID[th.ID] = true
	}
	if !attrByID[recentID] {
		t.Error("recent thread should appear in 14-day attribution window")
	}
	if !attrByID[mid3dID] {
		t.Error("3-day-old thread should appear in 14-day attribution window")
	}
	if attrByID[tooOldID] {
		t.Error("15-day-old thread must NOT appear in 14-day attribution window")
	}

	// confirm 3-day-old is outside the 2-day live window
	live, err := store.GetLiveThreads(ctx, 50)
	if err != nil {
		t.Fatalf("GetLiveThreads: %v", err)
	}
	liveByID := make(map[int64]bool)
	for _, th := range live {
		liveByID[th.ID] = true
	}
	if liveByID[mid3dID] {
		t.Error("3-day-old thread must NOT appear in 2-day live window")
	}
}

// TestStore_GetImplicitContext_ThreadFormat verifies that GetImplicitContext emits [thread:kind] subject — state for threads with a state, [thread:kind] subject for threads without one, and [now] for working_state (and no [about] lines — identity notes are relevance-gated, not dumped).
func TestStore_GetImplicitContext_ThreadFormat(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	// seed 10 notes — none should be dumped as [about] lines anymore (relevance-gated)
	for i := 0; i < 10; i++ {
		_, _ = store.LogNote(ctx, fmt.Sprintf("identity fact %d", i), "fact")
	}

	// thread with state
	_, _ = store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "Suits",
		Kind:    "entertainment",
		State:   "season 1 episode 3",
	})

	// thread without state (empty string)
	_, _ = store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "ORA project",
		Kind:    "work",
		State:   "",
	})

	_ = store.SetWorkingState(ctx, "debugging the audio pipeline")

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}

	var aboutCount int
	var foundThreadWithState, foundThreadNoState, foundNow bool
	for _, line := range branch {
		if strings.HasPrefix(line, "[about] ") {
			aboutCount++
		}
		// [thread:entertainment] Suits — season 1 episode 3
		if line == "[thread:entertainment] Suits — season 1 episode 3" {
			foundThreadWithState = true
		}
		// [thread:work] ORA project (no state → no em dash suffix)
		if line == "[thread:work] ORA project" {
			foundThreadNoState = true
		}
		if strings.HasPrefix(line, "[now] ") {
			foundNow = true
		}
	}

	if aboutCount != 0 {
		t.Errorf("identity notes must no longer be dumped as [about] lines (relevance-gated now), got %d", aboutCount)
	}
	if !foundThreadWithState {
		t.Errorf("[thread:entertainment] Suits — state line not found in: %v", branch)
	}
	if !foundThreadNoState {
		t.Errorf("[thread:work] ORA project (no-state) line not found in: %v", branch)
	}
	if !foundNow {
		t.Errorf("[now] line not found in: %v", branch)
	}
}

// TestStore_SearchMemory_FindsThread verifies that after UpsertThread, SearchMemory
// returns a hit whose Source is "thread".
func TestStore_SearchMemory_FindsThread(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()

	_, err = store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "quuxzap project",
		Kind:    "work",
		State:   "writing integration tests",
	})
	if err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "quuxzap")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchMemory returned no hits for thread subject word 'quuxzap'")
	}
	if hits[0].Source != "thread" {
		t.Errorf("expected source='thread', got %q", hits[0].Source)
	}
}

// ─── Relevance retrieval tests (B2) ───────────────────────────────────────────

// TestStore_GetImplicitContext_WiresRelevanceRetrieval verifies that the working-state focus drives the relevance-retrieval layer, surfacing a matching item as a [note] line. Exclusion of unrelated items is covered by the dedicated RetrieveRelevant tests.
func TestStore_GetImplicitContext_WiresRelevanceRetrieval(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// a relevant note; no summaries/tasks so the focus signal stays clean
	_, _ = store.LogNote(ctx, "debugging Linux audio pipeline crackle", "fact")

	const state = "debugging Linux audio"
	if err := store.SetWorkingState(ctx, state); err != nil {
		t.Fatalf("SetWorkingState: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}

	var hasRelevanceLine bool
	for _, b := range branch {
		if strings.HasPrefix(b, "[note]") && strings.Contains(b, "Linux audio pipeline") {
			hasRelevanceLine = true
		}
	}
	if !hasRelevanceLine {
		t.Errorf("expected a [note] relevance line driven by working-state focus: %+v", branch)
	}
}

func TestStore_RetrieveRelevant_FocusAffectsResults(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	_, _ = store.LogNote(ctx, "user works with Go and SQLite", "fact")
	_, _ = store.LogNote(ctx, "user likes hiking in mountains", "fact")

	// focus on Go should return Go note
	goResults, _ := store.RetrieveRelevant(ctx, "Go and SQLite", 5)
	foundGo := false
	for _, r := range goResults {
		if strings.Contains(r, "Go and SQLite") {
			foundGo = true
		}
	}
	if !foundGo {
		t.Errorf("focus 'Go' should surface Go note, got: %+v", goResults)
	}

	// different focus should not surface unrelated
	hikeResults, _ := store.RetrieveRelevant(ctx, "hiking in mountains", 5)
	foundHikeInGoFocus := false
	for _, r := range goResults {
		if strings.Contains(r, "hiking") {
			foundHikeInGoFocus = true
		}
	}
	if foundHikeInGoFocus {
		t.Errorf("focus on Go should not surface hike note: %+v", goResults)
	}
	// check that different focus returns different sets
	if len(goResults) > 0 && len(hikeResults) > 0 && goResults[0] == hikeResults[0] {
		t.Errorf("different focus should return different result sets, got: %+v vs %+v", goResults, hikeResults)
	}
}

// TestStore_RelevantNotes_CapsAndFilters verifies the fix for the unbounded note dump fed into DeriveState (cmd/daemon.go): RelevantNotes returns a relevance-ranked, capped subset of notes matching focus — not the entire notes table — as plain content strings (no "[note] " prefix, since DeriveState expects bare facts).
func TestStore_RelevantNotes_CapsAndFilters(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		if _, err := store.LogNote(ctx, fmt.Sprintf("user works on the ora recall project part %d", i), "fact"); err != nil {
			t.Fatalf("LogNote: %v", err)
		}
	}
	_, _ = store.LogNote(ctx, "user likes hiking in the mountains", "fact")

	all, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("expected 6 seeded notes, got %d", len(all))
	}

	relevant, err := store.RelevantNotes(ctx, "ora recall project", 3)
	if err != nil {
		t.Fatalf("RelevantNotes: %v", err)
	}
	if len(relevant) == 0 {
		t.Fatal("expected at least one relevant note")
	}
	if len(relevant) > 3 {
		t.Errorf("expected RelevantNotes to cap at 3, got %d: %+v", len(relevant), relevant)
	}
	if len(relevant) >= len(all) {
		t.Errorf("RelevantNotes should not return the whole notes table (%d notes), got %d", len(all), len(relevant))
	}
	for _, r := range relevant {
		if strings.HasPrefix(r, "[note]") {
			t.Errorf("RelevantNotes should return plain content, not prefixed lines: %q", r)
		}
		if strings.Contains(r, "hiking") {
			t.Errorf("unrelated note should not surface for an unrelated focus: %q", r)
		}
	}
}

// ─── Episode tests (Cycle 1: append-only episode storage) ────────────────────

// TestStore_LogEpisode_AppendOnly_NoDedupe verifies episodes are NOT deduped like nodes/notes are: logging the same app+title twice with different screen_text must persist as two distinct rows — the whole point of a dedicated episodes table instead of reusing the nodes tree, whose unique index on (parent_id,type,content) would wrongly collapse repeat visits. Also verifies SearchEpisodes finds a distinctive word via FTS5.
func TestStore_LogEpisode_AppendOnly_NoDedupe(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	id1, err := store.LogEpisode(ctx, "Firefox", "Batman Wiki", "Reading about the Riddler's origin story")
	if err != nil {
		t.Fatalf("LogEpisode (1): %v", err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero episode id")
	}

	id2, err := store.LogEpisode(ctx, "Firefox", "Batman Wiki", "Now reading about Two-Face instead")
	if err != nil {
		t.Fatalf("LogEpisode (2): %v", err)
	}
	if id2 == 0 {
		t.Fatal("expected non-zero episode id")
	}
	if id2 == id1 {
		t.Errorf("expected distinct ids for repeat app+title visits, got same id %d twice", id1)
	}

	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 distinct episode rows (no dedupe), got %d", count)
	}

	hits, err := store.SearchEpisodes(ctx, "Riddler")
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchEpisodes returned no hits for 'Riddler'")
	}
	if !strings.Contains(hits[0].Content, "Riddler") {
		t.Errorf("expected hit content to contain 'Riddler', got: %s", hits[0].Content)
	}
	if hits[0].Source != "episode" {
		t.Errorf("expected source='episode', got %q", hits[0].Source)
	}
}

// ─── Episode tests (Cycle 2: retrieval surfaces episodes) ────────────────────

// TestStore_RetrieveRelevant_IncludesEpisodes verifies that RetrieveRelevant merges episode hits alongside note/summary/thread hits, formatted via FormatHit as "[episode] App — Title: <screen_text excerpt>" (optionally "[episode (age)] …" when CreatedAt is known — see WP12 Part C), ordered by RankedEpisodes' weighted score (recency+importance+relevance) rather than plain FTS rank: a recent, important episode must surface before a stale, trivial-importance one matching the same focus term.
func TestStore_RetrieveRelevant_IncludesEpisodes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	richID, err := store.LogEpisode(ctx, "Firefox", "Gotham News", "Breaking: Commissioner Gordon holds press conference about the Riddler's latest scheme downtown")
	if err != nil {
		t.Fatalf("LogEpisode (rich/recent): %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET importance = 0.95, created_at = datetime('now') WHERE id = ?`, richID); err != nil {
		t.Fatalf("backdate rich episode: %v", err)
	}

	staleID, err := store.LogEpisode(ctx, "Notes", "old memo", "Riddler Riddler Riddler mentioned once in a stale note")
	if err != nil {
		t.Fatalf("LogEpisode (stale/trivial): %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE episodes SET importance = 0.05, created_at = datetime('now', '-720 hours') WHERE id = ?`, staleID); err != nil {
		t.Fatalf("backdate stale episode: %v", err)
	}

	results, err := store.RetrieveRelevant(ctx, "Riddler", 10)
	if err != nil {
		t.Fatalf("RetrieveRelevant: %v", err)
	}

	var foundEpisode bool
	richIdx, staleIdx := -1, -1
	for i, r := range results {
		if strings.HasPrefix(r, "[episode") && strings.Contains(r, "Riddler") {
			foundEpisode = true
		}
		if strings.Contains(r, "press conference") {
			richIdx = i
		}
		if strings.Contains(r, "stale note") {
			staleIdx = i
		}
	}
	if !foundEpisode {
		t.Errorf("expected a [episode] line matching focus in RetrieveRelevant results: %+v", results)
	}
	if richIdx == -1 {
		t.Fatalf("expected rich/recent episode in results: %+v", results)
	}
	if staleIdx == -1 {
		t.Fatalf("expected stale/trivial episode in results: %+v", results)
	}
	if richIdx > staleIdx {
		t.Errorf("expected recent+important episode (idx %d) to rank before stale low-importance one (idx %d), i.e. RetrieveRelevant should use RankedEpisodes not plain FTS: %+v", richIdx, staleIdx, results)
	}
}

// ─── Episode tests (Cycle 3: importance heuristic) ────────────────────────────

// TestStore_LogEpisode_ImportanceHeuristic verifies the stored importance score reflects both signals: richer screen_text and revisitation (a prior episode with the same app+title) should score higher than a sparse, first-visit one.
func TestStore_LogEpisode_ImportanceHeuristic(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	// first-visit, trivial screen_text -> low importance
	trivialID, err := store.LogEpisode(ctx, "Notepad", "untitled.txt", "hi")
	if err != nil {
		t.Fatalf("LogEpisode (trivial): %v", err)
	}

	// prior visit to the same app+title, so the next visit counts as a revisit
	if _, err := store.LogEpisode(ctx, "VSCode", "main.go — ora", "package main\n\nfunc main() {}"); err != nil {
		t.Fatalf("LogEpisode (seed revisit): %v", err)
	}
	richID, err := store.LogEpisode(ctx, "VSCode", "main.go — ora", strings.Repeat("word ", 300))
	if err != nil {
		t.Fatalf("LogEpisode (rich revisit): %v", err)
	}

	var trivialImportance, richImportance float64
	if err := store.DB().QueryRowContext(ctx, `SELECT importance FROM episodes WHERE id = ?`, trivialID).Scan(&trivialImportance); err != nil {
		t.Fatalf("query trivial importance: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT importance FROM episodes WHERE id = ?`, richID).Scan(&richImportance); err != nil {
		t.Fatalf("query rich importance: %v", err)
	}

	if richImportance <= trivialImportance {
		t.Errorf("expected rich+revisited episode importance (%v) > trivial first-visit importance (%v)", richImportance, trivialImportance)
	}
	if richImportance < 0 || richImportance > 1 {
		t.Errorf("importance must be in [0,1], got %v", richImportance)
	}
	if trivialImportance < 0 || trivialImportance > 1 {
		t.Errorf("importance must be in [0,1], got %v", trivialImportance)
	}
}

// ─── Episode tests (Cycle 4: ranking) ─────────────────────────────────────────

// TestStore_RankedEpisodes_WeightedOrdering constructs episodes where recency/importance/relevance pull in different directions and asserts that a slightly-less-relevant but far-more-important+recent episode outranks a stale, barely-relevant one, per the documented weighted formula.
func TestStore_RankedEpisodes_WeightedOrdering(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	raw := store.DB()

	// winner: recent, high importance, decent (but not perfect) relevance.
	winnerID, err := store.LogEpisode(ctx, "VSCode", "compiler.go", "refactoring the memory compiler ranking logic today")
	if err != nil {
		t.Fatalf("LogEpisode (winner): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.95, created_at = datetime('now') WHERE id = ?`, winnerID); err != nil {
		t.Fatalf("backdate winner: %v", err)
	}

	// loser: stale (30 days old), low importance, but a slightly more literal relevance match on the focus term.
	loserID, err := store.LogEpisode(ctx, "Notes", "old memo", "ranking ranking ranking notes from a month ago")
	if err != nil {
		t.Fatalf("LogEpisode (loser): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.05, created_at = datetime('now', '-720 hours') WHERE id = ?`, loserID); err != nil {
		t.Fatalf("backdate loser: %v", err)
	}

	results, err := store.RankedEpisodes(ctx, "ranking", 10)
	if err != nil {
		t.Fatalf("RankedEpisodes: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 ranked episodes, got %d: %+v", len(results), results)
	}

	winnerIdx, loserIdx := -1, -1
	for i, r := range results {
		if strings.Contains(r.Content, "refactoring the memory compiler") {
			winnerIdx = i
		}
		if strings.Contains(r.Content, "old memo") || strings.Contains(r.Content, "month ago") {
			loserIdx = i
		}
	}
	if winnerIdx == -1 {
		t.Fatalf("winner episode not found in results: %+v", results)
	}
	if loserIdx == -1 {
		t.Fatalf("loser episode not found in results: %+v", results)
	}
	if winnerIdx > loserIdx {
		t.Errorf("expected recent+important episode (idx %d) to outrank stale low-importance episode (idx %d): %+v", winnerIdx, loserIdx, results)
	}

	// limit is honored
	limited, err := store.RankedEpisodes(ctx, "ranking", 1)
	if err != nil {
		t.Fatalf("RankedEpisodes (limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("expected exactly 1 result with limit=1, got %d", len(limited))
	}
}

// ─── Episode tests (Cycle 5: culling / aging) ─────────────────────────────────

// TestStore_AgeEpisodes_DropsOnlyOldLowImportance seeds an old low-importance episode, an old high-importance episode, and a recent one, and verifies AgeEpisodes empties screen_text only for the old low-importance row while keeping the row itself intact — the other two keep their screen_text untouched.
func TestStore_AgeEpisodes_DropsOnlyOldLowImportance(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()
	raw := store.DB()

	oldLowID, err := store.LogEpisode(ctx, "Notes", "old low", "trivial old content")
	if err != nil {
		t.Fatalf("LogEpisode (old low): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.1, created_at = datetime('now', '-720 hours') WHERE id = ?`, oldLowID); err != nil {
		t.Fatalf("backdate old low: %v", err)
	}

	oldHighID, err := store.LogEpisode(ctx, "VSCode", "old high", "important old content about the core architecture")
	if err != nil {
		t.Fatalf("LogEpisode (old high): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.9, created_at = datetime('now', '-720 hours') WHERE id = ?`, oldHighID); err != nil {
		t.Fatalf("backdate old high: %v", err)
	}

	recentID, err := store.LogEpisode(ctx, "Notes", "recent low", "trivial recent content")
	if err != nil {
		t.Fatalf("LogEpisode (recent): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.1 WHERE id = ?`, recentID); err != nil {
		t.Fatalf("set recent importance: %v", err)
	}

	aged, err := store.AgeEpisodes(ctx, 168*time.Hour, 0.5)
	if err != nil {
		t.Fatalf("AgeEpisodes: %v", err)
	}
	if aged != 1 {
		t.Errorf("expected exactly 1 episode aged, got %d", aged)
	}

	var oldLowText, oldHighText, recentText string
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, oldLowID).Scan(&oldLowText); err != nil {
		t.Fatalf("query old low text: %v", err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, oldHighID).Scan(&oldHighText); err != nil {
		t.Fatalf("query old high text: %v", err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, recentID).Scan(&recentText); err != nil {
		t.Fatalf("query recent text: %v", err)
	}

	if oldLowText != "" {
		t.Errorf("expected old low-importance episode's screen_text to be emptied, got %q", oldLowText)
	}
	if oldHighText != "important old content about the core architecture" {
		t.Errorf("old high-importance episode's screen_text must survive, got %q", oldHighText)
	}
	if recentText != "trivial recent content" {
		t.Errorf("recent episode's screen_text must survive, got %q", recentText)
	}

	// rows must still exist (never deleted)
	var count int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 3 {
		t.Errorf("expected all 3 episode rows to survive aging, got %d", count)
	}

	// FTS mirror must not keep surfacing the cleared content (external-content fts5 requires the update trigger to purge the stale index entry).
	staleHits, err := store.SearchEpisodes(ctx, "trivial old content")
	if err != nil {
		t.Fatalf("SearchEpisodes after aging: %v", err)
	}
	for _, h := range staleHits {
		if strings.Contains(h.Content, "trivial old content") {
			t.Errorf("aged episode's old content still searchable via FTS: %+v", staleHits)
		}
	}
}

// TestStore_PruneAncientEpisodes_DeletesOnlyThinnedRowsPastThreshold seeds three episodes: one ancient AND already thinned (screen_text emptied by AgeEpisodes) — eligible for deletion; one ancient but NOT thinned — must survive, since PruneAncientEpisodes must never delete raw text that hasn't gone through the aging pass; and one recent + thinned — must survive since it isn't past the ancient threshold yet.
// Also confirms FTS5 no longer returns the deleted row's content, via both a keyword search and the fts5 'integrity-check' command, which fails loudly if the shadow index and content table drift.
func TestStore_PruneAncientEpisodes_DeletesOnlyThinnedRowsPastThreshold(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()
	raw := store.DB()

	// ancient + already thinned: eligible for deletion.
	ancientThinID, err := store.LogEpisode(ctx, "Notes", "ancient thinned", "zorptastic")
	if err != nil {
		t.Fatalf("LogEpisode (ancient thinned): %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.05, created_at = datetime('now', '-9000 hours') WHERE id = ?`,
		ancientThinID); err != nil {
		t.Fatalf("backdate ancient thinned: %v", err)
	}
	// thin it via the real aging path (not a hand-rolled UPDATE) so the FTS mirror is brought to empty by the existing episodes_au trigger first, exactly as would happen in production before a prune ever runs.
	if _, err := store.AgeEpisodes(ctx, 168*time.Hour, 0.5); err != nil {
		t.Fatalf("AgeEpisodes (thin ancientThinID): %v", err)
	}
	var thinnedCheck string
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, ancientThinID).Scan(&thinnedCheck); err != nil {
		t.Fatalf("query thinned check: %v", err)
	}
	if thinnedCheck != "" {
		t.Fatalf("precondition failed: ancientThinID must be thinned before prune, got %q", thinnedCheck)
	}

	// ancient but NOT thinned: still carries raw text, must survive.
	ancientRawID, err := store.LogEpisode(ctx, "VSCode", "ancient raw", "quibblefrond")
	if err != nil {
		t.Fatalf("LogEpisode (ancient raw): %v", err)
	}
	// high importance so AgeEpisodes above does not thin it too.
	if _, err := raw.ExecContext(ctx,
		`UPDATE episodes SET importance = 0.95, created_at = datetime('now', '-9000 hours') WHERE id = ?`,
		ancientRawID); err != nil {
		t.Fatalf("backdate ancient raw: %v", err)
	}

	// recent + thinned (screen_text already empty, but not past the ancient threshold): must survive.
	recentThinID, err := store.LogEpisode(ctx, "Notes", "recent thinned", "")
	if err != nil {
		t.Fatalf("LogEpisode (recent thinned): %v", err)
	}

	const ancientAfter = 365 * 24 * time.Hour
	pruned, err := store.PruneAncientEpisodes(ctx, ancientAfter)
	if err != nil {
		t.Fatalf("PruneAncientEpisodes: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected exactly 1 row pruned, got %d", pruned)
	}

	var count int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&count); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 surviving episode rows, got %d", count)
	}

	// the deleted row's id must actually be gone
	var stillThere int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM episodes WHERE id = ?`, ancientThinID).Scan(&stillThere); err != nil {
		t.Fatalf("check deleted row: %v", err)
	}
	if stillThere != 0 {
		t.Errorf("expected ancientThinID row to be deleted, but it still exists")
	}

	// the ancient-but-raw and recent-but-thinned rows must both survive untouched
	var ancientRawText string
	if err := raw.QueryRowContext(ctx, `SELECT screen_text FROM episodes WHERE id = ?`, ancientRawID).Scan(&ancientRawText); err != nil {
		t.Fatalf("query ancient raw survivor: %v", err)
	}
	if ancientRawText != "quibblefrond" {
		t.Errorf("ancient-but-not-thinned episode must survive with its raw text intact, got %q", ancientRawText)
	}
	var recentExists int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM episodes WHERE id = ?`, recentThinID).Scan(&recentExists); err != nil {
		t.Fatalf("check recent thinned survivor: %v", err)
	}
	if recentExists != 1 {
		t.Errorf("recent thinned episode must survive (not past the ancient threshold), but it's gone")
	}

	// FTS5 must not still surface the deleted row's old content — proves the episodes_ad DELETE trigger kept episodes_fts in sync rather than orphaning a shadow-index entry for the removed rowid.
	staleHits, err := store.SearchEpisodes(ctx, "zorptastic")
	if err != nil {
		t.Fatalf("SearchEpisodes after prune: %v", err)
	}
	if len(staleHits) != 0 {
		t.Errorf("deleted episode's old content still searchable via FTS after prune: %+v", staleHits)
	}

	// fts5 integrity-check: for an external-content table, this command scans the content table (episodes) and the shadow index and fails if they've drifted — the definitive proof the DELETE didn't orphan the index.
	if _, err := raw.ExecContext(ctx, `INSERT INTO episodes_fts(episodes_fts) VALUES('integrity-check')`); err != nil {
		t.Errorf("episodes_fts integrity-check failed after prune (shadow index orphaned): %v", err)
	}
}

// ─── Consolidation retrieval (Cycle 1: temporal walk) ─────────────────────────

// TestStore_EpisodesInWindow_ChronologicalAndBounded seeds episodes at controlled timestamps spanning a day, plus one episode clearly outside the window, and verifies EpisodesInWindow returns only the in-window rows, ordered oldest-first (the "day arc"), and honors limit.
func TestStore_EpisodesInWindow_ChronologicalAndBounded(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()
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

// TestStore_DiverseEpisodes_AvoidsNearDuplicateCluster seeds 5 near-identical episodes (same app+title, near-identical screen_text, all matching the focus term) plus 2 clearly-distinct episodes that also match. A plain top-N (RankedEpisodes) would return mostly near-duplicates since they all score similarly high; DiverseEpisodes must instead spread across the distinct content via MMR.
func TestStore_DiverseEpisodes_AvoidsNearDuplicateCluster(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	dupTexts := []string{
		"reviewing the DeepSeek post-training paper section on RLHF",
		"reviewing the DeepSeek post-training paper section on RLHF again",
		"reviewing the DeepSeek post-training paper section on RLHF once more",
		"still reviewing the DeepSeek post-training paper section on RLHF",
		"reviewing the DeepSeek post-training paper section on RLHF one more time",
	}
	dupIDs := make(map[int64]bool)
	for _, text := range dupTexts {
		id, err := store.LogEpisode(ctx, "Firefox", "DeepSeek Paper", text)
		if err != nil {
			t.Fatalf("LogEpisode (dup): %v", err)
		}
		dupIDs[id] = true
	}

	distinct1ID, err := store.LogEpisode(ctx, "Terminal", "training run", "kicking off a DeepSeek fine-tune job on the cluster")
	if err != nil {
		t.Fatalf("LogEpisode (distinct1): %v", err)
	}
	distinct2ID, err := store.LogEpisode(ctx, "Slack", "#research", "discussing DeepSeek benchmark results with the team")
	if err != nil {
		t.Fatalf("LogEpisode (distinct2): %v", err)
	}

	results, err := store.DiverseEpisodes(ctx, "DeepSeek", 3)
	if err != nil {
		t.Fatalf("DiverseEpisodes: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected some diverse episode results")
	}

	dupCount := 0
	foundDistinct := false
	for _, r := range results {
		if dupIDs[r.RefID] {
			dupCount++
		}
		if r.RefID == distinct1ID || r.RefID == distinct2ID {
			foundDistinct = true
		}
	}
	if dupCount > 2 {
		t.Errorf("expected at most 2 results from the near-duplicate cluster, got %d: %+v", dupCount, results)
	}
	if !foundDistinct {
		t.Errorf("expected at least one distinct episode in diverse results: %+v", results)
	}
}

// ─── Consolidation retrieval (Cycle 3: thread fusion) ─────────────────────────

// TestStore_RecallSubject_FusesThreadAndEpisodes seeds a live thread for subject "DeepSeek" plus several DeepSeek episodes, and verifies RecallSubject returns the thread's arc as a "[thread] ..." line followed by episode specifics as "[episode] ..." lines — arc first, then details, so a caller can narrate "you've been doing X, specifically Y, Z".
func TestStore_RecallSubject_FusesThreadAndEpisodes(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
		Subject: "DeepSeek",
		Kind:    "learning",
		State:   "studying post-training",
	}); err != nil {
		t.Fatalf("UpsertThread: %v", err)
	}

	episodeTexts := []string{
		"reading the DeepSeek post-training paper introduction",
		"skimming the DeepSeek RLHF section",
		"taking notes on the DeepSeek reward model design",
	}
	for _, text := range episodeTexts {
		if _, err := store.LogEpisode(ctx, "Firefox", "DeepSeek Paper", text); err != nil {
			t.Fatalf("LogEpisode: %v", err)
		}
	}

	lines, err := store.RecallSubject(ctx, "DeepSeek", 4)
	if err != nil {
		t.Fatalf("RecallSubject: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("expected some lines from RecallSubject")
	}

	var foundThread, foundEpisode bool
	threadIdx, episodeIdx := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "[thread] ") && strings.Contains(l, "DeepSeek") {
			foundThread = true
			if threadIdx == -1 {
				threadIdx = i
			}
		}
		if strings.HasPrefix(l, "[episode] ") {
			foundEpisode = true
			if episodeIdx == -1 {
				episodeIdx = i
			}
		}
	}
	if !foundThread {
		t.Errorf("expected a [thread] line in RecallSubject results: %+v", lines)
	}
	if !foundEpisode {
		t.Errorf("expected [episode] lines in RecallSubject results: %+v", lines)
	}
	if foundThread && foundEpisode && threadIdx > episodeIdx {
		t.Errorf("expected thread (arc) before episodes (specifics), got thread at %d, episode at %d: %+v", threadIdx, episodeIdx, lines)
	}
}

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

// TestCreateSchema_DomainColumnMigration_Idempotent verifies that reopening an existing on-disk DB via db.New doesn't error — createSchema's ensureColumn migration for the "domain" columns on nodes/episodes must be safe to run repeatedly, since it runs on every New() call. This matters because modernc.org/sqlite doesn't support ALTER TABLE ADD COLUMN IF NOT EXISTS (confirmed empirically — syntax error), so idempotency isn't free from SQLite itself.
func TestCreateSchema_DomainColumnMigration_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")

	first, err := db.New(path)
	if err != nil {
		t.Fatalf("New (first open): %v", err)
	}
	if _, err := first.LogEpisode(context.Background(), "Slack", "general", "some text"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same on-disk DB — createSchema runs again, exercising the "column already exists" branch of ensureColumn for both nodes and episodes.
	second, err := db.New(path)
	if err != nil {
		t.Fatalf("New (second open, migration re-run): %v", err)
	}
	defer second.Close()

	var domain string
	if err := second.DB().QueryRow(`SELECT domain FROM episodes WHERE app = 'Slack'`).Scan(&domain); err != nil {
		t.Fatalf("query domain after reopen: %v", err)
	}
	if domain != "work" {
		t.Errorf("domain after reopen = %q, want %q (data must survive the reopen+migration)", domain, "work")
	}
}

// TestLogEpisode_TagsDomainViaClassify verifies LogEpisode computes and stores memory.Classify(app, title) in the episodes.domain column.
func TestLogEpisode_TagsDomainViaClassify(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	id, err := store.LogEpisode(ctx, "Netflix", "The Bear S3E1", "watching")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	var domain string
	if err := store.DB().QueryRow(`SELECT domain FROM episodes WHERE id = ?`, id).Scan(&domain); err != nil {
		t.Fatalf("query episode domain: %v", err)
	}
	if domain != "personal" {
		t.Errorf("domain = %q, want %q", domain, "personal")
	}
}

// TestLogEpisode_DoesNotBlockOnSlowEmbedder proves LogEpisode returns immediately after its synchronous INSERT, even with a configured embedder that would block indefinitely — the async embed must run in its own goroutine, never inline on the caller's path.
func TestLogEpisode_DoesNotBlockOnSlowEmbedder(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

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

// TestLogSemanticNode_DomainInheritance_MajorityVote verifies that the summary node's domain is the majority vote over episodes logged since the current task's created_at, not a single Classify(app,title) call.
func TestLogSemanticNode_DomainInheritance_MajorityVote(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	// 1. Create the task (first flush — SameTask=false establishes currentTaskID and the task node's created_at anchor).
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "debugging session",
		Summary:  "started debugging",
	}); err != nil {
		t.Fatalf("LogSemanticNode (create task): %v", err)
	}

	// 2. Log episodes AFTER the task exists, 2 work-tagged vs 1 personal-tagged — work should win the majority vote.
	for _, app := range []string{"Slack", "Code", "Netflix"} {
		if _, err := store.LogEpisode(ctx, app, "generic title", "some content"); err != nil {
			t.Fatalf("LogEpisode(%s): %v", app, err)
		}
	}

	// 3. Continue the same task (SameTask=true) — this triggers the domain majority-vote query over episodes since the task's created_at.
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: true,
		TaskName: "debugging session",
		Summary:  "still debugging, mostly in Slack and Code",
	}); err != nil {
		t.Fatalf("LogSemanticNode (continue task): %v", err)
	}

	var domain string
	if err := store.DB().QueryRow(`
		SELECT domain FROM nodes
		WHERE type = 'summary'
		ORDER BY id DESC LIMIT 1
	`).Scan(&domain); err != nil {
		t.Fatalf("query summary node domain: %v", err)
	}
	if domain != "work" {
		t.Errorf("summary domain = %q, want %q (majority of Slack+Code work episodes over 1 personal)", domain, "work")
	}
}

// TestHybridSearch_DomainBoost_AppliesToLexicalHitsNotJustVectorHits guards a regression: HybridSearch's same-domain boost (applied when domainFilter=="") only ever looked at rrfCandidate.domain, but that field was populated for vector-sourced candidates only — every lexical/FTS5 candidate's domain was left at "", so the boost silently never fired for keyword-matched hits, the common case since the vector index starts empty and grows slowly.
// Two episodes with identical keyword-matchable content (so their base RRF scores tie exactly), one tagged "work" (matching the store's inferred current domain) and one "personal": the work one must rank first.
func TestHybridSearch_DomainBoost_AppliesToLexicalHitsNotJustVectorHits(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	// Personal-tagged episode logged first, so it's NOT the most recent
	// (avoids conflating "most recent" with "the one that should win").
	if _, err := store.LogEpisode(ctx, "Netflix", "generic title", "shared keyword zorptacular"); err != nil {
		t.Fatalf("LogEpisode (personal): %v", err)
	}
	// Work-tagged episode logged second/last -> this is currentDomain.
	if _, err := store.LogEpisode(ctx, "Slack", "generic title", "shared keyword zorptacular"); err != nil {
		t.Fatalf("LogEpisode (work): %v", err)
	}

	hits, err := store.HybridSearch(ctx, "zorptacular", "", 10)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d: %+v", len(hits), hits)
	}

	var workRank, personalRank = -1, -1
	for i, h := range hits {
		if h.Source != "episode" {
			continue
		}
		var domain string
		if err := store.DB().QueryRow(`SELECT domain FROM episodes WHERE id = ?`, h.RefID).Scan(&domain); err != nil {
			t.Fatalf("query hit domain: %v", err)
		}
		switch domain {
		case "work":
			workRank = i
		case "personal":
			personalRank = i
		}
	}
	if workRank == -1 || personalRank == -1 {
		t.Fatalf("expected both a work and a personal episode hit, got: %+v", hits)
	}
	if workRank >= personalRank {
		t.Errorf("expected the work-tagged episode (matching current domain) to rank above the personal one via the domain boost, got work at index %d, personal at index %d: %+v", workRank, personalRank, hits)
	}
}

// TestSearchMemory_PopulatesCreatedAtForSummaryHit verifies a summary-node hit carries its real created_at instead of the zero value — FormatHit's relative-age suffix needs this to render anything for summary/digest hits.
func TestSearchMemory_PopulatesCreatedAtForSummaryHit(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{SameTask: false, TaskName: "debugging session", Summary: "fixed the parser edge case"}); err != nil {
		t.Fatalf("LogSemanticNode: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "parser edge case")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit")
	}
	if hits[0].CreatedAt.IsZero() {
		t.Errorf("expected a non-zero CreatedAt on the summary hit, got zero value: %+v", hits[0])
	}
}

// TestSearchMemory_SummaryHitReturnsProseNotJSON verifies a summary hit's Content is the plain summary text, not the marshalled TaskSummary JSON that LogSemanticNode stores in nodes.content. The model sees this string verbatim, so a raw `{"same_task":false,...}` blob is both unreadable and wastes context.
func TestSearchMemory_SummaryHitReturnsProseNotJSON(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if err := store.LogSemanticNode(ctx, memory.TaskSummary{SameTask: false, TaskName: "debugging session", Summary: "fixed the parser edge case"}); err != nil {
		t.Fatalf("LogSemanticNode: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "parser edge case")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit")
	}
	if got := hits[0].Content; got != "fixed the parser edge case" {
		t.Errorf("expected the extracted summary prose, got %q", got)
	}
}

// TestSearchMemory_DigestHitReturnsPlainTextUnchanged guards the JSON-extraction above against digest nodes, whose content is plain prose (ReplaceAllNotes writes it directly) and must pass through untouched rather than tripping json_extract.
func TestSearchMemory_DigestHitReturnsPlainTextUnchanged(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	var dayID int64
	if err := store.DB().QueryRowContext(ctx, `INSERT INTO nodes (parent_id, type, content) VALUES (NULL, 'day', '2026-08-28') RETURNING id`).Scan(&dayID); err != nil {
		t.Fatalf("insert day node: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO nodes (parent_id, type, content) VALUES (?, 'digest', 'spent the day on the parser edge case')`, dayID); err != nil {
		t.Fatalf("insert digest node: %v", err)
	}

	hits, err := store.SearchMemory(ctx, "parser edge case")
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit")
	}
	if got := hits[0].Content; got != "spent the day on the parser edge case" {
		t.Errorf("expected the digest's plain text unchanged, got %q", got)
	}
}

// TestFormatHit_NoteWithCreatedAt_StaysAgeless verifies notes never get an age suffix even when CreatedAt is populated — they're durable facts, not time-decaying observations.
func TestFormatHit_NoteWithCreatedAt_StaysAgeless(t *testing.T) {
	h := db.MemoryHit{Source: "note", Content: "the user's favorite color is blue", CreatedAt: time.Now().Add(-30 * 24 * time.Hour)}

	got := db.FormatHit(h, 0)

	if strings.Contains(got, "ago") {
		t.Errorf("expected no age suffix on a note hit, got %q", got)
	}
}

// TestFormatHit_EpisodeProvenance_TableShapes is WP12 Part C: episode hits must render App/Title so the model can tell two unrelated captures apart instead of confabulating a connection between them (exactly how the Aug 7 log's confabulation happened — see systemInstructionText's new synthesis-rule comment). Every other source's shape stays exactly as it was.
func TestFormatHit_EpisodeProvenance_TableShapes(t *testing.T) {
	cases := []struct {
		name string
		hit  db.MemoryHit
		want string
	}{
		{
			name: "episode with app and title carries provenance",
			hit:  db.MemoryHit{Source: "episode", Content: "fixing the null pointer bug", App: "Code", Title: "tracker_linux.go"},
			want: "[episode] Code — tracker_linux.go: fixing the null pointer bug",
		},
		{
			name: "episode without app/title falls back to the plain shape",
			hit:  db.MemoryHit{Source: "episode", Content: "debugging the parser"},
			want: "[episode] debugging the parser",
		},
		{
			name: "thread hit unchanged",
			hit:  db.MemoryHit{Source: "thread", Content: "Suits Season 7 — watching episode 6"},
			want: "[thread] Suits Season 7 — watching episode 6",
		},
		{
			name: "note hit unchanged",
			hit:  db.MemoryHit{Source: "note", Content: "the user's favorite color is blue"},
			want: "[note] the user's favorite color is blue",
		},
		{
			name: "summary hit unchanged",
			hit:  db.MemoryHit{Source: "summary", Content: "wrote a blog post about Go generics"},
			want: "[summary] wrote a blog post about Go generics",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := db.FormatHit(tc.hit, 0); got != tc.want {
				t.Errorf("FormatHit(%+v) = %q, want %q", tc.hit, got, tc.want)
			}
		})
	}
}

// TestFormatHit_EpisodeWithAppTitleAndAge_CombinesAgeAndProvenance verifies the age suffix and the App/Title provenance both show up together for an episode hit that has both — "[episode (3d ago)] Code — tracker_linux.go: …", one doesn't replace the other.
func TestFormatHit_EpisodeWithAppTitleAndAge_CombinesAgeAndProvenance(t *testing.T) {
	h := db.MemoryHit{Source: "episode", Content: "fixing the null pointer bug", App: "Code", Title: "tracker_linux.go", CreatedAt: time.Now().Add(-3 * 24 * time.Hour)}

	got := db.FormatHit(h, 0)

	if !strings.Contains(got, "[episode (3d ago)]") {
		t.Errorf("expected age combined with the source label, got %q", got)
	}
	if !strings.Contains(got, "Code — tracker_linux.go: fixing the null pointer bug") {
		t.Errorf("expected app/title provenance preserved alongside age, got %q", got)
	}
}

// TestRetrieveRelevant_EmptyFocus_ReturnsNilWithoutSearching verifies an empty focus returns nil directly instead of substituting the literal string "recent context" and running a real search for those words — which could spuriously match unrelated stored content that happens to contain "recent" and "context".
func TestRetrieveRelevant_EmptyFocus_ReturnsNilWithoutSearching(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "stayed in a very recent context of debugging", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	out, err := store.RetrieveRelevant(ctx, "", 4)
	if err != nil {
		t.Fatalf("RetrieveRelevant: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for an empty focus, got %v", out)
	}
}

// TestRelevantNotes_EmptyFocus_ReturnsNilWithoutSearching is the same property for RelevantNotes.
func TestRelevantNotes_EmptyFocus_ReturnsNilWithoutSearching(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "stayed in a very recent context of debugging", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	out, err := store.RelevantNotes(ctx, "", 4)
	if err != nil {
		t.Fatalf("RelevantNotes: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for an empty focus, got %v", out)
	}
}

// TestGetImplicitContext_NoWorkingStateNoRecentTasks_SkipsRelevanceSearch verifies a cold-start store (no working_state, no recent task nodes) never runs the old literal "recent context" relevance search — proven by a note containing exactly those words that the buggy search would have matched, but which the fixed code must not surface via relevance at all.
func TestGetImplicitContext_NoWorkingStateNoRecentTasks_SkipsRelevanceSearch(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if _, err := store.LogNote(ctx, "stayed in a very recent context of debugging", "fact"); err != nil {
		t.Fatalf("LogNote: %v", err)
	}

	branch, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("GetImplicitContext: %v", err)
	}
	for _, line := range branch {
		if strings.Contains(line, "stayed in a very recent context of debugging") {
			t.Errorf("expected the placeholder-matching note NOT to surface via relevance search, got it in branch: %v", branch)
		}
	}
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

func TestLogEpisode_JunkOnlyRawFallbackIsStripped(t *testing.T) {
	ctx := context.Background()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	id, err := store.LogEpisode(ctx, "", "", "￼\n￼￼ ￼\n￼")
	if err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	var text string
	if err := store.DB().QueryRow(`SELECT screen_text FROM episodes WHERE id = ?`, id).Scan(&text); err != nil {
		t.Fatalf("query episode screen_text: %v", err)
	}
	if strings.ContainsRune(text, '￼') {
		t.Errorf("screen_text = %q, want no object replacement characters", text)
	}
}
