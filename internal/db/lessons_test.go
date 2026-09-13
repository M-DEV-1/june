package db

import (
	"context"
	"testing"
)

// TestLessonRoundTrips checks a lesson written by AddLesson and embedded comes back from SimilarLessons for a goal close enough in meaning, in the same app, carrying its own id, app, goal and text.
func TestLessonRoundTrips(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	emb := newActEmbedder()
	ctx := context.Background()

	id, err := store.AddLesson(ctx, "Teams", "add a title to the meeting", `Teams: entry "Add title": accessibility action failed; worked via pointer`)
	if err != nil {
		t.Fatalf("AddLesson: %v", err)
	}
	store.SetEmbedder(emb)
	emb.register(lessonEmbedText("add a title to the meeting", `Teams: entry "Add title": accessibility action failed; worked via pointer`), 0)
	if err := store.embedLesson(ctx, id, "add a title to the meeting", `Teams: entry "Add title": accessibility action failed; worked via pointer`); err != nil {
		t.Fatalf("embedLesson: %v", err)
	}
	emb.register("add a title to today's meeting", 5) // 5 degrees off — well inside the floor

	got, err := store.SimilarLessons(ctx, "Teams", "add a title to today's meeting", 3)
	if err != nil {
		t.Fatalf("SimilarLessons: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SimilarLessons returned %d lessons, want 1", len(got))
	}
	if got[0].ID != id || got[0].App != "Teams" || got[0].Goal != "add a title to the meeting" {
		t.Errorf("got %+v, want id %d, app Teams, goal preserved", got[0], id)
	}
}

// TestSimilarLessonsOnlyOffersTheSameApp checks a lesson learned in one app never surfaces for a question about another, however close the wording.
func TestSimilarLessonsOnlyOffersTheSameApp(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	emb := newActEmbedder()
	ctx := context.Background()

	id, err := store.AddLesson(ctx, "Teams", "add a title", "Teams: worked via pointer")
	if err != nil {
		t.Fatalf("AddLesson: %v", err)
	}
	store.SetEmbedder(emb)
	emb.register(lessonEmbedText("add a title", "Teams: worked via pointer"), 0)
	if err := store.embedLesson(ctx, id, "add a title", "Teams: worked via pointer"); err != nil {
		t.Fatalf("embedLesson: %v", err)
	}
	emb.register("add a title", 0)

	got, err := store.SimilarLessons(ctx, "Slack", "add a title", 3)
	if err != nil {
		t.Fatalf("SimilarLessons: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SimilarLessons(Slack) = %+v, want nothing — the lesson was learned in Teams", got)
	}
}

// TestSimilarLessonsPutsHitsFirst checks that among lessons all close enough to be shown, the ones that have actually helped before (more hits) come ahead of ones that have not, whatever their exact closeness.
func TestSimilarLessonsPutsHitsFirst(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	emb := newActEmbedder()
	ctx := context.Background()

	type seed struct {
		lesson string
		hits   int
	}
	seeds := []seed{
		{"no hits yet", 0},
		{"two hits", 2},
		{"one hit", 1},
	}
	store.SetEmbedder(emb)
	var ids []int64
	for i, s := range seeds {
		id, err := store.AddLesson(ctx, "Teams", "add a title", s.lesson)
		if err != nil {
			t.Fatalf("AddLesson(%q): %v", s.lesson, err)
		}
		emb.register(lessonEmbedText("add a title", s.lesson), float64(i))
		if err := store.embedLesson(ctx, id, "add a title", s.lesson); err != nil {
			t.Fatalf("embedLesson(%q): %v", s.lesson, err)
		}
		for j := 0; j < s.hits; j++ {
			if err := store.ScoreLessonsUsed(ctx, []int64{id}, "ok"); err != nil {
				t.Fatalf("ScoreLessonsUsed(%q): %v", s.lesson, err)
			}
		}
		ids = append(ids, id)
	}
	emb.register("add a title", 0)

	got, err := store.SimilarLessons(ctx, "Teams", "add a title", 3)
	if err != nil {
		t.Fatalf("SimilarLessons: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("SimilarLessons returned %d lessons, want 3", len(got))
	}
	want := []string{"two hits", "one hit", "no hits yet"}
	for i, w := range want {
		if got[i].Lesson != w {
			t.Errorf("position %d = %q, want %q (order: %v)", i, got[i].Lesson, w, got)
		}
	}
}

// TestScoreLessonsUsedDropsAWornOutLesson checks a lesson whose misses have overtaken its hits, with at least two misses, is deleted outright rather than kept on to be shown again.
func TestScoreLessonsUsedDropsAWornOutLesson(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	id, err := store.AddLesson(ctx, "Teams", "add a title", "click the plus icon")
	if err != nil {
		t.Fatalf("AddLesson: %v", err)
	}
	// One hit, then two misses: misses (2) > hits (1) and misses >= 2, so the lesson is dropped on the second miss.
	if err := store.ScoreLessonsUsed(ctx, []int64{id}, "ok"); err != nil {
		t.Fatalf("ScoreLessonsUsed(ok): %v", err)
	}
	if err := store.ScoreLessonsUsed(ctx, []int64{id}, "error"); err != nil {
		t.Fatalf("ScoreLessonsUsed(error): %v", err)
	}
	var stillThere int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lessons WHERE id = ?`, id).Scan(&stillThere); err != nil {
		t.Fatalf("count after one miss: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("lesson dropped after one miss (1 hit, 1 miss), want it kept")
	}
	if err := store.ScoreLessonsUsed(ctx, []int64{id}, "error"); err != nil {
		t.Fatalf("ScoreLessonsUsed(error) again: %v", err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lessons WHERE id = ?`, id).Scan(&stillThere); err != nil {
		t.Fatalf("count after two misses: %v", err)
	}
	if stillThere != 0 {
		t.Errorf("lesson still present after 1 hit, 2 misses, want it dropped")
	}
}

// TestHedgedNonAnswersNeverBecomeLessons checks the shape the real store filled up with on 2026-09-12 — 19 of its 21 lessons were a reflective call saying "nothing worth flagging" in a full sentence. A hedge handed to AddLesson is refused outright, and one already sitting in the table from before that guard existed is never offered to a later run.
func TestHedgedNonAnswersNeverBecomeLessons(t *testing.T) {
	store, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	const hedge = "Nothing worth flagging — just browsing, a Meet call, and a PDF read."
	if _, err := store.AddLesson(ctx, "Teams", "add a title", hedge); err == nil {
		t.Errorf("AddLesson stored %q as a lesson, want it refused as a non-answer", hedge)
	}

	// A hedge already in the table from before the guard: it still has to stay out of what a later run is shown.
	res, err := store.db.ExecContext(ctx, `INSERT INTO lessons (app, goal, lesson) VALUES (?, ?, ?)`, "Teams", "add a title", hedge)
	if err != nil {
		t.Fatalf("seed legacy hedge: %v", err)
	}
	id, _ := res.LastInsertId()
	emb := newActEmbedder()
	store.SetEmbedder(emb)
	emb.register(lessonEmbedText("add a title", hedge), 0)
	if err := store.embedLesson(ctx, id, "add a title", hedge); err != nil {
		t.Fatalf("embedLesson: %v", err)
	}
	emb.register("add a title", 0)

	got, err := store.SimilarLessons(ctx, "Teams", "add a title", 3)
	if err != nil {
		t.Fatalf("SimilarLessons: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SimilarLessons offered %+v, want nothing — the only row is a non-answer", got)
	}
}
