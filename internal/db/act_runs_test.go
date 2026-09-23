package db

import (
	"context"
	"strings"
	"testing"
)

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
