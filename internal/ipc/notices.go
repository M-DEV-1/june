// notices.go holds POST /notices/{kind}/{id}/action: the window's own Done/1h/Evening/Tomorrow buttons on a live notice's rail line, answered through the exact same code the desktop notification's own buttons call.
package ipc

import (
	"context"
	"errors"
	"net/http"

	"ora/internal/proactive"
)

// noIDSegment stands in for a notice with no id of its own — a morning brief, say — since an empty {id} path segment is not a URL Go's own mux will route: it collapses the double slash and answers 301 to a path that no longer matches. The window's own POST sends this in place of "" (see api.ts's actOnNotice); NoticeAction resolves it back to "" before calling Act, so the id Act sees is exactly the id the notice itself carried.
const noIDSegment = "-"

// NoticeActor applies one notice button exactly as pressing it on the desktop notification would. Implemented by *proactive.Scheduler.Act — passed in rather than imported as a concrete type, the same way ipc.Settings and ipc.Usage take their dependencies as plain values, so this package states what it needs from the scheduler without depending on its whole type.
type NoticeActor func(ctx context.Context, kind, id, title, body, action string) error

// NoticeAction serves POST /notices/{kind}/{id}/action with act, the scheduler's own Act method in production. Body: {"title","body","action"}; action is "done", "hour", "evening" or "tomorrow" — anything else is 400. A "task" notice whose task Act reports gone (proactive.ErrTaskGone) is 404. Any other method is 405, a body that will not parse is 400, and any other failure from act is 500.
func NoticeAction(act NoticeActor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		kind := r.PathValue("kind")
		id := r.PathValue("id")
		if id == noIDSegment {
			id = ""
		}
		var req struct {
			Title  string `json:"title"`
			Body   string `json:"body"`
			Action string `json:"action"`
		}
		if !DecodeJSON(w, r, &req) {
			return
		}
		err := act(r.Context(), kind, id, req.Title, req.Body, req.Action)
		switch {
		case errors.Is(err, proactive.ErrBadNoticeAction):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, proactive.ErrTaskGone):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			fail(w, err, http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}
