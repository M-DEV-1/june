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

// TestNotice broadcasts a Notice and checks every field the window reads off it, in one call: the
// event envelope (type, id), every field of the notice payload itself, and that it reaches every
// subscriber — not just the first. Renaming a field on this side stops the card drawing and nothing
// else would say so, which is why the whole payload is decoded and checked here rather than in pieces.
func TestNotice(t *testing.T) {
	s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
	one := s.hub.subscribe()
	defer s.hub.unsubscribe(one)
	two := s.hub.subscribe()
	defer s.hub.unsubscribe(two)

	n := Notice{Title: "Day's written down", Body: "A long day on the diary seam.", Place: "days", ID: "2026-09-05", Kind: "close"}
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
				t.Errorf("client %d: type = %q, want notice", i, got.Type)
			}
			want := map[string]string{"title": "Day's written down", "body": "A long day on the diary seam.", "place": "days", "id": "2026-09-05", "kind": "close"}
			for key, value := range want {
				if got.Notice[key] != value {
					t.Errorf("client %d: notice.%s = %q, want %q", i, key, got.Notice[key], value)
				}
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: nothing was broadcast", i)
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

// TestNotice_FieldShapes covers the notice fields TestNotice's plain case never sets: the
// snoozed-until pair a desktop notification's own action reports back with, the buttons a
// notice asking its own question carries, and the absence of an actions field when it has none
// (every other card reads its buttons from its kind, so an empty list would draw a rail with
// nothing on it). One row per shape, each decoding the broadcast event and checking its own fields.
func TestNotice_FieldShapes(t *testing.T) {
	cases := []struct {
		name   string
		notice Notice
		check  func(t *testing.T, raw []byte)
	}{
		{
			"snoozed carries action and until",
			Notice{Title: "Still open", Body: "Send the invoice", Kind: "task", ID: "42", Action: "snoozed", Until: "2026-09-05T18:00:00+05:30"},
			func(t *testing.T, raw []byte) {
				var got struct {
					Notice map[string]string `json:"notice"`
				}
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if got.Notice["action"] != "snoozed" || got.Notice["until"] != "2026-09-05T18:00:00+05:30" {
					t.Errorf("notice = %+v, want action snoozed and the moment it comes back", got.Notice)
				}
			},
		},
		{
			"a notice with its own question carries its own buttons",
			Notice{Title: "Still open", Body: "Me \u2014 settle the payment.", Place: "tasks", ID: "42", Kind: "stale", Actions: []NoticeButton{{Key: "done", Label: "Done"}, {Key: "dropped", Label: "Not happening"}}},
			func(t *testing.T, raw []byte) {
				var got struct {
					Notice struct {
						Kind    string              `json:"kind"`
						Actions []map[string]string `json:"actions"`
					} `json:"notice"`
				}
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				want := []map[string]string{{"key": "done", "label": "Done"}, {"key": "dropped", "label": "Not happening"}}
				if got.Notice.Kind != "stale" || !reflect.DeepEqual(got.Notice.Actions, want) {
					t.Errorf("notice.actions = %+v, want %+v", got.Notice.Actions, want)
				}
			},
		},
		{
			"a notice with no buttons of its own has no actions field",
			Notice{Title: "Morning brief", Body: "Two things are still open.", Kind: "brief"},
			func(t *testing.T, raw []byte) {
				var got struct {
					Notice map[string]json.RawMessage `json:"notice"`
				}
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if _, ok := got.Notice["actions"]; ok {
					t.Errorf("a notice with no buttons of its own carries an actions field: %s", raw)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&fakeAsker{}, dbtest.Open(t), nil, nil)
			ch := s.hub.subscribe()
			defer s.hub.unsubscribe(ch)

			s.Notice(tc.notice)
			ev := <-ch

			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			tc.check(t, raw)
		})
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

// TestNoticeAction covers POST /notices/{kind}/{id}/action end to end: the happy path (the body
// decodes and every field reaches Act unchanged, including the "-" no-id placeholder resolving to
// ""), and every way it can fail (a button Act never offered, a task already gone, any other
// failure, the wrong method, a body that will not parse) mapped to its own status code without
// ever reaching Act when the request itself was the problem.
func TestNoticeAction(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		err    error
		want   int
		check  func(t *testing.T, f *noticeAct)
	}{
		{
			name: "applies the action", path: "/notices/task/42/action",
			body: `{"title":"Still open","body":"Send the invoice","action":"hour"}`, want: http.StatusOK,
			check: func(t *testing.T, f *noticeAct) {
				if !f.called || f.kind != "task" || f.id != "42" || f.title != "Still open" || f.body != "Send the invoice" || f.action != "hour" {
					t.Errorf("Act called with %+v, want kind=task id=42 title=\"Still open\" body=\"Send the invoice\" action=hour", f)
				}
			},
		},
		{
			name: "no-id placeholder resolves to empty", path: "/notices/brief/-/action",
			body: `{"title":"Morning brief","body":"Two things today","action":"done"}`, want: http.StatusOK,
			check: func(t *testing.T, f *noticeAct) {
				if f.id != "" {
					t.Errorf("id = %q, want the placeholder resolved to empty", f.id)
				}
			},
		},
		{
			name: "a button Act refuses is 400, not 500", path: "/notices/task/42/action",
			body: `{"action":"never-mind"}`, err: proactive.ErrBadNoticeAction, want: http.StatusBadRequest,
		},
		{
			name: "a task already gone is 404", path: "/notices/task/42/action",
			body: `{"action":"done"}`, err: fmt.Errorf("closing task 42: %w", proactive.ErrTaskGone), want: http.StatusNotFound,
		},
		{
			name: "any other failure is 500", path: "/notices/task/42/action",
			body: `{"action":"done"}`, err: fmt.Errorf("store is busy"), want: http.StatusInternalServerError,
		},
		{
			name: "a body that will not parse is 400 and never reaches Act", path: "/notices/task/42/action",
			body: `not json`, want: http.StatusBadRequest,
			check: func(t *testing.T, f *noticeAct) {
				if f.called {
					t.Error("Act was called for a body that never parsed")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &noticeAct{err: tc.err}
			srv := newNoticeActionServer(t, f)
			if code := postJSON(t, srv, tc.path, tc.body, nil); code != tc.want {
				t.Fatalf("status = %d, want %d", code, tc.want)
			}
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}

	// GET is refused with 405, matching every other action-only route in this package, and never reaches Act.
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
