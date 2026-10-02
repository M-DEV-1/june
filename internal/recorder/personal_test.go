package recorder

import (
	"testing"
)

// A person whose name exists only in the transcript is a recogniser's guess, and on 2026-09-03 one such guess ("Oshveln" for Sorrek) became a permanent personal-context entry. The updater is told so, and the code refuses any new person subject whose evidence the model marks as heard-only, logging it as unsure instead.
func TestPersonalUpdate_HeardOnlyNamesAreNotWritten(t *testing.T) {
	out := `{"updates":[{"subject":"oshveln","content":"Contact of Zemna's (heard as \"Oshveln\")","heard_only":true},{"subject":"vexil-quorin","content":"Colleague; last worked 3 Sep"}],"unsure":["oshveln: name heard only in speech"]}`
	updates := parsePersonalUpdates(out)
	if len(updates) != 1 || updates[0].Subject != "vexil-quorin" {
		t.Fatalf("updates = %+v, want only the screen-backed entry", updates)
	}
}
