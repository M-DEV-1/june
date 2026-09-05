package ipc

import (
	"net/http"
)

// The desktop window is the daemon's child (see cmd/window.go), and the tray lives in the daemon, so a tray click reaches the window through the event stream the window is already reading rather than through a second channel of its own.

// windowActions are the things the tray may ask the window to do: open brings the main window to the front, toggle runs the hover's own show and hide.
var windowActions = map[string]bool{"open": true, "toggle": true}

// windowEventID is the id every window instruction carries. It is deliberately not of the "ask-N" shape, because no question caused it.
const windowEventID = "window"

// Window asks the desktop window to open or to toggle its hover. Input: a request whose "action" query parameter is one of the names in windowActions. Output: 204 once the instruction has been broadcast, or 400 naming the actions that exist. The window may not be running, and nothing here waits to find out: the instruction is sent and the caller carries on.
func (s *Server) Window(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if !windowActions[action] {
		http.Error(w, "unknown window action "+action+"; this daemon knows open and toggle", http.StatusBadRequest)
		return
	}
	s.hub.broadcast(Event{ID: windowEventID, Type: "window", Text: action, Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
	w.WriteHeader(http.StatusNoContent)
}
