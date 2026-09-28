package dream

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"june/internal/db/dbtest"
)

// The lesson loop writes a line after every screen run, and by 2026-09-23 one browser held 34 of them, several saying the same thing twice ("click the tab's Close button" and "click the tab's close button directly") and some overtaken by a later one. The night merges each app's lessons that say the same thing into one line that keeps their record, and drops the ones a newer lesson overrides.
func TestLessonsStage_MergesLessonsThatSayTheSameThing(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)
	a, _ := store.AddLesson(ctx, "Brave Browser", "close the tab", "Click the tab's Close button to close a tab")
	b, _ := store.AddLesson(ctx, "Brave Browser", "close that tab", "Click the tab's close button directly rather than pressing Ctrl+W")
	c, _ := store.AddLesson(ctx, "Brave Browser", "play a video", "Click the big Play button on the player")
	store.ScoreLessonsUsed(ctx, []int64{a, b}, "ok")

	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	r.brain = func(ctx context.Context, prompt string) (string, error) {
		if !strings.Contains(prompt, "Click the big Play button") {
			t.Errorf("the prompt does not list the app's lessons: %q", prompt)
		}
		return `{"merge":[{"from":[` + itoa(a) + `,` + itoa(b) + `],"lesson":"Click the tab's own Close button to close it, not Ctrl+W"}],"drop":[]}`, nil
	}
	if _, err := r.lessonsStage(ctx, night); err != nil {
		t.Fatalf("lessonsStage: %v", err)
	}

	got, err := store.Lessons(ctx)
	if err != nil {
		t.Fatalf("Lessons: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("lessons = %+v, want the merged one and the Play one", got)
	}
	for _, l := range got {
		if strings.Contains(l.Lesson, "not Ctrl+W") && l.Hits != 2 {
			t.Errorf("merged lesson has %d hits, want the 2 its two lessons had between them", l.Hits)
		}
		if l.ID == c && l.Lesson != "Click the big Play button on the player" {
			t.Errorf("an unmentioned lesson changed: %q", l.Lesson)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
