package ipc

import (
	"context"
	"errors"
	"testing"

	"ora/internal/db"
)

// A failed ask used to leave the conversation with a question and no reply, which the app showed as silence and the user read as a hang. The failure is filed as Ora's turn of kind "error" so the thread shows it, and the error event still goes out.
func TestRun_StoresAFailedAskAsAnErrorTurn(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	convID, err := store.CreateConversation(ctx, "smoke", "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	s := New(&fakeAsker{err: errors.New("Error 503, high demand")}, store, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.run(s.asker, "ask-1", convID, "say hello", "", false, nil)

	turns, err := store.ConversationTurns(ctx, convID)
	if err != nil {
		t.Fatalf("ConversationTurns: %v", err)
	}
	if len(turns) != 1 || turns[0].Role != "ora" || turns[0].Kind != "error" || turns[0].Text != "Error 503, high demand" {
		t.Fatalf("turns = %+v, want one ora turn of kind error carrying the message", turns)
	}
	types := []string{}
	for len(ch) > 0 {
		types = append(types, (<-ch).Type)
	}
	if len(types) != 2 || types[0] != "status" || types[1] != "error" {
		t.Errorf("events = %v, want status then error", types)
	}
}
