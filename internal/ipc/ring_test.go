package ipc

import "testing"

// Ring is how the agent's point_at tool reaches the screen: one ring around one rectangle, broadcast on the hub exactly like a POST /overlay ring so the extension needs no second path, stamped with the id of the ask whose point_at drew it so a client reading /events can tell which question produced the ring.
func TestRing_BroadcastsARingOverlay(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Ring("ask-7", 10, 20, 30, 40, "here")

	ev, got := waitOverlay(t, ch)
	if ev.Type != "overlay" {
		t.Errorf("event type = %q, want overlay", ev.Type)
	}
	if ev.ID != "ask-7" {
		t.Errorf("event id = %q, want ask-7, the ask whose point_at drew the ring", ev.ID)
	}
	if got.Kind != "ring" || got.Label != "here" || len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
		t.Errorf("overlay = %+v, want a ring labelled here around 10,20 30x40", got)
	}
	if got.TTLMs != defaultOverlayTTLMs {
		t.Errorf("ttl = %d, want the default %d", got.TTLMs, defaultOverlayTTLMs)
	}
}

// Marks is how the agent's show_marks tool reaches the screen: a numbered mark over every rect given, broadcast on the hub exactly like a POST /overlay marks request so the extension needs no second path, stamped with the ask whose show_marks drew them.
func TestMarks_BroadcastsAMarksOverlay(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	rects := []OverlayRect{{X: 1, Y: 2, W: 3, H: 4, Label: "1"}, {X: 5, Y: 6, W: 7, H: 8, Label: "2"}}
	s.Marks("ask-7", rects)

	ev, got := waitOverlay(t, ch)
	if ev.Type != "overlay" {
		t.Errorf("event type = %q, want overlay", ev.Type)
	}
	if ev.ID != "ask-7" {
		t.Errorf("event id = %q, want ask-7, the ask whose show_marks drew the marks", ev.ID)
	}
	if got.Kind != "marks" || len(got.Rects) != 2 || got.Rects[0] != rects[0] || got.Rects[1] != rects[1] {
		t.Errorf("overlay = %+v, want marks over the given rects", got)
	}
	if got.TTLMs != defaultOverlayTTLMs {
		t.Errorf("ttl = %d, want the default %d", got.TTLMs, defaultOverlayTTLMs)
	}
}

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
