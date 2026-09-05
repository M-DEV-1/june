package ipc

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A tray click reaches the window as an event on the stream it is already reading, so the daemon needs no second channel to the window it started.
func TestWindow_BroadcastsTheInstruction(t *testing.T) {
	s := New(&fakeAsker{}, newReadStore(t), nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	for _, action := range []string{"open", "toggle"} {
		rec := httptest.NewRecorder()
		s.Window(rec, httptest.NewRequest(http.MethodGet, "/window?action="+action, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d, want %d", action, rec.Code, http.StatusNoContent)
		}
		select {
		case ev := <-ch:
			if ev.Type != "window" || ev.Text != action || ev.ID != windowEventID {
				t.Errorf("%s: event = %+v", action, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: nothing was broadcast", action)
		}
	}
}

// An action the daemon does not know is refused with the ones it does, rather than broadcast for the window to puzzle over.
func TestWindow_RefusesAnUnknownAction(t *testing.T) {
	s := New(&fakeAsker{}, newReadStore(t), nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	for _, action := range []string{"", "quit", "OPEN"} {
		rec := httptest.NewRecorder()
		s.Window(rec, httptest.NewRequest(http.MethodGet, "/window?action="+action, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("action %q: status %d, want %d", action, rec.Code, http.StatusBadRequest)
		}
	}
	select {
	case ev := <-ch:
		t.Errorf("a refused action was broadcast anyway: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}
