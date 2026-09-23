// This file is the lessons stage: once a night, for every app that learned something new since the night before, the dream brain reads that app's lessons and says which ones say the same thing and which a newer one overrides. The screen-run hook writes a lesson after every run, so without this an app's lessons only ever grow, and the same advice shown three times in three wordings crowds out the one line that is different.
package dream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"ora/internal/db"
)

// lessonsReport is what the lessons stage did: how many apps it read, how many lessons it merged away, and how many it dropped.
type lessonsReport struct {
	apps    int
	merged  int
	dropped int
}

// lessonsVerdict is the brain's answer for one app: groups of lessons to merge into one line, and lessons a newer one overrides. A lesson it does not mention is kept as it is, so a reply that leaves one out costs nothing.
type lessonsVerdict struct {
	Merge []struct {
		From   []int64 `json:"from"`
		Lesson string  `json:"lesson"`
	} `json:"merge"`
	Drop []int64 `json:"drop"`
}

// lessonsPrompt asks the dream brain to consolidate one app's lessons. The lessons follow, one per line as "[id] lesson (hits, misses)".
const lessonsPrompt = `These are the lessons you have learned while using %s on the user's computer, one per line with its id and how often showing it preceded a run that worked (hits) or failed (misses).

%s

Consolidate them. Lessons that give the same advice in different words become one line that keeps everything useful in them. A lesson that a newer one contradicts, or that only describes one past task and teaches nothing about the app, is dropped. Leave every other lesson alone by not mentioning it.

Answer with JSON only: {"merge":[{"from":[ids],"lesson":"the one merged line"}],"drop":[ids]}`

// lessonsStage consolidates the lessons of every app that gained one since the day before the night. Input: the night key. Output: the report, or a store or brain transport error so the next wake retries; a reply that will not parse skips that app.
func (r *Runner) lessonsStage(ctx context.Context, night string) (lessonsReport, error) {
	var rep lessonsReport
	all, err := r.store.Lessons(ctx)
	if err != nil {
		return rep, err
	}
	since := dayOf(night).Add(-24 * time.Hour)
	byApp := map[string][]db.Lesson{}
	fresh := map[string]bool{}
	for _, l := range all {
		byApp[l.App] = append(byApp[l.App], l)
		if l.CreatedAt.After(since) {
			fresh[l.App] = true
		}
	}
	apps := make([]string, 0, len(fresh))
	for app := range fresh {
		if len(byApp[app]) > 1 {
			apps = append(apps, app)
		}
	}
	sort.Strings(apps)
	for _, app := range apps {
		lessons := byApp[app]
		ids := make([]int64, len(lessons))
		lines := make([]string, len(lessons))
		for i, l := range lessons {
			ids[i] = l.ID
			lines[i] = fmt.Sprintf("[%d] %s (%d hits, %d misses)", l.ID, l.Lesson, l.Hits, l.Misses)
		}
		var v lessonsVerdict
		if err := r.askJSON(ctx, night, "lessons", fmt.Sprintf(lessonsPrompt, app, strings.Join(lines, "\n")), &v); err != nil {
			if errors.Is(err, errUnparsable) {
				continue
			}
			return rep, err
		}
		rep.apps++
		// Each id is acted on once, and only when it belongs to this app, so a reply that names an id twice or invents one changes nothing it should not.
		used := map[int64]bool{}
		owned := func(id int64) bool { return slices.Contains(ids, id) && !used[id] }
		for _, m := range v.Merge {
			from := slices.DeleteFunc(slices.Clone(m.From), func(id int64) bool { return !owned(id) })
			if len(from) == 0 || strings.TrimSpace(m.Lesson) == "" {
				continue
			}
			if _, err := r.store.MergeLessons(ctx, from, m.Lesson); err != nil {
				return rep, err
			}
			for _, id := range from {
				used[id] = true
			}
			rep.merged += len(from) - 1
		}
		drop := slices.DeleteFunc(slices.Clone(v.Drop), func(id int64) bool { return !owned(id) })
		if err := r.store.DropLessons(ctx, drop); err != nil {
			return rep, err
		}
		rep.dropped += len(drop)
	}
	return rep, nil
}
