// This file is the store for lessons learned from earlier screen runs: one short line about what worked or what to avoid, scoped to one app and the goal it applied to, found later the same way SimilarActRuns finds a past run — by the cosine between an embedding of a new goal and a lesson's own stored embedding of its goal and text (see encodeActRunVector, decodeActRunVector, actRunCosine in act_reference.go, which this file reuses rather than duplicating). internal/agent/act_reference.go reads lessons before a screen ask starts; internal/agent/ask.go's end-of-ask hook writes new ones after one finishes and scores the ones that were shown against how the run ended.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// lessonSimilarityFloor is the cosine a lesson's stored embedding must reach against a goal, or one part of it, before it is offered. Measured on 2026-09-23 with EmbeddingGemma against the 51 lessons in the store, for nine parts of real prompts: every lesson that applied scored 0.67 to 0.83, the best wrong one 0.635, and a typical lesson about 0.40. The 0.70 it used to share with past runs turned away two of the right ones.
const lessonSimilarityFloor = 0.65

// lessonScanCap bounds how many lessons a lookup scores. Five hundred: reading one id/goal/hits/vec row is cheap, and the drop rule in ScoreLessonsUsed and the dream's merge keep the count far below this long before it would matter.
const lessonScanCap = 500

// Lesson is one thing learned from an earlier screen run in one app: the one-line lesson itself, the goal it was learned on, and how often showing it since has gone well (Hits) versus badly (Misses). See ScoreLessonsUsed.
type Lesson struct {
	ID         int64
	App        string
	Goal       string
	Lesson     string
	Hits       int
	Misses     int
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// nothingHedges are the openings a reflective reply takes when it means "nothing" but will not say only that. Every one of these was written by the real model into the real store: on 2026-09-12, 19 of the 21 lessons it held were one of these sentences, all of them retrievable and all of them due to be put in front of a later run as guidance.
//
// ponytail: a list of openings, not a classifier. It is matched against what the model actually wrote on this machine, so a new hedge shape gets refused once it is added here. Spending a model call to judge a line whose whole value is that it can be dropped for free is not worth it.
var nothingHedges = []string{
	"nothing worth", "nothing jumps", "nothing stands", "nothing of note", "nothing to report",
	"nothing to flag", "nothing to add", "nothing much", "nothing obvious", "nothing here",
	"nothing there", "nothing that", "nothing in particular", "nothing —", "nothing -", "nothing,",
}

// IsNothingLesson reports whether a line says there is nothing to carry forward, so it is refused at the store rather than filed as guidance a later run would be shown. It catches the sentinel a reflective prompt asks for ("none"), the bare word in any case or punctuation ("Nothing."), and the hedged forms in nothingHedges. Input: the candidate lesson text. Output: true when it is a non-answer.
//
// The hedges are the reason this is more than an equality check. A guard that compared against the single word "nothing" let "Nothing worth flagging — just browsing, a Meet call, and a PDF read" through, nineteen times out of twenty-one. A reply that opens with "nothing" and then says something real — "Nothing on the page worked until I focused the field first, so click it before typing" — is not a hedge and is kept.
func IsNothingLesson(lesson string) bool {
	bare := strings.ToLower(strings.TrimSpace(lesson))
	bare = strings.Trim(bare, ` .!"'`)
	if bare == "" || bare == "nothing" || bare == "none" {
		return true
	}
	for _, hedge := range nothingHedges {
		if strings.HasPrefix(bare, hedge) {
			return true
		}
	}
	return false
}

// AddLesson stores one lesson for an app and goal and embeds goal+lesson the same way a screen run's question is embedded (see embedActRunQuestion), so SimilarLessons can find it later by meaning. Input: ctx, the app it applies to, the goal it was learned on, and the one-line lesson text. Output: the new row's id, or an error from the store. A blank lesson, or one that says there is nothing to carry forward (see IsNothingLesson), is refused rather than stored as a line a later run would be shown as guidance.
func (s *Store) AddLesson(ctx context.Context, app, goal, lesson string) (int64, error) {
	app = strings.TrimSpace(app)
	goal = strings.TrimSpace(goal)
	lesson = strings.TrimSpace(lesson)
	if IsNothingLesson(lesson) {
		return 0, fmt.Errorf("add lesson: %q says nothing to carry forward", lesson)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO lessons (app, goal, lesson) VALUES (?, ?, ?)`, app, goal, lesson)
	if err != nil {
		return 0, fmt.Errorf("add lesson: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add lesson: %w", err)
	}
	s.embedLessonAsync(id, goal, lesson)
	return id, nil
}

// lessonEmbedText is what goes into the embedder for one lesson, goal and text joined the same way a caller would read them together — so "same goal, same wording" scores exactly like a stored question does in SimilarActRuns.
func lessonEmbedText(goal, lesson string) string {
	return goal + ": " + lesson
}

// embedLesson embeds one lesson's goal+lesson text and writes it to lesson_vectors. Input: ctx, the lesson's id, goal and text. Output: an error from the embedder or the store.
func (s *Store) embedLesson(ctx context.Context, id int64, goal, lesson string) error {
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil {
		return fmt.Errorf("embed lesson %d: no embedder", id)
	}
	vec, err := emb.Embed(ctx, "RETRIEVAL_DOCUMENT", lessonEmbedText(goal, lesson))
	if err != nil {
		return fmt.Errorf("embed lesson %d: %w", id, err)
	}
	if len(vec) == 0 {
		return fmt.Errorf("embed lesson %d: the embedder returned no vector", id)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO lesson_vectors (lesson_id, vec) VALUES (?, ?) ON CONFLICT(lesson_id) DO UPDATE SET vec = excluded.vec`,
		id, encodeActRunVector(vec)); err != nil {
		return fmt.Errorf("store lesson %d vector: %w", id, err)
	}
	return nil
}

// embedLessonAsync embeds one new lesson in the background, the same non-blocking shape embedActRunQuestionAsync uses, so AddLesson never waits on the embedding server.
func (s *Store) embedLessonAsync(id int64, goal, lesson string) {
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.embedLesson(ctx, id, goal, lesson); err != nil {
			slog.Warn("lessons: could not embed a new lesson", "lesson_id", id, "error", err)
		}
	}()
}

// SimilarLessons finds the lessons worth showing before a screen run on a goal in the user's own words. A goal usually asks for several things in several apps at once ("play this on Spotify, then message the group on WhatsApp"), and one embedding of all of it sits between its tasks and close to none of them, so three kinds of ranking are fused by reciprocal rank, the way HybridSearch fuses memory: the whole goal by meaning, each part of it by meaning (see goalParts), and every lesson sharing a naming word with the goal (see namedLessons). A lesson reached by meaning must clear lessonSimilarityFloor, and past it hits come before closeness. Input: ctx, the goal, and limit, the most lessons per part of the goal. Output: up to limit lessons for each part, best first, or nothing when no lesson is close; with no embedder only the naming-word ranking runs.
func (s *Store) SimilarLessons(ctx context.Context, goal string, limit int) ([]Lesson, error) {
	if limit <= 0 || len(tokenizeQuery(goal)) == 0 {
		return nil, nil
	}
	lessons, vecs, err := s.allLessons(ctx)
	if err != nil || len(lessons) == 0 {
		return nil, err
	}
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()

	parts := goalParts(goal)
	var lists [][]rrfCandidate
	for _, text := range append([]string{goal}, parts...) {
		if emb == nil {
			break
		}
		asked, err := emb.Embed(ctx, "RETRIEVAL_QUERY", text)
		if err != nil || len(asked) == 0 {
			slog.Warn("lessons: could not embed part of the goal, looking up the rest", "error", err)
			continue
		}
		type scored struct {
			i   int
			cos float64
		}
		var near []scored
		for i, v := range vecs {
			if len(v) != len(asked) {
				continue
			}
			if c := actRunCosine(asked, v); c >= lessonSimilarityFloor {
				near = append(near, scored{i, c})
			}
		}
		// Closeness is the floor, not the order: past the floor a lesson that has helped before beats one that only reads closer and has never been tried.
		sort.SliceStable(near, func(a, b int) bool {
			if ha, hb := lessons[near[a].i].Hits, lessons[near[b].i].Hits; ha != hb {
				return ha > hb
			}
			return near[a].cos > near[b].cos
		})
		list := make([]rrfCandidate, 0, len(near))
		for _, n := range near {
			list = append(list, rrfCandidate{id: strconv.FormatInt(lessons[n.i].ID, 10)})
		}
		lists = append(lists, list)
	}
	lists = append(lists, namedLessons(goal, lessons))

	byID := make(map[string]Lesson, len(lessons))
	for _, l := range lessons {
		byID[strconv.FormatInt(l.ID, 10)] = l
	}
	fused := reciprocalRankFusion(rrfK, lists...)
	sort.SliceStable(fused, func(a, b int) bool {
		if fused[a].score != fused[b].score {
			return fused[a].score > fused[b].score
		}
		return byID[fused[a].id].Hits > byID[fused[b].id].Hits
	})
	want := limit * max(1, len(parts))
	var out []Lesson
	for _, c := range fused {
		if len(out) == want {
			break
		}
		out = append(out, byID[c.id])
	}
	return out, nil
}

// allLessons reads every lesson worth offering with its stored vector, newest first, up to lessonScanCap. Output: the lessons and their vectors in the same order; a lesson with no vector has a nil one, and a lesson saying there is nothing to carry forward is left out.
func (s *Store) allLessons(ctx context.Context) ([]Lesson, [][]float32, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.id, l.app, l.goal, l.lesson, l.hits, l.misses, l.created_at, l.last_used_at, v.vec
		FROM lessons l
		LEFT JOIN lesson_vectors v ON v.lesson_id = l.id
		ORDER BY l.id DESC
		LIMIT ?`, lessonScanCap)
	if err != nil {
		return nil, nil, fmt.Errorf("read lessons: %w", err)
	}
	defer rows.Close()
	var lessons []Lesson
	var vecs [][]float32
	for rows.Next() {
		var l Lesson
		var createdAt string
		var lastUsedAt *string
		var blob []byte
		if err := rows.Scan(&l.ID, &l.App, &l.Goal, &l.Lesson, &l.Hits, &l.Misses, &createdAt, &lastUsedAt, &blob); err != nil {
			return nil, nil, fmt.Errorf("scan lesson: %w", err)
		}
		if IsNothingLesson(l.Lesson) {
			continue // stored before AddLesson refused these; no migration for a table this small, it simply stops being offered
		}
		l.CreatedAt = parseSQLiteTime(createdAt)
		if lastUsedAt != nil {
			l.LastUsedAt = parseSQLiteTime(*lastUsedAt)
		}
		lessons = append(lessons, l)
		vecs = append(vecs, decodeActRunVector(blob))
	}
	return lessons, vecs, rows.Err()
}

// goalSplit cuts a goal at the places one task ends and the next begins: sentence ends, commas, semicolons, and "and"/"then"/"also" between clauses.
var goalSplit = regexp.MustCompile(`(?i),?\s+(?:and then|then|and also|also|after that|and)\s+|[.;!?\n]+|,\s*`)

// goalParts splits a goal into the tasks it asks for, keeping each part that still says something once stopwords are gone. Output: the parts, or nil when the goal is one task.
func goalParts(goal string) []string {
	var parts []string
	for _, p := range goalSplit.Split(goal, -1) {
		if p = strings.TrimSpace(p); len(tokenizeQuery(p)) >= 2 {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return nil
	}
	return parts
}

// namingWordLessons is how many lessons a word may appear in and still name something specific. On the 51 lessons in the store on 2026-09-23, "spotify" and "recommender" each appeared in one or two, while "play", "open" and "find", which pulled in unrelated lessons when any shared word counted, appeared in five or more.
const namingWordLessons = 2

// namedLessons ranks the lessons that share a naming word with the goal: a word of the goal found in a lesson's app or text that appears in no more than namingWordLessons lessons, so "spotify" picks lessons out and "play" does not. It is what still finds a lesson when the embedder is busy, and what finds a WhatsApp lesson filed under the browser it ran in. Input: the goal and every lesson. Output: the lessons sharing at least one naming word, most shared words first, then most hits.
func namedLessons(goal string, lessons []Lesson) []rrfCandidate {
	terms := make([]map[string]bool, len(lessons))
	df := map[string]int{}
	for i, l := range lessons {
		terms[i] = map[string]bool{}
		for _, t := range tokenizeQuery(l.App + " " + l.Lesson) {
			if !terms[i][t] {
				terms[i][t] = true
				df[t]++
			}
		}
	}
	var naming []string
	for _, t := range tokenizeQuery(goal) {
		if df[t] > 0 && df[t] <= namingWordLessons {
			naming = append(naming, t)
		}
	}
	type match struct{ i, shared int }
	var found []match
	for i := range lessons {
		n := 0
		for _, t := range naming {
			if terms[i][t] {
				n++
			}
		}
		if n > 0 {
			found = append(found, match{i, n})
		}
	}
	sort.SliceStable(found, func(a, b int) bool {
		if found[a].shared != found[b].shared {
			return found[a].shared > found[b].shared
		}
		return lessons[found[a].i].Hits > lessons[found[b].i].Hits
	})
	out := make([]rrfCandidate, 0, len(found))
	for _, m := range found {
		out = append(out, rrfCandidate{id: strconv.FormatInt(lessons[m.i].ID, 10)})
	}
	return out
}

// ScoreLessonsUsed records what happened to the run that a set of shown lessons rode along on: a hit on each when the run ended ok, a miss on each otherwise, and last_used_at bumped to now either way. A lesson whose misses have overtaken its hits, and which has missed at least twice, is dropped outright — it has been shown at least three times without earning its place. Input: ctx, the lesson ids the reference block showed this run, and the run's outcome ("ok" or anything else). Output: an error from the store.
func (s *Store) ScoreLessonsUsed(ctx context.Context, ids []int64, outcome string) error {
	if len(ids) == 0 {
		return nil
	}
	hit := outcome == "ok"
	for _, id := range ids {
		var err error
		if hit {
			_, err = s.db.ExecContext(ctx, `UPDATE lessons SET hits = hits + 1, last_used_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
		} else {
			_, err = s.db.ExecContext(ctx, `UPDATE lessons SET misses = misses + 1, last_used_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
		}
		if err != nil {
			return fmt.Errorf("score lesson %d: %w", id, err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM lessons WHERE id = ? AND misses > hits AND misses >= 2`, id); err != nil {
			return fmt.Errorf("drop lesson %d: %w", id, err)
		}
	}
	return nil
}

// Lessons returns every lesson worth offering, newest first, for the night's consolidation to read. Output: the lessons, or an error from the store.
func (s *Store) Lessons(ctx context.Context) ([]Lesson, error) {
	lessons, _, err := s.allLessons(ctx)
	return lessons, err
}

// MergeLessons replaces several lessons of one app with one line that says what they all said, carrying their record: the hits and misses they earned between them, and the newest goal among them. Input: ctx, the ids to replace, and the merged line. Output: the new lesson's id, or an error when an id does not exist, the ids span more than one app, or the store fails; nothing changes on an error.
func (s *Store) MergeLessons(ctx context.Context, from []int64, lesson string) (int64, error) {
	lesson = strings.TrimSpace(lesson)
	if len(from) == 0 || lesson == "" || IsNothingLesson(lesson) {
		return 0, fmt.Errorf("merge lessons: nothing to merge into %q", lesson)
	}
	var id int64
	var goal string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var app string
		var hits, misses int
		for i, old := range from {
			var a, g string
			var h, m int
			if err := tx.QueryRowContext(ctx, `SELECT app, goal, hits, misses FROM lessons WHERE id = ?`, old).Scan(&a, &g, &h, &m); err != nil {
				return fmt.Errorf("merge lessons: lesson %d: %w", old, err)
			}
			if i > 0 && a != app {
				return fmt.Errorf("merge lessons: lesson %d is from %q, not %q", old, a, app)
			}
			app, hits, misses = a, hits+h, misses+m
			if old == slices.Max(from) {
				goal = g
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO lessons (app, goal, lesson, hits, misses) VALUES (?, ?, ?, ?, ?)`, app, goal, lesson, hits, misses)
		if err != nil {
			return fmt.Errorf("merge lessons: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("merge lessons: %w", err)
		}
		for _, old := range from {
			if _, err := tx.ExecContext(ctx, `DELETE FROM lessons WHERE id = ?`, old); err != nil {
				return fmt.Errorf("merge lessons: drop %d: %w", old, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.embedLessonAsync(id, goal, lesson)
	return id, nil
}

// DropLessons deletes lessons outright, for the ones a newer lesson overrides. Input: ctx and the ids. Output: an error from the store.
func (s *Store) DropLessons(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM lessons WHERE id = ?`, id); err != nil {
			return fmt.Errorf("drop lesson %d: %w", id, err)
		}
	}
	return nil
}

// CommitLessonsStage marks the night's lessons token done so the consolidation runs once a night: a merged lesson is new, and without the token every later wake would find its app changed and ask the brain again.
func (s *Store) CommitLessonsStage(ctx context.Context, night string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return markStageDone(ctx, tx, night, StageLessons)
	})
}
