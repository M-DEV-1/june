package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"ora/internal/memory"
)

// TestConversationsAndTurns writes a conversation with two turns and reads it back: the list carries the newest turn's text, the detail carries the turns oldest first with their evidence and tool names intact.
func TestConversationsAndTurns(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "what did priya ask about", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	evidence := json.RawMessage(`[{"title":"Lodestone sync","meta":"meeting","body":"emission factors"}]`)
	if _, err := store.AddTurn(ctx, id, "you", "what did priya ask about", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn you: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "ora", "she asked about emission factors", "ask", evidence, []string{"query_memory", "recall"}); err != nil {
		t.Fatalf("AddTurn ora: %v", err)
	}

	list, err := store.ListConversations(ctx, 50)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListConversations returned %d conversations, want 1", len(list))
	}
	if list[0].Title != "what did priya ask about" || list[0].Brain != "claude" {
		t.Errorf("ListConversations[0] = %+v, want the title and brain it was created with", list[0])
	}
	if list[0].Last != "she asked about emission factors" {
		t.Errorf("Last = %q, want the newest turn's text", list[0].Last)
	}

	conv, err := store.Conversation(ctx, id)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if conv.Title != "what did priya ask about" {
		t.Errorf("Conversation title = %q", conv.Title)
	}

	turns, err := store.ConversationTurns(ctx, id)
	if err != nil {
		t.Fatalf("ConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("ConversationTurns returned %d turns, want 2", len(turns))
	}
	if turns[0].Role != "you" || turns[1].Role != "ora" {
		t.Errorf("turn roles = %q, %q, want you then ora", turns[0].Role, turns[1].Role)
	}
	if len(turns[0].Tools) != 0 {
		t.Errorf("a turn stored with no tools came back with %v, want none", turns[0].Tools)
	}
	if got := turns[1].Tools; len(got) != 2 || got[0] != "query_memory" || got[1] != "recall" {
		t.Errorf("tools = %v, want the two names in order", got)
	}
	var items []struct{ Title, Meta, Body string }
	if err := json.Unmarshal(turns[1].Evidence, &items); err != nil {
		t.Fatalf("evidence did not come back as JSON: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Lodestone sync" {
		t.Errorf("evidence = %+v, want the one row it was stored with", items)
	}
	if turns[1].When.IsZero() {
		t.Errorf("a turn came back with no time")
	}
}

// TestConversationMissing checks that reading a conversation that was never written is an error rather than a blank one.
func TestConversationMissing(t *testing.T) {
	store := newStore(t)
	if _, err := store.Conversation(context.Background(), 404); err == nil {
		t.Fatalf("Conversation on a missing id returned no error")
	}
}

// TestTurnsBetween checks that the window's day page only sees the turns inside the window it asks for.
func TestTurnsBetween(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "today", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	turnID, err := store.AddTurn(ctx, id, "you", "what is on my plate", "ask", nil, nil)
	if err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	now := time.Now()
	inside, err := store.TurnsBetween(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("TurnsBetween: %v", err)
	}
	if len(inside) != 1 || inside[0].ID != turnID {
		t.Errorf("TurnsBetween over now returned %d turns, want the one just written", len(inside))
	}

	outside, err := store.TurnsBetween(ctx, now.AddDate(0, 0, -3), now.AddDate(0, 0, -2))
	if err != nil {
		t.Fatalf("TurnsBetween: %v", err)
	}
	if len(outside) != 0 {
		t.Errorf("TurnsBetween over a past window returned %d turns, want none", len(outside))
	}
}

// TestUserTasks checks the tasks the user types in themselves: they come back with the conversation they were opened with, and marking one done sticks.
func TestUserTasks(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	convID, err := store.CreateConversation(ctx, "book the flight", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	id, err := store.AddUserTask(ctx, "book the flight", convID)
	if err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}

	tasks, err := store.UserTasks(ctx)
	if err != nil {
		t.Fatalf("UserTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "book the flight" || tasks[0].ConversationID != convID {
		t.Fatalf("UserTasks = %+v, want the one task with its conversation", tasks)
	}
	if tasks[0].Done {
		t.Errorf("a new task came back done")
	}

	if err := store.SetUserTaskDone(ctx, id, true); err != nil {
		t.Fatalf("SetUserTaskDone: %v", err)
	}
	tasks, err = store.UserTasks(ctx)
	if err != nil {
		t.Fatalf("UserTasks after done: %v", err)
	}
	if len(tasks) != 1 || !tasks[0].Done {
		t.Errorf("task after SetUserTaskDone = %+v, want done", tasks)
	}

	if err := store.SetUserTaskDone(ctx, 999, true); err == nil {
		t.Errorf("SetUserTaskDone on a missing id returned no error")
	}
}

// TestActiveDays checks that a day shows up once when an episode, a meeting or a diary entry landed on it, and that days older than the window are left out.
func TestActiveDays(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	today := time.Now().Format("2006-01-02")
	if _, err := store.WriteEpisode(ctx, EpisodeWrite{App: "Brave", Title: "a tab", ScreenText: "text"}); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	if _, err := store.LogNote(ctx, "# Lodestone sync", meetingKindForTest); err != nil {
		t.Fatalf("seed meeting note: %v", err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := store.SetDiaryEntry(ctx, yesterday, "day", "yesterday went fine"); err != nil {
		t.Fatalf("seed diary: %v", err)
	}
	if err := store.SetDiaryEntry(ctx, "2020-01-01", "day", "long ago"); err != nil {
		t.Fatalf("seed old diary: %v", err)
	}

	days, err := store.ActiveDays(ctx, time.Now().AddDate(0, 0, -60))
	if err != nil {
		t.Fatalf("ActiveDays: %v", err)
	}
	if len(days) != 2 || days[0] != today || days[1] != yesterday {
		t.Fatalf("ActiveDays = %v, want [%s %s]", days, today, yesterday)
	}
}

// meetingKindForTest is the notes kind meeting minutes are filed under, spelled out here so this test does not depend on internal/ipc.
const meetingKindForTest = "meeting"

// TestEpisodeCountsByDay checks that episodes are tallied per local calendar day within the given bounds, and that a day outside them is left out entirely.
func TestEpisodeCountsByDay(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	if _, err := store.WriteEpisode(ctx, EpisodeWrite{App: "Brave", Title: "a tab", ScreenText: "text"}); err != nil {
		t.Fatalf("seed episode 1: %v", err)
	}
	if _, err := store.WriteEpisode(ctx, EpisodeWrite{App: "Brave", Title: "another tab", ScreenText: "text"}); err != nil {
		t.Fatalf("seed episode 2: %v", err)
	}
	oldID, err := store.WriteEpisode(ctx, EpisodeWrite{App: "Brave", Title: "yesterday's tab", ScreenText: "text"})
	if err != nil {
		t.Fatalf("seed episode 3: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE episodes SET created_at = ? WHERE id = ?`, sqliteUTC(time.Now().AddDate(0, 0, -1)), oldID); err != nil {
		t.Fatalf("backdate episode: %v", err)
	}
	outOfRangeID, err := store.WriteEpisode(ctx, EpisodeWrite{App: "Brave", Title: "ancient tab", ScreenText: "text"})
	if err != nil {
		t.Fatalf("seed episode 4: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE episodes SET created_at = ? WHERE id = ?`, sqliteUTC(time.Now().AddDate(0, 0, -90)), outOfRangeID); err != nil {
		t.Fatalf("backdate episode: %v", err)
	}

	counts, err := store.EpisodeCountsByDay(ctx, time.Now().AddDate(0, 0, -2), time.Now())
	if err != nil {
		t.Fatalf("EpisodeCountsByDay: %v", err)
	}
	if counts[today] != 2 {
		t.Errorf("counts[today] = %d, want 2", counts[today])
	}
	if counts[yesterday] != 1 {
		t.Errorf("counts[yesterday] = %d, want 1", counts[yesterday])
	}
	if _, ok := counts["2020-01-01"]; ok {
		t.Errorf("counts carries a day 90 days back that fell outside the window: %v", counts)
	}
}

// TestListConversationsLastKind checks that a conversation's last-turn kind travels alongside its text, which is what lets the window show a failed turn's reason instead of its raw error in the sidebar.
func TestListConversationsLastKind(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "the flight", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "you", "when does it leave", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn you: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "ora", "Error 503, high demand", "error", nil, nil); err != nil {
		t.Fatalf("AddTurn error: %v", err)
	}

	list, err := store.ListConversations(ctx, 50)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 1 || list[0].LastKind != "error" {
		t.Fatalf("ListConversations = %+v, want the newest turn's kind 'error'", list)
	}

	if _, err := store.AddTurn(ctx, id, "ora", "half past four", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn ask: %v", err)
	}
	list, err = store.ListConversations(ctx, 50)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 1 || list[0].LastKind != "ask" {
		t.Fatalf("ListConversations = %+v, want the newest turn's kind 'ask' once a later turn landed", list)
	}
}

// TestRenameConversation checks that a title can be changed, that a blank one is refused, and that renaming a conversation that does not exist is an error rather than a silent no-op.
func TestRenameConversation(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "old title", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := store.RenameConversation(ctx, id, "  new title  "); err != nil {
		t.Fatalf("RenameConversation: %v", err)
	}
	conv, err := store.Conversation(ctx, id)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if conv.Title != "new title" {
		t.Errorf("title = %q, want the renamed, trimmed title", conv.Title)
	}

	if err := store.RenameConversation(ctx, id, "   "); err == nil {
		t.Errorf("RenameConversation with a blank title returned no error")
	}
	if err := store.RenameConversation(ctx, 999, "x"); err == nil {
		t.Errorf("RenameConversation on a missing id returned no error")
	}
}

// TestActionNotesStayNotes is a guard on the assumption /tasks rests on: an action item is still an ordinary note of kind "action" that memory.ParseAction can read back.
func TestActionNotesStayNotes(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.AddActionItems(ctx, []memory.ActionItem{{Owner: "Alex", Text: "send the deck", Status: memory.StatusOpen, Priority: memory.PriorityNormal}}); err != nil {
		t.Fatalf("AddActionItems: %v", err)
	}
	notes, err := store.NotesOfKindSince(ctx, memory.ActionNoteKind, time.Time{})
	if err != nil {
		t.Fatalf("NotesOfKindSince: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("action notes = %d, want 1", len(notes))
	}
	if a, ok := memory.ParseAction(notes[0].Content); !ok || a.Text != "send the deck" {
		t.Errorf("ParseAction(%q) = %+v, %v", notes[0].Content, a, ok)
	}
}

// TestDeleteConversation checks that deleting a conversation removes it and every turn said in it, and that deleting one that does not exist is an error rather than a silent no-op.
func TestDeleteConversation(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "the flight", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "you", "when does it leave", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	if err := store.DeleteConversation(ctx, id); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if _, err := store.Conversation(ctx, id); err == nil {
		t.Errorf("Conversation still reads back after delete")
	}
	turns, err := store.ConversationTurns(ctx, id)
	if err != nil {
		t.Fatalf("ConversationTurns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("turns after delete = %d, want none", len(turns))
	}

	if err := store.DeleteConversation(ctx, 999); err == nil {
		t.Errorf("DeleteConversation on a missing id returned no error")
	}
}

// TestAddTurnRequiresAConversation is the regression guard on the orphan turn. Two things now refuse it: AddTurn's own existence check, and conversation_turns' foreign key on conversation_id, which is enforced because PRAGMA foreign_keys(1) rides in the store's DSN and the driver replays it on every connection. This test holds both to the same promise — a turn written against an id that names no conversation is refused and nothing is stored — and the check is kept in front of the key because it names the conversation in its error instead of surfacing a bare constraint failure. Before either was in place the orphan was stored and reported as stored: the window's /ask takes the conversation id from the caller and does not check it either, so a stale id wrote every question and answer into a conversation that could never be opened again — GET /conversations/{id} answered 404 while GET /days/{date} still listed the questions under "You".
func TestAddTurnRequiresAConversation(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.AddTurn(ctx, 999999, "you", "into the void", "ask", nil, nil); err == nil {
		t.Fatalf("AddTurn against a conversation that does not exist returned no error")
	}

	var stored int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_turns`).Scan(&stored); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if stored != 0 {
		t.Errorf("a rejected AddTurn left %d rows in conversation_turns, want none", stored)
	}

	// The day page reads every conversation's turns, so an orphan would surface there even though no conversation could be opened to show it.
	now := time.Now()
	turns, err := store.TurnsBetween(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("TurnsBetween: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("the day page sees %d orphan turns, want none", len(turns))
	}
}

// TestAddTurnTouchesItsConversation checks the other half of the same write: a stored turn always leaves its conversation's updated_at agreeing with it, which is what ListConversations orders on.
func TestAddTurnTouchesItsConversation(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "the flight", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE conversations SET updated_at = '2020-01-01 00:00:00' WHERE id = ?`, id); err != nil {
		t.Fatalf("age the conversation: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "you", "when does it leave", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	conv, err := store.Conversation(ctx, id)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if conv.Updated.Year() == 2020 {
		t.Errorf("updated_at is still %v after a turn was added", conv.Updated)
	}
}

// TestTurnsBetweenDayEdges pins the window's edges to what internal/ipc's Day handler asks for: one local calendar day, from midnight to a nanosecond short of the next. The first and last second of the day both belong to it, and neither neighbouring day claims them. TestTurnsBetween above only checks an hour either side of now, which would pass just as well if either edge were exclusive.
func TestTurnsBetweenDayEdges(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "edges", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.Local)
	end := day.AddDate(0, 0, 1).Add(-time.Nanosecond) // exactly what ipc.Day passes as `to`

	for _, at := range []time.Time{
		day,                                    // first second of the day
		day.Add(12 * time.Hour),                // midday
		day.AddDate(0, 0, 1).Add(-time.Second), // last second of the day
		day.AddDate(0, 0, 1),                   // first second of the next day
		day.Add(-time.Second),                  // last second of the day before
	} {
		turnID, err := store.AddTurn(ctx, id, "you", at.Format(time.RFC3339), "ask", nil, nil)
		if err != nil {
			t.Fatalf("AddTurn: %v", err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE conversation_turns SET created_at = ? WHERE id = ?`, sqliteUTC(at), turnID); err != nil {
			t.Fatalf("backdate turn: %v", err)
		}
	}

	got, err := store.TurnsBetween(ctx, day, end)
	if err != nil {
		t.Fatalf("TurnsBetween: %v", err)
	}
	want := []string{
		day.Format(time.RFC3339),
		day.Add(12 * time.Hour).Format(time.RFC3339),
		day.AddDate(0, 0, 1).Add(-time.Second).Format(time.RFC3339),
	}
	if len(got) != len(want) {
		t.Fatalf("TurnsBetween over one local day returned %d turns, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("turn %d = %q, want %q", i, got[i].Text, w)
		}
	}

	// The next day's window must claim the midnight turn and nothing the day before it held.
	next, err := store.TurnsBetween(ctx, day.AddDate(0, 0, 1), day.AddDate(0, 0, 2).Add(-time.Nanosecond))
	if err != nil {
		t.Fatalf("TurnsBetween next day: %v", err)
	}
	if len(next) != 1 || next[0].Text != day.AddDate(0, 0, 1).Format(time.RFC3339) {
		t.Errorf("the next day sees %+v, want only its own midnight turn", next)
	}
}

// TestTurnTimeComesBackAsTheInstantItWasWritten guards the timestamp round trip. created_at is written by CURRENT_TIMESTAMP as UTC 'YYYY-MM-DD HH:MM:SS' and read back by the driver, which parses a DATETIME column's text into a time.Time. Asserting only that the time is non-zero (as TestConversationsAndTurns did) would pass just as well if it came back read as local wall-clock, which on a +05:30 machine is five and a half hours out — enough to move a turn onto the wrong day page.
func TestTurnTimeComesBackAsTheInstantItWasWritten(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "clock", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	before := time.Now()
	if _, err := store.AddTurn(ctx, id, "you", "what time is it", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	turns, err := store.ConversationTurns(ctx, id)
	if err != nil {
		t.Fatalf("ConversationTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("ConversationTurns returned %d turns, want 1", len(turns))
	}
	// A second either way covers CURRENT_TIMESTAMP truncating to whole seconds; anything larger is a timezone read the wrong way round.
	if off := turns[0].When.Sub(before); off < -2*time.Second || off > 2*time.Second {
		t.Errorf("turn time is %v, %v away from when it was written", turns[0].When, off)
	}

	conv, err := store.Conversation(ctx, id)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if off := conv.Created.Sub(before); off < -2*time.Second || off > 2*time.Second {
		t.Errorf("conversation created at %v, %v away from when it was written", conv.Created, off)
	}
}

// TestAddTurnUnderConcurrentWriters checks that wrapping AddTurn's two writes in one transaction did not turn a busy database into a failed write. A file-backed store is used deliberately: the in-memory pool is pinned to one connection, so only a real file exercises WAL and the DSN's busy_timeout.
func TestAddTurnUnderConcurrentWriters(t *testing.T) {
	store, err := New(t.TempDir() + "/ora.db")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "everyone at once", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	const writers, each = 8, 25
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := store.AddTurn(ctx, id, "you", fmt.Sprintf("writer %d turn %d", w, i), "ask", nil, nil); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AddTurn under concurrent writers: %v", err)
	}

	turns, err := store.ConversationTurns(ctx, id)
	if err != nil {
		t.Fatalf("ConversationTurns: %v", err)
	}
	if len(turns) != writers*each {
		t.Errorf("stored %d turns, want %d", len(turns), writers*each)
	}
}

// TestDeleteConversation_UnlinksTheTasksThatPointedAtIt pins that deleting a thread leaves no task pointing at an id that names nothing. user_tasks.conversation_id carries no foreign key, so nothing in the database does this on its own, and a task left pointing at a deleted conversation opens an empty thread in the window.
func TestDeleteConversation_UnlinksTheTasksThatPointedAtIt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.CreateConversation(ctx, "book the flights", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	taskID, err := store.AddUserTask(ctx, "book the flights", id)
	if err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}
	other, err := store.CreateConversation(ctx, "something else", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	keptID, err := store.AddUserTask(ctx, "something else", other)
	if err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}

	if err := store.DeleteConversation(ctx, id); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}

	tasks, err := store.UserTasks(ctx)
	if err != nil {
		t.Fatalf("UserTasks: %v", err)
	}
	for _, task := range tasks {
		switch task.ID {
		case taskID:
			if task.ConversationID != 0 {
				t.Errorf("the task still points at conversation %d, which was deleted", task.ConversationID)
			}
		case keptID:
			if task.ConversationID != other {
				t.Errorf("an unrelated task's link changed to %d, want %d", task.ConversationID, other)
			}
		}
	}
}
