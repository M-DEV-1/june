// structuring stage: the long-haul question. After many "days" of low-signal noise pile up on a handful of real facts, does ORA's world model still keep them retrievable and prominent, or does noise bury them like an append-only log would? Regression test for "context rot".
//
// runs against a real file-backed sqlite store (evals/harness), no live LLM calls — internal/memory's Gemini-backed Summarizer/Digester are skipped, testing the DB-level primitives underneath them instead: LogNote/GetNotes/ReplaceAllNotes, UpsertThread/GetLiveThreads, LogEpisode/AgeEpisodes, OldSummaryGroups/ReplaceSummariesWithDigest, GetImplicitContext.
package structuring_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ora/evals/dataset"
	"ora/evals/harness"
	"ora/internal/db"
	"ora/internal/memory"
)

// noisyDays and notesPerNoisyDay set the scale of the long-haul simulation — ~17 notes/day of unreconciled accumulation is already flagged as rot-inducing, notesPerNoisyDay sits past that.
const (
	noisyDays        = 30
	notesPerNoisyDay = 25 // ORA project memory already flags ~17/day as rot-inducing
	episodesPerDay   = 5
)

// fillerVerbs is the low-signal vocabulary for noisy-day notes/threads/episodes — ambient, forgettable activity an always-on capture loop logs constantly, and which should never crowd out a real fact.
var fillerVerbs = []string{
	"opened a terminal",
	"watched a video",
	"browsed reddit",
	"checked email",
	"opened a pdf",
	"listened to music",
	"switched tabs",
	"opened slack",
	"scrolled twitter",
	"opened the file manager",
}

// noiseThreadSubjects get reinforced every noisy day — exercises the same update-in-place path as TestStructuring_ThreadDedupInvariant, but as incidental background noise.
var noiseThreadSubjects = []string{
	"background music",
	"terminal fiddling",
	"casual browsing",
}

// seedFact writes one dataset.Fact into the store via the storage API matching its Kind.
// mirrors evals/capture's capture-stage seeding.
func seedFact(t *testing.T, ctx context.Context, store *db.Store, f dataset.Fact) {
	t.Helper()
	switch f.Kind {
	case "note":
		if _, err := store.LogNote(ctx, f.Content, "note"); err != nil {
			t.Fatalf("seed LogNote(%q) failed: %v", f.ID, err)
		}
	case "thread":
		if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: f.Content,
			Kind:    "thread",
			State:   "seeded",
		}); err != nil {
			t.Fatalf("seed UpsertThread(%q) failed: %v", f.ID, err)
		}
	default:
		t.Fatalf("fact %q has unrecognized kind %q (expected \"note\" or \"thread\")", f.ID, f.Kind)
	}
}

// simulateNoisyDay logs one "day" worth of low-signal filler: a batch of unique generic notes, a reinforcement touch on a handful of recurring noise threads, and a few filler episodes backdated to `day`. Dedup should keep the noise threads from growing new rows.
// daysFromNow is how many days "ago" this simulated day is, relative to the end of the run.
func simulateNoisyDay(t *testing.T, ctx context.Context, store *db.Store, day, daysFromNow int) {
	t.Helper()

	for i := 0; i < notesPerNoisyDay; i++ {
		content := fmt.Sprintf("day %d filler: user %s (tick %d)", day, fillerVerbs[i%len(fillerVerbs)], i)
		if _, err := store.LogNote(ctx, content, "note"); err != nil {
			t.Fatalf("noisy-day filler LogNote failed: %v", err)
		}
	}

	for _, subj := range noiseThreadSubjects {
		if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: subj,
			Kind:    "routine",
			State:   fmt.Sprintf("day %d", day),
		}); err != nil {
			t.Fatalf("noisy-day thread reinforcement failed: %v", err)
		}
	}

	for i := 0; i < episodesPerDay; i++ {
		app := "Terminal"
		title := fmt.Sprintf("day %d shell session %d", day, i) // unique per call: no revisit bonus
		screenText := fmt.Sprintf("noisy filler screen text day %d iter %d lorem ipsum", day, i)
		id, err := store.LogEpisode(ctx, app, title, screenText)
		if err != nil {
			t.Fatalf("noisy-day filler LogEpisode failed: %v", err)
		}
		// backdate so AgeEpisodes has a realistic time spread to act on later.
		if _, err := store.DB().ExecContext(ctx,
			`UPDATE episodes SET created_at = datetime('now', ?) WHERE id = ?`,
			fmt.Sprintf("-%d days", daysFromNow), id); err != nil {
			t.Fatalf("backdate filler episode: %v", err)
		}
	}
}

// probeSucceeds reports whether every substring in want appears somewhere in hits, case-insensitive.
func probeSucceeds(hits []string, want []string) bool {
	joined := strings.ToLower(strings.Join(hits, "\n"))
	for _, w := range want {
		if !strings.Contains(joined, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

func containsNoteContent(notes []db.Note, content string) bool {
	for _, n := range notes {
		if n.Content == content {
			return true
		}
	}
	return false
}

func containsThreadSubject(threads []memory.Thread, subject string) bool {
	for _, th := range threads {
		if th.Subject == subject {
			return true
		}
	}
	return false
}

// TestStructuring_LongHaul is the suite's centerpiece.
// seed the 10 real facts, bury them under noisyDays of noisy accumulation, and confirm they're still retrievable/prominent afterward.
func TestStructuring_LongHaul(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)

	facts, err := dataset.Load()
	if err != nil {
		t.Fatalf("dataset.Load() failed: %v", err)
	}
	if len(facts) == 0 {
		t.Fatal("dataset.Load() returned no facts — nothing to eval")
	}

	for _, f := range facts {
		seedFact(t, ctx, store, f)
	}

	beforeNotes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("baseline GetNotes failed: %v", err)
	}
	beforeThreads, err := store.GetLiveThreads(ctx, 1000)
	if err != nil {
		t.Fatalf("baseline GetLiveThreads failed: %v", err)
	}

	// baseline probe recall, taken right after seeding and before any noise.
	// isolates what noise specifically did, instead of conflating it with a pre-existing retrieval gap.
	type probeCheck struct {
		factID string
		query  string
		want   []string
		okNow  bool
	}
	var probes []probeCheck
	for _, f := range facts {
		for _, p := range f.Probes {
			hits, err := store.RetrieveRelevant(ctx, p.Query, 10)
			if err != nil {
				t.Fatalf("baseline RetrieveRelevant(%q) failed: %v", p.Query, err)
			}
			probes = append(probes, probeCheck{f.ID, p.Query, p.ExpectContains, probeSucceeds(hits, p.ExpectContains)})
		}
	}

	// simulate noisyDays of low-signal accumulation.
	totalFillerEpisodes := 0
	for day := 1; day <= noisyDays; day++ {
		simulateNoisyDay(t, ctx, store, day, noisyDays-day)
		totalFillerEpisodes += episodesPerDay
	}
	totalFillerNotes := noisyDays * notesPerNoisyDay

	afterNotes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("post-noise GetNotes failed: %v", err)
	}
	afterThreads, err := store.GetLiveThreads(ctx, 1000)
	if err != nil {
		t.Fatalf("post-noise GetLiveThreads failed: %v", err)
	}

	// sanity check on the simulation itself: the notes table really did grow by the expected filler volume.
	if len(afterNotes) < len(beforeNotes)+totalFillerNotes {
		t.Fatalf("sanity: expected notes table to grow by >= %d filler rows, got %d -> %d",
			totalFillerNotes, len(beforeNotes), len(afterNotes))
	}

	// 1. facts still present verbatim in the primary stores.
	survived := 0
	for _, f := range facts {
		var ok bool
		switch f.Kind {
		case "note":
			ok = containsNoteContent(afterNotes, f.Content)
			if !ok {
				t.Errorf("fact %q (note) not found verbatim in GetNotes after %d noisy days", f.ID, noisyDays)
			}
		case "thread":
			ok = containsThreadSubject(afterThreads, f.Content)
			if !ok {
				t.Errorf("fact %q (thread) not found verbatim in GetLiveThreads after %d noisy days", f.ID, noisyDays)
			}
		}
		if ok {
			survived++
		}
	}

	// 2. facts still surface via search, not just present in a raw scan. Queries with the fact's own stored content since SearchMemory does exact FTS5 phrase matching — this is the "still findable, not pushed out by filler rows" check.
	// threads are mirrored into memory_fts as "subject — state", so match on containment rather than exact equality.
	selfSearchSurvived := 0
	for _, f := range facts {
		hits, err := store.SearchMemory(ctx, f.Content)
		if err != nil {
			t.Errorf("SearchMemory(%q) failed: %v", f.ID, err)
			continue
		}
		found := false
		for _, h := range hits {
			if h.Content == f.Content || strings.Contains(h.Content, f.Content) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("fact %q not found via SearchMemory on its own content after %d noisy days / %d filler notes — signal buried by noise", f.ID, noisyDays, totalFillerNotes)
		} else {
			selfSearchSurvived++
		}
	}

	// 3. natural-language probes: before vs. after, not absolute pass/fail. A probe that already failed at baseline is a pre-existing retrieval gap, so it's logged, not hard-failed. A probe that passed at baseline but fails after noise IS a structuring regression — noise buried a previously-retrievable fact — and hard-fails.
	regressions, stillOK, neverOK := 0, 0, 0
	for _, pc := range probes {
		hits, err := store.RetrieveRelevant(ctx, pc.query, 10)
		if err != nil {
			t.Errorf("post-noise RetrieveRelevant(%q) failed: %v", pc.query, err)
			continue
		}
		okAfter := probeSucceeds(hits, pc.want)
		switch {
		case pc.okNow && okAfter:
			stillOK++
		case pc.okNow && !okAfter:
			regressions++
			t.Errorf("REGRESSION: probe %q (fact %q) surfaced the fact before %d noisy days but not after — noise buried a previously-retrievable fact; hits=%v",
				pc.query, pc.factID, noisyDays, hits)
		case !pc.okNow && okAfter:
			stillOK++
		default:
			neverOK++
			t.Logf("KNOWN GAP (not a structuring regression): probe %q (fact %q) never surfaced via RetrieveRelevant, before or after noise — a retrieval-stage gap (evals/retrieval's territory), not context rot",
				pc.query, pc.factID)
		}
	}

	// 4. episode aging reclaims space on old, low-importance filler.
	aged, err := store.AgeEpisodes(ctx, 7*24*time.Hour, 0.5)
	if err != nil {
		t.Fatalf("AgeEpisodes failed: %v", err)
	}
	if aged == 0 {
		t.Errorf("AgeEpisodes cleared 0 rows despite %d backdated low-importance filler episodes older than 7 days — episodic aging isn't reclaiming space as designed", totalFillerEpisodes)
	}

	t.Logf("SCORECARD: %d/%d facts survived %d noisy days (%d filler notes, %d filler episodes)",
		survived, len(facts), noisyDays, totalFillerNotes, totalFillerEpisodes)
	t.Logf("SCORECARD: %d/%d facts still found via SearchMemory on their own content", selfSearchSurvived, len(facts))
	t.Logf("SCORECARD: probes — %d still/newly OK, %d regressed (noise-caused), %d never worked (pre-existing retrieval-stage gap) out of %d total", stillOK, regressions, neverOK, len(probes))
	t.Logf("SCORECARD: notes %d -> %d, live threads %d -> %d, episodes aged (screen_text cleared) %d/%d",
		len(beforeNotes), len(afterNotes), len(beforeThreads), len(afterThreads), aged, totalFillerEpisodes)
}

// TestStructuring_ThreadDedupInvariant pins down the core anti-context-rot mechanism for threads: re-asserting the same (subject, kind) many times must update one row in place, not grow a new row per observation.
// without this, an ambient capture loop re-observing "still watching the same show" every tick would grow the threads table without bound.
func TestStructuring_ThreadDedupInvariant(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)

	const subject = "eval: dedup regression thread"
	const kind = "test"
	const reinforcements = 50

	var lastID int64
	for i := 0; i < reinforcements; i++ {
		id, err := store.UpsertThread(ctx, memory.ThreadUpdate{
			Subject: subject,
			Kind:    kind,
			State:   fmt.Sprintf("state update #%d", i),
		})
		if err != nil {
			t.Fatalf("UpsertThread reinforcement #%d failed: %v", i, err)
		}
		if i > 0 && id != lastID {
			t.Fatalf("UpsertThread returned a new id (%d) on reinforcement #%d, want same id %d — dedup broken, thread rows would grow unboundedly", id, i, lastID)
		}
		lastID = id
	}

	threads, err := store.GetLiveThreads(ctx, 1000)
	if err != nil {
		t.Fatalf("GetLiveThreads failed: %v", err)
	}

	var matches []memory.Thread
	for _, th := range threads {
		if th.Subject == subject && th.Kind == kind {
			matches = append(matches, th)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 row after %d reinforcements of the same subject+kind, got %d rows — dedup/update-in-place invariant broken", reinforcements, len(matches))
	}

	got := matches[0]
	if got.TimesSeen != reinforcements {
		t.Errorf("times_seen = %d after %d reinforcements, want %d", got.TimesSeen, reinforcements, reinforcements)
	}
	wantState := fmt.Sprintf("state update #%d", reinforcements-1)
	if got.State != wantState {
		t.Errorf("state = %q after last reinforcement, want %q (state should update in place, not append)", got.State, wantState)
	}
	if got.Salience != 1.0 {
		t.Errorf("salience = %.4f after %d reinforcements (+0.05 each from a 0.5 base), want clamped to 1.0", got.Salience, reinforcements)
	}

	t.Logf("SCORECARD: %d UpsertThread calls on the same subject collapsed to 1 row (id=%d), times_seen=%d, salience=%.2f (clamped)",
		reinforcements, got.ID, got.TimesSeen, got.Salience)
}

// TestStructuring_ImplicitContextBounded checks that GetImplicitContext (previously an unconditional note-dump) stays roughly the same size regardless of how many notes/threads/episodes exist underneath it.
// also logs (not a failure here) that the notes table itself is NOT bounded — GetNotes still feeds every row, uncapped, into cmd/daemon.go's working-state prompt.
func TestStructuring_ImplicitContextBounded(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)

	// seed a small amount of real signal so the relevance/live-thread paths have something to surface.
	if _, err := store.UpsertThread(ctx, memory.ThreadUpdate{Subject: "baseline thread", Kind: "test", State: "in progress"}); err != nil {
		t.Fatalf("seed thread failed: %v", err)
	}
	if _, err := store.LogNote(ctx, "baseline note about something specific", "note"); err != nil {
		t.Fatalf("seed note failed: %v", err)
	}
	if err := store.SetWorkingState(ctx, "doing baseline work"); err != nil {
		t.Fatalf("SetWorkingState failed: %v", err)
	}

	before, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("baseline GetImplicitContext failed: %v", err)
	}
	beforeNotes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("baseline GetNotes failed: %v", err)
	}

	// pile on the same noisy-day volume as the long-haul test — this test only cares about GetImplicitContext's own output size.
	for day := 1; day <= noisyDays; day++ {
		simulateNoisyDay(t, ctx, store, day, noisyDays-day)
	}

	after, err := store.GetImplicitContext(ctx)
	if err != nil {
		t.Fatalf("post-noise GetImplicitContext failed: %v", err)
	}
	afterNotes, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("post-noise GetNotes failed: %v", err)
	}

	// GetImplicitContext's own internals cap out at RetrieveRelevant(maxRel=6) + GetLiveThreads(6) + 1 working-state line, roughly 13 regardless of table size.
	// sanityBound gives generous slack above that structural cap.
	const sanityBound = 25
	if len(after) > sanityBound {
		t.Errorf("GetImplicitContext returned %d lines after %d noisy days (%d total notes), want <= %d — looks like the unconditional note-dump regression is back",
			len(after), noisyDays, len(afterNotes), sanityBound)
	}

	t.Logf("SCORECARD: GetImplicitContext stayed bounded: %d lines at baseline (%d notes in table) vs %d lines after %d noisy days (%d notes in table)",
		len(before), len(beforeNotes), len(after), noisyDays, len(afterNotes))

	// sanity check on the simulation: the notes table itself really is unbounded, which is the premise of the finding logged below.
	if len(afterNotes) <= len(beforeNotes) {
		t.Fatalf("sanity: expected notes table to grow with noisy accumulation, got %d -> %d", len(beforeNotes), len(afterNotes))
	}
	t.Logf("FINDING: GetImplicitContext itself no longer dumps all notes, but GetNotes() — which returns ALL %d notes uncapped, up from %d at baseline — is still passed wholesale into cmd/daemon.go's DeriveState call. The unconditional note-dump this project diagnosed relocated to that call site, it didn't disappear.",
		len(afterNotes), len(beforeNotes))
}

// TestStructuring_NoteConsolidationReclaimsBoundedness exercises ReplaceAllNotes, the bulk note-reconciliation primitive behind periodic note compaction (production drives it via an LLM call, skipped here).
// threads dedup/upsert per write, but notes have no such mechanism — they rely on a periodic consolidation pass replacing the whole set with a curated one. This confirms that pass actually collapses noise back to just the real facts.
func TestStructuring_NoteConsolidationReclaimsBoundedness(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)

	facts, err := dataset.Load()
	if err != nil {
		t.Fatalf("dataset.Load() failed: %v", err)
	}

	var curated []string
	for _, f := range facts {
		if f.Kind != "note" {
			continue
		}
		curated = append(curated, f.Content)
		if _, err := store.LogNote(ctx, f.Content, "note"); err != nil {
			t.Fatalf("seed LogNote(%q) failed: %v", f.ID, err)
		}
	}
	if len(curated) == 0 {
		t.Fatal("dataset has no note-kind facts — nothing to consolidate")
	}

	for day := 1; day <= noisyDays; day++ {
		for i := 0; i < notesPerNoisyDay; i++ {
			content := fmt.Sprintf("day %d filler: user %s (tick %d)", day, fillerVerbs[i%len(fillerVerbs)], i)
			if _, err := store.LogNote(ctx, content, "note"); err != nil {
				t.Fatalf("filler LogNote failed: %v", err)
			}
		}
	}

	bloated, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes (bloated) failed: %v", err)
	}
	wantBloated := len(curated) + noisyDays*notesPerNoisyDay
	if len(bloated) != wantBloated {
		t.Fatalf("sanity: expected %d notes before consolidation (curated + filler), got %d", wantBloated, len(bloated))
	}

	// simulates what a reconciliation pass (memory.Summarizer.ReconcileNotes, LLM-backed, skipped here) decides: replace the whole note set with just the real, curated facts.
	// ReplaceAllNotes is the pure-DB primitive behind it — no LLM needed to exercise it.
	if err := store.ReplaceAllNotes(ctx, curated); err != nil {
		t.Fatalf("ReplaceAllNotes failed: %v", err)
	}

	after, err := store.GetNotes(ctx)
	if err != nil {
		t.Fatalf("GetNotes (after) failed: %v", err)
	}
	if len(after) != len(curated) {
		t.Errorf("expected exactly %d notes (curated set only) after ReplaceAllNotes, got %d — noise survived consolidation", len(curated), len(after))
	}
	for _, c := range curated {
		if !containsNoteContent(after, c) {
			t.Errorf("curated fact lost after ReplaceAllNotes: %q", c)
		}
	}

	t.Logf("SCORECARD: note consolidation — %d rows (%d real facts + %d filler) collapsed to %d rows via ReplaceAllNotes",
		len(bloated), len(curated), len(bloated)-len(curated), len(after))
}

// TestStructuring_SummaryRollup exercises the episodic-rollup layer: OldSummaryGroups finds summary nodes old enough to compact, ReplaceSummariesWithDigest atomically swaps many of them for one digest node (production computes the digest string via an LLM-backed Digester, skipped here — needs a live API key).
func TestStructuring_SummaryRollup(t *testing.T) {
	ctx := context.Background()
	store := harness.NewStore(t)

	// log several semantic summary nodes (simulating many completed tasks across "days") and backdate them so OldSummaryGroups considers them ready to roll up.
	const summaryCount = 12
	for i := 0; i < summaryCount; i++ {
		if err := store.LogSemanticNode(ctx, memory.TaskSummary{
			SameTask: false,
			TaskName: fmt.Sprintf("filler task %d", i),
			Summary:  fmt.Sprintf("did some filler work item %d", i),
		}); err != nil {
			t.Fatalf("LogSemanticNode #%d failed: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE nodes SET created_at = datetime('now', '-10 days') WHERE type = 'summary'`); err != nil {
		t.Fatalf("backdate summary nodes: %v", err)
	}

	groups, err := store.OldSummaryGroups(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("OldSummaryGroups failed: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("OldSummaryGroups found no groups despite backdated summary nodes older than 7 days")
	}

	var totalSummaries int
	for _, g := range groups {
		totalSummaries += len(g.Summaries)
	}
	if totalSummaries != summaryCount {
		t.Errorf("OldSummaryGroups returned %d total summaries across %d groups, want %d", totalSummaries, len(groups), summaryCount)
	}

	for _, g := range groups {
		ids := make([]int64, len(g.Summaries))
		for i, s := range g.Summaries {
			ids[i] = s.ID
		}
		digest := fmt.Sprintf("digest of %d rolled-up summaries for day %d", len(ids), g.DayID)
		if err := store.ReplaceSummariesWithDigest(ctx, g.DayID, ids, digest); err != nil {
			t.Fatalf("ReplaceSummariesWithDigest(day=%d) failed: %v", g.DayID, err)
		}
	}

	// rolled-up summaries should no longer show up as old summary groups, they were replaced by a single digest node each.
	remaining, err := store.OldSummaryGroups(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("OldSummaryGroups (post-rollup) failed: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 old summary groups after rollup, got %d — summaries survived compaction instead of collapsing into a digest", len(remaining))
	}

	var digestCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE type = 'digest'`).Scan(&digestCount); err != nil {
		t.Fatalf("count digest nodes: %v", err)
	}
	if digestCount != len(groups) {
		t.Errorf("expected %d digest nodes (one per day group), got %d", len(groups), digestCount)
	}

	t.Logf("SCORECARD: summary rollup — %d summary nodes across %d day group(s) collapsed into %d digest node(s)",
		summaryCount, len(groups), digestCount)
}
