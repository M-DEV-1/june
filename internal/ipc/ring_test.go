package ipc

import "testing"

// Ring and Marks (how the agent's point_at and show_marks tools reach the screen) have their field
// mapping and ask-id stamping checked as rows of TestOverlayMethods_BroadcastFieldMapping in
// overlay_test.go, alongside every other shape the agent's drawing tools call directly.

// A drawing made with no id — the direct /overlay route, or a voice session, which no question is behind — carries the non-ask id rather than a blank one, so every overlay event on the stream has something in its id field.
func TestDraw_NoAskGivesTheNonAskID(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Ring("", 1, 2, 3, 4, "")

	ev, _ := waitOverlay(t, ch)
	if ev.ID != overlayNoAsk {
		t.Errorf("event id = %q, want %q", ev.ID, overlayNoAsk)
	}
}

// DrawingAsk is what cmd/daemon.go stamps a drawing with, because the one shared agent it wires point_at and show_marks to carries no ask id of its own. While exactly one ask is running that ask drew it; with none running, or with two whose drawings cannot be told apart, the drawing belongs to no ask and says so.
func TestDrawingAsk_NamesTheOneAskRunning(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)

	if got := s.DrawingAsk(); got != overlayNoAsk {
		t.Errorf("with no ask running, DrawingAsk = %q, want %q", got, overlayNoAsk)
	}

	s.startAsk("ask-1")
	if got := s.DrawingAsk(); got != "ask-1" {
		t.Errorf("with one ask running, DrawingAsk = %q, want ask-1", got)
	}

	s.startAsk("ask-2")
	if got := s.DrawingAsk(); got != overlayNoAsk {
		t.Errorf("with two asks running, DrawingAsk = %q, want %q: neither can be shown to have drawn it", got, overlayNoAsk)
	}

	s.endAsk("ask-2")
	if got := s.DrawingAsk(); got != "ask-1" {
		t.Errorf("after the second ask finished, DrawingAsk = %q, want ask-1", got)
	}

	s.endAsk("ask-1")
	if got := s.DrawingAsk(); got != overlayNoAsk {
		t.Errorf("after every ask finished, DrawingAsk = %q, want %q", got, overlayNoAsk)
	}
}
