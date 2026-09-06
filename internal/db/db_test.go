package db_test

// tests are first class citizens

import (
	"context"
	"database/sql"
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

// memStore opens a throwaway in-memory store that is closed when the test ends.
func memStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// t param is test controller. object to provide methods to control the flow of the test + reporting
func TestStore_Notes_DeleteAndDedupe(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id, _ := store.LogNote(ctx, "user works at Acme ESG", "fact")

	// re-logging same content + kind is a no-op (idempotent)
	id2, err := store.LogNote(ctx, "user works at Acme ESG", "fact")
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
	store := memStore(t)

	// seed a summary and a note
	_ = store.LogSemanticNode(ctx, memory.TaskSummary{
		SameTask: false,
		TaskName: "Voice Pipeline",
		Summary:  "Debugging WebSocket reconnect loop in Gemini Live session",
	})
	_, _ = store.LogNote(ctx, "user works at Acme ESG as an intern", "fact")

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
	noteHits, err := store.SearchMemory(ctx, "Acme")
	if err != nil {
		t.Fatalf("SearchMemory notes: %v", err)
	}
	if len(noteHits) == 0 {
		t.Fatal("FTS5 returned no hits for 'Acme'")
	}
	if noteHits[0].Source != "note" {
		t.Errorf("expected source=note, got %s", noteHits[0].Source)
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

// TestStore_SearchMemory_AllStopwordQuery_FallsBackWithoutError verifies that when tokenization drops every term (an all-stopword query), buildFTSMatch falls back to the original whole-query phrase rather than handing FTS5 an empty/invalid MATCH expression that would error.
func TestStore_SearchMemory_AllStopwordQuery_FallsBackWithoutError(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	_, _ = store.LogNote(ctx, "user prefers dark mode", "preference")

	if _, err := store.SearchMemory(ctx, "is are was"); err != nil {
		t.Fatalf("expected all-stopword query to fall back to whole-query phrase without error, got: %v", err)
	}
}

// TestStore_SearchEpisodes_NaturalLanguageQuery_ORofTerms is the SearchEpisodes analogue of the SearchMemory OR-of-terms fix — same whole-query phrase-quoting bug, same shared buildFTSMatch fix.
func TestStore_SearchEpisodes_NaturalLanguageQuery_ORofTerms(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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

func TestStore_ExistingNotes(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	id1, _ := store.LogNote(ctx, "user is a Go developer", "fact")
	id2, _ := store.LogNote(ctx, "user prefers dark mode", "fact")
	// Every caller of ExistingNotes feeds the result to a model that merges and drops entries, then writes the survivors back, so only facts may be handed over. A meeting note offered up here would be summarised away.
	if _, err := store.LogNote(ctx, "# Meeting minutes\n\n- ship on friday", "meeting"); err != nil {
		t.Fatalf("LogNote(meeting): %v", err)
	}

	refs, err := store.ExistingNotes(ctx)
	if err != nil {
		t.Fatalf("ExistingNotes: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs (the facts only), got %d: %+v", len(refs), refs)
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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

	// seed 3 summaries
	for _, name := range []string{"first-summary-alpha", "second-summary-beta", "third-summary-gamma"} {
		_ = store.LogSemanticNode(ctx, memory.TaskSummary{
			SameTask: false,
			TaskName: name,
			Summary:  name + " content",
		})
	}

	// seed a digest via direct insert (should also appear)
	_, err := store.DB().ExecContext(ctx,
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
	store := memStore(t)

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
	_, err := store.DB().ExecContext(ctx,
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
	store := memStore(t)

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

	// digest must exist under dayID
	var digestID int64
	var digestContent string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT id, content FROM nodes WHERE type='digest' AND parent_id=?`, dayID).Scan(&digestID, &digestContent); err != nil {
		t.Fatalf("find digest node: %v", err)
	}
	if digestContent != digest {
		t.Errorf("digest content mismatch: got %q", digestContent)
	}

	// summaries must survive, reparented under the digest rather than deleted
	var sumCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE type='summary'`).Scan(&sumCount); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if sumCount != 2 {
		t.Errorf("expected 2 summaries to survive replace, got %d", sumCount)
	}
	for _, id := range summaryIDs {
		var parentID int64
		if err := store.DB().QueryRowContext(ctx, `SELECT parent_id FROM nodes WHERE id=?`, id).Scan(&parentID); err != nil {
			t.Fatalf("find summary %d: %v", id, err)
		}
		if parentID != digestID {
			t.Errorf("summary %d: expected parent_id=%d, got %d", id, digestID, parentID)
		}
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

	// FTS: terms unique to the surviving summaries must still be searchable — they were reparented, not deleted.
	oldHits1, err := store.SearchMemory(ctx, "uniquetoken1")
	if err != nil {
		t.Fatalf("SearchMemory for surviving summary term: %v", err)
	}
	if len(oldHits1) == 0 {
		t.Error("FTS lost surviving summary term 'uniquetoken1' — summary should not have been deleted")
	}

	oldHits2, err := store.SearchMemory(ctx, "uniquetoken2")
	if err != nil {
		t.Fatalf("SearchMemory for surviving summary term: %v", err)
	}
	if len(oldHits2) == 0 {
		t.Error("FTS lost surviving summary term 'uniquetoken2' — summary should not have been deleted")
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
	if err := store.ReplaceSummariesWithDigest(ctx, dayID, []int64{loose1, loose2}, "a fresh digest text that must be ignored"); err != nil {
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
	if digestContent != "first digest text" {
		t.Errorf("digest content = %q, want the original left untouched, not the freshly generated text", digestContent)
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

// TestStore_UpsertThread_NewThread verifies that a zero-ID upsert creates a new row with the right initial salience (0.5 when Novel=false, 0.6 when Novel=true), times_seen=1, and status='active'.
func TestStore_UpsertThread_NewThread(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

	_, err := store.UpsertThread(ctx, memory.ThreadUpdate{
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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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

// TestStore_DiverseEpisodes_AvoidsNearDuplicateCluster seeds 5 near-identical episodes (same app+title, near-identical screen_text, all matching the focus term) plus 2 clearly-distinct episodes that also match. A plain top-N (RankedEpisodes) would return mostly near-duplicates since they all score similarly high; DiverseEpisodes must instead spread across the distinct content via MMR.
func TestStore_DiverseEpisodes_AvoidsNearDuplicateCluster(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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
	store := memStore(t)

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
		if strings.HasPrefix(l, "[thread#") && strings.Contains(l, "DeepSeek") {
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
	store := memStore(t)

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

// TestLogSemanticNode_DomainInheritance_MajorityVote verifies that the summary node's domain is the majority vote over episodes logged since the current task's created_at, not a single Classify(app,title) call.
func TestLogSemanticNode_DomainInheritance_MajorityVote(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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
	store := memStore(t)

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

// A summary-node hit must carry the plain summary text, not the marshalled TaskSummary JSON that LogSemanticNode stores in nodes.content (the model sees this string verbatim), and its real created_at rather than the zero value (FormatHit's relative-age suffix needs it).
func TestSearchMemory_SummaryHit_CarriesProseAndCreatedAt(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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
	if hits[0].CreatedAt.IsZero() {
		t.Errorf("expected a non-zero CreatedAt on the summary hit, got zero value: %+v", hits[0])
	}
}

// TestSearchMemory_DigestHitReturnsPlainTextUnchanged guards the JSON-extraction above against digest nodes, whose content is plain prose (ReplaceAllNotes writes it directly) and must pass through untouched rather than tripping json_extract.
func TestSearchMemory_DigestHitReturnsPlainTextUnchanged(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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

// TestFormatHit_EpisodeProvenance_TableShapes is WP12 Part C: episode hits must render App/Title so the model can tell two unrelated captures apart instead of confabulating a connection between them (exactly how the Aug 7 log's confabulation happened — see systemInstructionText's new synthesis-rule comment). Every other source's shape stays exactly as it was.
func TestFormatHit_EpisodeProvenance_TableShapes(t *testing.T) {
	cases := []struct {
		name string
		hit  db.MemoryHit
		want string
	}{
		{
			name: "episode with app and title carries provenance trailing, content first",
			hit:  db.MemoryHit{Source: "episode", Content: "fixing the null pointer bug", App: "Code", Title: "tracker_linux.go"},
			want: "[episode] fixing the null pointer bug (Code — tracker_linux.go)",
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
		{
			// Notes are durable facts, not time-decaying observations, so they never get an age suffix.
			name: "note with a created_at stays ageless",
			hit:  db.MemoryHit{Source: "note", Content: "the user's favorite color is blue", CreatedAt: time.Now().Add(-30 * 24 * time.Hour)},
			want: "[note] the user's favorite color is blue",
		},
		{
			// Age and provenance both show up: one does not replace the other. Past the first day the label also carries the calendar date, so "which day was that" is answerable straight off the row.
			name: "episode with app, title and age",
			hit:  db.MemoryHit{Source: "episode", Content: "fixing the null pointer bug", App: "Code", Title: "tracker_linux.go", CreatedAt: time.Now().Add(-3 * 24 * time.Hour)},
			want: "[episode (" + time.Now().Add(-3*24*time.Hour).Local().Format("Mon Jan 2 15:04") + ", 3d ago)] fixing the null pointer bug (Code — tracker_linux.go)",
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

// TestRetrieveRelevant_EmptyFocus_ReturnsNilWithoutSearching verifies an empty focus returns nil directly instead of substituting the literal string "recent context" and running a real search for those words — which could spuriously match unrelated stored content that happens to contain "recent" and "context".
func TestRetrieveRelevant_EmptyFocus_ReturnsNilWithoutSearching(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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
	store := memStore(t)

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

// TestStore_SummaryFTS_IndexesSummaryTextNotJSONKeys proves that a summary node's FTS entry holds the plain summary prose, not the marshalled TaskSummary. Summary nodes store the whole struct as JSON in nodes.content, so indexing it verbatim made the JSON keys ("same_task", "task_name") live search terms that matched every summary ever written.
func TestStore_SummaryFTS_IndexesSummaryTextNotJSONKeys(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	const summaryText = "Rolled the staging cluster to 1.30 and watched every pod drain cleanly."
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "Kubernetes upgrade", Summary: summaryText}); err != nil {
		t.Fatalf("LogSemanticNode: %v", err)
	}

	if hits, err := store.SearchMemory(ctx, "same_task"); err != nil {
		t.Fatalf("SearchMemory(same_task): %v", err)
	} else if len(hits) != 0 {
		t.Errorf("searching a JSON key returned %d hits, want 0: %+v", len(hits), hits)
	}

	hits, err := store.SearchMemory(ctx, "staging cluster")
	if err != nil {
		t.Fatalf("SearchMemory(staging cluster): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit for the summary prose, got %d: %+v", len(hits), hits)
	}
	if hits[0].Content != summaryText {
		t.Errorf("hit content = %q, want %q", hits[0].Content, summaryText)
	}
}

// TestCreateSchema_RebuildsSummaryFTSContent checks the migration for DBs written before the trigger extracted $.summary: their memory_fts rows still hold raw TaskSummary JSON, so reopening the store must rewrite them to the summary text.
func TestCreateSchema_RebuildsSummaryFTSContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")

	first, err := db.New(path)
	if err != nil {
		t.Fatalf("New (first open): %v", err)
	}
	// Write the FTS row the way the pre-fix trigger did: the whole marshalled struct.
	const summaryText = "Reviewed the quarterly invoice reconciliation spreadsheet."
	raw := `{"same_task":false,"task_name":"Invoices","summary":"` + summaryText + `"}`
	if _, err := first.DB().Exec(`INSERT INTO memory_fts(content, source, ref_id) VALUES (?, 'summary', 4242)`, raw); err != nil {
		t.Fatalf("seed legacy fts row: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := db.New(path)
	if err != nil {
		t.Fatalf("New (second open, migration re-run): %v", err)
	}
	defer second.Close()

	var content string
	if err := second.DB().QueryRow(`SELECT content FROM memory_fts WHERE ref_id = 4242`).Scan(&content); err != nil {
		t.Fatalf("read migrated fts row: %v", err)
	}
	if content != summaryText {
		t.Errorf("migrated content = %q, want %q", content, summaryText)
	}
	if hits, err := second.SearchMemory(context.Background(), "task_name"); err != nil {
		t.Fatalf("SearchMemory(task_name): %v", err)
	} else if len(hits) != 0 {
		t.Errorf("searching a JSON key returned %d hits, want 0: %+v", len(hits), hits)
	}
}

func TestLogSemanticNode_StripsObjectChars(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	err := store.LogSemanticNode(ctx, memory.TaskSummary{SameTask: false, TaskName: "Photos ￼ review", Summary: "Browsing ￼￼ the gallery ￼"})
	if err != nil {
		t.Fatalf("LogSemanticNode: %v", err)
	}

	var content string
	if err := store.DB().QueryRow(`SELECT content FROM nodes WHERE type = 'summary'`).Scan(&content); err != nil {
		t.Fatalf("query summary node: %v", err)
	}
	if strings.ContainsRune(content, '￼') {
		t.Errorf("summary node content = %q, want no object replacement characters", content)
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
	if err := memory.NewNoteCompactor(llm, store).Compact(ctx); err != nil {
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

// TestFormatHit_ThreadCarriesRefID verifies a thread hit is surfaced with its id, the way notes already are. Without one the model can see that a thread's summary is wrong (it diagnosed exactly that in a real session) and have nothing to name in a repair call.
func TestFormatHit_ThreadCarriesRefID(t *testing.T) {
	got := db.FormatHit(db.MemoryHit{
		Source:    "thread",
		RefID:     19,
		Content:   "mf x mdev — recurring Microsoft Teams sync",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}, 0)

	if !strings.HasPrefix(got, "[thread#19 ") {
		t.Errorf("FormatHit = %q, want it to lead with the thread's id", got)
	}
	if !strings.Contains(got, "2d ago") {
		t.Errorf("FormatHit = %q, want the age kept alongside the id", got)
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
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "climate scoring", Summary: "adjusting vulnerability scores"}); err != nil {
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

// A thread says what the user was doing; the episodes say what was actually on screen while they did it. Nothing joined the two, so a question like "what were those code review findings" could reach the thread's one-line summary and never the evidence behind it — 261 threads and 4,901 episodes with no edge between them.
func TestStore_ThreadEpisodes_LinkAndReadBack(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	inWindow, err := store.WriteEpisode(ctx, db.EpisodeWrite{App: "Code", Title: "search.go", ScreenText: "eleven findings, two of them high severity"})
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Minute)

	threadID, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "code review of ora", Kind: "work", State: "reading the findings"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LinkEpisodesToThread(ctx, threadID, since, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := store.EpisodesForThread(ctx, threadID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != inWindow {
		t.Fatalf("got %+v, want the one episode written inside the window", got)
	}
	if !strings.Contains(got[0].ScreenText, "eleven findings") {
		t.Errorf("the evidence did not come back with the episode: %q", got[0].ScreenText)
	}
}

// The compiler discarded the episode-to-thread attribution for months, but it left a trace: one summary node per thread per flush, carrying the thread's subject and the moment of the flush. Those timestamps are the flush boundaries, so the edge can be rebuilt exactly rather than guessed at — every episode between one flush and the next belongs to the threads that flush produced.
func TestStore_BackfillThreadEdges_RebuildsFromSummaryNodes(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	threadID, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "code review of ora", Kind: "work"})
	if err != nil {
		t.Fatal(err)
	}
	inFlush, err := store.WriteEpisode(ctx, db.EpisodeWrite{App: "Code", Title: "search.go", ScreenText: "eleven findings"})
	if err != nil {
		t.Fatal(err)
	}
	// The summary node the compiler wrote for that flush, named after the thread.
	if err := store.LogSemanticNode(ctx, memory.TaskSummary{TaskName: "code review of ora", Summary: "reviewed the code"}); err != nil {
		t.Fatal(err)
	}

	linked, err := store.BackfillThreadEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if linked == 0 {
		t.Fatal("backfill linked nothing")
	}

	eps, err := store.EpisodesForThread(ctx, threadID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].ID != inFlush {
		t.Fatalf("got %+v, want the episode captured before that flush", eps)
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

	body := "Attendees: Alex, Priya. " + strings.Repeat("the payments team walked through the checkout flow again. ", 8) + "DECISION: ship the kubernetes migration on Friday."
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

// TestRankedEpisodes_MultiWordFocusMatchesWordsApart checks that a multi-word recall subject is tokenised rather than quoted as one FTS5 phrase. Input: a focus whose words appear in an episode but not adjacent. Output: the episode is still a candidate.
func TestRankedEpisodes_MultiWordFocusMatchesWordsApart(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if _, err := store.LogEpisode(ctx, "Firefox", "Riddler", "reviewing the Riddler puzzle generator and its scoring project notes"); err != nil {
		t.Fatalf("LogEpisode: %v", err)
	}

	hits, err := store.RankedEpisodes(ctx, "Riddler project", 5)
	if err != nil {
		t.Fatalf("RankedEpisodes: %v", err)
	}
	if len(hits) == 0 {
		t.Error(`a two-word subject matched nothing: the focus is being quoted as one contiguous FTS5 phrase`)
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

// TestDeleteNote_MissingID_Errors checks that deleting an id naming nothing is reported as a failure. A nil error here made the revise tool answer "deleted" for an id the model invented, and fired the vector-index delete on it.
func TestDeleteNote_MissingID_Errors(t *testing.T) {
	ctx := context.Background()
	store := memStore(t)

	if err := store.DeleteNote(ctx, 4242); err == nil {
		t.Error("expected an error deleting a nonexistent note id, got nil")
	}
}
