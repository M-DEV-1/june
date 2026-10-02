package db

// act_reference_test.go lives in package db (white-box) because setting a run's started_at and reading back a stored vector need the store's own handle, and because the tests drive the write-side embed helper by hand instead of racing its goroutine.
//
// No test here talks to an embedding server. Every vector is a fixed two-dimensional unit vector at a chosen angle, so the cosine between any two of them is the cosine of the angle between them and every expected score in this file is arithmetic a reader can check: 0 degrees apart is 1.000, 20 apart is 0.940, 60 apart is 0.500. Two dimensions rather than EmbeddingGemma's 768 is deliberate — nothing in the lookup may assume a dimension.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

// actUnit builds the two-dimensional unit vector at deg degrees. Input: an angle in degrees. Output: [cos deg, sin deg], so the cosine similarity between actUnit(a) and actUnit(b) is exactly cos(a-b).
func actUnit(deg float64) []float32 {
	r := deg * math.Pi / 180
	return []float32{float32(math.Cos(r)), float32(math.Sin(r))}
}

// actTestEmbedder is a stand-in for the local embedding engine: it answers with the fixed vector registered for a piece of text and errors on anything else, so a question the test forgot to register fails loudly instead of scoring against a silent default. It records the task each text was embedded under, which is how the tests check the query and document sides are asked for the right way round.
type actTestEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	tasks   map[string]string
	calls   int
	err     error
}

// newActEmbedder builds an empty actTestEmbedder.
func newActEmbedder() *actTestEmbedder {
	return &actTestEmbedder{vectors: map[string][]float32{}, tasks: map[string]string{}}
}

// register gives text the unit vector at deg degrees, so every later embed of that text answers with it.
func (e *actTestEmbedder) register(text string, deg float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vectors[text] = actUnit(deg)
}

// Embed answers with the registered vector for text, recording the task it was asked under. Input: the task name and the text. Output: the fixed vector, or an error when the embedder was told to fail or the text was never registered.
func (e *actTestEmbedder) Embed(ctx context.Context, task string, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	vec, ok := e.vectors[text]
	if !ok {
		return nil, fmt.Errorf("actTestEmbedder: no vector registered for %q", text)
	}
	e.tasks[text] = task
	return vec, nil
}

// twoScreenSteps is the shape of a real successful screen run from the user's own store: a look, then a ring drawn around what was found.
func twoScreenSteps() []ActStep {
	return []ActStep{
		{Name: "observe_screen", Args: map[string]any{}, Result: `Brave Browser · Watch Family Guy S16 Episode 9 on JioHotstar - Brave` + "\n" + `[1] push button "Minimize" (51,20)`},
		{Name: "point_at", Args: map[string]any{"n": float64(28), "label": "subtitles"}, Result: `ringed [28] push button "Audio & Subtitles"`},
	}
}

// actQuestion is one stored screen run in a test: the words it was asked in, and the angle of the vector standing in for what it means.
type actQuestion struct {
	text string
	deg  float64
}

// seedActRuns stores one successful screen run per question and gives each its vector, returning the runs' ids in the order given. The runs are written before the embedder is wired so nothing here depends on the write-side goroutine's timing: the vectors are written by calling the store's own write-side helper directly, which is the same code that goroutine runs.
func seedActRuns(t *testing.T, store *Store, emb *actTestEmbedder, questions ...actQuestion) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, len(questions))
	for _, q := range questions {
		emb.register(q.text, q.deg)
		id, err := store.AddActRun(ctx, ActRun{Question: q.text, Model: "gemini-3-flash", Outcome: "ok", Steps: twoScreenSteps()})
		if err != nil {
			t.Fatalf("AddActRun(%q): %v", q.text, err)
		}
		ids = append(ids, id)
	}
	store.SetEmbedder(emb)
	for i, q := range questions {
		if err := store.embedActRunQuestion(ctx, ids[i], q.text); err != nil {
			t.Fatalf("embedActRunQuestion(%q): %v", q.text, err)
		}
	}
	return ids
}

// actRunVector reads back the vector stored against one run, as raw bytes, and gives back nothing for a run that has none.
func actRunVector(t *testing.T, store *Store, id int64) []byte {
	t.Helper()
	var blob []byte
	err := store.db.QueryRow(`SELECT vec FROM act_run_vectors WHERE run_id = ?`, id).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the vector of act run %d: %v", id, err)
	}
	return blob
}

// waitForActRunVector waits up to a second for a run's vector to appear, for the one test that exercises the write-side goroutine rather than calling the helper it runs.
func waitForActRunVector(t *testing.T, store *Store, id int64) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if blob := actRunVector(t, store, id); len(blob) > 0 {
			return blob
		}
		if time.Now().After(deadline) {
			t.Fatalf("act run %d still has no vector after a second", id)
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSimilarActRunsFindsTheRunThatMeansTheSameThing is the whole change in one test, on the pair the old measure got wrong. The user asks "show me how to change subtitles". One stored run asked "where can i change subtitles here?", which shares only two significant words of the seven the two questions have between them — 0.29 by word overlap, below the old 0.5 threshold, so it was dropped. Another stored run asked "show me how to change the graph function", which shares four of seven — 0.57 by word overlap, above the old threshold, so it was kept, and it is about a graph.
// By meaning the two swap places: the subtitles run is 20 degrees away (0.940) and the graph run is 60 (0.500, below the floor). Only the subtitles run comes back.
func TestSimilarActRunsFindsTheRunThatMeansTheSameThing(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)

	ids := seedActRuns(t, store, emb,
		actQuestion{"where can i change subtitles here?", 20},
		actQuestion{"show me how to change the graph function", 60},
	)

	matches, err := store.SimilarActRuns(context.Background(), asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("SimilarActRuns returned %d matches, want only the subtitles run: %+v", len(matches), matches)
	}
	if matches[0].Run.ID != ids[0] {
		t.Fatalf("SimilarActRuns returned run %d (%q), want the subtitles run %d", matches[0].Run.ID, matches[0].Run.Question, ids[0])
	}
	if diff := matches[0].Score - 0.9397; diff > 0.001 || diff < -0.001 {
		t.Errorf("match score = %.4f, want cos 20° ≈ 0.940", matches[0].Score)
	}
	if len(matches[0].Run.Steps) != 2 || matches[0].Run.Steps[1].Name != "point_at" {
		t.Errorf("match steps = %+v, want the run's own observe_screen then point_at", matches[0].Run.Steps)
	}
}

// TestSimilarActRunsOnlyOffersRunsThatWorkedOnTheScreen checks the two hard filters together: a run that ended in an error is never offered however close it is, and neither is a successful run that never touched the screen. Both stored runs ask the exact question the lookup is given, so any match at all is one of the two filters gone.
func TestSimilarActRunsOnlyOffersRunsThatWorkedOnTheScreen(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	ctx := context.Background()
	const question = "show me how to change subtitles on this page"
	emb.register(question, 0)

	failed, err := store.AddActRun(ctx, ActRun{Question: question, Outcome: "error", Error: "the element has gone", Steps: twoScreenSteps()})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	// A turn that only searched memory and read a file stores no steps at all, because AddActRun keeps the screen hops and nothing else.
	looked, err := store.AddActRun(ctx, ActRun{Question: question, Outcome: "ok", Steps: []ActStep{{Name: "query_memory", Args: map[string]any{"query": "subtitles"}, Result: "nothing"}}})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	store.SetEmbedder(emb)
	for _, id := range []int64{failed, looked} {
		if err := store.embedActRunQuestion(ctx, id, question); err != nil {
			t.Fatalf("embedActRunQuestion: %v", err)
		}
	}

	matches, err := store.SimilarActRuns(ctx, question, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("SimilarActRuns returned %d matches, want none — a failed run and a run that never touched the screen are both unusable as reference: %+v", len(matches), matches)
	}
}

// TestSimilarActRunsOffersOneRunPerGoal checks the same question asked over and over cannot fill the whole result with itself. The user's store holds one question six times; without grouping, every match offered would be the same words with the same steps. The newest run of a goal is the one kept, since it is the one whose screen is most likely still there.
func TestSimilarActRunsOffersOneRunPerGoal(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const repeated = "Look at my screen and draw a ring around the address bar."
	// The same goal in different case and with stray spacing is the same goal, grouped the way the nightly notes stage groups one, and being the last one written it is also the newest of the four.
	const restated = "  look at my screen and draw a ring around the address bar.  "
	ids := seedActRuns(t, store, emb,
		actQuestion{repeated, 0},
		actQuestion{restated, 0},
	)
	// Two more of the first wording, written after both, so the newest run of the goal is one of these.
	more := seedActRuns(t, store, emb, actQuestion{repeated, 0}, actQuestion{repeated, 0})
	_ = ids

	matches, err := store.SimilarActRuns(context.Background(), repeated, 4)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("SimilarActRuns returned %d matches for one goal asked four times, want 1: %+v", len(matches), matches)
	}
	if matches[0].Run.ID != more[1] {
		t.Errorf("match is run %d, want the newest run of the goal (%d)", matches[0].Run.ID, more[1])
	}
}

// TestSimilarActRunsSurvivesAnEmbedderThatFails checks an embedding server that is down or wedged costs the ask its reference block and nothing else: no error reaches the caller, because the reference is a help and never a requirement.
func TestSimilarActRunsSurvivesAnEmbedderThatFails(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)
	seedActRuns(t, store, emb, actQuestion{"where can i change subtitles here?", 20})
	emb.err = errors.New("embed: post to http://127.0.0.1:6943: connection refused")

	matches, err := store.SimilarActRuns(context.Background(), asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns returned an error for a failed embed: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("SimilarActRuns returned %d matches with no query vector, want none", len(matches))
	}
}

// TestSimilarActRunsBackfillsRunsStoredBeforeTheyWereEmbedded is what makes embedding at write time safe: every run already in the user's store was written before this existed and has no vector at all. Such a run cannot be scored on the lookup that finds it, so it is skipped that time and embedded in the background, and the next ask has it.
func TestSimilarActRunsBackfillsRunsStoredBeforeTheyWereEmbedded(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	ctx := context.Background()
	const asked = "show me how to change subtitles"
	const stored = "where can i change subtitles here?"
	emb.register(asked, 0)
	emb.register(stored, 20)

	id, err := store.AddActRun(ctx, ActRun{Question: stored, Outcome: "ok", Steps: twoScreenSteps()})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	store.SetEmbedder(emb)

	matches, err := store.SimilarActRuns(ctx, asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("SimilarActRuns scored a run with no vector, want it skipped until it has one: %+v", matches)
	}
	waitForActRunVector(t, store, id)

	matches, err = store.SimilarActRuns(ctx, asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns after the backfill: %v", err)
	}
	if len(matches) != 1 || matches[0].Run.ID != id {
		t.Fatalf("SimilarActRuns returned %+v after the backfill, want the one run %d", matches, id)
	}
}
