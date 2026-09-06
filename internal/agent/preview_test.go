package agent

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// A TTS response that refused the prompt and one that simply came back empty have the same shape — no usable parts — so a refusal was reported to the window as "returned no audio", which reads as a bug in Ora rather than a decision by the model. Measured on 2026-09-07: "Hi, I'm Ora. This is how I sound." is blocked PROHIBITED_CONTENT, because a synthetic voice introducing itself by name reads to the filter as impersonation.
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

// The preview line must never introduce itself by name: that is the exact shape the TTS safety filter refuses.
func TestPreviewPhrase_ClaimsNoIdentity(t *testing.T) {
	lower := strings.ToLower(previewPhrase)
	for _, banned := range []string{"i'm ora", "i am ora", "this is ora"} {
		if strings.Contains(lower, banned) {
			t.Errorf("previewPhrase %q says %q, which the TTS filter blocks as impersonation", previewPhrase, banned)
		}
	}
}

// The TTS model's free tier allows ten requests a day for the whole project, so a spent allowance has to read as a spent allowance and not as a broken speaker.
func TestQuotaSpent_TellsAnExhaustedAllowanceFromAnythingElse(t *testing.T) {
	spent := errors.New("Error 429, Message: You exceeded your current quota, Status: RESOURCE_EXHAUSTED")
	if !quotaSpent(spent) {
		t.Error("a 429 quota error does not read as a spent allowance, so the window would say the voice could not be played")
	}
	for name, err := range map[string]error{
		"nothing":        nil,
		"a real failure": errors.New("dial tcp: connection refused"),
		"a refusal":      errors.New("the model refused the line it was given (PROHIBITED_CONTENT)"),
	} {
		if quotaSpent(err) {
			t.Errorf("%s reads as a spent allowance", name)
		}
	}
}

// A voice is synthesised once and kept, because thirty voices against a ten-a-day allowance is otherwise unusable. The path is per voice and case-insensitive, so picking "puck" and "Puck" reads the same file.
func TestPreviewPath_IsOnePerVoice(t *testing.T) {
	if previewPath("Puck") == previewPath("Kore") {
		t.Error("two voices share one preview file, so one would be played for the other")
	}
	if previewPath("Puck") != previewPath("puck") {
		t.Error("the same voice in another casing reads a different file, so it would be synthesised twice")
	}
	if !strings.HasSuffix(previewPath("Puck"), "puck.pcm") {
		t.Errorf("preview path = %q, want it named after the voice", previewPath("Puck"))
	}
}
