package ipc

import (
	"encoding/json"
	"errors"
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

// TestOverlayMethods_BroadcastFieldMapping is the same field-mapping check as
// TestOverlay_KindFieldMapping, but over the Go methods (Ring, Marks, Arrow, Line, Path, Box,
// Circle) the agent's tools call directly rather than through POST /overlay — one row per method,
// each also checking the ask id passed through onto the broadcast event.
func TestOverlayMethods_BroadcastFieldMapping(t *testing.T) {
	cases := []struct {
		name  string
		call  func(s *Server) error
		check func(t *testing.T, ev Event, got OverlayRequest)
	}{
		{"Ring", func(s *Server) error { return s.Ring("ask-7", 10, 20, 30, 40, "here") }, func(t *testing.T, ev Event, got OverlayRequest) {
			if ev.ID != "ask-7" {
				t.Errorf("event id = %q, want ask-7, the ask whose point_at drew the ring", ev.ID)
			}
			if got.Kind != "ring" || got.Label != "here" || len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
				t.Errorf("overlay = %+v, want a ring labelled here around 10,20 30x40", got)
			}
			if got.TTLMs != defaultOverlayTTLMs {
				t.Errorf("ttl = %d, want the default %d", got.TTLMs, defaultOverlayTTLMs)
			}
		}},
		{"Marks", func(s *Server) error {
			return s.Marks("ask-7", []OverlayRect{{X: 1, Y: 2, W: 3, H: 4, Label: "1"}, {X: 5, Y: 6, W: 7, H: 8, Label: "2"}})
		}, func(t *testing.T, ev Event, got OverlayRequest) {
			if ev.ID != "ask-7" {
				t.Errorf("event id = %q, want ask-7, the ask whose show_marks drew the marks", ev.ID)
			}
			want := []OverlayRect{{X: 1, Y: 2, W: 3, H: 4, Label: "1"}, {X: 5, Y: 6, W: 7, H: 8, Label: "2"}}
			if got.Kind != "marks" || len(got.Rects) != 2 || got.Rects[0] != want[0] || got.Rects[1] != want[1] {
				t.Errorf("overlay = %+v, want marks over the given rects", got)
			}
			if got.TTLMs != defaultOverlayTTLMs {
				t.Errorf("ttl = %d, want the default %d", got.TTLMs, defaultOverlayTTLMs)
			}
		}},
		{"Arrow", func(s *Server) error { return s.Draw("", "arrow", [][2]int{{1, 2}, {3, 4}}, 0, 0, 0, 0, "here") }, func(t *testing.T, ev Event, got OverlayRequest) {
			points := [][2]int{{1, 2}, {3, 4}}
			if got.Kind != "arrow" || got.Label != "here" || len(got.Points) != 2 || got.Points[0] != points[0] || got.Points[1] != points[1] {
				t.Errorf("overlay = %+v, want an arrow labelled here through %v", got, points)
			}
		}},
		{"Line", func(s *Server) error { return s.Draw("", "line", [][2]int{{1, 2}, {3, 4}}, 0, 0, 0, 0, "here") }, func(t *testing.T, ev Event, got OverlayRequest) {
			points := [][2]int{{1, 2}, {3, 4}}
			if got.Kind != "line" || got.Label != "here" || len(got.Points) != 2 || got.Points[0] != points[0] || got.Points[1] != points[1] {
				t.Errorf("overlay = %+v, want a line labelled here through %v", got, points)
			}
		}},
		{"Path", func(s *Server) error {
			return s.Draw("", "path", [][2]int{{1, 2}, {3, 4}, {5, 6}}, 0, 0, 0, 0, "route")
		}, func(t *testing.T, ev Event, got OverlayRequest) {
			if got.Kind != "path" || len(got.Points) != 3 {
				t.Errorf("path overlay = %+v, want kind path through 3 points", got)
			}
		}},
		{"Box", func(s *Server) error { return s.Draw("", "box", nil, 10, 20, 30, 40, "drop here") }, func(t *testing.T, ev Event, got OverlayRequest) {
			if got.Kind != "box" || len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
				t.Errorf("box overlay = %+v, want kind box around 10,20 30x40", got)
			}
		}},
		{"Circle", func(s *Server) error { return s.Draw("", "circle", nil, 10, 20, 30, 40, "click here") }, func(t *testing.T, ev Event, got OverlayRequest) {
			if got.Kind != "circle" || len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
				t.Errorf("circle overlay = %+v, want kind circle inscribed in 10,20 30x40", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&fakeAsker{}, nil, nil, nil)
			ch := s.hub.subscribe()
			defer s.hub.unsubscribe(ch)

			if err := tc.call(s); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			ev, got := waitOverlay(t, ch)
			if ev.Type != "overlay" {
				t.Errorf("event type = %q, want overlay", ev.Type)
			}
			tc.check(t, ev, got)
		})
	}
}

// Draw dispatches to Arrow, Line, Path, Box or Circle by shape, so cmd/daemon.go can wire the agent's Draw field straight to this method without a closure. A shape that is none of the five is refused and nothing is broadcast.
func TestDraw_DispatchesByShapeAndRejectsUnknownOnes(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	cases := []struct {
		shape string
		want  string
	}{
		{"arrow", "arrow"},
		{"line", "line"},
		{"path", "path"},
		{"box", "box"},
		{"circle", "circle"},
	}
	for _, tc := range cases {
		if err := s.Draw("", tc.shape, [][2]int{{1, 1}, {2, 2}, {3, 3}}, 10, 20, 30, 40, "go"); err != nil {
			t.Fatalf("Draw(%s) error: %v", tc.shape, err)
		}
		if _, got := waitOverlay(t, ch); got.Kind != tc.want {
			t.Errorf("kind = %q, want %q", got.Kind, tc.want)
		}
	}

	if err := s.Draw("", "sparkle", [][2]int{{1, 1}, {2, 2}}, 0, 0, 0, 0, "go"); err == nil {
		t.Fatal("Draw(sparkle) error = nil, want a complaint about the shape")
	}
	select {
	case ev := <-ch:
		t.Fatalf("a rejected shape was still broadcast: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOverlay_RejectsBadBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"unknown kind", `{"kind":"sparkles","rects":[{"x":1,"y":1,"w":2,"h":2}]}`},
		{"ring with no rects", `{"kind":"ring"}`},
		{"zero sized rect", `{"kind":"marks","rects":[{"x":1,"y":1,"w":0,"h":5}]}`},
		{"negative size", `{"kind":"marks","rects":[{"x":1,"y":1,"w":5,"h":-5}]}`},
		{"arrow with one point", `{"kind":"arrow","points":[[1,1]]}`},
		{"line with no points", `{"kind":"line"}`},
		{"arrow with a negative coordinate", `{"kind":"arrow","points":[[1,1],[-2,3]]}`},
		{"path with two points", `{"kind":"path","points":[[1,1],[2,2]]}`},
		{"box with no rects", `{"kind":"box"}`},
		{"circle with no rects", `{"kind":"circle"}`},
		{"circle with two rects", `{"kind":"circle","rects":[{"x":1,"y":1,"w":2,"h":2},{"x":3,"y":3,"w":2,"h":2}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&fakeAsker{}, nil, nil, nil)
			ch := s.hub.subscribe()
			defer s.hub.unsubscribe(ch)

			if rec := postOverlay(t, s, tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			select {
			case ev := <-ch:
				t.Fatalf("a rejected overlay was still broadcast: %+v", ev)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// An overlay posted straight to /overlay was drawn by no question at all, so the event it arrives on must not carry an "ask-N" id: a client reading the stream would take that for the id of a question it never saw. Minting one also moved the counter real asks are numbered from, so the next ask's id jumped.
func TestOverlay_DirectRouteCarriesANonAskID(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	if rec := postOverlay(t, s, `{"kind":"ring","label":"here","rects":[{"x":1,"y":2,"w":3,"h":4}]}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}

	ev, _ := waitOverlay(t, ch)
	if strings.HasPrefix(ev.ID, "ask-") {
		t.Errorf("overlay event id = %q, want an id no ask could have", ev.ID)
	}
	if got := s.newID(); got != "ask-1" {
		t.Errorf("first ask id after a direct overlay = %q, want ask-1: the drawing spent an id real asks are numbered from", got)
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

	ch := s.hub.subscribe()
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

// The drawing calls the agent's tools go through used to throw away the one fact only they knew: whether anybody was there to draw it. A model told "drew it" then narrates a diagram the user cannot see, so every one of them now hands back ErrNoOverlayWindow when the drawing went to nobody, and nil once a client is on the hub.
func TestDraw_SaysWhenTheDrawingReachedNoWindow(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)

	calls := map[string]func() error{
		"Ring":  func() error { return s.Ring("", 1, 2, 3, 4, "here") },
		"Marks": func() error { return s.Marks("", []OverlayRect{{X: 1, Y: 2, W: 3, H: 4, Label: "1"}}) },
		"Draw":  func() error { return s.Draw("", "box", nil, 1, 2, 3, 4, "go") },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNoOverlayWindow) {
			t.Errorf("%s with nothing on the hub = %v, want ErrNoOverlayWindow", name, err)
		}
	}

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	go func() {
		for range ch {
		}
	}()
	for name, call := range calls {
		if err := call(); err != nil {
			t.Errorf("%s with a client on the hub = %v, want nil", name, err)
		}
	}
}

// A drawing is several shapes broadcast one at a time, and the overlay keeps them together only when they name the same draw call. Every shape of one call therefore has to carry that name out on the wire; without it a twenty-shape formula drawn by a voice session showed up as one stroke, each shape wiping the last (2026-09-07).
func TestDraw_EveryShapeOfOneCallCarriesItsGroup(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	go func() {
		_ = s.Draw("d4", "box", nil, 1, 2, 3, 4, "one")
		_ = s.Draw("d4", "arrow", [][2]int{{1, 1}, {2, 2}}, 0, 0, 0, 0, "two")
		_ = s.Draw("d5", "circle", nil, 5, 6, 7, 8, "next drawing")
	}()

	for _, want := range []struct{ kind, group string }{{"box", "d4"}, {"arrow", "d4"}, {"circle", "d5"}} {
		_, got := waitOverlay(t, ch)
		if got.Kind != want.kind || got.Group != want.group {
			t.Errorf("overlay = kind %q group %q, want kind %q group %q", got.Kind, got.Group, want.kind, want.group)
		}
	}
}
