package ipc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postOverlay sends body to /overlay on a Server whose hub is being read, and returns the response status.
func postOverlay(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Overlay(rec, httptest.NewRequest(http.MethodPost, "/overlay", strings.NewReader(body)))
	return rec
}

// waitOverlay reads the next event off ch, failing if none arrives, and returns the overlay payload decoded from its Text.
func waitOverlay(t *testing.T, ch chan Event) (Event, OverlayRequest) {
	t.Helper()
	select {
	case ev := <-ch:
		var got OverlayRequest
		if err := json.Unmarshal([]byte(ev.Text), &got); err != nil {
			t.Fatalf("overlay text is not JSON: %v (%q)", err, ev.Text)
		}
		return ev, got
	case <-time.After(time.Second):
		t.Fatal("no overlay event broadcast")
	}
	return Event{}, OverlayRequest{}
}

// TestOverlay_KindFieldMapping is one table over every kind POST /overlay accepts, one row each,
// checking that kind's own field mapping into the broadcast payload: ring's rects and its ttl_ms
// cap, clear needing no rects and defaulting its ttl, arrow and line's points, path's three-point
// minimum, and box/circle's rects.
func TestOverlay_KindFieldMapping(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, got OverlayRequest)
	}{
		{
			"ring broadcasts its rect, label and a capped ttl", `{"kind":"ring","label":"here","rects":[{"x":10,"y":20,"w":30,"h":40,"label":"box"}],"ttl_ms":99000}`,
			func(t *testing.T, got OverlayRequest) {
				if got.Kind != "ring" || got.Label != "here" {
					t.Fatalf("payload = %+v, want kind ring label here", got)
				}
				if got.TTLMs != maxOverlayTTLMs {
					t.Fatalf("ttl_ms = %d, want it capped at %d", got.TTLMs, maxOverlayTTLMs)
				}
				if len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40, Label: "box"}) {
					t.Fatalf("rects = %+v, want the one posted", got.Rects)
				}
			},
		},
		{
			"clear needs no rects and defaults its ttl", `{"kind":"clear"}`,
			func(t *testing.T, got OverlayRequest) {
				if got.Kind != "clear" || len(got.Rects) != 0 {
					t.Fatalf("payload = %+v, want an empty clear", got)
				}
				if got.TTLMs != defaultOverlayTTLMs {
					t.Fatalf("ttl_ms = %d, want the default %d", got.TTLMs, defaultOverlayTTLMs)
				}
			},
		},
		{
			"arrow broadcasts its points", `{"kind":"arrow","label":"press here","points":[[10,20],[30,40],[50,60]]}`,
			checkOverlayPoints("arrow", "press here", [][2]int{{10, 20}, {30, 40}, {50, 60}}),
		},
		{
			"line broadcasts its points", `{"kind":"line","label":"press here","points":[[10,20],[30,40],[50,60]]}`,
			checkOverlayPoints("line", "press here", [][2]int{{10, 20}, {30, 40}, {50, 60}}),
		},
		{
			"path through three points", `{"kind":"path","points":[[1,1],[2,2],[3,3]]}`,
			func(t *testing.T, got OverlayRequest) {
				if got.Kind != "path" || len(got.Points) != 3 {
					t.Fatalf("payload = %+v, want kind path through 3 points", got)
				}
			},
		},
		{
			"tap broadcasts its one point", `{"kind":"tap","label":"Send","points":[[40,50]]}`,
			func(t *testing.T, got OverlayRequest) {
				if got.Kind != "tap" || len(got.Points) != 1 || got.Points[0] != [2]int{40, 50} {
					t.Fatalf("tap payload = %+v, want kind tap at one point", got)
				}
			},
		},
		{
			"box broadcasts its rect", `{"kind":"box","label":"drop here","rects":[{"x":1,"y":2,"w":3,"h":4}]}`,
			func(t *testing.T, got OverlayRequest) {
				if got.Kind != "box" || len(got.Rects) != 1 {
					t.Fatalf("box payload = %+v, want one rect", got)
				}
			},
		},
		{
			"circle broadcasts its rect", `{"kind":"circle","label":"click here","rects":[{"x":1,"y":2,"w":3,"h":4}]}`,
			func(t *testing.T, got OverlayRequest) {
				if got.Kind != "circle" || len(got.Rects) != 1 {
					t.Fatalf("circle payload = %+v, want one rect", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&fakeAsker{}, nil, nil, nil)
			ch := s.hub.subscribe()
			defer s.hub.unsubscribe(ch)

			rec := postOverlay(t, s, tc.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
			}
			ev, got := waitOverlay(t, ch)
			if ev.Type != "overlay" {
				t.Fatalf("event type = %q, want overlay", ev.Type)
			}
			tc.check(t, got)
		})
	}
}

// checkOverlayPoints builds the check func an arrow/line row of TestOverlay_KindFieldMapping shares: the payload's kind, label and points must match what was posted.
func checkOverlayPoints(kind, label string, points [][2]int) func(t *testing.T, got OverlayRequest) {
	return func(t *testing.T, got OverlayRequest) {
		if got.Kind != kind || got.Label != label {
			t.Fatalf("payload = %+v, want kind %s label %q", got, kind, label)
		}
		if len(got.Points) != len(points) {
			t.Fatalf("points = %+v, want %+v", got.Points, points)
		}
		for i := range points {
			if got.Points[i] != points[i] {
				t.Fatalf("points = %+v, want %+v", got.Points, points)
			}
		}
	}
}

// A 202 with no body said only that the request validated, so a drawing broadcast to nobody looked exactly like one that reached the screen. The response now says which of the two happened, because that is the one fact a caller cannot find out any other way.
func TestOverlay_ReportsWhetherAnyoneWasListening(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)

	rec := postOverlay(t, s, `{"kind":"ring","rects":[{"x":1,"y":2,"w":3,"h":4}]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	var quiet OverlayResult
	if err := json.Unmarshal(rec.Body.Bytes(), &quiet); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if quiet.Drawn {
		t.Errorf("drawn = true with no client on the hub, want false")
	}
	if quiet.Reason == "" {
		t.Errorf("reason is empty, want a sentence naming why nothing was drawn")
	}

	// A stream reader that is not the drawing layer (the main window, a curl) receives the drawing and puts nothing on the screen.
	window := s.hub.subscribe()
	defer s.hub.unsubscribe(window)
	rec = postOverlay(t, s, `{"kind":"ring","rects":[{"x":1,"y":2,"w":3,"h":4}]}`)
	var unseen OverlayResult
	if err := json.Unmarshal(rec.Body.Bytes(), &unseen); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if unseen.Drawn {
		t.Errorf("drawn = true with only a non-overlay client on the hub, want false")
	}
	waitOverlay(t, window)

	ch := s.hub.subscribeAs(overlayRole)
	defer s.hub.unsubscribe(ch)
	rec = postOverlay(t, s, `{"kind":"ring","rects":[{"x":1,"y":2,"w":3,"h":4}]}`)
	var heard OverlayResult
	if err := json.Unmarshal(rec.Body.Bytes(), &heard); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if !heard.Drawn {
		t.Errorf("drawn = false with a client on the hub, want true (reason %q)", heard.Reason)
	}
	if heard.Reason != "" {
		t.Errorf("reason = %q, want empty when the drawing went out", heard.Reason)
	}
	waitOverlay(t, ch)
}
