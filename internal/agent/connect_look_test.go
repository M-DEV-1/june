package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"ora/internal/tracker"
)

// A live voice session's tool calls must share one screen state, or every one of them gets a throwaway and draw can never tell that look ran at all.
// Before this, withAskLookState was attached on every typed ask path (ask.go, claude.go, codex.go, agy.go) and on none of the live path, so a real session on 2026-09-07 called look, got a picture back, called draw three seconds later and was told "I need to look at the screen first" — then looked again and was told the same thing. That is a loop with no exit, which is why the session reported its screen tools broken.
func TestLiveScreenScope_DrawKnowsThatLookRan(t *testing.T) {
	ctx := liveScreenScope(context.Background())
	recordLook(ctx, tracker.Capture{W: 1280, H: 698})

	_, ok, blind := lookSeen(ctx)
	if ok {
		t.Fatal("the live session was handed a picture it cannot display")
	}
	// The Live API carries tool results as text, so the picture is never delivered and no further look will change that. The honest refusal says so and names the path that does work; the loop said "look first" forever.
	if !blind {
		t.Errorf("draw would answer %q and send the model round the same loop again", needLookFirst)
	}
	if !strings.Contains(cannotSeePictures, "observe_screen") {
		t.Error("the refusal a blind session gets does not name the tool that does work")
	}
}

// lockTryingSession is a liveSession whose SendRealtimeInput probes writeMu with TryLock: TryLock only succeeds when nothing else holds the mutex, so a successful TryLock inside the call proves the caller sent without holding it.
type lockTryingSession struct {
	writeMu   *sync.Mutex
	wasLocked bool // true when writeMu was already held (TryLock failed) at the moment SendRealtimeInput ran
}

func (s *lockTryingSession) SendRealtimeInput(genai.LiveRealtimeInput) error {
	if s.writeMu.TryLock() {
		s.writeMu.Unlock() // acquired it, so it was free — the caller sent without holding it
	} else {
		s.wasLocked = true
	}
	return nil
}
func (s *lockTryingSession) SendToolResponse(genai.LiveSendToolResponseParameters) error { return nil }
func (s *lockTryingSession) SendClientContent(genai.LiveSendClientContentParameters) error {
	return nil
}
func (s *lockTryingSession) Receive() (*genai.LiveServerMessage, error) { return nil, nil }
func (s *lockTryingSession) Close() error                               { return nil }

// A picture pushed with sessionPictureSender must go out under a.writeMu, the same lock audioSendLoop, textSendLoop and every other write to the session already hold. Before this, sessionPictureSender took no lock at all, so the look tool's picture push could land its SendRealtimeInput call at the same instant as audioSendLoop's own, and gorilla/websocket allows only one writer at a time — reproduced in production on 2026-09-10 as "panic: concurrent write to websocket connection".
func TestSessionPictureSender_HoldsWriteMu(t *testing.T) {
	a := &Agent{}
	checking := &lockTryingSession{writeMu: &a.writeMu}
	send := a.sessionPictureSender(checking)

	if err := send(context.Background(), tracker.Capture{Data: []byte("png"), Mime: "image/png"}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if !checking.wasLocked {
		t.Fatal("sessionPictureSender called SendRealtimeInput without holding a.writeMu — this is the race that panics with \"concurrent write to websocket connection\" when audioSendLoop writes at the same instant")
	}
}
