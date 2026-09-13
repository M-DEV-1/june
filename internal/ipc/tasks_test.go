package ipc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
	"ora/internal/db/dbtest"
	"ora/internal/memory"
)

// TestTasksListsBothSources checks that /tasks shows the action items a meeting raised as "noticed" and the tasks the user typed in as "you".
func TestTasksListsBothSources(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: memory.MeOwner, Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "Lodestone sync", Raised: time.Now()}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct {
		ID             string
		ConversationID string `json:"conversation_id"`
	}
	if code := postJSON(t, srv, "/tasks", `{"title":"book the flight"}`, &created); code != http.StatusCreated {
		t.Fatalf("POST /tasks status = %d, want 201", code)
	}
	if created.ID == "" || created.ConversationID == "" {
		t.Fatalf("POST /tasks = %+v, want an id and a conversation", created)
	}

	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 2 {
		t.Fatalf("tasks = %d, want the noticed one and the typed one", len(list.Tasks))
	}
	var yours, noticed *Task
	for i := range list.Tasks {
		switch list.Tasks[i].Source {
		case "you":
			yours = &list.Tasks[i]
		case "noticed":
			noticed = &list.Tasks[i]
		}
	}
	if yours == nil || noticed == nil {
		t.Fatalf("tasks = %+v, want one of each source", list.Tasks)
	}
	if yours.Title != "book the flight" || yours.ConversationID != created.ConversationID {
		t.Errorf("your task = %+v", *yours)
	}
	if yours.Detail != "you said" {
		t.Errorf("a task you typed in has detail = %q, want %q", yours.Detail, "you said")
	}
	if yours.Owner != memory.OwnerMe {
		t.Errorf("a task you typed in has owner = %q, want %q", yours.Owner, memory.OwnerMe)
	}
	if noticed.Title != "send the deck" || !strings.HasPrefix(noticed.Detail, "Lodestone sync") {
		t.Errorf("noticed task = %+v, want its detail to name the meeting it was raised from", *noticed)
	}
	if noticed.Done {
		t.Errorf("an open action item came back done")
	}
	if noticed.When == "" {
		t.Errorf("noticed task has no time")
	}
}

// TestTaskDone checks both halves of the tick: a task the user typed in flips in user_tasks, and a noticed one closes the action note the same way the agent's revise tool does.
func TestTaskDone(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: memory.MeOwner, Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/tasks", `{"title":"book the flight"}`, &created)

	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	var noticedID string
	for _, task := range list.Tasks {
		if task.Source == "noticed" {
			noticedID = task.ID
		}
	}

	for _, id := range []string{created.ID, noticedID} {
		if code := postJSON(t, srv, "/tasks/"+id+"/done", `{"done":true}`, nil); code != http.StatusOK {
			t.Fatalf("POST /tasks/%s/done status = %d, want 200", id, code)
		}
	}

	getJSON(t, srv, "/tasks", &list)
	for _, task := range list.Tasks {
		if !task.Done {
			t.Errorf("task %+v is still open after being ticked", task)
		}
	}

	open, err := store.OpenActionItems(ctx)
	if err != nil {
		t.Fatalf("OpenActionItems: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("open action items = %d, want none — the tick must close the note itself", len(open))
	}
}

// TestTaskDoneStatusBody checks that POST /tasks/{id}/done also accepts {"status": "open"|"done"|"dropped"} for a noticed task, and that a dropped one drops off GET /tasks without ever coming back as done.
func TestTaskDoneStatusBody(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: memory.MeOwner, Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("tasks = %d, want the one seeded action item", len(list.Tasks))
	}
	id := list.Tasks[0].ID

	if code := postJSON(t, srv, "/tasks/"+id+"/done", `{"status":"dropped"}`, nil); code != http.StatusOK {
		t.Fatalf("POST /tasks/%s/done status=dropped = %d, want 200", id, code)
	}
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 0 {
		t.Errorf("tasks after dropping = %+v, want the dropped task gone from the list", list.Tasks)
	}

	if code := postJSON(t, srv, "/tasks/"+id+"/done", `{"status":"open"}`, nil); code != http.StatusOK {
		t.Fatalf("POST /tasks/%s/done status=open = %d, want 200", id, code)
	}
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 1 || list.Tasks[0].Done {
		t.Errorf("tasks after reopening = %+v, want the task back and open", list.Tasks)
	}

	if code := postJSON(t, srv, "/tasks/"+id+"/done", `{"status":"done"}`, nil); code != http.StatusOK {
		t.Fatalf("POST /tasks/%s/done status=done = %d, want 200", id, code)
	}
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 0 {
		t.Errorf("tasks after closing = %+v, want the closed task gone from the list too", list.Tasks)
	}

	if code := postJSON(t, srv, "/tasks/"+id+"/done", `{"status":"sideways"}`, nil); code != http.StatusBadRequest {
		t.Errorf("POST /tasks/%s/done status=sideways = %d, want 400", id, code)
	}
}

// TestTaskDoneStatusBodyUserTask checks that a task the user typed in takes status open/done through the same body, and rejects dropped since user_tasks has no such state.
func TestTaskDoneStatusBodyUserTask(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/tasks", `{"title":"book the flight"}`, &created)

	if code := postJSON(t, srv, "/tasks/"+created.ID+"/done", `{"status":"done"}`, nil); code != http.StatusOK {
		t.Fatalf("POST /tasks/%s/done status=done = %d, want 200", created.ID, code)
	}
	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 1 || !list.Tasks[0].Done {
		t.Errorf("tasks after closing = %+v, want the typed task marked done", list.Tasks)
	}

	if code := postJSON(t, srv, "/tasks/"+created.ID+"/done", `{"status":"dropped"}`, nil); code != http.StatusBadRequest {
		t.Errorf("POST /tasks/%s/done status=dropped = %d, want 400 — a typed task cannot be dropped", created.ID, code)
	}
}

// TestTaskDoneUnknownID checks that ticking a task that does not exist is refused rather than silently accepted.
func TestTaskDoneUnknownID(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)
	for _, id := range []string{"task-999", "999", "nonsense"} {
		if code := postJSON(t, srv, "/tasks/"+id+"/done", `{"done":true}`, nil); code == http.StatusOK {
			t.Errorf("POST /tasks/%s/done was accepted, want an error", id)
		}
	}
}

// TestPatchTaskOwner checks that PATCH /tasks/{id} lets the user correct whose task a noticed item really is, and that the new class is what GET /tasks reports afterwards — hearing about a thing in a meeting does not make it his, and the user is the one who can say so.
func TestPatchTaskOwner(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: memory.MeOwner, Text: "look into the vendor's new pricing", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks?owner=all", &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("tasks = %d, want the one seeded action item", len(list.Tasks))
	}
	id := list.Tasks[0].ID

	if code := patchJSON(t, srv, "/tasks/"+id, `{"owner":"them"}`); code != http.StatusOK {
		t.Fatalf("PATCH /tasks/%s owner=them = %d, want 200", id, code)
	}
	getJSON(t, srv, "/tasks?owner=them", &list)
	if len(list.Tasks) != 1 || list.Tasks[0].ID != id || list.Tasks[0].Owner != memory.OwnerThem {
		t.Fatalf("after PATCH owner=them, GET /tasks?owner=them = %+v", list.Tasks)
	}
	getJSON(t, srv, "/tasks", &list)
	if len(list.Tasks) != 0 {
		t.Errorf("the moved item is still on the default (mine) list: %+v", list.Tasks)
	}
}

// TestPatchTaskOwner_Rejects checks the three ways PATCH /tasks/{id} refuses a request: a value that is not me/them/unclear, an id naming no action item, and a "task-N" id, since a task the user typed in is always his and has nothing to correct.
func TestPatchTaskOwner_Rejects(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: memory.MeOwner, Text: "renew the domain", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	id := list.Tasks[0].ID

	if code := patchJSON(t, srv, "/tasks/"+id, `{"owner":"sideways"}`); code != http.StatusBadRequest {
		t.Errorf("PATCH /tasks/%s owner=sideways = %d, want 400", id, code)
	}
	if code := patchJSON(t, srv, "/tasks/9999999", `{"owner":"them"}`); code != http.StatusNotFound {
		t.Errorf("PATCH /tasks/9999999 owner=them = %d, want 404", code)
	}

	var created struct{ ID string }
	postJSON(t, srv, "/tasks", `{"title":"book the flight"}`, &created)
	if code := patchJSON(t, srv, "/tasks/"+created.ID, `{"owner":"them"}`); code != http.StatusBadRequest {
		t.Errorf("PATCH /tasks/%s owner=them = %d, want 400 — a typed task is always yours", created.ID, code)
	}
}

// TestTasksEmptyListIsNotNull guards the shape the window renders directly.
func TestTasksEmptyListIsNotNull(t *testing.T) {
	store := dbtest.Open(t)
	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	if list.Tasks == nil {
		t.Errorf("tasks came back null, want an empty list")
	}
}

// storeWithMeeting is a small helper used by the day tests: it files one set of minutes so the day counts as active.
func storeWithMeeting(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.LogNote(context.Background(), sampleMinutes, meetingNoteKind); err != nil {
		t.Fatalf("seed meeting: %v", err)
	}
}

// TestTasksListsOnlyTheUsersOwnNoticedItems checks that GET /tasks, which reads action notes directly rather than through OpenActionItems, applies the same rule: an item owed by somebody else never appears, an item owed by "Me" does. On 2026-09-05 all 47 other people's items were still listed after the store-side filter went in, because this route never went through it.
func TestTasksListsOnlyTheUsersOwnNoticedItems(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	items := []memory.ActionItem{
		{Owner: "Priya", Text: "send the workbook", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "PRDO sync", Raised: time.Now()},
		{Owner: memory.MeOwner, Text: "push the PR", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "PRDO sync", Raised: time.Now()},
	}
	if _, err := store.AddActionItems(ctx, items); err != nil {
		t.Fatal(err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)
	var body struct {
		Tasks []Task `json:"tasks"`
	}
	getJSON(t, srv, "/tasks", &body)
	if len(body.Tasks) != 1 || body.Tasks[0].Title != "push the PR" {
		t.Fatalf("tasks = %+v, want only the item owed by Me", body.Tasks)
	}
}

// TestTasksOwnerFilter checks the three lists behind one page: his own work by default, other people's work on request, and everything at once. Before this, the default list was the exact opposite — his own items were hidden because they carry his name, and every item nobody was named for was shown as his.
func TestTasksOwnerFilter(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if err := store.SetPersonalContext(ctx, "identity", "The user is Alex Rivera — goes by Alex."); err != nil {
		t.Fatal(err)
	}
	items := []memory.ActionItem{
		{Owner: "Alex Rivera", Text: "raise the PR", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "PRDO sync", Raised: time.Now()},
		{Owner: "Priya Shah", Text: "send the workbook", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "PRDO sync", Raised: time.Now()},
		{Owner: memory.UnknownOwner, Text: "clean up the lockfile situation", Status: memory.StatusOpen, Priority: memory.PriorityNormal, Source: "PRDO sync", Raised: time.Now()},
	}
	if _, err := store.AddActionItems(ctx, items); err != nil {
		t.Fatal(err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	for _, c := range []struct {
		query string
		title string
		owner string
	}{
		{"/tasks", "raise the PR", memory.OwnerMe},
		{"/tasks?owner=me", "raise the PR", memory.OwnerMe},
		{"/tasks?owner=them", "send the workbook", memory.OwnerThem},
		{"/tasks?owner=unclear", "clean up the lockfile situation", memory.OwnerUnclear},
	} {
		var body struct {
			Tasks []Task `json:"tasks"`
		}
		getJSON(t, srv, c.query, &body)
		if len(body.Tasks) != 1 || body.Tasks[0].Title != c.title {
			t.Fatalf("GET %s = %+v, want only %q", c.query, body.Tasks, c.title)
		}
		if body.Tasks[0].Owner != c.owner {
			t.Errorf("GET %s owner = %q, want %q", c.query, body.Tasks[0].Owner, c.owner)
		}
	}

	var all struct {
		Tasks []Task `json:"tasks"`
	}
	getJSON(t, srv, "/tasks?owner=all", &all)
	if len(all.Tasks) != 3 {
		t.Fatalf("GET /tasks?owner=all = %d tasks, want 3", len(all.Tasks))
	}
}

// A ticked task stayed on the list for ever, struck through, and the only thing its menu offered was Reopen. Nothing anywhere removed one: GET /tasks answers with every user_tasks row whatever its done flag says, the screen filters on the search box alone, and there was no delete in the UI or in the store. Asked on 2026-09-12 why three finished tasks would not go away, the answer was that nothing had ever been written to make them.
func TestTaskDelete(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: memory.MeOwner, Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("seed action item: %v", err)
	}
	_, srv := newWindowServer(t, &fakeAsker{}, store)

	var created struct{ ID string }
	postJSON(t, srv, "/tasks", `{"title":"book the flight"}`, &created)

	var list struct{ Tasks []Task }
	getJSON(t, srv, "/tasks", &list)
	var noticedID string
	for _, task := range list.Tasks {
		if task.Source == "noticed" {
			noticedID = task.ID
		}
	}

	if code := deleteRequest(t, srv, "/tasks/"+created.ID); code != http.StatusNoContent {
		t.Fatalf("DELETE /tasks/%s status = %d, want 204", created.ID, code)
	}
	getJSON(t, srv, "/tasks", &list)
	for _, task := range list.Tasks {
		if task.ID == created.ID {
			t.Errorf("task %q is still listed after being deleted", task.ID)
		}
	}

	// A noticed item is a note in memory, not a row of the user's own list, and deleting it here would quietly delete a piece of a meeting's minutes. Dropping it is what that is for, through /done.
	if code := deleteRequest(t, srv, "/tasks/"+noticedID); code != http.StatusBadRequest {
		t.Errorf("DELETE /tasks/%s status = %d, want 400 — a noticed item is dropped, not deleted", noticedID, code)
	}

	if code := deleteRequest(t, srv, "/tasks/task-9999"); code != http.StatusNotFound {
		t.Errorf("DELETE of a task that does not exist = %d, want 404", code)
	}
}
