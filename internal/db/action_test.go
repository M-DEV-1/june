package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"ora/internal/memory"
)

func item(owner, text string) memory.ActionItem {
	return memory.ActionItem{
		Owner: owner, Text: text,
		Status: memory.StatusOpen, Priority: memory.PriorityNormal,
		Source: "vq x zb tool", Raised: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	}
}

// The zombie test: re-filing the same minutes after the user has closed an item must not resurrect it. LogNote dedupes on exact content, and a closed item's content differs by its status tag, so the match has to be on the work itself.
func TestAddActionItems_DoesNotResurrectAClosedItem(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "finish the acme-basics setup.")}); err != nil {
		t.Fatal(err)
	}
	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetActionStatus(ctx, open[0].NoteID, memory.StatusDone); err != nil {
		t.Fatal(err)
	}

	// The recorder re-files the same meeting's minutes, as it does on every retry.
	added, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "finish the acme-basics setup.")})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("re-filing added %d items, want 0", added)
	}
	open, err = store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("a closed item came back open: %+v", open)
	}
}

// Correcting the work an item describes keeps it a tracked action item: the rendered line is re-rendered around the new text, so its status, priority and provenance survive and it stays in the open list rather than dropping out of every read that goes through ParseAction.
func TestSetActionText_KeepsTheItemTrackedAndItsOtherFields(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "deploy the staging PR.")}); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenActionItems(ctx)
	if err := store.SetActionPriority(ctx, open[0].NoteID, memory.PriorityHigh); err != nil {
		t.Fatal(err)
	}

	if err := store.SetActionText(ctx, open[0].NoteID, "deploy the checkout-flow PR."); err != nil {
		t.Fatal(err)
	}

	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("want the item still open after correcting its text, got %+v", open)
	}
	if open[0].Text != "deploy the checkout-flow PR." {
		t.Errorf("text = %q, want the correction", open[0].Text)
	}
	if open[0].Owner != "Me" || open[0].Priority != memory.PriorityHigh || open[0].Source != "vq x zb tool" {
		t.Errorf("correcting the text lost the rest of the item: %+v", open[0])
	}
}

// An id the model invented must be reported, not silently ignored — reporting a correction as applied when it was not throws the user's words away.
func TestSetActionStatus_UnknownID(t *testing.T) {
	if err := newStore(t).SetActionStatus(context.Background(), 4242, memory.StatusDone); err == nil {
		t.Fatal("want an error for an id that is not an action note")
	}
}

// The user's own correction wins over whatever the minutes' owner text would otherwise read as: hearing about a thing does not make it his, and a class he picked by hand is the one place that call is actually made.
func TestSetOwnerClass_OverridesTheParsedOwner(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "look into the vendor's new pricing.")}); err != nil {
		t.Fatal(err)
	}
	open, _ := store.OpenActionItems(ctx)
	if len(open) != 1 {
		t.Fatalf("want 1 open item, got %d", len(open))
	}
	if err := store.SetOwnerClass(ctx, open[0].NoteID, memory.OwnerThem); err != nil {
		t.Fatal(err)
	}

	all, err := store.ActionItemsByOwner(ctx, memory.OwnerThem)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].NoteID != open[0].NoteID {
		t.Fatalf("owner=them = %+v, want the item just moved there", all)
	}
	mine, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 0 {
		t.Errorf("the item is still on the user's own open list after being moved to them: %+v", mine)
	}
}

// testIdentity is the personal-context entry that says who the user is, the same shape the store migrates in on open.
const testIdentity = "The user is Zemna Braxen — goes by Zemna; git handle zbraxen."

// With an identity on file the user's own name is his work, and an item nobody was named for is not — which is the whole complaint about the tasks page: it was full of other people's business.
func TestActionItemsByOwner(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		item("Zemna Braxen", "raise the PR for the prompt change."),
		item("Me", "send the deck by Friday."),
		item("Vexil Quorin", "create the Northwind Freight test account."),
		item(memory.UnknownOwner, "clean up the mixed lockfile situation in the frontend."),
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		owner string
		want  int
	}{{memory.OwnerMe, 2}, {memory.OwnerThem, 1}, {memory.OwnerUnclear, 1}, {"all", 4}} {
		got, err := store.ActionItemsByOwner(ctx, c.owner)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("owner %q: want %d items, got %d: %+v", c.owner, c.want, len(got), got)
		}
	}

	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("OpenActionItems should be his own alone, got %d: %+v", len(open), open)
	}
}

// Evidence that a task was finished closes it and says where that was read, on a positive; a near miss and a plain mention leave it open.
func TestCloseDoneActionItems(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{
		item("Me", "deploy the Route Planning & risk-statements PR (#5632)."),
		item("Me", "rework the hardcoded location-finder logic in the route statements file."),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(ctx, "Merged the Route Planning risk-statements PR #5632 to main; the location-finder rework is still open.", "meeting"); err != nil {
		t.Fatal(err)
	}

	closed, err := store.CloseDoneActionItems(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	all, err := store.ActionItemsByOwner(ctx, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if strings.Contains(a.Text, "#5632") {
			if a.Status != memory.StatusDone {
				t.Errorf("the deployed PR is still %q", a.Status)
			}
			if a.DoneSource == "" {
				t.Errorf("a task closed by evidence must say where that evidence was: %+v", a)
			}
		} else if a.Status != memory.StatusOpen {
			t.Errorf("the untouched task moved to %q", a.Status)
		}
	}

	// A second sweep over the same evidence has nothing left to close, so the nightly run is idempotent.
	again, err := store.CloseDoneActionItems(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("a repeat sweep closed %d more", again)
	}
}

// The minutes that raise an item are filed in the same second as the item and always talk about the same work, so they must never be read as evidence that it is finished. On 2026-09-02 the 1:1 minutes said the branches had been pushed in the same breath as asking for them to be pushed, and a sweep without this guard closed the task on the spot.
func TestCloseDoneActionItems_TheMeetingThatRaisedItCannotCloseIt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	raiser := "1:1 with Vexil Quorin"
	a := item(memory.MeOwner, "rebase the two pending branches and push them in logical chunks.")
	// The recorder raises an item with the recording's own start (liftActionItems in internal/recorder/recorder.go), so the minutes and the items they raise always share a day; that pair is what identifies the raising meeting now that its name alone no longer does.
	a.Source, a.Raised = raiser, time.Now()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(ctx, "# "+raiser+"\n\n**"+raiser+" — Tue 2 Sep 2026, 12:00**\n\n## Your part\n- Pushed the two pending branches after rebasing them in logical chunks.\n", "meeting"); err != nil {
		t.Fatal(err)
	}

	closed, err := store.CloseDoneActionItems(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Errorf("the meeting that raised the task closed it: %d closed", closed)
	}
}

// A recurring meeting is the one place "yes, I did that" gets said, so today's standup must be able to close what last week's standup raised. The two share a name, which is all the provenance an action item carries, so the day it was raised on is what tells the instances apart.
func TestCloseDoneActionItems_ALaterInstanceOfARecurringMeetingClosesIt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	standup := "Daily Platform standup"
	a := item(memory.MeOwner, "deploy the Route Planning & risk-statements PR (#5632).")
	a.Source, a.Raised = standup, time.Now().AddDate(0, 0, -7)
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(ctx, "# "+standup+"\n\n**"+standup+" — Fri 5 Sep 2026, 09:30**\n\n## Your part\n- Merged the Route Planning risk-statements PR #5632 to main.\n", "meeting"); err != nil {
		t.Fatal(err)
	}

	closed, err := store.CloseDoneActionItems(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("closed = %d, want 1: a later instance of a recurring meeting must close what an earlier one raised", closed)
	}
}

// TestCloseDoneActionItems_ReadsOnlyTheDayPages pins which diary rows count as evidence: the day pages, and nothing else. The morning brief is generated from the open task list itself, so it names every open task and often carries a completion word about one of them; the understanding doc, the dream reports and the task-notice watermark are not writing about the user's day at all. All of them used to be read as evidence, and every one of them was labelled "your day, ".
func TestCloseDoneActionItems_ReadsOnlyTheDayPages(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "deploy the Route Planning & risk-statements PR (#5632).")}); err != nil {
		t.Fatal(err)
	}
	const said = "Merged the Route Planning risk-statements PR #5632 to main."
	if err := store.SetDiaryEntry(ctx, "2026-09-06", "brief", said); err != nil {
		t.Fatal(err)
	}

	closed, err := store.CloseDoneActionItems(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Errorf("a morning brief closed %d items, want 0 — only a day page is evidence", closed)
	}

	if err := store.SetDiaryEntry(ctx, "2026-09-06", "day", said); err != nil {
		t.Fatal(err)
	}
	closed, err = store.CloseDoneActionItems(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("the day page closed %d items, want 1", closed)
	}
}

// TestCloseDoneActionItems_ReadsEvidenceWrittenAtTheSinceInstant pins that the diary read binds its cutoff the way every other dated query in this package binds one. A raw time.Time renders as "2026-09-06 08:15:04 +0000 UTC", which compares as a string against a stored "2026-09-06 08:15:04" and excludes the very row it was derived from.
func TestCloseDoneActionItems_ReadsEvidenceWrittenAtTheSinceInstant(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.SetPersonalContext(ctx, "identity", testIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{item("Me", "deploy the Route Planning & risk-statements PR (#5632).")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDiaryEntry(ctx, "2026-09-06", "day", "Merged the Route Planning risk-statements PR #5632 to main."); err != nil {
		t.Fatal(err)
	}
	var written time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT created_at FROM diary WHERE kind = 'day'`).Scan(&written); err != nil {
		t.Fatalf("read the day page's created_at: %v", err)
	}

	closed, err := store.CloseDoneActionItems(ctx, written)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Errorf("evidence written at the since instant closed %d items, want 1", closed)
	}
}
