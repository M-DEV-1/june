package dream

import (
	"context"
	"strings"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
	"june/internal/memory"
)

// screenDoneFixture is a store holding two of the user's open tasks and, captured after them, the screen showing the first one merged and the second one only open in a tab.
func screenDoneFixture(t *testing.T) (*db.Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		{Owner: "Me", Text: "merge the billing retry pull request", Status: memory.StatusOpen, Priority: memory.PriorityHigh, Source: "standup", Raised: time.Now().AddDate(0, 0, -1)},
		{Owner: "Me", Text: "send Vexil the Meridian shipping file", Status: memory.StatusOpen, Priority: memory.PriorityHigh, Source: "standup", Raised: time.Now().AddDate(0, 0, -1)},
	}); err != nil {
		t.Fatal(err)
	}
	open, err := store.OpenActionItems(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("OpenActionItems = %v, %v", open, err)
	}
	var billing, meridian int64
	for _, a := range open {
		if strings.Contains(a.Text, "billing") {
			billing = a.NoteID
		} else {
			meridian = a.NoteID
		}
	}
	if _, err := store.LogEpisode(ctx, "Brave", "Billing retry pull request #88 - GitHub - Brave", "Billing retry pull request #88\nMerged\nTrelvo Kordis merged 3 commits into main 2 minutes ago"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogEpisode(ctx, "Brave", "Meridian shipping file - Drive - Brave", "Meridian shipping file.xlsx\nShare\nOnly you can access this file"); err != nil {
		t.Fatal(err)
	}
	return store, billing, meridian
}

// A task the screen shows finished is closed, and records the words that showed it, so the window can say why it moved and the user can put it back. A task that is only open on screen stays open even when the brain claims it, because the quote it gives is not on that task's screen.
func TestScreenDoneStage_ClosesOnlyWhatTheScreenShowsFinished(t *testing.T) {
	ctx := context.Background()
	store, billing, meridian := screenDoneFixture(t)
	night := at(23, 30).Format(time.DateOnly)
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	var prompt string
	r.brain = func(ctx context.Context, p string) (string, error) {
		prompt = p
		return `{"done":[{"id":` + itoa(billing) + `,"quote":"Trelvo Kordis merged 3 commits into main"},{"id":` + itoa(meridian) + `,"quote":"the file was sent to Vexil"}]}`, nil
	}

	closed, err := r.screenDoneStage(ctx, night)
	if err != nil {
		t.Fatalf("screenDoneStage: %v", err)
	}
	if closed != 1 {
		t.Errorf("closed %d tasks, want 1", closed)
	}
	if !strings.Contains(prompt, "merged 3 commits") || !strings.Contains(prompt, "Only you can access this file") {
		t.Errorf("the prompt does not carry each task's screen moments:\n%s", prompt)
	}
	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].NoteID != meridian {
		t.Fatalf("open tasks = %+v, want only the Meridian file, whose quote is not on its screen", open)
	}
	all, err := store.ActionItemsByOwner(ctx, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.NoteID == billing && !strings.Contains(a.DoneSource, "merged 3 commits into main") {
			t.Errorf("the closed task's evidence is %q, want the quote that closed it", a.DoneSource)
		}
	}
}

// Every way the brain's word can fail to hold closes nothing: a quote taken from another task's screen, a quote too short to say which work it is about, an id that is not one of the tasks shown, and a reply that is not JSON.
func TestScreenDoneStage_ClosesNothingOnAnswersThatDoNotHold(t *testing.T) {
	cases := map[string]func(billing, meridian int64) string{
		"another task's quote": func(billing, meridian int64) string {
			return `{"done":[{"id":` + itoa(meridian) + `,"quote":"Trelvo Kordis merged 3 commits into main"}]}`
		},
		"a one-word quote": func(billing, meridian int64) string {
			return `{"done":[{"id":` + itoa(billing) + `,"quote":"Merged"}]}`
		},
		"an id it was not shown": func(billing, meridian int64) string {
			return `{"done":[{"id":9999,"quote":"Trelvo Kordis merged 3 commits into main"}]}`
		},
		"no JSON at all": func(billing, meridian int64) string { return "Both tasks look finished to me." },
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, billing, meridian := screenDoneFixture(t)
			r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
			r.brain = func(ctx context.Context, p string) (string, error) { return reply(billing, meridian), nil }

			closed, err := r.screenDoneStage(ctx, at(23, 30).Format(time.DateOnly))
			if err != nil {
				t.Fatalf("screenDoneStage: %v", err)
			}
			open, _ := store.OpenActionItems(ctx)
			if closed != 0 || len(open) != 2 {
				t.Errorf("closed %d, %d still open; want nothing closed", closed, len(open))
			}
		})
	}
}

// A night where no task has a screen moment that names its work makes no brain call at all.
func TestScreenDoneStage_NoScreenMomentsNoBrainCall(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		{Owner: "Me", Text: "renew the Ossaline domain certificate", Status: memory.StatusOpen, Priority: memory.PriorityHigh, Source: "standup", Raised: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogEpisode(ctx, "Terminal", "zsh", "go test ./..."); err != nil {
		t.Fatal(err)
	}
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	r.brain = func(ctx context.Context, p string) (string, error) {
		t.Fatal("the brain must not be called when no task has a screen moment")
		return "", nil
	}
	if closed, err := r.screenDoneStage(ctx, at(23, 30).Format(time.DateOnly)); err != nil || closed != 0 {
		t.Fatalf("screenDoneStage = %d, %v; want 0, nil", closed, err)
	}
}

// A task the user reopens after the screen sweep closed it stays open: the moment that closed it is still in the screen history, and checking it again would close it again every night.
func TestScreenDoneStage_LeavesAReopenedTaskOpen(t *testing.T) {
	ctx := context.Background()
	store, billing, _ := screenDoneFixture(t)
	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	r.brain = func(ctx context.Context, p string) (string, error) {
		return `{"done":[{"id":` + itoa(billing) + `,"quote":"Trelvo Kordis merged 3 commits into main"}]}`, nil
	}
	night := at(23, 30).Format(time.DateOnly)
	if closed, err := r.screenDoneStage(ctx, night); err != nil || closed != 1 {
		t.Fatalf("first night closed %d (%v), want 1", closed, err)
	}
	if err := store.SetActionStatus(ctx, billing, memory.StatusOpen); err != nil {
		t.Fatal(err)
	}
	if closed, err := r.screenDoneStage(ctx, night); err != nil || closed != 0 {
		t.Fatalf("after the user reopened it the sweep closed %d (%v), want 0", closed, err)
	}
}
