package recorder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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
			return `{"updates":[{"subject":"plandor-morvex","content":"Plandor Morvex is a colleague at Acme."}]}`, nil
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
	if got := store.personalWrites["plandor-morvex"]; !strings.Contains(got, "Plandor Morvex") {
		t.Errorf("processing a meeting did not update personal context, writes: %v", store.personalWrites)
	}
}

// A person whose name exists only in the transcript is a recogniser's guess, and on 2026-09-03 one such guess ("Oshveln" for Sorrek) became a permanent personal-context entry. The updater is told so, and the code refuses any new person subject whose evidence the model marks as heard-only, logging it as unsure instead.
func TestPersonalUpdate_HeardOnlyNamesAreNotWritten(t *testing.T) {
	if !strings.Contains(personalUpdateInstruction, "heard as") {
		t.Fatal("updater instruction must explain that a name marked heard-as in the minutes is not evidence for a new person")
	}
	out := `{"updates":[{"subject":"oshveln","content":"Contact of Zemna's (heard as \"Oshveln\")","heard_only":true},{"subject":"vexil-quorin","content":"Colleague; last worked 3 Sep"}],"unsure":["oshveln: name heard only in speech"]}`
	updates := parsePersonalUpdates(out)
	if len(updates) != 1 || updates[0].Subject != "vexil-quorin" {
		t.Fatalf("updates = %+v, want only the screen-backed entry", updates)
	}
}
