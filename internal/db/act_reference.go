// This file is the read half of the act runs: given a new screen question, it finds the past runs that worked on something close enough in meaning to be worth showing the model as reference. The write half and the act_runs table itself are in act_runs.go, whose header says an act run is never searched — this is the one exception to that, and it is a read of the question column and the embedding of it that this file keeps in act_run_vectors (declared with the rest of the schema in store.go). Nothing here performs anything: the caller renders what comes back as a record of what happened, and the model still decides for itself.
package db

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
)

// DefaultActRunSimilarity is how close a past question must be to the new one, in meaning, before its run is offered as reference at all. It is a cosine between the two questions' embeddings: one is the same question, zero is nothing to do with each other.
//
// Seventy hundredths, measured on 2026-09-05 against the embedding engine this store actually uses (EmbeddingGemma-300M on 127.0.0.1:6943, with the same query and document prefixes internal/embed applies) and against questions that are not the user's own. Twelve pairs of everyday desktop asks were written out for the purpose, each pair two wordings of one task — "mute the tab that is making noise" against "stop the sound coming from this tab", "empty the trash" against "delete everything in the recycle bin" — giving 24 same-task scores and 528 different-task scores over the same 24 questions, each scored the way the lookup scores: the asking side as a query, the stored side as a document. Same-task scores ran 0.717 to 0.931, lowest at "empty the trash". Different-task scores ran to 0.678 at the top, with the 99th of them at 0.655; the highest were tab-about-tab pairs such as "mute the tab that is making noise" against "close all the other tabs". The two bands do not overlap, and 0.70 is the middle of the gap between them.
//
// It is deliberately not calibrated on the user's own stored questions. Those are the eval questions this system is being tested with, near-paraphrases of each other by construction, so any gap found in them is a property of the test set and would not transfer. The number above is a property of the embedding model on ordinary desktop asks instead, which is the closest honest stand-in available before there is real usage to measure. What would change it: a few dozen real questions asked days apart, hand-labelled for which pairs are the same task, scored the same way — if the same-task band on those reaches lower than 0.717, the floor comes down with it. Until then it is a starting point, not a finding, and config.EmbedConfig.ActRunSimilarityFloor moves it without a rebuild.
//
// The number is tied to the embedding model. A different model scores the same pair of questions on a different scale — internal/config.DefaultLocalSimilarityFloor exists for exactly that reason on the hybrid search side — so switching embedders means measuring this again.
const DefaultActRunSimilarity = 0.70

// actRunScanCap bounds how many of the newest successful screen runs the lookup scores. Two thousand, which is config.DefaultActRunKeep — the whole table as the retention pass leaves it — because the scan reads three short columns and one small vector per row and never touches steps_json, so scoring every run the store keeps costs less than fetching the winners' steps afterwards.
const actRunScanCap = 2000

// actRunBackfillCap bounds how many questions one background backfill pass embeds. Sixty-four: a local embed of a one-line question takes a few milliseconds, so a pass costs well under a second, and the runs a store already holds are covered in a handful of asks rather than in one long pass competing with the ask that triggered it.
const actRunBackfillCap = 64

// actRunBackfillTimeout bounds one background backfill pass, so a wedged embedding server cannot leave the pass — and the flag that stops a second one starting — alive forever.
const actRunBackfillTimeout = 2 * time.Minute

// ActMatch is one past act run offered as reference for a new question: the run with its steps, when it happened, and the cosine between the two questions' embeddings. Score is what lets a caller tell a near-repeat from a loose association — every match returned already clears the store's floor (see DefaultActRunSimilarity), so the caller is never handed something it has to filter itself.
type ActMatch struct {
	Run   ActRun
	When  time.Time
	Score float64
}

// SetActRunSimilarityFloor overrides the cosine a past question must reach before its run is offered as reference. Values of zero or below are ignored, so the floor can never be turned off entirely and a half-written config cannot start feeding the model unrelated runs.
func (s *Store) SetActRunSimilarityFloor(floor float64) {
	if floor <= 0 {
		return
	}
	s.mu.Lock()
	s.actRunSimilarityFloor = floor
	s.mu.Unlock()
}

// actRunFloor is the cosine floor in force, defaulting to DefaultActRunSimilarity when nothing has overridden it.
func (s *Store) actRunFloor() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.actRunSimilarityFloor > 0 {
		return s.actRunSimilarityFloor
	}
	return DefaultActRunSimilarity
}

// encodeActRunVector packs an embedding for storage in act_run_vectors.vec. Input: the vector. Output: its numbers as little-endian float32s, four bytes each, or nil for an empty vector.
func encodeActRunVector(vec []float32) []byte {
	if len(vec) == 0 {
		return nil
	}
	out := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(v))
	}
	return out
}

// decodeActRunVector unpacks a stored act_run_vectors.vec back into an embedding. Input: the stored bytes. Output: the vector, or nil when there are none or they are not a whole number of float32s, which is a blob this store did not write.
func decodeActRunVector(blob []byte) []float32 {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil
	}
	out := make([]float32, len(blob)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:]))
	}
	return out
}

// actRunCosine measures how close two questions are in meaning as the cosine of the angle between their embeddings. Input: the two vectors, in either order. Output: minus one to one, and zero when the vectors are of different lengths, when either is empty, or when either has no length at all.
//
// Different lengths score zero rather than comparing the overlap: a stored vector of another length was written by another embedding model, and a number produced by mixing two models' spaces would be meaningless while looking exactly like a real score. The lookup treats such a row as one to re-embed instead.
func actRunCosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// actGoalKey is how two runs are recognised as being about the same goal: their question trimmed and case-folded, the same grouping the nightly procedures stage uses to decide that one goal has already been written up.
func actGoalKey(question string) string {
	return strings.ToLower(strings.TrimSpace(question))
}

// SimilarActRuns finds the past screen runs that worked on something close in meaning to a new question, closest first, so the caller can show the model how a similar thing went before. Input: ctx, the question about to be asked, and limit, the most matches to return. Output: up to limit matches, each with its steps decoded, when it happened, and its cosine score; an empty result when nothing is close enough, which is the normal answer for a question unlike anything asked before.
//
// Closeness is meaning, not wording: the question is embedded as a query, each stored question was embedded as a document when its run was written, and the score is the cosine between them. Word overlap, which this used to use, gets the common case backwards — "where can i change subtitles here?" and "show me how to change subtitles" share two significant words of seven and scored 0.29, while "show me how to change the graph function" shares four and scored 0.57.
//
// Only runs whose outcome was "ok" are considered, and only those that actually stored a screen step — a failed run is not a way something was done, and a run with no screen steps has nothing to show. Runs are grouped by goal (see actGoalKey) with the newest of a goal kept, so one question asked six times cannot fill the whole result with itself. Everything below the store's floor (see DefaultActRunSimilarity) is dropped. Ties in closeness are broken by the newer run.
//
// A store with no embedder wired, or an embedding server that will not answer, returns nothing rather than falling back to counting shared words: there is no second measure worth trusting, and a reference block is a help to an ask and never a requirement. Runs whose vector is missing or came from another model are skipped for this lookup and embedded in the background, so the next ask has them (see backfillActRunVectors).
//
// Age never filters and never ranks. What a run contributes is words — "looked at the screen, pointed at item 28 (Audio & Subtitles)" — and the item numbers in it are regenerated by observe_screen every single turn, so an old run cannot mislead the model into acting on a stale number the way a stored coordinate could. What can go stale is a label, if the site was redesigned; the caller renders how long ago the run happened alongside it so the model can weigh that itself, and the store's own retention cap already bounds the oldest run at roughly two months of asking.
func (s *Store) SimilarActRuns(ctx context.Context, question string, limit int) ([]ActMatch, error) {
	if limit <= 0 {
		return nil, nil
	}
	// A question of nothing but stopwords has nothing for a vector to be about, and asking the engine for one costs a round trip to learn that.
	if len(tokenizeQuery(question)) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil {
		return nil, nil
	}
	asked, err := emb.Embed(ctx, "RETRIEVAL_QUERY", question)
	if err != nil {
		slog.Warn("act reference: could not embed the question, offering no past run", "error", err)
		return nil, nil
	}
	if len(asked) == 0 {
		return nil, nil
	}

	// Newest first, so the first run seen for a goal is the newest one and later duplicates of it can simply be skipped. steps_json is not read here: the question and its vector are all the scoring needs, and the winners' steps are fetched afterwards. The join is outer because a run written before act runs were embedded, or one whose embed failed, has no vector row at all and is a run to backfill rather than one to hide.
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.started_at, r.question, v.vec
		FROM act_runs r
		LEFT JOIN act_run_vectors v ON v.run_id = r.id
		WHERE r.outcome = 'ok' AND r.steps_json <> '' AND r.steps_json <> '[]'
		ORDER BY r.id DESC
		LIMIT ?`, actRunScanCap)
	if err != nil {
		return nil, fmt.Errorf("similar act runs: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id    int64
		when  time.Time
		score float64
	}
	var scored []candidate
	var unembedded []int64
	seenGoal := make(map[string]bool)
	floor := s.actRunFloor()
	for rows.Next() {
		var id int64
		var startedAt, q string
		var blob []byte
		if err := rows.Scan(&id, &startedAt, &q, &blob); err != nil {
			return nil, fmt.Errorf("scan act run question: %w", err)
		}
		goal := actGoalKey(q)
		if goal == "" || seenGoal[goal] {
			continue
		}
		stored := decodeActRunVector(blob)
		if len(stored) != len(asked) {
			// Written before act runs were embedded, or written by another model: unscorable now, so it is queued for the background pass and the lookup goes on without it. The goal is deliberately not marked as seen — an older run of the same goal that does have a vector is the best this lookup can still offer, and marking it here hid every embedded run of the goal until the backfill caught up.
			unembedded = append(unembedded, id)
			continue
		}
		seenGoal[goal] = true
		score := actRunCosine(asked, stored)
		if score < floor {
			continue
		}
		scored = append(scored, candidate{id: id, when: parseSQLiteTime(startedAt), score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate act run questions: %w", err)
	}
	rows.Close()

	if len(unembedded) > 0 {
		s.startActRunBackfill(unembedded)
	}
	if len(scored) == 0 {
		return nil, nil
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].id > scored[j].id
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}

	// A second pass for the winners only, after the cursor above is closed — the same shape searchMemoryWindow uses for its per-hit follow-up, and the reason steps_json is never pulled for the whole table.
	out := make([]ActMatch, 0, len(scored))
	for _, c := range scored {
		run, err := s.actRun(ctx, c.id)
		if err != nil {
			return nil, err
		}
		out = append(out, ActMatch{Run: run, When: c.when, Score: c.score})
	}
	return out, nil
}

// embedActRunQuestion embeds one stored run's question and writes it to that run's row. Input: ctx, the run's id and the question as it was asked. Output: an error from the embedder or the store, and nil when the row now holds a vector.
//
// The question is embedded as a document, matching the query side in SimilarActRuns — the model is trained on that asymmetry and loses recall without it.
func (s *Store) embedActRunQuestion(ctx context.Context, id int64, question string) error {
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil {
		return fmt.Errorf("embed act run %d: no embedder", id)
	}
	if strings.TrimSpace(question) == "" {
		return fmt.Errorf("embed act run %d: no question", id)
	}
	vec, err := emb.Embed(ctx, "RETRIEVAL_DOCUMENT", question)
	if err != nil {
		return fmt.Errorf("embed act run %d: %w", id, err)
	}
	if len(vec) == 0 {
		return fmt.Errorf("embed act run %d: the embedder returned no vector", id)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO act_run_vectors (run_id, vec) VALUES (?, ?) ON CONFLICT(run_id) DO UPDATE SET vec = excluded.vec`,
		id, encodeActRunVector(vec)); err != nil {
		return fmt.Errorf("store act run %d vector: %w", id, err)
	}
	return nil
}

// embedActRunQuestionAsync embeds one run's question in the background, the same non-blocking shape LogNote uses. Input: the run's id and its question. Output: none — a failure is logged, and the run is picked up by the next lookup's backfill instead.
//
// Its own context, not the caller's: the ask that produced this run is finished by the time it is stored, and cancelling that ask must not leave the run permanently unscorable.
func (s *Store) embedActRunQuestionAsync(id int64, question string) {
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.embedActRunQuestion(ctx, id, question); err != nil {
			slog.Warn("act reference: could not embed a new run's question, leaving it for the backfill", "act_run_id", id, "error", err)
		}
	}()
}

// startActRunBackfill embeds, in the background, the questions of runs a lookup could not score. Input: the ids that lookup found unscorable, newest first. Output: none.
//
// Only one pass runs at a time: several asks in a row all see the same unembedded rows, and each starting its own pass would embed the same questions several times over.
func (s *Store) startActRunBackfill(ids []int64) {
	if len(ids) == 0 || !s.actRunBackfilling.CompareAndSwap(false, true) {
		return
	}
	if len(ids) > actRunBackfillCap {
		ids = ids[:actRunBackfillCap]
	}
	go func() {
		defer s.actRunBackfilling.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), actRunBackfillTimeout)
		defer cancel()
		if n, err := s.backfillActRunVectors(ctx, ids); err != nil {
			slog.Warn("act reference: backfilling past runs' questions stopped early", "embedded", n, "error", err)
		}
	}()
}

// backfillActRunVectors embeds the questions of the given runs and writes each one's vector. Input: ctx and the run ids to embed. Output: how many were embedded, and the first error that stopped the pass.
//
// This is what makes embedding at write time safe on a store that already holds runs: every run written before act runs were embedded has no vector, and every run written by a previous embedding model has one of the wrong length. Both are unscorable, both are fixed here, and one lookup's worth of them is fixed per pass.
func (s *Store) backfillActRunVectors(ctx context.Context, ids []int64) (int, error) {
	done := 0
	for _, id := range ids {
		var question string
		if err := s.db.QueryRowContext(ctx, `SELECT question FROM act_runs WHERE id = ?`, id).Scan(&question); err != nil {
			return done, fmt.Errorf("backfill act run %d: %w", id, err)
		}
		if err := s.embedActRunQuestion(ctx, id, question); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// actRun reads one act run by id with its steps decoded. Input: ctx and the run's id. Output: the run, or an error from the store or from steps that will not decode.
func (s *Store) actRun(ctx context.Context, id int64) (ActRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, question, model, outcome, answer, error, duration_ms, steps_json FROM act_runs WHERE id = ?`, id)
	r, err := scanActRun(row)
	if err != nil {
		return ActRun{}, fmt.Errorf("act run %d: %w", id, err)
	}
	return r, nil
}
