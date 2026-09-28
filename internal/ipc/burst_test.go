package ipc

import (
	"testing"

	"june/internal/agent"
)

// One drawing is many events: the draw tool resolves each shape against the screen and broadcasts it on its own, up to agent.MaxDrawShapes of them, inside the same millisecond. The client's buffer has to hold that whole burst, or the overlay window is dropped part-way through and the drawing lands half-finished.
// A real voice session drew an octopus on 2026-09-07 as nine shapes; the buffer was eight, and the log recorded "dropping a client that is not keeping up, buffered: 8" twice at 06:08:48.7524 — the drop and the half-drawn octopus are the same event.
func TestHub_KeepsAClientThroughAWholeDrawing(t *testing.T) {
	h := newHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	for i := 0; i < agent.MaxDrawShapes; i++ {
		h.broadcast(Event{Type: "overlay", ID: "overlay"})
	}

	if got := h.clientCount(); got != 1 {
		t.Fatalf("clients = %d after one full drawing, want the overlay still subscribed", got)
	}
	if len(ch) != agent.MaxDrawShapes {
		t.Errorf("the client queued %d of %d shapes, so the drawing would land incomplete", len(ch), agent.MaxDrawShapes)
	}
}

// A client that has genuinely stalled is still dropped, so one dead listener cannot back up the others for ever.
func TestHub_StillDropsAClientThatHasStalled(t *testing.T) {
	h := newHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	for i := 0; i < clientBufferSize+1; i++ {
		h.broadcast(Event{Type: "answer", ID: "ask-1"})
	}
	if got := h.clientCount(); got != 0 {
		t.Errorf("clients = %d, want the stalled one dropped", got)
	}
}
