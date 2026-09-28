package agent

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// A TTS response that refused the prompt and one that simply came back empty have the same shape — no usable parts — so a refusal was reported to the window as "returned no audio", which reads as a bug in June rather than a decision by the model. Measured on 2026-09-07: "Hi, I'm June. This is how I sound." is blocked PROHIBITED_CONTENT, because a synthetic voice introducing itself by name reads to the filter as impersonation.
func TestNoAudioReason_TellsARefusalFromAnEmptyAnswer(t *testing.T) {
	blocked := &genai.GenerateContentResponse{PromptFeedback: &genai.GenerateContentResponsePromptFeedback{BlockReason: genai.BlockedReasonProhibitedContent}}
	if got := noAudioReason(blocked); !strings.Contains(got, "refused") || !strings.Contains(got, "PROHIBITED_CONTENT") {
		t.Errorf("a blocked response reads as %q, want it to name the refusal and its reason", got)
	}
	for name, resp := range map[string]*genai.GenerateContentResponse{
		"nothing at all": nil,
		"no feedback":    {},
		"empty feedback": {PromptFeedback: &genai.GenerateContentResponsePromptFeedback{}},
	} {
		if got := noAudioReason(resp); strings.Contains(got, "refused") {
			t.Errorf("%s reads as %q, want it not to claim a refusal nobody reported", name, got)
		}
	}
}
