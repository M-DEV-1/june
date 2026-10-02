package dream

import (
	"context"

	"sort"
	"strings"
	"testing"
	"time"

	"june/internal/db"
	"june/internal/db/dbtest"
)

// insertActRun writes one act run through the db package's own insert method, so the stage reads exactly what a real ask files.
func insertActRun(t *testing.T, store *db.Store, question, outcome string, steps []db.ActStep) {
	t.Helper()
	if _, err := store.AddActRun(context.Background(), db.ActRun{
		Question: question,
		Model:    "gemini-2.5-flash",
		Outcome:  outcome,
		Steps:    steps,
	}); err != nil {
		t.Fatalf("AddActRun(%q): %v", question, err)
	}
}

// procedureNotes reads back the content of every note the stage wrote, sorted, so a test can compare it against a fixed list.
func procedureNotes(t *testing.T, store *db.Store) []string {
	t.Helper()
	notes, err := store.NotesOfKindSince(context.Background(), procedureNoteKind, time.Time{})
	if err != nil {
		t.Fatalf("NotesOfKindSince: %v", err)
	}
	out := make([]string, len(notes))
	for i, n := range notes {
		out[i] = n.Content
	}
	sort.Strings(out)
	return out
}

// Every ok run becomes one note per distinct goal, written in plain words from the tool names and arguments — item numbers and labels, never an accessibility ref, an object path or a raw result dump. A run that typed a passphrase into a password box is written up with neither the passphrase nor the box's contents, and a run that ended in an error teaches nothing and is never written up.
func TestProceduresStage_WritesOnePlainNotePerGoal(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)

	insertActRun(t, store, "  Open the settings page  ", "ok", []db.ActStep{
		{Name: "observe_screen", Args: map[string]any{}, Result: "code · Settings\n[1] push button \"Menu\" (10,20)"},
		{Name: "click", Args: map[string]any{"n": 3.0}, Result: "clicked [3] push button \"Settings\" via press:1.42/org/a11y/atspi/accessible/17; call observe_screen to see the result"},
		{Name: "scroll_to", Args: map[string]any{"n": 8.0}, Result: "scrolled to [8] link \"Privacy\"; call observe_screen to see the page now"},
		{Name: "click", Args: map[string]any{"n": 5.0}, Result: "clicked [5] push button \":1.42/org/a11y/atspi/accessible/17\" via press; call observe_screen to see the result"},
	})
	insertActRun(t, store, "search for the invoice", "ok", []db.ActStep{
		{Name: "point_at", Args: map[string]any{"n": 2.0, "label": "search box"}, Result: "ringed [2] entry \"Search\""},
		{Name: "type_text", Args: map[string]any{"text": "invoice 401", "enter": true}, Result: "typed 12 characters; call observe_screen to see the result"},
	})
	const secret = "correct horse battery staple"
	insertActRun(t, store, "unlock my password manager", "ok", []db.ActStep{
		{Name: "observe_screen", Args: map[string]any{}, Result: "keepassxc · Unlock Database"},
		{Name: "click", Args: map[string]any{"n": 4.0}, Result: `clicked [4] password text "` + secret + `" via press; call observe_screen to see the result`},
		{Name: "type_text", Args: map[string]any{"text": secret, "enter": true}, Result: "typed 28 characters; call observe_screen to see the result"},
	})
	insertActRun(t, store, "log in to the bank", "error", []db.ActStep{
		{Name: "observe_screen", Args: map[string]any{}, Result: "browser · Bank"},
		{Name: "click", Args: map[string]any{"n": 1.0}, Result: "could not click [1] push button \"Log in\": no such element"},
	})

	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	rep, err := r.proceduresStage(ctx, night)
	if err != nil {
		t.Fatalf("proceduresStage: %v", err)
	}
	if rep.goals != 3 || rep.written != 3 {
		t.Errorf("report = %+v, want 3 goals and 3 notes written", rep)
	}

	got := procedureNotes(t, store)
	want := []string{
		`How I did Open the settings page: looked at the screen, clicked item 3 (Settings), scrolled to item 8 (Privacy), clicked item 5.`,
		// The ring was aimed at an entry, whose label may be whatever the user has typed into it, so the note says the role rather than any name for it; and what was typed is never quoted.
		`How I did search for the invoice: pointed at item 2 (entry), typed into the box in front and pressed Enter.`,
		`How I did unlock my password manager: looked at the screen, clicked item 4 (password text), typed into the box in front and pressed Enter.`,
	}
	if len(got) != len(want) {
		t.Fatalf("notes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("note %d =\n  %q\nwant\n  %q", i, got[i], want[i])
		}
	}
	for _, n := range got {
		for _, banned := range []string{"/org/a11y", "atspi", ":1.", "push button", "observe_screen", "[3]", secret, "bank"} {
			if strings.Contains(n, banned) {
				t.Errorf("note %q carries %q, which is machine detail and must never reach a note", n, banned)
			}
		}
	}
}

// Running the stage twice in a row writes nothing the second time: a goal whose note already exists is skipped on its exact "How I did <goal>" prefix.
func TestProceduresStage_SecondRunWritesNothingNew(t *testing.T) {
	ctx := context.Background()
	store := dbtest.Open(t)
	night := at(23, 30).Format(time.DateOnly)

	insertActRun(t, store, "open the settings page", "ok", []db.ActStep{
		{Name: "observe_screen", Args: map[string]any{}, Result: "code · Settings"},
		{Name: "click", Args: map[string]any{"n": 3.0}, Result: "clicked [3] push button \"Settings\" via press; call observe_screen to see the result"},
	})

	r := newRunner(store, &fakeBrain{}, yesProbes(), at(23, 30))
	first, err := r.proceduresStage(ctx, night)
	if err != nil {
		t.Fatalf("first proceduresStage: %v", err)
	}
	if first.written != 1 {
		t.Fatalf("first report = %+v, want one note written", first)
	}

	second, err := r.proceduresStage(ctx, night)
	if err != nil {
		t.Fatalf("second proceduresStage: %v", err)
	}
	if second.written != 0 || second.skipped != 1 {
		t.Errorf("second report = %+v, want nothing written and one goal skipped as already known", second)
	}
	if notes := procedureNotes(t, store); len(notes) != 1 {
		t.Errorf("notes after two runs = %#v, want exactly one", notes)
	}
}
