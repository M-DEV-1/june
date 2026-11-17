package recorder

import (
	"strings"
	"testing"
)

// The diarizer over-splits when it estimates the number of speakers, so the model must not be told that each speaker number is exactly one person.
func TestMinutesInstruction_AllowsOnePersonUnderSeveralNumbers(t *testing.T) {
	if !strings.Contains(minutesInstruction, "One person may appear under more than one number") {
		t.Error("the minutes instruction no longer says one person may appear under more than one speaker number")
	}
	for _, claim := range []string{"Each number is one person", "are two different people"} {
		if strings.Contains(minutesInstruction, claim) {
			t.Errorf("the minutes instruction still says %q", claim)
		}
	}
}
