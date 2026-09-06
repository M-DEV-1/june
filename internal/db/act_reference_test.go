package db

// act_reference_test.go lives in package db (white-box) because the closeness score is a cosine between two stored vectors and the tests pin its arithmetic directly, because setting a run's started_at and reading back a stored vector need the store's own handle, and because the tests drive the write-side embed helper by hand instead of racing its goroutine.
//
// No test here talks to an embedding server. Every vector is a fixed two-dimensional unit vector at a chosen angle, so the cosine between any two of them is the cosine of the angle between them and every expected score in this file is arithmetic a reader can check: 0 degrees apart is 1.000, 20 apart is 0.940, 60 apart is 0.500. Two dimensions rather than EmbeddingGemma's 768 is deliberate — nothing in the lookup may assume a dimension.

import (
	"context"
	"database/sql"
	"encoding/json"
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

// registerVector gives text a vector of the test's own making, for the cases where the dimension itself is what is under test.
func (e *actTestEmbedder) registerVector(text string, vec []float32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vectors[text] = vec
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

// taskFor reports the task a text was last embedded under, or "" when it was never embedded.
func (e *actTestEmbedder) taskFor(text string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tasks[text]
}

// callCount reports how many times Embed has been called.
func (e *actTestEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
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

// backdateActRun moves one stored run's started_at, so a test can say how old a run is without waiting.
func backdateActRun(t *testing.T, store *Store, id int64, at time.Time) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE act_runs SET started_at = ? WHERE id = ?`, at.UTC().Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("backdate act run %d: %v", id, err)
	}
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

// TestActRunCosineIsTheAngleBetweenTwoVectors pins the arithmetic every score in this package is: the cosine of the angle between the asking question's vector and a stored one's. Two vectors of different lengths score zero rather than comparing what they have in common, because a stored vector of another length came from another embedding model and means nothing on this one's scale.
func TestActRunCosineIsTheAngleBetweenTwoVectors(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"the same direction", actUnit(0), actUnit(0), 1},
		{"twenty degrees apart", actUnit(0), actUnit(20), 0.9397},
		{"sixty degrees apart", actUnit(0), actUnit(60), 0.5},
		{"at right angles", actUnit(0), actUnit(90), 0},
		{"opposite", actUnit(0), actUnit(180), -1},
		{"length has no say", []float32{3, 0}, []float32{7, 0}, 1},
		{"a vector from another model", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"nothing to compare", []float32{0, 0}, actUnit(0), 0},
		{"no vector at all", nil, actUnit(0), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := actRunCosine(c.a, c.b)
			if diff := got - c.want; diff > 0.001 || diff < -0.001 {
				t.Errorf("actRunCosine(%v, %v) = %.4f, want %.4f", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestActRunVectorSurvivesTheRoundTrip checks a vector written to the row and read back is the same vector, since a score is only worth anything if the stored side of it survived storage.
func TestActRunVectorSurvivesTheRoundTrip(t *testing.T) {
	want := []float32{0.5, -0.25, 0, 1}
	got := decodeActRunVector(encodeActRunVector(want))
	if len(got) != len(want) {
		t.Fatalf("round trip gave %d numbers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("number %d came back as %v, want %v", i, got[i], want[i])
		}
	}
	if v := decodeActRunVector(nil); v != nil {
		t.Errorf("decoding no bytes gave %v, want nothing", v)
	}
	// Four bytes to a number, so a blob that is not a whole number of them is not a vector this store wrote.
	if v := decodeActRunVector([]byte{1, 2, 3}); v != nil {
		t.Errorf("decoding a broken blob gave %v, want nothing", v)
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

// TestSimilarActRunsCarriesTheRunAndWhenItHappened checks what a match is made of beyond its score: the whole run as it was stored, with its steps decoded, and the time it started, which is what the caller renders as "6 hours ago".
func TestSimilarActRunsCarriesTheRunAndWhenItHappened(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)

	ctx := context.Background()
	const question = "show me how to change subtitles on this page, or where"
	emb.register(question, 10)
	id, err := store.AddActRun(ctx, ActRun{Question: question, Model: "gemini-3-flash", Outcome: "ok", Answer: "the Audio & Subtitles button", DurationMS: 8092, Steps: twoScreenSteps()})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	store.SetEmbedder(emb)
	if err := store.embedActRunQuestion(ctx, id, question); err != nil {
		t.Fatalf("embedActRunQuestion: %v", err)
	}
	when := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	backdateActRun(t, store, id, when)

	matches, err := store.SimilarActRuns(ctx, asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("SimilarActRuns returned %d matches, want the one run: %+v", len(matches), matches)
	}
	got := matches[0]
	if !got.When.Equal(when.UTC()) {
		t.Errorf("match When = %v, want the run's started_at %v", got.When, when.UTC())
	}
	if got.Run.Answer != "the Audio & Subtitles button" || got.Run.DurationMS != 8092 || got.Run.Model != "gemini-3-flash" {
		t.Errorf("match run = %+v, want the fields the run was stored with", got.Run)
	}
}

// TestSimilarActRunsScoresEveryMatchAtOrAboveTheFloor checks the caller is never handed a match it would have to filter itself, and that the score it can read is the one the floor was applied to.
func TestSimilarActRunsScoresEveryMatchAtOrAboveTheFloor(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)
	seedActRuns(t, store, emb,
		actQuestion{"where can i change subtitles here?", 20},
		actQuestion{"show me how to change subtitles on this page, or where", 10},
		actQuestion{"take me to the open tab where family guy is playing", 75},
		actQuestion{"Read the numbers you can see in the window in front and list them in one line.", 88},
	)

	matches, err := store.SimilarActRuns(context.Background(), asked, 4)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("SimilarActRuns returned %d matches, want the two about subtitles: %+v", len(matches), matches)
	}
	for _, m := range matches {
		if m.Score < store.actRunFloor() {
			t.Errorf("match %q scored %.3f, below the floor %.2f the lookup applies", m.Run.Question, m.Score, store.actRunFloor())
		}
	}
}

// TestSimilarActRunsPutsTheClosestFirst checks the order the caller relies on: matches come back closest first, so the run put in front of the model is the closest one and not whichever was written last.
func TestSimilarActRunsPutsTheClosestFirst(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)
	// Stored in the opposite order to their closeness, so a lookup that just read the table back would fail this.
	ids := seedActRuns(t, store, emb,
		actQuestion{"show me how to change subtitles and audio and the picture quality on this page", 35},
		actQuestion{"where can i change subtitles here?", 20},
		actQuestion{"show me how to change subtitles on this page", 5},
	)

	matches, err := store.SimilarActRuns(context.Background(), asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("SimilarActRuns returned %d matches, want all three: %+v", len(matches), matches)
	}
	want := []int64{ids[2], ids[1], ids[0]}
	for i, id := range want {
		if matches[i].Run.ID != id {
			t.Fatalf("match %d is run %d (%q, %.3f), want run %d — matches must come back closest first", i, matches[i].Run.ID, matches[i].Run.Question, matches[i].Score, id)
		}
	}
	if !(matches[0].Score > matches[1].Score && matches[1].Score > matches[2].Score) {
		t.Errorf("scores = %.3f, %.3f, %.3f, want strictly falling", matches[0].Score, matches[1].Score, matches[2].Score)
	}
}

// TestSimilarActRunsBreaksTiesWithTheNewerRun checks what happens when two different goals are exactly as close to the new question as each other: the newer run goes first, because it is the one whose screen is most likely still arranged the way it was.
func TestSimilarActRunsBreaksTiesWithTheNewerRun(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "ring the address bar"
	emb.register(asked, 0)
	// The same angle either side of the asking question, so both score cos 15°.
	ids := seedActRuns(t, store, emb,
		actQuestion{"ring the address bar now", -15},
		actQuestion{"please ring the address bar", 15},
	)

	matches, err := store.SimilarActRuns(context.Background(), asked, 2)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("SimilarActRuns returned %d matches, want both: %+v", len(matches), matches)
	}
	if diff := matches[0].Score - matches[1].Score; diff > 0.0001 || diff < -0.0001 {
		t.Fatalf("scores = %.4f and %.4f, want the tie this test is about", matches[0].Score, matches[1].Score)
	}
	if matches[0].Run.ID != ids[1] || matches[1].Run.ID != ids[0] {
		t.Errorf("tied matches came back as %d then %d, want the newer run (%d) first", matches[0].Run.ID, matches[1].Run.ID, ids[1])
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

// TestSimilarActRunsHonoursTheLimit checks the caller's cap is what bounds the result, since this goes into a prompt with a budget.
func TestSimilarActRunsHonoursTheLimit(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)
	seedActRuns(t, store, emb,
		actQuestion{"show me how to change subtitles now", 5},
		actQuestion{"show me how to change subtitles on this page", 10},
		actQuestion{"show me how to change subtitles on this page, or where", 15},
	)

	matches, err := store.SimilarActRuns(context.Background(), asked, 2)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("SimilarActRuns returned %d matches under a limit of 2, want 2", len(matches))
	}
	for _, limit := range []int{0, -1} {
		matches, err := store.SimilarActRuns(context.Background(), asked, limit)
		if err != nil {
			t.Fatalf("SimilarActRuns(limit=%d): %v", limit, err)
		}
		if len(matches) != 0 {
			t.Errorf("SimilarActRuns(limit=%d) returned %d matches, want none", limit, len(matches))
		}
	}
}

// TestSimilarActRunsHasNothingToSayWithoutAQuestion checks an empty or all-stopword question asks for nothing rather than matching everything, and that it costs no embed call: there is nothing in "what is it" for a vector to be about.
func TestSimilarActRunsHasNothingToSayWithoutAQuestion(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	seedActRuns(t, store, emb, actQuestion{"show me how to change subtitles", 0})
	before := emb.callCount()

	for _, q := range []string{"", "   ", "what is it"} {
		matches, err := store.SimilarActRuns(context.Background(), q, 3)
		if err != nil {
			t.Fatalf("SimilarActRuns(%q): %v", q, err)
		}
		if len(matches) != 0 {
			t.Errorf("SimilarActRuns(%q) returned %d matches, want none", q, len(matches))
		}
	}
	if emb.callCount() != before {
		t.Errorf("the embedder was called %d times for questions with nothing in them, want none", emb.callCount()-before)
	}
}

// TestSimilarActRunsEmbedsBothSidesTheRightWayRound checks the asymmetry the embedding model is trained on: the question being asked now is embedded as a query, and a stored question is embedded as a document. Getting this the wrong way round costs recall on every lookup and shows up nowhere else.
func TestSimilarActRunsEmbedsBothSidesTheRightWayRound(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	const stored = "where can i change subtitles here?"
	emb.register(asked, 0)
	seedActRuns(t, store, emb, actQuestion{stored, 20})

	if _, err := store.SimilarActRuns(context.Background(), asked, 3); err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if got := emb.taskFor(asked); got != "RETRIEVAL_QUERY" {
		t.Errorf("the question being asked was embedded as %q, want RETRIEVAL_QUERY", got)
	}
	if got := emb.taskFor(stored); got != "RETRIEVAL_DOCUMENT" {
		t.Errorf("the stored question was embedded as %q, want RETRIEVAL_DOCUMENT", got)
	}
}

// TestSimilarActRunsOffersNothingWithoutAnEmbedder checks a store with no embedder wired — a bare db.New, or a machine with no embedding engine configured — answers with nothing rather than falling back to counting shared words. There is no second measure to fall back to, and a lookup nobody can score honestly is one that should cost the prompt nothing.
func TestSimilarActRunsOffersNothingWithoutAnEmbedder(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()
	if _, err := store.AddActRun(ctx, ActRun{Question: "show me how to change subtitles", Outcome: "ok", Steps: twoScreenSteps()}); err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	matches, err := store.SimilarActRuns(ctx, "show me how to change subtitles", 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("SimilarActRuns returned %d matches with no embedder wired, want none: %+v", len(matches), matches)
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

// TestAddActRunEmbedsTheQuestionAsItIsStored checks where the cost of embedding is paid: once, when the run is written, not once per stored run on every lookup. A run that failed or never touched the screen is never offered as reference, so it is never embedded either.
func TestAddActRunEmbedsTheQuestionAsItIsStored(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	ctx := context.Background()
	const worked = "show me how to change subtitles"
	const failed = "take me to the open tab where family guy is playing"
	const looked = "what window is in front"
	for _, q := range []string{worked, failed, looked} {
		emb.register(q, 0)
	}
	store.SetEmbedder(emb)

	workedID, err := store.AddActRun(ctx, ActRun{Question: worked, Outcome: "ok", Steps: twoScreenSteps()})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	failedID, err := store.AddActRun(ctx, ActRun{Question: failed, Outcome: "error", Error: "gone", Steps: twoScreenSteps()})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	lookedID, err := store.AddActRun(ctx, ActRun{Question: looked, Outcome: "ok", Steps: []ActStep{{Name: "query_memory"}}})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	if blob := waitForActRunVector(t, store, workedID); len(blob) != 8 {
		t.Errorf("the successful run's vector is %d bytes, want the 8 a two-number vector takes", len(blob))
	}
	if got := emb.taskFor(worked); got != "RETRIEVAL_DOCUMENT" {
		t.Errorf("the stored question was embedded as %q, want RETRIEVAL_DOCUMENT", got)
	}
	// Neither of the other two can ever be offered as reference, so neither is worth an embed.
	time.Sleep(50 * time.Millisecond)
	for _, id := range []int64{failedID, lookedID} {
		if blob := actRunVector(t, store, id); len(blob) != 0 {
			t.Errorf("act run %d was embedded, want no vector for a run that can never be offered", id)
		}
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

// TestSimilarActRunsReplacesAVectorFromAnotherModel checks what happens when the embedding model changes under the store: EmbeddingGemma writes 768 numbers where the Gemini API wrote 3072, and a cosine between the two is meaningless. A stored vector of another length is not scored, it is thrown away and the question re-embedded, the same path a run with no vector at all takes.
func TestSimilarActRunsReplacesAVectorFromAnotherModel(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	ctx := context.Background()
	const asked = "show me how to change subtitles"
	const stored = "where can i change subtitles here?"
	emb.register(asked, 0)

	id, err := store.AddActRun(ctx, ActRun{Question: stored, Outcome: "ok", Steps: twoScreenSteps()})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	store.SetEmbedder(emb)
	// Three numbers where this store's model writes two, which is what a store carried over from another embedding model holds.
	emb.registerVector(stored, []float32{1, 0, 0})
	if err := store.embedActRunQuestion(ctx, id, stored); err != nil {
		t.Fatalf("embedActRunQuestion: %v", err)
	}
	ids := []int64{id}
	if got := len(actRunVector(t, store, ids[0])); got != 12 {
		t.Fatalf("the seeded vector is %d bytes, want the 12 a three-number vector takes", got)
	}

	matches, err := store.SimilarActRuns(ctx, asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("SimilarActRuns scored a vector of another length, want it skipped: %+v", matches)
	}
	// The backfill re-embeds it, and by then the embedder answers on this model's scale.
	emb.register(stored, 20)
	deadline := time.Now().Add(time.Second)
	for len(actRunVector(t, store, ids[0])) != 8 {
		if time.Now().After(deadline) {
			t.Fatalf("the stale vector is still %d bytes after a second, want it replaced with 8", len(actRunVector(t, store, ids[0])))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestActRunFloorIsAConfigurableDefault checks the number this whole thing turns on can be moved without a rebuild, and that it cannot be turned off by accident: a floor of zero or less is ignored and the default stands.
func TestActRunFloorIsAConfigurableDefault(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	const asked = "show me how to change subtitles"
	emb.register(asked, 0)
	// 55 degrees is 0.574: below the default floor, above a floor of 0.5.
	ids := seedActRuns(t, store, emb, actQuestion{"show me how to change the graph function", 55})

	if store.actRunFloor() != DefaultActRunSimilarity {
		t.Errorf("a store with nothing set uses a floor of %.2f, want the default %.2f", store.actRunFloor(), DefaultActRunSimilarity)
	}
	matches, err := store.SimilarActRuns(context.Background(), asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("SimilarActRuns returned %d matches under the default floor, want none: %+v", len(matches), matches)
	}

	for _, ignored := range []float64{0, -1} {
		store.SetActRunSimilarityFloor(ignored)
		if store.actRunFloor() != DefaultActRunSimilarity {
			t.Errorf("a floor of %v changed the floor to %.2f, want it ignored", ignored, store.actRunFloor())
		}
	}

	store.SetActRunSimilarityFloor(0.5)
	matches, err = store.SimilarActRuns(context.Background(), asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns under a lowered floor: %v", err)
	}
	if len(matches) != 1 || matches[0].Run.ID != ids[0] {
		t.Fatalf("SimilarActRuns returned %+v under a floor of 0.5, want the one run %d", matches, ids[0])
	}
}

// TestActRunVectorsDieWithTheirRun checks the retention pass leaves nothing behind: a vector is worth about three kilobytes on the real embedder, and one kept for a run that has been pruned would be three kilobytes nothing can ever read again.
func TestActRunVectorsDieWithTheirRun(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	ids := seedActRuns(t, store, emb,
		actQuestion{"show me how to change subtitles", 0},
		actQuestion{"take me to the open tab where family guy is playing", 40},
	)
	for _, id := range ids {
		if len(actRunVector(t, store, id)) == 0 {
			t.Fatalf("act run %d was seeded without a vector", id)
		}
	}

	// Keep one run, which drops the older of the two.
	removed, err := store.PruneActRuns(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("PruneActRuns: %v", err)
	}
	if removed != 1 {
		t.Fatalf("PruneActRuns removed %d runs, want 1", removed)
	}
	if blob := actRunVector(t, store, ids[0]); len(blob) != 0 {
		t.Errorf("the pruned run's vector is still there, %d bytes of it", len(blob))
	}
	if len(actRunVector(t, store, ids[1])) == 0 {
		t.Errorf("the kept run lost its vector")
	}
}

// TestSimilarActRunsFallsBackToAnEmbeddedRunOfTheSameGoal pins what happens when the newest run of a goal has no vector yet: it is queued for the background backfill, and the lookup goes on to the older run of the same goal that does have one. Marking the goal as seen before the vector was decoded hid every embedded run of that goal from the ask that triggered the backfill.
func TestSimilarActRunsFallsBackToAnEmbeddedRunOfTheSameGoal(t *testing.T) {
	store := newFileStore(t)
	emb := newActEmbedder()
	ctx := context.Background()
	const asked = "show me how to change subtitles"
	const stored = "where can i change subtitles here?"
	emb.register(asked, 0)
	ids := seedActRuns(t, store, emb, actQuestion{stored, 20})

	// A newer run of the same goal, written straight to the table so it has no vector — the state every run was in before act runs were embedded.
	steps, err := json.Marshal(twoScreenSteps())
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO act_runs (question, model, outcome, answer, error, duration_ms, steps_json) VALUES (?, 'gemini-3-flash', 'ok', '', '', 10, ?)`,
		stored, string(steps)); err != nil {
		t.Fatalf("insert the unembedded run: %v", err)
	}

	matches, err := store.SimilarActRuns(ctx, asked, 3)
	if err != nil {
		t.Fatalf("SimilarActRuns: %v", err)
	}
	if len(matches) != 1 || matches[0].Run.ID != ids[0] {
		t.Fatalf("SimilarActRuns returned %+v, want the older run %d that does have a vector", matches, ids[0])
	}
}
