// This file is the store for lessons learned from earlier screen runs: one short line about what worked or what to avoid, scoped to one app and the goal it applied to, found later the same way SimilarActRuns finds a past run — by the cosine between an embedding of a new goal and a lesson's own stored embedding of its goal and text (see encodeActRunVector, decodeActRunVector, actRunCosine in act_reference.go, which this file reuses rather than duplicating). internal/agent/act_reference.go reads lessons before a screen ask starts; internal/agent/ask.go's end-of-ask hook writes new ones after one finishes and scores the ones that were shown against how the run ended.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// lessonSimilarityFloor is the cosine a lesson's stored embedding must reach against a new goal before it is offered — the same number DefaultActRunSimilarity uses, since both compare one short line of English against another through the same embedding model.
const lessonSimilarityFloor = DefaultActRunSimilarity

// lessonScanCap bounds how many of one app's lessons a lookup scores. Five hundred: reading one id/goal/hits/vec row is cheap, and the drop rule in ScoreLessonsUsed keeps any one app's lesson count far below this long before it would matter.
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

// SimilarLessons finds the lessons learned in one app that are close enough in meaning to a new goal to be worth showing before a screen run on that app starts. Input: ctx, the front app's name, the goal about to be asked, and limit, the most lessons to return. Output: up to limit lessons, most hits first among those close enough, or nothing when the app has no lessons, none are close enough, or the store has no embedder.
//
// Closeness is the floor, not the order: once a lesson's cosine against the new goal clears lessonSimilarityFloor it is a candidate like any other, and what decides which candidates are shown is Hits — a lesson that has actually helped before beats one that merely reads close to this goal and has never been tried.
func (s *Store) SimilarLessons(ctx context.Context, app, goal string, limit int) ([]Lesson, error) {
	if limit <= 0 || strings.TrimSpace(app) == "" {
		return nil, nil
	}
	if len(tokenizeQuery(goal)) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	emb := s.embedder
	s.mu.RUnlock()
	if emb == nil {
		return nil, nil
	}
	asked, err := emb.Embed(ctx, "RETRIEVAL_QUERY", goal)
	if err != nil {
		slog.Warn("lessons: could not embed the goal, offering no lesson", "error", err)
		return nil, nil
	}
	if len(asked) == 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.id, l.app, l.goal, l.lesson, l.hits, l.misses, l.created_at, l.last_used_at, v.vec
		FROM lessons l
		JOIN lesson_vectors v ON v.lesson_id = l.id
		WHERE l.app = ?
		ORDER BY l.id DESC
		LIMIT ?`, app, lessonScanCap)
	if err != nil {
		return nil, fmt.Errorf("similar lessons: %w", err)
	}
	defer rows.Close()

	var scored []Lesson
	for rows.Next() {
		var l Lesson
		var createdAt string
		var lastUsedAt *string
		var blob []byte
		if err := rows.Scan(&l.ID, &l.App, &l.Goal, &l.Lesson, &l.Hits, &l.Misses, &createdAt, &lastUsedAt, &blob); err != nil {
			return nil, fmt.Errorf("scan lesson: %w", err)
		}
		if IsNothingLesson(l.Lesson) {
			continue // stored before AddLesson refused these; no migration for a table this small, it simply stops being offered
		}
		stored := decodeActRunVector(blob)
		if len(stored) != len(asked) {
			// Written by another embedding model, or somehow lacking a vector at all: unscorable now, and not worth a backfill pass of its own for a table this small — the next AddLesson of the same line simply supersedes it.
			continue
		}
		if actRunCosine(asked, stored) < lessonSimilarityFloor {
			continue
		}
		l.CreatedAt = parseSQLiteTime(createdAt)
		if lastUsedAt != nil {
			l.LastUsedAt = parseSQLiteTime(*lastUsedAt)
		}
		scored = append(scored, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lessons: %w", err)
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Hits != scored[j].Hits {
			return scored[i].Hits > scored[j].Hits
		}
		return scored[i].ID > scored[j].ID
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
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
