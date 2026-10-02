package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConversationsAndTurns writes a conversation with two turns and reads it back: the list carries the newest turn's text, the detail carries the turns oldest first with their evidence and tool names intact.
func TestConversationsAndTurns(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	id, err := store.CreateConversation(ctx, "what did vexil ask about", "claude")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	evidence := json.RawMessage(`[{"title":"Lodestone sync","meta":"meeting","body":"shipping factors"}]`)
	if _, err := store.AddTurn(ctx, id, "you", "what did vexil ask about", "ask", nil, nil); err != nil {
		t.Fatalf("AddTurn you: %v", err)
	}
	if _, err := store.AddTurn(ctx, id, "june", "she asked about shipping factors", "ask", evidence, []string{"query_memory", "recall"}); err != nil {
		t.Fatalf("AddTurn june: %v", err)
	}

	list, err := store.ListConversations(ctx, 50)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListConversations returned %d conversations, want 1", len(list))
	}
	if list[0].Title != "what did vexil ask about" || list[0].Brain != "claude" {
		t.Errorf("ListConversations[0] = %+v, want the title and brain it was created with", list[0])
	}
	if list[0].Last != "she asked about shipping factors" {
		t.Errorf("Last = %q, want the newest turn's text", list[0].Last)
	}

	conv, err := store.Conversation(ctx, id)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if conv.Title != "what did vexil ask about" {
		t.Errorf("Conversation title = %q", conv.Title)
	}

	turns, err := store.ConversationTurns(ctx, id)
	if err != nil {
		t.Fatalf("ConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("ConversationTurns returned %d turns, want 2", len(turns))
	}
	if turns[0].Role != "you" || turns[1].Role != "june" {
		t.Errorf("turn roles = %q, %q, want you then june", turns[0].Role, turns[1].Role)
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
	store, err := New(t.TempDir() + "/june.db")
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

// On 2026-09-12 the user asked three times to change and then delete a task June had just made for him, and every attempt was refused: a row in user_tasks could be created and ticked and nothing else, ever, by any part of the product. Correcting its words and getting rid of it are the two things he actually asked for.
func TestUserTaskCanBeRewordedAndRemoved(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	id, err := store.AddUserTask(ctx, "research fly brain training", 0)
	if err != nil {
		t.Fatalf("AddUserTask: %v", err)
	}

	if err := store.SetUserTaskTitle(ctx, id, "research fly brain training: compute, timeline, links"); err != nil {
		t.Fatalf("SetUserTaskTitle: %v", err)
	}
	tasks, err := store.UserTasks(ctx)
	if err != nil {
		t.Fatalf("UserTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "research fly brain training: compute, timeline, links" {
		t.Fatalf("tasks = %+v, want the reworded title", tasks)
	}

	if err := store.DeleteUserTask(ctx, id); err != nil {
		t.Fatalf("DeleteUserTask: %v", err)
	}
	tasks, err = store.UserTasks(ctx)
	if err != nil {
		t.Fatalf("UserTasks after delete: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks = %+v, want none left", tasks)
	}

	// An id that matches nothing must say so rather than report a change it never made, the same rule SetUserTaskDone already holds to.
	if err := store.SetUserTaskTitle(ctx, id, "gone"); err == nil {
		t.Error("SetUserTaskTitle on a deleted task returned no error")
	}
	if err := store.DeleteUserTask(ctx, id); err == nil {
		t.Error("DeleteUserTask on a deleted task returned no error")
	}
}
