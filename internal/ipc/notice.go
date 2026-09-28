package ipc

import (
	"context"
	"errors"
	"net/http"
	"time"

	"ora/internal/proactive"
)

// A notice is Ora speaking first: the morning brief, the evening close, a meeting prep. It reaches the desktop window as an event on the stream that window is already reading, and the window draws it as its own card. Nothing here waits for the window, and nothing here knows whether it drew anything — see Subscribed for what the caller asks before choosing this over the desktop's own notifications.

// Notice is what one "notice" event carries. Title is the card's bold first line and Body the few lines under it. Place and ID say what a click on the card opens: Place names one of the app window's own screens ("chats", "tasks", "days") and ID the row to select there, both empty when the notice points at nothing in particular and a click should just open the window. Kind names the moment it came from — "brief", "close", "meeting", "day", "task", "routine" — so the window can tell one apart from another without reading the title.
//
// Actions is the notice's own buttons, and it is empty for all but a notice that asked a question of its own. A card with no actions draws the buttons its kind implies (Done and the snoozes); one with actions draws exactly these, labels and all, and POSTs the pressed one's Key as the action on /notices/{kind}/{id}/action — which is how the stale-item question's "Not happening" and "Not urgent", neither of them a key a desktop notification offers, reach the goroutine waiting on the answer.
//
// Action and Until are empty on a notice arriving for the first time, and filled in when the user has since dealt with it from the desktop notification it was also posted as: Action is "snoozed" or "done", and Until is the RFC 3339 moment a snoozed notice comes back, so the window can show "snoozed until 18:00" against the card it already drew instead of drawing it again. Kind and ID say which card that is.
type Notice struct {
	Title   string         `json:"title"`
	Body    string         `json:"body"`
	Place   string         `json:"place"`
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Action  string         `json:"action"`
	Until   string         `json:"until"`
	Actions []NoticeButton `json:"actions,omitempty"`
	// Expires is the RFC 3339 moment the notice's own buttons stop working, empty on a notice that asked no question. The window counts down to it and removes the card at zero.
	Expires string `json:"expires,omitempty"`
}

// NoticeButton is one button a notice carries. Key is what the window POSTs back as the action, and Label is what the user reads on the button. Named for the button rather than the action because NoticeAction is already the route handler above.
type NoticeButton struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// noticeEventID is the id every notice event carries. Like windowEventID it is deliberately not of the "ask-N" shape, because no question caused it: the window matches an ask's events against the id it is waiting on, and a notice must never be mistaken for one of those.
const noticeEventID = "notice"

// Notice broadcasts one notice to every window reading the event stream. Input: the notice. Output: nothing; a notice sent while nothing is listening is simply lost, which is what Subscribed exists to prevent.
func (s *Server) Notice(n Notice) {
	s.hub.broadcast(Event{ID: noticeEventID, Type: "notice", Notice: &n, Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
}

// Subscribed reports whether a window is there to draw a notice. Input: how recently a window must have been reading the event stream to count. Output: true when one is reading it right now, or when the last one joined or left inside that window — a window being reloaded is gone for a moment and should not push the notice it was about to receive onto the desktop's own notifications instead.
func (s *Server) Subscribed(within time.Duration) bool {
	return s.hub.subscribedWithin(within, time.Now())
}

// subscribedWithin is Subscribed's answer, taken from the hub's own record of who is connected rather than from a guess. Input: how recently a client must have been here, and the current time. Output: true when a client is connected now or the last join or departure was less than that long ago.
func (h *hub) subscribedWithin(within time.Duration, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) > 0 {
		return true
	}
	return !h.lastSeen.IsZero() && now.Sub(h.lastSeen) < within
}

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
