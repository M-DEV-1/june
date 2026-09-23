package agent

// lessons_test.go covers the two halves of the continual lesson loop that live in this package: automaticLessons, which reads a lesson straight off a hop sequence with no model call, and AfterScreenRun's reflective half, which does make one.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAutomaticLessonsFindsAFailThenASuccessOnTheSameElement reproduces the night's real Teams failure: a click on "Add title" that failed via the accessibility path, then a later click on the very same button that went through. automaticLessons must pair the two by role and label alone — the item number is not part of the identity, since observe_screen mints a fresh one on every look — and write the one line the task asks for.
func TestAutomaticLessonsFindsAFailThenASuccessOnTheSameElement(t *testing.T) {
	hops := []ToolHop{
		{Name: "observe_screen", Result: "Microsoft Teams · New meeting"},
		{Name: "click", Args: map[string]any{"n": float64(25)}, Result: `error: could not click [25] entry "Add title": no accessible action`},
		{Name: "observe_screen", Result: "Microsoft Teams · New meeting"},
		{Name: "click", Args: map[string]any{"n": float64(25)}, Result: `clicked [25] entry "Add title" via pointer; the window is now "Microsoft Teams · New meeting"`},
	}
	got := automaticLessons("Teams", hops, lessonCapPerRun)
	if len(got) != 1 {
		t.Fatalf("automaticLessons returned %d lines, want 1:\n%v", len(got), got)
	}
	want := `Teams: entry "Add title": no accessible action; worked via pointer`
	if got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

// lessonSpyBrain is a toolTestBrain that also answers the lesson store side, recording every AddLesson and ScoreLessonsUsed call so a test can assert on what AfterScreenRun did without a real store.
type lessonSpyBrain struct {
	*toolTestBrain
	added []string
}

func (b *lessonSpyBrain) AddLesson(ctx context.Context, app, goal, lesson string) (int64, error) {
	b.added = append(b.added, lesson)
	return int64(len(b.added)), nil
}

func (b *lessonSpyBrain) ScoreLessonsUsed(ctx context.Context, ids []int64, outcome string) error {
	return nil
}

// teamsAgent builds an Agent whose front app always reads as "Teams", wired to a fake Gemini backend that answers reply to every request it gets, for testing AfterScreenRun's reflective half without a real model call.
func teamsAgent(t *testing.T, brain *lessonSpyBrain, reply string) (*Agent, *int) {
	t.Helper()
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"`+reply+`"}]}}]}`)
	}))
	t.Cleanup(backend.Close)
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, brain, nil, "test-key")
	a.rememberTarget(ScreenTarget{Window: "Teams"})
	return a, &requests
}

// The reflective call asks for "nothing" when there is nothing, and the model almost never says just that. On the real machine it hedged — "Nothing worth flagging — just browsing, a Meet call, and a PDF read" — and because the guard only matched the bare word, 19 of the 21 lessons in the store on 2026-09-12 were that sentence in different clothes. Every one of them is retrievable, and every one would be put in front of a later run as guidance.
func TestAfterScreenRunWritesNoLessonForAHedgedNothingReply(t *testing.T) {
	hedges := []string{
		"Nothing worth flagging — just browsing, a Meet call, and a PDF read.",
		"Nothing jumps out as a mistake there — reading the OASIS_IF file was fine.",
		"Nothing — those were just browsing sessions.",
		"nothing worth flagging here",
	}
	for _, reply := range hedges {
		brain := &lessonSpyBrain{toolTestBrain: &toolTestBrain{}}
		a, _ := teamsAgent(t, brain, reply)
		a.AfterScreenRun(t.Context(), reflectiveTrace(), "ok")
		if len(brain.added) != 0 {
			t.Errorf("reply %q wrote lessons %v, want none", reply, brain.added)
		}
	}
}

// A real lesson must still get through. "Nothing" appearing inside a sentence that goes on to say something is not a hedge, and the guard must not swallow it.
func TestAfterScreenRunStillWritesARealLesson(t *testing.T) {
	for _, reply := range []string{
		"Use Ctrl+W to close a tab instead of hunting for a close button.",
		"Nothing on the page worked until I focused the field first, so click it before typing.",
	} {
		brain := &lessonSpyBrain{toolTestBrain: &toolTestBrain{}}
		a, _ := teamsAgent(t, brain, reply)
		a.AfterScreenRun(t.Context(), reflectiveTrace(), "ok")
		if len(brain.added) != 1 || brain.added[0] != reply {
			t.Errorf("reply %q wrote lessons %v, want just that reply", reply, brain.added)
		}
	}
}

// The reflective call was asked "what would you do differently next time in Brave Browser?" and nothing else — not the question, not a single step. So it answered about whatever its own memory lookup had surfaced, which is how a run that opened Spotify came to file a lesson about Spearman's correlation. What the run actually did has to be in the question.
func TestReflectiveCallIsToldWhatTheRunDid(t *testing.T) {
	brain := &lessonSpyBrain{toolTestBrain: &toolTestBrain{}}
	var asked string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"nothing"}]}}]}`)
	}))
	t.Cleanup(backend.Close)
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	a := NewAgent(nil, nil, brain, nil, "test-key")
	a.rememberTarget(ScreenTarget{Window: "Brave Browser"})
	a.AfterScreenRun(t.Context(), TurnTrace{
		Question: "close all tabs except the Hotstar one",
		ToolHops: []ToolHop{
			{Name: "observe_screen", Result: "Brave Browser · Notifications - Brave"},
			{Name: "click", Args: map[string]any{"n": float64(99)}, Result: "error: there is no element 99 in the last observe_screen list"},
			{Name: "click", Args: map[string]any{"n": float64(3)}, Result: `clicked [3] push button "Close" via press`},
		},
	}, "ok")

	// The question the run was asked, and the step that failed, both have to reach the model — a reflection on a run it cannot see is a reflection on nothing.
	for _, want := range []string{"close all tabs except the Hotstar one", "there is no element 99"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the reflective request did not carry %q; it was:\n%s", want, asked)
		}
	}
}

// reflectiveTrace is a run long enough to earn the reflective call (reflectiveLessonMinHops) with nothing in it for automaticLessons to find, so a test that cares only about the reply sees exactly one lesson slot in play.
func reflectiveTrace() TurnTrace {
	return TurnTrace{
		Question: "add a title",
		ToolHops: []ToolHop{
			{Name: "observe_screen", Result: "Microsoft Teams · New meeting"},
			{Name: "click", Args: map[string]any{"n": float64(3)}, Result: `clicked [3] entry "Add title" via pointer`},
			{Name: "type_text", Args: map[string]any{"text": "Standup"}, Result: "typed 7 characters"},
		},
	}
}
