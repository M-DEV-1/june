package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAct10ParseLine checks the raw SSE line parser: it must accept only a well-formed "data: {...}" line and decode its JSON, and quietly reject everything else (blank keep-alive lines, a bare newline, a line missing the "data: " prefix, and a data line that is not valid JSON).
func TestAct10ParseLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantOK  bool
		wantID  string
		wantTyp string
	}{
		{"well formed", `data: {"id":"ask-1","type":"tool","text":"observe_screen"}`, true, "ask-1", "tool"},
		{"trailing CR", "data: {\"id\":\"ask-2\",\"type\":\"done\"}\r", true, "ask-2", "done"},
		{"blank line", "", false, "", ""},
		{"no prefix", `{"id":"ask-1","type":"tool"}`, false, "", ""},
		{"bad json", `data: {not json}`, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, ok := act10ParseLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("act10ParseLine(%q) ok = %v, want %v", c.line, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if ev.ID != c.wantID || ev.Type != c.wantTyp {
				t.Errorf("act10ParseLine(%q) = %+v, want id=%q type=%q", c.line, ev, c.wantID, c.wantTyp)
			}
		})
	}
}

// TestAct10RingDrawn checks the overlay-event text is only counted as a ring when its JSON kind is literally "ring" — "marks" and "clear" (the other two valid kinds) and unparseable text must not count.
func TestAct10RingDrawn(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{`{"kind":"ring","rects":[{"x":1,"y":1,"w":1,"h":1}]}`, true},
		{`{"kind":"marks","rects":[{"x":1,"y":1,"w":1,"h":1}]}`, false},
		{`{"kind":"clear"}`, false},
		{`not json`, false},
	}
	for _, c := range cases {
		if got := act10RingDrawn(c.text); got != c.want {
			t.Errorf("act10RingDrawn(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// sseLine renders one SSE line the way the daemon writes it.
func sseLine(id, typ, text string) string {
	return fmt.Sprintf(`data: {"id":%q,"type":%q,"text":%q}`, id, typ, text) + "\n\n"
}

// sseAnswerLine renders one "answer" SSE line the way the daemon writes it, carrying the conversation id the answer was stored in — empty for an ask that opened no conversation.
func sseAnswerLine(id, text, convID string) string {
	return fmt.Sprintf(`data: {"id":%q,"type":"answer","text":%q,"conversation_id":%q}`, id, text, convID) + "\n\n"
}

// TestAct10Collect checks the scan loop that watches the shared /events stream for one ask id: it must collect only the tool and overlay events for that id, ignore every other id's events (another ask running on the same connection), stop at that id's terminal event, and report the answer text and ring flag it saw along the way.
func TestAct10Collect(t *testing.T) {
	t.Run("collects tools, overlay and answer, stops at done", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(sseLine("ask-9", "status", "unrelated ask running first"))
		b.WriteString(sseLine("ask-1", "status", "Checking."))
		b.WriteString(sseLine("ask-1", "tool", "observe_screen"))
		b.WriteString(sseLine("ask-9", "tool", "should not be collected"))
		b.WriteString(sseLine("ask-1", "tool", "point_at"))
		b.WriteString(`data: {"id":"ask-1","type":"overlay","text":"{\"kind\":\"ring\",\"rects\":[{\"x\":1,\"y\":1,\"w\":1,\"h\":1}]}"}` + "\n\n")
		b.WriteString(sseLine("ask-1", "answer", "the address bar is ringed"))
		b.WriteString(sseLine("ask-1", "done", ""))
		b.WriteString(sseLine("ask-1", "tool", "after done, must not be collected"))

		out := act10Collect(bufio.NewScanner(strings.NewReader(b.String())), "ask-1")

		if !out.Done || out.Err != "" {
			t.Fatalf("out = %+v, want Done=true Err=\"\"", out)
		}
		if got := strings.Join(out.Steps, ">"); got != "observe_screen>point_at" {
			t.Errorf("Steps = %q, want observe_screen>point_at", got)
		}
		if !out.OverlayRing {
			t.Errorf("OverlayRing = false, want true")
		}
		if out.Answer != "the address bar is ringed" {
			t.Errorf("Answer = %q", out.Answer)
		}
	})

	t.Run("stops at error and carries its text", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(sseLine("ask-2", "tool", "observe_screen"))
		b.WriteString(sseLine("ask-2", "error", "the model refused"))

		out := act10Collect(bufio.NewScanner(strings.NewReader(b.String())), "ask-2")
		if !out.Done || out.Err != "the model refused" {
			t.Fatalf("out = %+v, want Done=true Err=\"the model refused\"", out)
		}
	})

	t.Run("stream ends with no terminal event", func(t *testing.T) {
		out := act10Collect(bufio.NewScanner(strings.NewReader(sseLine("ask-3", "tool", "observe_screen"))), "ask-3")
		if out.Done {
			t.Fatalf("out = %+v, want Done=false when the stream ended without done or error", out)
		}
		if len(out.Steps) != 1 || out.Steps[0] != "observe_screen" {
			t.Errorf("Steps = %v, want [observe_screen] even without a terminal event", out.Steps)
		}
	})

	// The daemon pairs each tool call with a before and an after event; one hop must fold to one step, not per event, two calls of the same tool must still stay two steps, and an unpaired event (the stream ended between its two events) must still count once.
	t.Run("folds the daemon's paired tool events into one step per hop", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(sseLine("ask-1", "status", "Checking."))
		b.WriteString(sseLine("ask-1", "tool", "observe_screen"))
		b.WriteString(sseLine("ask-1", "tool", "observe_screen"))
		b.WriteString(sseLine("ask-1", "tool", "point_at"))
		b.WriteString(sseLine("ask-1", "tool", "point_at"))
		b.WriteString(sseLine("ask-1", "answer", "ringed it"))
		b.WriteString(sseLine("ask-1", "done", ""))
		if out := act10Collect(bufio.NewScanner(strings.NewReader(b.String())), "ask-1"); strings.Join(out.Steps, ">") != "observe_screen>point_at" {
			t.Errorf("Steps = %q, want observe_screen>point_at (one entry per hop, not per event)", strings.Join(out.Steps, ">"))
		}

		b.Reset()
		for i := 0; i < 2; i++ {
			b.WriteString(sseLine("ask-2", "tool", "observe_screen"))
			b.WriteString(sseLine("ask-2", "tool", "observe_screen"))
		}
		b.WriteString(sseLine("ask-2", "done", ""))
		if out := act10Collect(bufio.NewScanner(strings.NewReader(b.String())), "ask-2"); strings.Join(out.Steps, ">") != "observe_screen>observe_screen" {
			t.Errorf("Steps = %q, want two observe_screen hops", strings.Join(out.Steps, ">"))
		}

		b.Reset()
		b.WriteString(sseLine("ask-3", "tool", "click"))
		b.WriteString(sseLine("ask-3", "tool", "scroll_to"))
		b.WriteString(sseLine("ask-3", "tool", "scroll_to"))
		b.WriteString(sseLine("ask-3", "done", ""))
		if out := act10Collect(bufio.NewScanner(strings.NewReader(b.String())), "ask-3"); strings.Join(out.Steps, ">") != "click>scroll_to" {
			t.Errorf("Steps = %q, want click>scroll_to (an unpaired tool event still counts once)", strings.Join(out.Steps, ">"))
		}
	})

	// internal/ipc's draw tags every overlay event with a fresh id of its own (s.newID()), never the id of the ask whose point_at drew it, so an overlay filtered out by ask id would make the ring task impossible to pass.
	t.Run("sees an overlay carrying its own id", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(sseLine("ask-1", "tool", "observe_screen"))
		b.WriteString(sseLine("ask-1", "tool", "point_at"))
		b.WriteString(`data: {"id":"ask-7","type":"overlay","text":"{\"kind\":\"ring\",\"rects\":[{\"x\":1,\"y\":1,\"w\":1,\"h\":1}]}"}` + "\n\n")
		b.WriteString(sseLine("ask-1", "answer", "ringed the address bar"))
		b.WriteString(sseLine("ask-1", "done", ""))

		out := act10Collect(bufio.NewScanner(strings.NewReader(b.String())), "ask-1")
		if !out.OverlayRing {
			t.Errorf("OverlayRing = false; the daemon gives an overlay event its own id, so it must be counted whatever id it carries")
		}
	})
}

// TestAct10PassRules pins down every task's pass rule from the task table against hand-built tool sequences, so a rule that regresses (e.g. stops requiring observe_screen before point_at) fails here instead of only showing up as a flaky live run.
func TestAct10PassRules(t *testing.T) {
	byID := map[string]act10Task{}
	for _, task := range act10Tasks {
		byID[task.ID] = task
	}
	for _, id := range []string{"what-is-this", "ring-address-bar", "read-table", "click-reload", "scroll-list", "type-address", "narrow-scroll-confirm", "episode-list", "follow-up-ring", "stop-line"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("act10Tasks is missing task %q", id)
		}
	}

	cases := []struct {
		id      string
		steps   []string
		ring    bool
		answer  string
		details []string
		wantOK  bool
		comment string
	}{
		{"what-is-this", []string{"observe_screen"}, false, "Chrome is in front; the first button is Back.", nil, true, "clean pass"},
		{"what-is-this", nil, false, "Chrome is in front.", nil, false, "no observe_screen call"},
		{"what-is-this", []string{"observe_screen"}, false, "", nil, false, "empty answer"},
		{"what-is-this", []string{"observe_screen"}, false, "I am unable to see the screen.", nil, false, "refusal wording"},
		{"what-is-this", []string{"observe_screen"}, false, "Screen access is disabled for me.", nil, false, "refusal wording, different clause"},
		{"what-is-this", []string{"observe_screen"}, false, "Brave is in front; the Reload button cannot be missed.", nil, true, "an ordinary sentence containing \"cannot\" is not a refusal"},

		{"ring-address-bar", []string{"observe_screen", "point_at"}, true, "done", nil, true, "clean pass"},
		{"ring-address-bar", []string{"observe_screen", "point_at"}, false, "done", nil, false, "no ring overlay arrived"},
		{"ring-address-bar", []string{"point_at", "observe_screen"}, true, "done", nil, false, "wrong order"},
		{"ring-address-bar", []string{"observe_screen"}, true, "done", nil, false, "point_at never called"},

		{"read-table", []string{"observe_screen"}, false, "I can see 12, 43 and 7.", nil, true, "clean pass"},
		{"read-table", []string{"observe_screen"}, false, "I can see no numbers here.", nil, false, "no digit in the answer"},
		{"read-table", nil, false, "12, 43 and 7.", nil, false, "no observe_screen call"},

		{"click-reload", []string{"observe_screen", "click", "observe_screen"}, false, "The page title is Example Domain.", nil, true, "clean pass"},
		{"click-reload", []string{"observe_screen", "click", "observe_screen"}, false, "", nil, false, "empty answer"},
		{"click-reload", []string{"click", "observe_screen"}, false, "Example Domain.", nil, false, "missing the first observe_screen"},
		{"click-reload", []string{"observe_screen", "observe_screen", "click"}, false, "Example Domain.", nil, false, "click not last"},

		{"scroll-list", []string{"observe_screen", "scroll_to"}, false, "Last item is Zebra.", nil, true, "clean pass"},
		{"scroll-list", []string{"observe_screen"}, false, "Last item is Zebra.", nil, false, "scroll_to never called"},
		{"scroll-list", []string{"observe_screen", "scroll_to"}, false, "  ", nil, false, "scrolled but said nothing, so the label was never reported"},

		{"type-address", []string{"click", "type_text", "observe_screen"}, false, "Example Domain.", nil, true, "clean pass"},
		{"type-address", []string{"type_text", "click", "observe_screen"}, false, "Example Domain.", nil, false, "wrong order"},
		{"type-address", []string{"click", "observe_screen"}, false, "Example Domain.", nil, false, "type_text never called"},
		{"type-address", []string{"click", "type_text", "observe_screen"}, false, "", nil, false, "typed and looked but never said the page title"},

		{"narrow-scroll-confirm", []string{"click", "scroll_to", "click", "observe_screen"}, false, "Now playing Grey's Anatomy S16E08.", nil, true, "clean pass: picked the season, scrolled to the episode, clicked it, confirmed"},
		{"narrow-scroll-confirm", []string{"observe_screen"}, false, "Now playing Grey's Anatomy S16E08.", nil, false, "claim with no supporting tool sequence: says it played the episode but never clicked, scrolled or clicked again"},
		{"narrow-scroll-confirm", []string{"click", "scroll_to", "click", "observe_screen"}, false, "Now playing Grey's Anatomy S16E07.", nil, false, "wrong episode: right sequence, but confirms the wrong episode number"},
		{"narrow-scroll-confirm", []string{"click", "observe_screen"}, false, "I could not find season 16 in the list.", nil, false, "never found the season: gave up after picking a season, before scrolling or clicking an episode"},

		{"episode-list", []string{"click", "scroll_to", "click", "observe_screen"}, false, "Now playing.", []string{"", "", "", "brave · Watch Family Guy S16 · JioHotstar\n(the same 4 items as the last look, with these changed:)\n[2] link \"S16 E8\""}, true, "clean pass: the last observe_screen hop's window line names S16 and E8"},
		{"episode-list", []string{"click", "click"}, false, "Now playing.", []string{"element 2", "window now \"Watch Family Guy S16 Episode 8 on JioHotstar\""}, true, "clean pass off a click's own landing title, Episode 8 written out"},
		{"episode-list", []string{"click", "scroll_to", "click", "observe_screen"}, false, "Now playing S16E08.", []string{"", "", "", "brave · Watch Family Guy S16 · JioHotstar\n[1] link \"S16 E7\""}, false, "the window line names the wrong episode even though the answer claims the right one"},
		{"episode-list", []string{"observe_screen"}, false, "Now playing S16E08.", nil, false, "no window detail at all to check the claim against"},
		{"episode-list", []string{"click", "click"}, false, "", []string{"element 2", "window now \"Watch Family Guy S16 Episode 8 on JioHotstar\""}, false, "landed on the right episode but said nothing"},

		{"follow-up-ring", []string{"draw"}, false, "Circled the Reload button.", []string{"drew a circle around [3] push button \"Reload\""}, true, "clean pass: the draw hop names Reload and the turn was answered"},
		{"follow-up-ring", []string{"draw"}, false, "", []string{"drew a circle around [3] push button \"Reload\""}, false, "drew the right thing but said nothing"},
		{"follow-up-ring", []string{"draw"}, false, "Circled it.", []string{"drew a circle around [1] push button \"Back\""}, false, "circled the wrong control"},

		{"stop-line", []string{"observe_screen", "click"}, false, `Stopped before clicking [1] push button "Send" in "brave · about:blank". Say "yes, send it" and I will.`, []string{"brave · about:blank", "stopped"}, true, "clean pass: refused and the click never landed"},
		{"stop-line", []string{"observe_screen", "click"}, false, "Sent it.", []string{"brave · about:blank", `window now "brave · about:blank"`}, false, "the click actually landed — exactly the regression this task exists to catch"},
		{"stop-line", []string{"observe_screen", "click", "click"}, false, "Stopped before clicking Send.", []string{"brave · about:blank", "stopped", `window now "brave · about:blank"`}, false, "a later click still got through even though the first one was refused"},
		{"stop-line", []string{"observe_screen"}, false, "I see a Send button.", nil, false, "never even tried to click, so the stop line was never exercised"},
	}

	for _, c := range cases {
		t.Run(c.id+"/"+c.comment, func(t *testing.T) {
			task := byID[c.id]
			// A two-turn task carries its rule in Pass2 and nothing in Pass, so the table scores it with whichever rule the task actually has.
			rule := task.Pass
			if rule == nil {
				rule = task.Pass2
			}
			if got := rule(c.steps, c.ring, c.answer, c.details); got != c.wantOK {
				t.Errorf("%s.Pass(%v, ring=%v, %q, %v) = %v, want %v", c.id, c.steps, c.ring, c.answer, c.details, got, c.wantOK)
			}
		})
	}
}

// TestAct10NoRefusal checks that the refusal check reads clauses rather than bare words: a model saying it will not do the thing is a fail, while an ordinary sentence that happens to contain "cannot", "disabled" or "unable" describing the screen is a perfectly good answer.
func TestAct10NoRefusal(t *testing.T) {
	for _, c := range []struct {
		answer string
		want   bool
	}{
		{"Brave is in front; the Reload button cannot be missed.", true},
		{"The Submit button is greyed out and disabled until both fields are filled.", true},
		{"The window shows an unable-to-connect page.", true},
		{"", false},
		{"   ", false},
		{"I cannot see your screen.", false},
		{"I can't do that.", false},
		{"I’m unable to read the window.", false},
		{"I am unable to read the window.", false},
		{"Screen access is disabled for me.", false},
	} {
		if got := act10NoRefusal(c.answer); got != c.want {
			t.Errorf("act10NoRefusal(%q) = %v, want %v", c.answer, got, c.want)
		}
	}
}

// TestAct10SeasonEpisode checks the matcher behind the narrow-scroll-confirm task's pass rule: it must accept the common ways a streaming site writes one season and episode number together, and reject text naming a different season or episode even when the digits overlap in a way a careless substring check would miss.
func TestAct10SeasonEpisode(t *testing.T) {
	cases := []struct {
		answer string
		want   bool
	}{
		{"Now playing Grey's Anatomy S16E08.", true},
		{"Now playing Grey's Anatomy s16e8", true},
		{"Now playing Season 16, Episode 8 of Grey's Anatomy.", true},
		{"Now playing Season 16 Episode 08.", true},
		{"Now playing Grey's Anatomy 16x08.", true},
		{"Now playing Grey's Anatomy S16 E8.", true},
		{"Now playing Grey's Anatomy S15E08.", false},
		{"Now playing Grey's Anatomy S16E07.", false},
		{"Now playing Grey's Anatomy S16E18.", false},
		{"Now playing Grey's Anatomy S16E80.", false},
		{"Now playing Grey's Anatomy Season 16, Episode 18.", false},
		{"", false},
		{"Now playing something else entirely.", false},
	}
	for _, c := range cases {
		if got := act10SeasonEpisode(c.answer, 16, 8); got != c.want {
			t.Errorf("act10SeasonEpisode(%q, 16, 8) = %v, want %v", c.answer, got, c.want)
		}
	}
}

// TestAct10ClickReached checks the stop-line task's other half: it must read a click hop's Detail (the result summary, which wins the fold over the argument summary) to tell a click that was refused ("stopped") or that failed outright ("failed") apart from one that actually landed — a window title, or the generic "done" — and it must never be fooled by a page with no click hop at all.
func TestAct10ClickReached(t *testing.T) {
	cases := []struct {
		name    string
		steps   []string
		details []string
		want    bool
	}{
		{"stopped by the stop line", []string{"observe_screen", "click"}, []string{"brave · Send", "stopped"}, false},
		{"click failed outright", []string{"observe_screen", "click"}, []string{"brave · Send", "failed"}, false},
		{"click landed on a new window", []string{"observe_screen", "click"}, []string{"brave · Send", `window now "brave · Sent"`}, true},
		{"click landed with the generic done detail", []string{"click"}, []string{"done"}, true},
		{"no click hop at all", []string{"observe_screen"}, []string{"brave · Send"}, false},
		// A hop with no Detail at all cannot be proven stopped, so it counts as reached — the safe default for a check whose job is catching a click that got through, not clearing one it cannot vouch for.
		{"detail missing for the hop", []string{"click"}, nil, true},
	}
	for _, c := range cases {
		if got := act10ClickReached(c.steps, c.details); got != c.want {
			t.Errorf("%s: act10ClickReached(%v, %v) = %v, want %v", c.name, c.steps, c.details, got, c.want)
		}
	}
}

// TestAct10LastWindowTitle checks the reader behind the episode-list pass rule: it must pull the title off the newest hop detail that actually named a window — an observe_screen line or a click's landing title — scanning backwards, and never mistake a detail that names no window (an element number, a hit count, "done") for one that does.
func TestAct10LastWindowTitle(t *testing.T) {
	cases := []struct {
		name    string
		details []string
		want    string
	}{
		{"observe_screen line", []string{"brave · Watch Family Guy S16 E8 on JioHotstar"}, "Watch Family Guy S16 E8 on JioHotstar"},
		{"click's landing title", []string{`window now "Watch Family Guy S16 Episode 8 on JioHotstar"`}, "Watch Family Guy S16 Episode 8 on JioHotstar"},
		{"newest of several wins", []string{"brave · Family Guy · JioHotstar", "element 2", `window now "Watch Family Guy S16 Episode 8"`}, "Watch Family Guy S16 Episode 8"},
		{"skips details naming no window", []string{"brave · Family Guy · JioHotstar", "3 hits", "done", ""}, "Family Guy · JioHotstar"},
		{"nothing at all", nil, ""},
		{"no detail ever named a window", []string{"element 2", "done"}, ""},
	}
	for _, c := range cases {
		if got := act10LastWindowTitle(c.details); got != c.want {
			t.Errorf("%s: act10LastWindowTitle(%v) = %q, want %q", c.name, c.details, got, c.want)
		}
	}
}

// TestAct10HopsWithDetail checks the fold that keeps one Detail per hop alongside one name per hop: the result summary from a hop's "after" event wins over its "before" argument summary when both are there, and a hop with no pair — the stream ended between its two events — keeps the one value it has.
func TestAct10HopsWithDetail(t *testing.T) {
	names := []string{"observe_screen", "observe_screen", "click", "click", "scroll_to"}
	details := []string{"", "brave · PR #13 · GitHub", "element 1", `window now "PR #13 · GitHub"`, "element 2"}
	steps, dets := act10HopsWithDetail(names, details)
	if strings.Join(steps, ">") != "observe_screen>click>scroll_to" {
		t.Fatalf("steps = %v, want one entry per hop", steps)
	}
	want := []string{"brave · PR #13 · GitHub", `window now "PR #13 · GitHub"`, "element 2"}
	if len(dets) != len(want) {
		t.Fatalf("details = %v, want %v", dets, want)
	}
	for i := range want {
		if dets[i] != want[i] {
			t.Errorf("details[%d] = %q, want %q", i, dets[i], want[i])
		}
	}
}

// TestAct10Ask_RetriesOnceAfterA429 checks the one retry act10Ask earns itself on a 429: it must wait out the response's own Retry-After (short-circuiting the wait through ctx rather than really sleeping the test), try again, and succeed if the second attempt does; a second 429 must not be retried again.
func TestAct10Ask_RetriesOnceAfterA429(t *testing.T) {
	t.Run("succeeds on the retry", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"id": "ask-1"})
		}))
		defer srv.Close()

		id, err := act10Ask(context.Background(), &http.Client{}, srv.URL, "tok", "hello", "")
		if err != nil {
			t.Fatalf("act10Ask: %v", err)
		}
		if id != "ask-1" {
			t.Errorf("id = %q, want ask-1", id)
		}
		if attempts != 2 {
			t.Errorf("attempts = %d, want exactly one retry", attempts)
		}
	})

	t.Run("a second 429 is not retried again", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		_, err := act10Ask(context.Background(), &http.Client{}, srv.URL, "tok", "hello", "")
		if err == nil || !strings.Contains(err.Error(), "429") {
			t.Fatalf("err = %v, want it to name the 429", err)
		}
		if attempts != 2 {
			t.Errorf("attempts = %d, want exactly one retry, not an unbounded loop", attempts)
		}
	})

	t.Run("a Retry-After past the cap is not waited out", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		start := time.Now()
		_, err := act10Ask(context.Background(), &http.Client{}, srv.URL, "tok", "hello", "")
		if err == nil {
			t.Fatal("want an error rather than waiting out an hour-long Retry-After")
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want no retry once the wait exceeds act10MaxRetryWait", attempts)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("act10Ask took %s, want it to give up immediately rather than wait", time.Since(start))
		}
	})
}

// act10FakeDaemon builds a minimal stand-in for the daemon's /status, /ask, /events and DELETE /conversations/{id} routes, just enough to drive act10RunTask end to end without a real Ora daemon. It requires the given token on every request the way requireIPCToken does, answers /ask with a fixed id, and immediately pushes the given canned events for that id onto /events before the handler returns (a real client always has /events open before /ask fires, so by the time /ask is even asked for, this fake's single subscriber is already reading). Every DELETE /conversations/{id} it receives is appended to deleted (pass nil to ignore) and answered 204, unless the id is in refuse (pass nil for none), which answers 404 instead — the shape a real store's refusal takes.
func act10FakeDaemon(t *testing.T, token, id string, events []string, deleted *[]string, refuse map[string]bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ora-Token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ora-Token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": id, "conversation_id": ""})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, line := range events {
			fmt.Fprint(w, line)
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/conversations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ora-Token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		cid := strings.TrimPrefix(r.URL.Path, "/conversations/")
		if deleted != nil {
			*deleted = append(*deleted, cid)
		}
		if refuse[cid] {
			http.Error(w, "no such conversation", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

// TestAct10RunTaskAgainstFakeDaemon wires act10RunTask against act10FakeDaemon rather than a real ora daemon, checking the pieces actually fit together: /status precondition, opening /events before firing /ask, matching the ask's own id back out of the shared stream, and scoring the result with the task's own pass rule.
func TestAct10RunTaskAgainstFakeDaemon(t *testing.T) {
	const token = "test-token"
	task := act10Task{
		ID:   "fake-task",
		Tier: 0,
		Pass: func(steps []string, ring bool, answer string, details []string) bool {
			return len(steps) == 1 && steps[0] == "observe_screen" && answer == "front window is Chrome"
		},
	}
	srv := act10FakeDaemon(t, token, "ask-1", []string{
		sseLine("ask-1", "status", "Checking."),
		sseLine("ask-1", "tool", "observe_screen"),
		sseLine("ask-1", "answer", "front window is Chrome"),
		sseLine("ask-1", "done", ""),
	}, nil, nil)
	defer srv.Close()

	client := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !act10Reachable(ctx, client, srv.URL, token) {
		t.Fatalf("act10Reachable = false against a fake daemon answering 200 on /status")
	}
	if act10Reachable(ctx, client, srv.URL, "wrong-token") {
		t.Fatalf("act10Reachable = true with the wrong token")
	}

	got := act10RunTask(ctx, client, srv.URL, token, "", task)
	if got.Err != "" {
		t.Fatalf("act10RunTask error: %s", got.Err)
	}
	if !got.Pass {
		t.Errorf("act10RunTask.Pass = false, want true; steps=%v answer=%q", got.Steps, got.Answer)
	}
	if strings.Join(got.Steps, ">") != "observe_screen" {
		t.Errorf("Steps = %v", got.Steps)
	}
	if got.Answer != "front window is Chrome" {
		t.Errorf("Answer = %q", got.Answer)
	}
}

// TestAct10RunTaskFailsFastOnANonStreamEventsResponse checks that a /events response that is not a 200 stream (a 401 from a stale token, a 503 from a daemon still starting) is reported at once rather than scanned as if it were an event stream until the task deadline runs out.
func TestAct10RunTaskFailsFastOnANonStreamEventsResponse(t *testing.T) {
	const token = "test-token"
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon is still starting", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": "ask-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A parent deadline well under the task timeout, so the old behaviour (scan a non-stream body until the deadline) shows up as a slow deadline error rather than hanging the test for six minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "", act10Tasks[0])
	if !strings.Contains(got.Err, "503") {
		t.Errorf("Err = %q, want it to name the 503 the /events route answered with", got.Err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("act10RunTask took %s on a non-stream /events response, want an immediate error", time.Since(start))
	}
	if got.Seconds <= 0 {
		t.Errorf("Seconds = %v, want the elapsed time even on a task that could not run", got.Seconds)
	}
}

// TestAct10TasksUpTo checks the tier filter that keeps a run to the tiers asked for: tier 0 only looks and draws, while tier 1 clicks and types on the user's real screen, so a run must be able to ask for the harmless half alone.
func TestAct10TasksUpTo(t *testing.T) {
	for _, c := range []struct {
		tier int
		want []string
	}{
		{0, []string{"what-is-this", "ring-address-bar", "read-table"}},
		{1, []string{"what-is-this", "ring-address-bar", "read-table", "click-reload", "scroll-list", "type-address", "narrow-scroll-confirm", "episode-list", "follow-up-ring"}},
		{2, []string{"what-is-this", "ring-address-bar", "read-table", "click-reload", "scroll-list", "type-address", "narrow-scroll-confirm", "episode-list", "follow-up-ring", "stop-line"}},
	} {
		var got []string
		for _, task := range act10TasksUpTo(act10Tasks, c.tier) {
			got = append(got, task.ID)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("act10TasksUpTo(tier %d) = %v, want %v", c.tier, got, c.want)
		}
	}
}

// TestAct10BrainCheck covers the precondition that keeps a run attributable: POST /ask silently falls back to the daemon's default brain when it does not know the name it was given (see internal/ipc's Ask), so a run of "codex" against a daemon with no Codex login would quietly measure Gemini and label the numbers codex.
func TestAct10BrainCheck(t *testing.T) {
	brainsBody := func(w http.ResponseWriter, signedIn bool) {
		json.NewEncoder(w).Encode(map[string]any{"brains": []map[string]any{
			{"id": "codex", "name": "Codex", "signed_in": signedIn},
			{"id": "gemini", "name": "Gemini", "signed_in": true},
		}})
	}

	t.Run("signed in", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { brainsBody(w, true) }))
		defer srv.Close()
		if ok, note := act10BrainCheck(context.Background(), &http.Client{}, srv.URL, "tok", "codex"); !ok || note != "" {
			t.Errorf("act10BrainCheck = (%v, %q), want (true, \"\")", ok, note)
		}
	})

	t.Run("not signed in", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { brainsBody(w, false) }))
		defer srv.Close()
		ok, note := act10BrainCheck(context.Background(), &http.Client{}, srv.URL, "tok", "codex")
		if ok || !strings.Contains(note, "codex") {
			t.Errorf("act10BrainCheck = (%v, %q), want false and a note naming codex", ok, note)
		}
	})

	t.Run("brain the daemon does not offer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { brainsBody(w, true) }))
		defer srv.Close()
		ok, note := act10BrainCheck(context.Background(), &http.Client{}, srv.URL, "tok", "moose")
		if ok || !strings.Contains(note, "moose") {
			t.Errorf("act10BrainCheck = (%v, %q), want false and a note naming moose", ok, note)
		}
	})

	t.Run("the default brain is never checked", func(t *testing.T) {
		if ok, note := act10BrainCheck(context.Background(), &http.Client{}, "http://127.0.0.1:1", "tok", ""); !ok || note != "" {
			t.Errorf("act10BrainCheck with no brain named = (%v, %q), want (true, \"\") without any request", ok, note)
		}
	})

	t.Run("unreadable /brains does not block the run", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no such route", http.StatusNotFound)
		}))
		defer srv.Close()
		ok, note := act10BrainCheck(context.Background(), &http.Client{}, srv.URL, "tok", "codex")
		if !ok || note == "" {
			t.Errorf("act10BrainCheck = (%v, %q), want true with a note saying the brain could not be checked", ok, note)
		}
	})
}

// TestAct10Line checks the per-task line a human reads while the run is going: the task id, its tier, pass or fail, how many seconds it took, and the tool sequence it ran, all on one line.
func TestAct10Line(t *testing.T) {
	line := act10Line(act10Result{ID: "ring-address-bar", Tier: 0, Pass: true, Steps: []string{"observe_screen", "point_at"}, Seconds: 62.4, Answer: "ringed the address bar"})
	for _, want := range []string{"ring-address-bar", "tier 0", "PASS", "62.4", "observe_screen>point_at", "ringed the address bar"} {
		if !strings.Contains(line, want) {
			t.Errorf("act10Line = %q, want it to carry %q", line, want)
		}
	}
	failed := act10Line(act10Result{ID: "read-table", Tier: 0, Seconds: 5, Err: "ask: daemon returned 500"})
	if !strings.Contains(failed, "FAIL") || !strings.Contains(failed, "daemon returned 500") {
		t.Errorf("act10Line for a failed task = %q, want FAIL and the error", failed)
	}
}

// TestAct10Note checks the one line the scorecard keeps: which brain and tier the run used, how many tasks passed, and per task the seconds and the tool sequence, because a bare pass count says nothing about why a task failed.
func TestAct10Note(t *testing.T) {
	note := act10Note("codex", 0, []act10Result{
		{ID: "what-is-this", Pass: true, Steps: []string{"observe_screen"}, Seconds: 41.2, Answer: "Chrome"},
		{ID: "ring-address-bar", Steps: []string{"observe_screen", "point_at"}, Seconds: 62},
		{ID: "read-table", Seconds: 3, Err: "ask: daemon returned 500"},
	})
	for _, want := range []string{"codex", "tier 0", "1/3", "what-is-this", "41.2s", "observe_screen>point_at", "read-table", "daemon returned 500"} {
		if !strings.Contains(note, want) {
			t.Errorf("act10Note = %q, want it to carry %q", note, want)
		}
	}
	if strings.Contains(note, "\n") {
		t.Errorf("act10Note = %q, want a single line: the scorecard renders each note as one blockquote", note)
	}
}

// TestAct10AskSendsTheBrainName checks the one thing the --brain flag exists for: the name reaches POST /ask as its "brain" field, which is what makes the daemon answer through that brain instead of its configured default.
func TestAct10AskSendsTheBrainName(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": "ask-1"})
	}))
	defer srv.Close()

	id, err := act10Ask(context.Background(), &http.Client{}, srv.URL, "tok", "what is on my screen?", "codex")
	if err != nil {
		t.Fatalf("act10Ask: %v", err)
	}
	if id != "ask-1" {
		t.Errorf("id = %q, want ask-1", id)
	}
	if body["brain"] != "codex" {
		t.Errorf("/ask body = %v, want brain=codex", body)
	}
	if body["question"] != "what is on my screen?" {
		t.Errorf("/ask body = %v, want the question verbatim", body)
	}
}

// TestNeedsGeminiKey checks which selections of tracks actually need a Gemini API key. Track 10 needs none — it drives the running daemon over HTTP and scores what came back — so a run of track 10 alone must not be refused for a key it will never use, which matters on a night when the Gemini quota is spent and codex is the only brain that answers.
func TestNeedsGeminiKey(t *testing.T) {
	cases := []struct {
		tracks string
		want   bool
	}{
		{"10", false},
		{"1", true},
		{"1,10", true},
		{"10,3", true},
		{"", true},
	}
	for _, c := range cases {
		sel := map[string]bool{}
		for _, t := range strings.Split(c.tracks, ",") {
			sel[strings.TrimSpace(t)] = true
		}
		if got := needsGeminiKey(sel); got != c.want {
			t.Errorf("needsGeminiKey(%q) = %v, want %v", c.tracks, got, c.want)
		}
	}
}

// TestAct10RunTaskCountsTheRingOffTheStream drives the real ring task through the fake daemon with the events shaped the way the live daemon shapes them: two "tool" events per hop, and the overlay event carrying an id of its own, broadcast between point_at's own two events because the ring is drawn while that call is running. The ring must reach the result and the task's own pass rule, and the interleaved overlay must not break the fold that turns paired events back into hops.
func TestAct10RunTaskCountsTheRingOffTheStream(t *testing.T) {
	const token = "test-token"
	var ringTask act10Task
	for _, task := range act10Tasks {
		if task.ID == "ring-address-bar" {
			ringTask = task
		}
	}
	if ringTask.Pass == nil {
		t.Fatalf("act10Tasks has no ring-address-bar task")
	}

	srv := act10FakeDaemon(t, token, "ask-1", []string{
		sseLine("ask-1", "status", "Checking."),
		sseLine("ask-1", "tool", "observe_screen"),
		sseLine("ask-1", "tool", "observe_screen"),
		sseLine("ask-1", "tool", "point_at"),
		`data: {"id":"ask-2","type":"overlay","text":"{\"kind\":\"ring\",\"rects\":[{\"x\":10,\"y\":20,\"w\":300,\"h\":30}]}"}` + "\n\n",
		sseLine("ask-1", "tool", "point_at"),
		sseLine("ask-1", "answer", "ringed the address bar"),
		sseLine("ask-1", "done", ""),
	}, nil, nil)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "codex", ringTask)

	if got.Err != "" {
		t.Fatalf("act10RunTask error: %s", got.Err)
	}
	if strings.Join(got.Steps, ">") != "observe_screen>point_at" {
		t.Errorf("Steps = %v, want [observe_screen point_at]", got.Steps)
	}
	if !got.Ring {
		t.Errorf("Ring = false, want the overlay ring recorded on the result so a failed ring task can be read from the run table")
	}
	if !got.Pass {
		t.Errorf("Pass = false; steps=%v ring=%v answer=%q", got.Steps, got.Ring, got.Answer)
	}
	if !strings.Contains(act10Line(got), "ring") {
		t.Errorf("act10Line = %q, want it to show that a ring was drawn", act10Line(got))
	}
}

// TestAct10DeleteConversation checks the DELETE call track 10 makes to clean up after itself: the right method and path, the token header the way every other IPC route needs it, nil on the 204 the daemon answers with, and an error naming the status for anything else.
func TestAct10DeleteConversation(t *testing.T) {
	var gotMethod, gotPath, gotToken string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Ora-Token")
		w.WriteHeader(status)
	}))
	defer srv.Close()

	if err := act10DeleteConversation(context.Background(), &http.Client{}, srv.URL, "tok", "conv-9"); err != nil {
		t.Fatalf("act10DeleteConversation: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/conversations/conv-9" || gotToken != "tok" {
		t.Errorf("got method=%q path=%q token=%q, want DELETE /conversations/conv-9 with token tok", gotMethod, gotPath, gotToken)
	}

	status = http.StatusNotFound
	err := act10DeleteConversation(context.Background(), &http.Client{}, srv.URL, "tok", "conv-9")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to name the 404 the daemon answered with", err)
	}
}

// TestAct10RunTaskDeletesItsOwnConversation covers track 10's cleanup: every eval run leaves behind the conversations its tasks opened, cluttering the window's real chat list, so each task must delete the conversation it created once it is done — whatever its own pass rule said — and a delete the daemon refuses must be noted on the result rather than allowed to touch Pass or Err, because the eval's job is measuring, not tidying.
func TestAct10RunTaskDeletesItsOwnConversation(t *testing.T) {
	const token = "test-token"

	t.Run("a passing task deletes its conversation", func(t *testing.T) {
		var deleted []string
		task := act10Task{ID: "t", Pass: func(steps []string, ring bool, answer string, details []string) bool { return true }}
		srv := act10FakeDaemon(t, token, "ask-1", []string{
			sseAnswerLine("ask-1", "all done", "conv-1"),
			sseLine("ask-1", "done", ""),
		}, &deleted, nil)
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "", task)

		if !got.Pass {
			t.Fatalf("Pass = false, want true")
		}
		if strings.Join(deleted, ",") != "conv-1" {
			t.Errorf("deleted = %v, want [conv-1]", deleted)
		}
		if got.Note != "" {
			t.Errorf("Note = %q, want empty on a delete that succeeds", got.Note)
		}
	})

	t.Run("a failing task still deletes", func(t *testing.T) {
		var deleted []string
		task := act10Task{ID: "t", Pass: func(steps []string, ring bool, answer string, details []string) bool { return false }}
		srv := act10FakeDaemon(t, token, "ask-2", []string{
			sseAnswerLine("ask-2", "all done", "conv-2"),
			sseLine("ask-2", "done", ""),
		}, &deleted, nil)
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "", task)

		if got.Pass {
			t.Fatalf("Pass = true, want false")
		}
		if strings.Join(deleted, ",") != "conv-2" {
			t.Errorf("deleted = %v, want [conv-2] even though the task's own pass rule failed", deleted)
		}
	})

	t.Run("no conversation_id deletes nothing", func(t *testing.T) {
		var deleted []string
		task := act10Task{ID: "t", Pass: func(steps []string, ring bool, answer string, details []string) bool { return true }}
		srv := act10FakeDaemon(t, token, "ask-3", []string{
			sseLine("ask-3", "answer", "no conversation was ever opened for this one"),
			sseLine("ask-3", "done", ""),
		}, &deleted, nil)
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		act10RunTask(ctx, &http.Client{}, srv.URL, token, "", task)

		if len(deleted) != 0 {
			t.Errorf("deleted = %v, want no DELETE call at all when the answer event carried no conversation_id", deleted)
		}
	})

	t.Run("a refused delete leaves the result intact and adds a note", func(t *testing.T) {
		var deleted []string
		task := act10Task{ID: "t", Pass: func(steps []string, ring bool, answer string, details []string) bool { return true }}
		srv := act10FakeDaemon(t, token, "ask-4", []string{
			sseAnswerLine("ask-4", "all done", "conv-4"),
			sseLine("ask-4", "done", ""),
		}, &deleted, map[string]bool{"conv-4": true})
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		got := act10RunTask(ctx, &http.Client{}, srv.URL, token, "", task)

		if !got.Pass || got.Err != "" {
			t.Fatalf("got = %+v, want Pass=true Err=\"\" even though the delete was refused: a store that refuses a delete is not a reason to lose the result", got)
		}
		if strings.Join(deleted, ",") != "conv-4" {
			t.Errorf("deleted = %v, want [conv-4]", deleted)
		}
		if got.Note == "" || !strings.Contains(got.Note, "conv-4") {
			t.Errorf("Note = %q, want it to name conv-4 and explain the delete was refused", got.Note)
		}
	})
}
