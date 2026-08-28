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
		{Subject: "identity", Content: "The user is Mahadevan KS."},
		{Subject: "trupti-hosmani", Content: "Trupti Hosmani is a colleague at Credibl. Last worked together on 20 August 2026."},
	}, "# Meeting minutes\n## Attendees\n**In the meeting**\n- Trupti Hosmani (spoke)", "  10:00  Chrome — Meet")

	for _, want := range []string{
		"identity: The user is Mahadevan KS.",
		"trupti-hosmani: Trupti Hosmani is a colleague at Credibl.",
		"Trupti Hosmani (spoke)",
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
	store := &fakeStore{personal: []db.PersonalEntry{{Subject: "identity", Content: "The user is Mahadevan KS."}}}
	r, _, _ := newTestRecorder(t, store)
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		return "```json\n{\"updates\":[{\"subject\":\"trupti-hosmani\",\"content\":\"Trupti Hosmani is a colleague at Credibl. Last worked together on 28 August 2026.\"}]}\n```", nil
	}

	r.updatePersonalContext(context.Background(), "# Meeting minutes", time.Now(), time.Now())

	if got := store.personalWrites["trupti-hosmani"]; !strings.Contains(got, "colleague at Credibl") {
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
			return `{"updates":[{"subject":"parth-patil","content":"Parth Patil is a colleague at Credibl."}]}`, nil
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
	if got := store.personalWrites["parth-patil"]; !strings.Contains(got, "Parth Patil") {
		t.Errorf("processing a meeting did not update personal context, writes: %v", store.personalWrites)
	}
}
