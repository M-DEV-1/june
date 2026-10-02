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

	"june/internal/db/dbtest"
	"june/internal/proactive"
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
