package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAddActRun_RoundTrips writes a run with two steps and reads it back through ActRuns, newest first.
func TestAddActRun_RoundTrips(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	older, err := store.AddActRun(ctx, ActRun{
		Question:   "what is on my screen",
		Model:      "gemini-2.5-flash",
		Outcome:    "ok",
		Answer:     "a browser window",
		DurationMS: 1200,
		Steps: []ActStep{
			{Name: "observe_screen", Args: map[string]any{}, Result: "1. button Save"},
			{Name: "click", Args: map[string]any{"n": float64(1)}, Result: "clicked [1] button \"Save\""},
		},
	})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	newer, err := store.AddActRun(ctx, ActRun{
		Question: "click the login button",
		Model:    "gemini-2.5-flash",
		Outcome:  "error",
		Error:    "boom",
		Steps:    []ActStep{{Name: "click", Args: map[string]any{"n": float64(2)}, Result: "no such element"}},
	})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ActRuns returned %d runs, want 2", len(runs))
	}
	if runs[0].ID != newer || runs[1].ID != older {
		t.Fatalf("ActRuns order = %d, %d, want newest first (%d, %d)", runs[0].ID, runs[1].ID, newer, older)
	}

	got := runs[1]
	if got.Question != "what is on my screen" || got.Model != "gemini-2.5-flash" || got.Outcome != "ok" || got.Answer != "a browser window" || got.DurationMS != 1200 {
		t.Errorf("older run = %+v, want the fields it was stored with", got)
	}
	if len(got.Steps) != 2 || got.Steps[0].Name != "observe_screen" || got.Steps[1].Name != "click" {
		t.Fatalf("older run steps = %+v, want observe_screen then click", got.Steps)
	}
	if got.Steps[1].Result != "clicked [1] button \"Save\"" {
		t.Errorf("click step result = %q", got.Steps[1].Result)
	}

	failed := runs[0]
	if failed.Outcome != "error" || failed.Error != "boom" {
		t.Errorf("newer run = %+v, want outcome error carrying its message", failed)
	}
}

// TestAddActRun_CutsStepResultsTo300Runes checks a long tool result is stored capped at 300 runes, so the steps column never grows unbounded on a chatty tool.
func TestAddActRun_CutsStepResultsTo300Runes(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	long := ""
	for i := 0; i < 500; i++ {
		long += "a"
	}
	id, err := store.AddActRun(ctx, ActRun{
		Question: "scroll down",
		Outcome:  "ok",
		Steps:    []ActStep{{Name: "scroll_to", Args: map[string]any{"n": float64(1)}, Result: long}},
	})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	var got ActRun
	for _, r := range runs {
		if r.ID == id {
			got = r
		}
	}
	if len(got.Steps) != 1 {
		t.Fatalf("steps = %+v, want 1", got.Steps)
	}
	if n := len([]rune(got.Steps[0].Result)); n != 300 {
		t.Errorf("step result rune count = %d, want 300", n)
	}
}

// TestActRuns_ZeroLimitReturnsNothing matches the zero-limit behaviour of ListConversations: a caller that asks for none gets none, not an error.
func TestActRuns_ZeroLimitReturnsNothing(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.AddActRun(ctx, ActRun{Question: "x", Outcome: "ok"}); err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	runs, err := store.ActRuns(ctx, 0)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("ActRuns(0) = %d runs, want 0", len(runs))
	}
}

// TestAddActRun_StepsWithNoStepsStoresEmptyArray checks a run with no steps round-trips to a nil/empty Steps slice rather than an unmarshal error.
func TestAddActRun_StepsWithNoStepsStoresEmptyArray(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id, err := store.AddActRun(ctx, ActRun{Question: "x", Outcome: "ok"})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}
	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	var got ActRun
	for _, r := range runs {
		if r.ID == id {
			got = r
		}
	}
	if len(got.Steps) != 0 {
		t.Errorf("Steps = %+v, want none", got.Steps)
	}
}

// TestActStepJSONShape checks the JSON an ActStep encodes to, since steps_json is read by whatever later builds the replay learning off of it.
func TestActStepJSONShape(t *testing.T) {
	b, err := json.Marshal(ActStep{Name: "click", Args: map[string]any{"n": float64(1)}, Result: "ok"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m["name"] != "click" || m["result"] != "ok" {
		t.Errorf("json = %s, want name and result fields", b)
	}
}

// TestAddActRun_DropsTheTypedTextAndCapsTheOtherArguments pins the privacy half of AddActRun: the string the user dictated into type_text is never stored, because a run's steps are read back by the nightly procedures stage and rendered into a note that is embedded into the search index, so a passphrase typed once would come back on a later unrelated question. Every other argument is kept, and any long one is cut the same way a result is.
func TestAddActRun_DropsTheTypedTextAndCapsTheOtherArguments(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	const secret = "correct horse battery staple"
	long := strings.Repeat("b", 900)
	id, err := store.AddActRun(ctx, ActRun{
		Question: "type my passphrase",
		Outcome:  "ok",
		Steps: []ActStep{
			{Name: "type_text", Args: map[string]any{"text": secret, "enter": true}, Result: "typed 28 characters"},
			{Name: "point_at", Args: map[string]any{"n": float64(2), "label": long}, Result: `ringed [2] entry "Search"`},
		},
	})
	if err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	var stepsJSON string
	if err := store.db.QueryRowContext(ctx, `SELECT steps_json FROM act_runs WHERE id = ?`, id).Scan(&stepsJSON); err != nil {
		t.Fatalf("read steps_json: %v", err)
	}
	if strings.Contains(stepsJSON, secret) {
		t.Errorf("steps_json holds the typed text: %s", stepsJSON)
	}

	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runs) != 1 || len(runs[0].Steps) != 2 {
		t.Fatalf("runs = %+v, want one run of two steps", runs)
	}
	typed := runs[0].Steps[0]
	if _, ok := typed.Args["text"]; ok {
		t.Errorf("type_text args = %+v, want no text argument", typed.Args)
	}
	if enter, _ := typed.Args["enter"].(bool); !enter {
		t.Errorf("type_text args = %+v, want the enter flag kept", typed.Args)
	}
	label, _ := runs[0].Steps[1].Args["label"].(string)
	if n := len([]rune(label)); n != stepArgRuneCap {
		t.Errorf("stored label rune count = %d, want the argument cap %d", n, stepArgRuneCap)
	}
	if n, _ := runs[0].Steps[1].Args["n"].(float64); n != 2 {
		t.Errorf("point_at args = %+v, want n kept as 2", runs[0].Steps[1].Args)
	}
}

// TestAddActRun_KeepsOnlyTheScreenHops pins what an act run is a record of: the screen work. A turn that looked at the screen also searches memory, reads files and runs commands, and those hops carry the results of that work; keeping them puts a memory hit or a file's contents into a row that exists only to teach the replay learning how a screen goal was reached.
func TestAddActRun_KeepsOnlyTheScreenHops(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.AddActRun(ctx, ActRun{
		Question: "open the settings page",
		Outcome:  "ok",
		Steps: []ActStep{
			{Name: "query_memory", Args: map[string]any{"query": "settings"}, Result: "a private memory hit"},
			{Name: "observe_screen", Args: map[string]any{}, Result: "code · Settings"},
			{Name: "shell_exec", Args: map[string]any{"command": "cat ~/.ssh/id_ed25519"}, Result: "a private key"},
			{Name: "click", Args: map[string]any{"n": float64(3)}, Result: `clicked [3] push button "Settings" via press`},
		},
	}); err != nil {
		t.Fatalf("AddActRun: %v", err)
	}

	runs, err := store.ActRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ActRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one", runs)
	}
	var names []string
	for _, s := range runs[0].Steps {
		names = append(names, s.Name)
	}
	if len(names) != 2 || names[0] != "observe_screen" || names[1] != "click" {
		t.Fatalf("stored steps = %v, want only the screen hops, in call order", names)
	}
	for _, s := range runs[0].Steps {
		if strings.Contains(s.Result, "private") {
			t.Errorf("step %q kept a non-screen result: %q", s.Name, s.Result)
		}
	}
}
