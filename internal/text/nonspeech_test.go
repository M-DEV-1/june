package text

import "testing"

// TestNonSpeechLine checks the shared pattern against whisper's bracketed and parenthesised markers, and against an ordinary line of speech that must not match.
func TestNonSpeechLine(t *testing.T) {
	for _, line := range []string{"[BLANK_AUDIO]", "(upbeat music)", "[SOUND]", "[ Silence ]"} {
		if !NonSpeechLine.MatchString(line) {
			t.Errorf("NonSpeechLine.MatchString(%q) = false, want true", line)
		}
	}
	if NonSpeechLine.MatchString("this is what someone actually said") {
		t.Error("NonSpeechLine matched a line of real speech")
	}
}
