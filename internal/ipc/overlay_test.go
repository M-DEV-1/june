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

func TestOverlay_RingBroadcastsNormalisedPayload(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	rec := postOverlay(t, s, `{"kind":"ring","label":"here","rects":[{"x":10,"y":20,"w":30,"h":40,"label":"box"}],"ttl_ms":99000}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}

	ev, got := waitOverlay(t, ch)
	if ev.Type != "overlay" {
		t.Fatalf("event type = %q, want overlay", ev.Type)
	}
	if got.Kind != "ring" || got.Label != "here" {
		t.Fatalf("payload = %+v, want kind ring label here", got)
	}
	if got.TTLMs != maxOverlayTTLMs {
		t.Fatalf("ttl_ms = %d, want it capped at %d", got.TTLMs, maxOverlayTTLMs)
	}
	if len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40, Label: "box"}) {
		t.Fatalf("rects = %+v, want the one posted", got.Rects)
	}
}

func TestOverlay_DefaultTTLAndClearNeedsNoRects(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	if rec := postOverlay(t, s, `{"kind":"clear"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("clear status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	_, got := waitOverlay(t, ch)
	if got.Kind != "clear" || len(got.Rects) != 0 {
		t.Fatalf("payload = %+v, want an empty clear", got)
	}
	if got.TTLMs != defaultOverlayTTLMs {
		t.Fatalf("ttl_ms = %d, want the default %d", got.TTLMs, defaultOverlayTTLMs)
	}
}

func TestOverlay_ArrowAndLineBroadcastPoints(t *testing.T) {
	for _, kind := range []string{"arrow", "line"} {
		t.Run(kind, func(t *testing.T) {
			s := New(&fakeAsker{}, nil, nil, nil)
			ch := s.hub.subscribe()
			defer s.hub.unsubscribe(ch)

			rec := postOverlay(t, s, `{"kind":"`+kind+`","label":"press here","points":[[10,20],[30,40],[50,60]]}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
			}

			ev, got := waitOverlay(t, ch)
			if ev.Type != "overlay" {
				t.Fatalf("event type = %q, want overlay", ev.Type)
			}
			if got.Kind != kind || got.Label != "press here" {
				t.Fatalf("payload = %+v, want kind %s label %q", got, kind, "press here")
			}
			want := [][2]int{{10, 20}, {30, 40}, {50, 60}}
			if len(got.Points) != len(want) {
				t.Fatalf("points = %+v, want %+v", got.Points, want)
			}
			for i := range want {
				if got.Points[i] != want[i] {
					t.Fatalf("points = %+v, want %+v", got.Points, want)
				}
			}
		})
	}
}

func TestOverlay_PathNeedsThreePoints(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	rec := postOverlay(t, s, `{"kind":"path","points":[[1,1],[2,2],[3,3]]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if _, got := waitOverlay(t, ch); got.Kind != "path" || len(got.Points) != 3 {
		t.Fatalf("payload = %+v, want kind path through 3 points", got)
	}
}

func TestOverlay_BoxAndCircleBroadcastRects(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	rec := postOverlay(t, s, `{"kind":"box","label":"drop here","rects":[{"x":1,"y":2,"w":3,"h":4}]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("box status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if _, got := waitOverlay(t, ch); got.Kind != "box" || len(got.Rects) != 1 {
		t.Fatalf("box payload = %+v, want one rect", got)
	}

	rec = postOverlay(t, s, `{"kind":"circle","label":"click here","rects":[{"x":1,"y":2,"w":3,"h":4}]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("circle status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if _, got := waitOverlay(t, ch); got.Kind != "circle" || len(got.Rects) != 1 {
		t.Fatalf("circle payload = %+v, want one rect", got)
	}
}

// Arrow and Line are how the agent's draw tool reaches the screen: a path through points, broadcast on the hub exactly like a POST /overlay arrow or line so the extension needs no second path, stamped with the id of the ask that drew it.
func TestArrowAndLine_BroadcastAPathOverlay(t *testing.T) {
	cases := []struct {
		kind string
		call func(s *Server, points [][2]int, label string)
	}{
		{"arrow", func(s *Server, points [][2]int, label string) { s.Arrow("ask-7", points, label) }},
		{"line", func(s *Server, points [][2]int, label string) { s.Line("ask-7", points, label) }},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			s := New(&fakeAsker{}, nil, nil, nil)
			ch := s.hub.subscribe()
			defer s.hub.unsubscribe(ch)

			points := [][2]int{{1, 2}, {3, 4}}
			tc.call(s, points, "here")

			ev, got := waitOverlay(t, ch)
			if ev.ID != "ask-7" {
				t.Errorf("event id = %q, want ask-7, the ask whose draw drew it", ev.ID)
			}
			if got.Kind != tc.kind || got.Label != "here" || len(got.Points) != 2 || got.Points[0] != points[0] || got.Points[1] != points[1] {
				t.Errorf("overlay = %+v, want a %s labelled here through %v", got, tc.kind, points)
			}
		})
	}
}

// Path, Box and Circle are the other three shapes the agent's draw tool can reach: a free-form stroke, a dashed rectangle and a dashed circle, each broadcast on the hub exactly like the matching POST /overlay kind.
func TestPathBoxCircle_BroadcastTheirOwnKind(t *testing.T) {
	s := New(&fakeAsker{}, nil, nil, nil)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.Path("ask-7", [][2]int{{1, 2}, {3, 4}, {5, 6}}, "route")
	if _, got := waitOverlay(t, ch); got.Kind != "path" || len(got.Points) != 3 {
		t.Errorf("path overlay = %+v, want kind path through 3 points", got)
	}

	s.Box("ask-7", 10, 20, 30, 40, "drop here")
	if _, got := waitOverlay(t, ch); got.Kind != "box" || len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
		t.Errorf("box overlay = %+v, want kind box around 10,20 30x40", got)
	}

	s.Circle("ask-7", 10, 20, 30, 40, "click here")
	if _, got := waitOverlay(t, ch); got.Kind != "circle" || len(got.Rects) != 1 || got.Rects[0] != (OverlayRect{X: 10, Y: 20, W: 30, H: 40}) {
		t.Errorf("circle overlay = %+v, want kind circle inscribed in 10,20 30x40", got)
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
		if err := s.Draw(tc.shape, [][2]int{{1, 1}, {2, 2}, {3, 3}}, 10, 20, 30, 40, "go"); err != nil {
			t.Fatalf("Draw(%s) error: %v", tc.shape, err)
		}
		if _, got := waitOverlay(t, ch); got.Kind != tc.want {
			t.Errorf("kind = %q, want %q", got.Kind, tc.want)
		}
	}

	if err := s.Draw("sparkle", [][2]int{{1, 1}, {2, 2}}, 0, 0, 0, 0, "go"); err == nil {
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
