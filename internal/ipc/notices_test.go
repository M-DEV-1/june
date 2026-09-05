package ipc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ora/internal/proactive"
)

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
