package ipc

import (
	"encoding/json"
	"testing"
	"time"
)

// Ora's own moments reach the desktop window as an event on the stream it is already reading, the same way a tray click does, so the card the window draws replaces GNOME's notification rather than sitting beside it.
func TestNotice_BroadcastsToEveryWindow(t *testing.T) {
	s := New(&fakeAsker{}, newReadStore(t), nil, nil)
	one := s.hub.subscribe()
	defer s.hub.unsubscribe(one)
	two := s.hub.subscribe()
	defer s.hub.unsubscribe(two)

	n := Notice{Title: "Morning brief", Body: "Two things are still open.", Place: "tasks", ID: "17", Kind: "brief"}
	s.Notice(n)

	for i, ch := range []chan Event{one, two} {
		select {
		case ev := <-ch:
			if ev.Type != "notice" || ev.ID != noticeEventID {
				t.Errorf("client %d: event = %+v", i, ev)
			}
			if ev.Notice == nil || *ev.Notice != n {
				t.Errorf("client %d: notice = %+v, want %+v", i, ev.Notice, n)
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: nothing was broadcast", i)
		}
	}
}

// The window reads the card straight off these field names, so they are pinned here: renaming one on this side stops the card drawing and nothing else would say so.
func TestNotice_JSONShape(t *testing.T) {
	s := New(&fakeAsker{}, newReadStore(t), nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Notice(Notice{Title: "Day's written down", Body: "A long day on the diary seam.", Place: "days", ID: "2026-09-05", Kind: "close"})
	ev := <-ch

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Type   string            `json:"type"`
		Notice map[string]string `json:"notice"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "notice" {
		t.Errorf("type = %q, want notice", got.Type)
	}
	want := map[string]string{"title": "Day's written down", "body": "A long day on the diary seam.", "place": "days", "id": "2026-09-05", "kind": "close"}
	for key, value := range want {
		if got.Notice[key] != value {
			t.Errorf("notice.%s = %q, want %q", key, got.Notice[key], value)
		}
	}
}

// An event carrying no notice must marshal exactly as it did before this file existed: the overlay page and the Rust side both parse every event off this stream, and an extra field on an ask's events is a change they never asked for.
func TestNotice_LeavesEveryOtherEventAlone(t *testing.T) {
	data, err := json.Marshal(Event{ID: "ask-1", Type: "status", Text: "Checking.", Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["notice"]; ok {
		t.Errorf("a status event carries a notice field: %s", data)
	}
}

// Subscribed is what decides whether a moment can be drawn by a window at all, so notify-send is the fallback only when nothing is there to draw it.
func TestSubscribed_AsksTheHubWhoIsListening(t *testing.T) {
	s := New(&fakeAsker{}, newReadStore(t), nil, nil)
	if s.Subscribed(time.Minute) {
		t.Error("a daemon no window has ever opened a stream on says one is subscribed")
	}

	ch := s.hub.subscribe()
	if !s.Subscribed(time.Minute) {
		t.Error("a window reading the stream right now does not count as subscribed")
	}

	s.hub.unsubscribe(ch)
	if !s.Subscribed(time.Minute) {
		t.Error("a window that has only just gone does not count for the minute after it left")
	}
	if s.Subscribed(0) {
		t.Error("a window that has gone still counts as subscribed with no window at all to wait in")
	}
}

// A notice the user has already dealt with from its own desktop notification comes back over the same event with action and until filled in, which is how the window learns to show "snoozed until 18:00" rather than drawing the card again. The window reads these two names, so they are pinned here.
func TestNotice_SnoozedJSONShape(t *testing.T) {
	s := New(&fakeAsker{}, newReadStore(t), nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Notice(Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42", Action: "snoozed", Until: "2026-09-05T18:00:00+05:30"})
	ev := <-ch

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Notice map[string]string `json:"notice"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Notice["action"] != "snoozed" || got.Notice["until"] != "2026-09-05T18:00:00+05:30" {
		t.Errorf("notice = %+v, want action snoozed and the moment it comes back", got.Notice)
	}
}
