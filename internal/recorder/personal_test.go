package recorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// TestPersonalUpdatePrompt_CarriesTheStoreAndTheBar checks the updater is asked the right question: it sees exactly what is stored today, and it is told what does not belong in the store.
func TestPersonalUpdatePrompt_CarriesTheStoreAndTheBar(t *testing.T) {
	prompt := personalUpdatePrompt([]db.PersonalEntry{
		{Subject: "identity", Content: "The user is Alex Rivera."},
		{Subject: "priya-shah", Content: "Priya Shah is a colleague at Acme. Last worked together on 20 August 2026."},
	}, "# Meeting minutes\n## Attendees\n**In the meeting**\n- Priya Shah (spoke)", "  10:00  Chrome — Meet")

	for _, want := range []string{
		"identity: The user is Alex Rivera.",
		"priya-shah: Priya Shah is a colleague at Acme.",
		"Priya Shah (spoke)",
		"Chrome — Meet",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the updater prompt is missing %q:\n%s", want, prompt)
		}
	}
	for _, bar := range []string{"already", "username", "identity"} {
		if !strings.Contains(strings.ToLower(prompt), bar) {
			t.Errorf("the updater prompt never states the bar about %q", bar)
		}
	}
}

// TestUpdatePersonalContext_WritesANewPerson is the write path: a colleague the minutes place in the meeting becomes an entry.
func TestUpdatePersonalContext_WritesANewPerson(t *testing.T) {
	store := &fakeStore{personal: []db.PersonalEntry{{Subject: "identity", Content: "The user is Alex Rivera."}}}
	r, _, _ := newTestRecorder(t, store)
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		return "```json\n{\"updates\":[{\"subject\":\"priya-shah\",\"content\":\"Priya Shah is a colleague at Acme. Last worked together on 28 August 2026.\"}]}\n```", nil
	}

	r.updatePersonalContext(context.Background(), "# Meeting minutes", time.Now(), time.Now())

	if got := store.personalWrites["priya-shah"]; !strings.Contains(got, "colleague at Acme") {
		t.Errorf("the new person was not written: %q", got)
	}
}

// TestUpdatePersonalContext_WritesNothingWhenTheStoreAlreadySaysIt covers the dedupe case and every shape of nothing-to-do: an empty list, junk, and a failed call all have to leave the store alone.
func TestUpdatePersonalContext_WritesNothingWhenTheStoreAlreadySaysIt(t *testing.T) {
	for _, c := range []struct {
		name string
		out  string
		err  error
	}{
		{"nothing new", `{"updates":[]}`, nil},
		{"no updates key", `{}`, nil},
		{"prose instead of json", "There is nothing to update.", nil},
		{"blank subject", `{"updates":[{"subject":"  ","content":"x"}]}`, nil},
		{"blank content", `{"updates":[{"subject":"someone","content":""}]}`, nil},
		{"call failed", "", errors.New("gemini is down")},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{}
			r, _, _ := newTestRecorder(t, store)
			r.minutes = func(ctx context.Context, prompt string) (string, error) { return c.out, c.err }

			r.updatePersonalContext(context.Background(), "# Meeting minutes", time.Now(), time.Now())

			if len(store.personalWrites) != 0 {
				t.Errorf("the store was written to: %v", store.personalWrites)
			}
		})
	}
}

// TestMinutesPipeline_UpdatesPersonalContext checks the updater actually runs as part of processing a meeting, not just in isolation.
func TestMinutesPipeline_UpdatesPersonalContext(t *testing.T) {
	store := &fakeStore{}
	r, _, _ := newTestRecorder(t, store)
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		if strings.HasPrefix(prompt, personalUpdateInstruction) {
			return `{"updates":[{"subject":"nitin-patil","content":"Nitin Patil is a colleague at Acme."}]}`, nil
		}
		return "# Minutes\n\n- ship friday", nil
	}
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s, err := r.stop()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := r.process(context.Background(), s); err != nil {
		t.Fatalf("process: %v", err)
	}
	if got := store.personalWrites["nitin-patil"]; !strings.Contains(got, "Nitin Patil") {
		t.Errorf("processing a meeting did not update personal context, writes: %v", store.personalWrites)
	}
}

// A person whose name exists only in the transcript is a recogniser's guess, and on 2026-09-03 one such guess ("Ashar" for Sneha) became a permanent personal-context entry. The updater is told so, and the code refuses any new person subject whose evidence the model marks as heard-only, logging it as unsure instead.
func TestPersonalUpdate_HeardOnlyNamesAreNotWritten(t *testing.T) {
	if !strings.Contains(personalUpdateInstruction, "heard as") {
		t.Fatal("updater instruction must explain that a name marked heard-as in the minutes is not evidence for a new person")
	}
	out := `{"updates":[{"subject":"ashar","content":"Contact of Alex's (heard as \"Ashar\")","heard_only":true},{"subject":"priya-shah","content":"Colleague; last worked 3 Sep"}],"unsure":["ashar: name heard only in speech"]}`
	updates := parsePersonalUpdates(out)
	if len(updates) != 1 || updates[0].Subject != "priya-shah" {
		t.Fatalf("updates = %+v, want only the screen-backed entry", updates)
	}
}
