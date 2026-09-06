package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"ora/internal/db/dbtest"
	"ora/internal/proactive"
)

// Ora's own moments reach the desktop window as an event on the stream it is already reading, the same way a tray click does, so the card the window draws replaces GNOME's notification rather than sitting beside it.
func TestNotice_BroadcastsToEveryWindow(t *testing.T) {
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
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
			if ev.Notice == nil || !reflect.DeepEqual(*ev.Notice, n) {
				t.Errorf("client %d: notice = %+v, want %+v", i, ev.Notice, n)
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: nothing was broadcast", i)
		}
	}
}

// The window reads the card straight off these field names, so they are pinned here: renaming one on this side stops the card drawing and nothing else would say so.
func TestNotice_JSONShape(t *testing.T) {
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
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
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
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
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
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

// A notice that asks its own question carries its own buttons, and the window draws them straight off these names. The stale-item question is the first one: "Done", "Not happening", "Not urgent" are none of the five a desktop notification offers, so the card has to be told what to draw.
func TestNotice_ActionsJSONShape(t *testing.T) {
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Notice(Notice{Title: "Still open", Body: "Me \u2014 settle the payment.", Place: "tasks", ID: "42", Kind: "stale", Actions: []NoticeButton{{Key: "done", Label: "Done"}, {Key: "dropped", Label: "Not happening"}}})
	ev := <-ch

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Notice struct {
			Kind    string              `json:"kind"`
			Actions []map[string]string `json:"actions"`
		} `json:"notice"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []map[string]string{{"key": "done", "label": "Done"}, {"key": "dropped", "label": "Not happening"}}
	if got.Notice.Kind != "stale" || !reflect.DeepEqual(got.Notice.Actions, want) {
		t.Errorf("notice.actions = %+v, want %+v", got.Notice.Actions, want)
	}
}

// A notice with no buttons of its own must not grow an actions field: every other card reads its buttons from its kind, and an empty list would have the window draw a rail with nothing on it.
func TestNotice_NoActionsFieldWhenThereAreNone(t *testing.T) {
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Notice(Notice{Title: "Morning brief", Body: "Two things are still open.", Kind: "brief"})
	ev := <-ch

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Notice map[string]json.RawMessage `json:"notice"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.Notice["actions"]; ok {
		t.Errorf("a notice with no buttons of its own carries an actions field: %s", data)
	}
}

// noticeAct records one call and answers with whatever the test set it to answer, standing in for *proactive.Scheduler.Act.
type noticeAct struct {
	kind, id, title, body, action string
	called                        bool
	err                           error
}

func (f *noticeAct) act(_ context.Context, kind, id, title, body, action string) error {
	f.called = true
	f.kind, f.id, f.title, f.body, f.action = kind, id, title, body, action
	return f.err
}

// newNoticeActionServer wires NoticeAction behind a real HTTP server under the same pattern cmd/daemon.go gives it, so {kind} and {id} resolve exactly as they do in the daemon.
func newNoticeActionServer(t *testing.T, f *noticeAct) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notices/{kind}/{id}/action", NoticeAction(f.act))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestNoticeAction_AppliesTheAction checks the route decodes the body and forwards kind, id, title, body and action to Act unchanged, then answers 200 on success.
func TestNoticeAction_AppliesTheAction(t *testing.T) {
	f := &noticeAct{}
	srv := newNoticeActionServer(t, f)

	code := postJSON(t, srv, "/notices/task/42/action", `{"title":"Still open","body":"Send the invoice","action":"hour"}`, nil)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !f.called || f.kind != "task" || f.id != "42" || f.title != "Still open" || f.body != "Send the invoice" || f.action != "hour" {
		t.Errorf("Act called with %+v, want kind=task id=42 title=\"Still open\" body=\"Send the invoice\" action=hour", f)
	}
}

// TestNoticeAction_NoIDPlaceholder checks the "-" path segment a notice with no id (a morning brief, say) is sent under is passed to Act as "", not as the literal placeholder.
func TestNoticeAction_NoIDPlaceholder(t *testing.T) {
	f := &noticeAct{}
	srv := newNoticeActionServer(t, f)

	code := postJSON(t, srv, "/notices/brief/-/action", `{"title":"Morning brief","body":"Two things today","action":"done"}`, nil)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if f.id != "" {
		t.Errorf("id = %q, want the placeholder resolved to empty", f.id)
	}
}

// TestNoticeAction_BadAction checks an action Act refuses as ErrBadNoticeAction is answered 400, not 500 — the button never existed rather than something failing.
func TestNoticeAction_BadAction(t *testing.T) {
	f := &noticeAct{err: proactive.ErrBadNoticeAction}
	srv := newNoticeActionServer(t, f)

	code := postJSON(t, srv, "/notices/task/42/action", `{"action":"never-mind"}`, nil)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestNoticeAction_TaskGone checks a task Act reports gone (ErrTaskGone, wrapped the way SetTaskDone's own loopback call wraps a 404) is answered 404, so Done on a notice for a task already closed elsewhere does not read as a server failure.
func TestNoticeAction_TaskGone(t *testing.T) {
	f := &noticeAct{err: fmt.Errorf("closing task 42: %w", proactive.ErrTaskGone)}
	srv := newNoticeActionServer(t, f)

	code := postJSON(t, srv, "/notices/task/42/action", `{"action":"done"}`, nil)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// TestNoticeAction_OtherFailure checks any other error Act returns is a plain 500, same as every other route here.
func TestNoticeAction_OtherFailure(t *testing.T) {
	f := &noticeAct{err: fmt.Errorf("store is busy")}
	srv := newNoticeActionServer(t, f)

	code := postJSON(t, srv, "/notices/task/42/action", `{"action":"done"}`, nil)

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}

// TestNoticeAction_MethodNotAllowed checks GET on the route is 405, matching every other action-only route in this package.
func TestNoticeAction_MethodNotAllowed(t *testing.T) {
	f := &noticeAct{}
	srv := newNoticeActionServer(t, f)

	resp, err := http.Get(srv.URL + "/notices/task/42/action")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if f.called {
		t.Error("Act was called for a method the route refuses")
	}
}

// TestNoticeAction_BadBody checks a body that will not parse as JSON is 400, and never reaches Act.
func TestNoticeAction_BadBody(t *testing.T) {
	f := &noticeAct{}
	srv := newNoticeActionServer(t, f)

	code := postJSON(t, srv, "/notices/task/42/action", `not json`, nil)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if f.called {
		t.Error("Act was called for a body that never parsed")
	}
}

