// overlay.go holds POST /overlay: the daemon telling the GNOME Shell extension to draw on the screen — a ring around one rectangle, numbered marks on several, or a clear.
package ipc

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// defaultOverlayTTLMs is how long a drawing stays up when the caller names no ttl.
const defaultOverlayTTLMs = 3000

// maxOverlayTTLMs is the longest a drawing may stay up; a larger ttl is capped to this so a bad caller cannot leave the screen marked for ever.
const maxOverlayTTLMs = 15000

// overlayTTLFromInk is the ttl the five shapes the draw tool dispatches to send instead of a number of their own: it leaves the reading time to the overlay, which counts the ink actually on screen and gives a ten-shape diagram longer to be read than a single box. A drawing is several shapes that arrive one after another and keep the ones before them, so only the overlay knows how much is standing there by the time the last one lands. A caller that names its own ttl through POST /overlay still gets exactly that, since the handler fills a zero in with defaultOverlayTTLMs before broadcasting; zero therefore reaches the overlay only from a draw.
const overlayTTLFromInk = 0

// overlayNoAsk is the id an overlay event carries when no question drew it: a POST /overlay straight from another program, or a voice session's own ring. It is deliberately not of the "ask-N" shape newID mints, so a client reading /events can see at once that there is no question to trace this drawing back to, instead of being handed an id that names an ask it never saw.
const overlayNoAsk = "overlay"

// OverlayRect is one rectangle in screen coordinates, with the label drawn beside it.
type OverlayRect struct {
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Label string `json:"label"`
}

// OverlayRequest is the body of POST /overlay and, once validated, exactly what the extension receives as the text of the overlay event. Kind is "ring" (a rounded rectangle with a short label), "marks" (a red numbered circle per rectangle), "arrow" or "line" (a path through Points, which the extension smooths and animates, with an arrowhead for "arrow" and without one for "line"), "path" (a free-form smooth stroke through at least three Points, no arrowhead), "box" (a dashed rectangle per Rect, to mark a region), "circle" (a dashed circle inscribed in one Rect, to spotlight one control) or "clear" (erase whatever is drawn).
type OverlayRequest struct {
	Kind   string        `json:"kind"`
	Label  string        `json:"label"`
	Rects  []OverlayRect `json:"rects"`
	Points [][2]int      `json:"points"`
	TTLMs  int           `json:"ttl_ms"`
}

// Overlay handles POST /overlay. Input: an OverlayRequest as JSON. Output: 202 and no body once the request has been broadcast on the hub as an event of type "overlay" whose text is the validated request re-encoded as JSON, or 400 when the body is not JSON, names a kind other than ring, marks, arrow, line, path, box, circle or clear, carries no rectangles for a ring, marks or box, carries a rectangle count other than one for a circle, carries a rectangle with a width or height that is not positive, carries fewer than two points for an arrow or line, carries fewer than three points for a path, or carries a point with a negative coordinate. A ttl above maxOverlayTTLMs is capped rather than refused, and a missing one becomes defaultOverlayTTLMs.
func (s *Server) Overlay(w http.ResponseWriter, r *http.Request) {
	var req OverlayRequest
	if !DecodeJSON(w, r, &req) {
		return
	}

	switch req.Kind {
	case "ring", "marks", "box":
		if len(req.Rects) == 0 {
			http.Error(w, "kind "+req.Kind+" needs at least one rect", http.StatusBadRequest)
			return
		}
	case "circle":
		if len(req.Rects) != 1 {
			http.Error(w, "kind circle needs exactly one rect", http.StatusBadRequest)
			return
		}
	case "arrow", "line":
		if len(req.Points) < 2 {
			http.Error(w, "kind "+req.Kind+" needs at least two points", http.StatusBadRequest)
			return
		}
	case "path":
		if len(req.Points) < 3 {
			http.Error(w, "kind path needs at least three points", http.StatusBadRequest)
			return
		}
	case "clear":
		req.Rects = nil
		req.Points = nil
	default:
		http.Error(w, "kind must be ring, marks, arrow, line, path, box, circle or clear", http.StatusBadRequest)
		return
	}

	for _, rect := range req.Rects {
		if rect.W <= 0 || rect.H <= 0 {
			http.Error(w, "every rect needs a positive w and h", http.StatusBadRequest)
			return
		}
	}

	for _, p := range req.Points {
		if p[0] < 0 || p[1] < 0 {
			http.Error(w, "every point needs non-negative coordinates", http.StatusBadRequest)
			return
		}
	}

	if req.TTLMs <= 0 {
		req.TTLMs = defaultOverlayTTLMs
	}
	if req.TTLMs > maxOverlayTTLMs {
		req.TTLMs = maxOverlayTTLMs
	}

	s.draw(overlayNoAsk, req)
	w.WriteHeader(http.StatusAccepted)
}

// Ring draws one ring around a rectangle on the screen for the default time, the way POST /overlay would. Input: the id of the ask whose point_at asked for the ring (empty, or overlayNoAsk, when no ask did — see DrawingAsk), the rectangle in screen pixels and the label drawn beside it. Output: none; the drawing is broadcast on the hub for the extension, stamped with that ask's id.
func (s *Server) Ring(askID string, x, y, w, h int, label string) {
	s.draw(askID, OverlayRequest{Kind: "ring", Label: label, Rects: []OverlayRect{{X: x, Y: y, W: w, H: h}}, TTLMs: defaultOverlayTTLMs})
}

// Marks draws a numbered mark over every rect for the default time, the way POST /overlay with kind marks would. Input: the id of the ask whose show_marks asked for them (empty, or overlayNoAsk, when no ask did) and the rectangles in screen pixels, in the order they should be numbered. Output: none; the drawing is broadcast on the hub for the extension, stamped with that ask's id.
func (s *Server) Marks(askID string, rects []OverlayRect) {
	s.draw(askID, OverlayRequest{Kind: "marks", Rects: rects, TTLMs: defaultOverlayTTLMs})
}

// Arrow draws an arrowed path through the given points for as long as the overlay judges its ink needs, the way POST /overlay with kind arrow would. Input: the id of the ask whose draw asked for it (empty, or overlayNoAsk, when no ask did — see DrawingAsk), the points in screen pixels (at least two, in the order the arrow should follow) and the label drawn beside it. Output: none; the drawing is broadcast on the hub for the extension, stamped with that ask's id.
func (s *Server) Arrow(askID string, points [][2]int, label string) {
	s.draw(askID, OverlayRequest{Kind: "arrow", Label: label, Points: points, TTLMs: overlayTTLFromInk})
}

// Line draws a plain path through the given points for as long as the overlay judges its ink needs, the way POST /overlay with kind line would. Input and output are as Arrow, but the extension draws it with no arrowhead.
func (s *Server) Line(askID string, points [][2]int, label string) {
	s.draw(askID, OverlayRequest{Kind: "line", Label: label, Points: points, TTLMs: overlayTTLFromInk})
}

// Path draws a free-form smooth stroke through the given points (at least three) with no arrowhead, for as long as the overlay judges its ink needs, the way POST /overlay with kind path would. Input and output are as Arrow.
func (s *Server) Path(askID string, points [][2]int, label string) {
	s.draw(askID, OverlayRequest{Kind: "path", Label: label, Points: points, TTLMs: overlayTTLFromInk})
}

// Box draws a dashed rectangle around a region for as long as the overlay judges its ink needs, the way POST /overlay with kind box would. Input: the id of the ask, the rectangle in screen pixels and the label drawn beside it. Output: none; the drawing is broadcast on the hub for the extension, stamped with that ask's id.
func (s *Server) Box(askID string, x, y, w, h int, label string) {
	s.draw(askID, OverlayRequest{Kind: "box", Label: label, Rects: []OverlayRect{{X: x, Y: y, W: w, H: h}}, TTLMs: overlayTTLFromInk})
}

// Circle draws a dashed circle inscribed in a rectangle for the default time, the way POST /overlay with kind circle would. Input and output are as Box; the extension draws the circle inscribed in the given rectangle rather than the rectangle itself.
func (s *Server) Circle(askID string, x, y, w, h int, label string) {
	s.draw(askID, OverlayRequest{Kind: "circle", Label: label, Rects: []OverlayRect{{X: x, Y: y, W: w, H: h}}, TTLMs: overlayTTLFromInk})
}

// Draw dispatches to Arrow, Line, Path, Box or Circle by shape, for the agent's draw tool to call directly without knowing which ask is running. Input: the shape, the points to draw through (arrow, line, path — ignored otherwise) and the rectangle to draw around or inscribe within (box, circle — ignored otherwise), and the label. Output: nil once broadcast under the ask DrawingAsk names, or an error naming the bad shape when it is none of the five.
func (s *Server) Draw(shape string, points [][2]int, x, y, w, h int, label string) error {
	askID := s.DrawingAsk()
	switch shape {
	case "arrow":
		s.Arrow(askID, points, label)
	case "line":
		s.Line(askID, points, label)
	case "path":
		s.Path(askID, points, label)
	case "box":
		s.Box(askID, x, y, w, h, label)
	case "circle":
		s.Circle(askID, x, y, w, h, label)
	default:
		return fmt.Errorf("shape must be arrow, line, path, box or circle, got %q", shape)
	}
	return nil
}

// draw broadcasts a validated overlay request as an event of type "overlay" whose text is the request as JSON, under the id of the ask that drew it. Input: that ask's id, and the request. An empty id becomes overlayNoAsk, so every overlay event has an id and no drawing is ever given one of the ask ids newID hands out.
func (s *Server) draw(askID string, req OverlayRequest) {
	if askID == "" {
		askID = overlayNoAsk
	}
	body, _ := json.Marshal(req)
	s.hub.broadcast(Event{ID: askID, Type: "overlay", Text: string(body), Evidence: []EvidenceItem{}, Actions: []ActionItem{}})
}
