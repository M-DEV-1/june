package ui

import (
	"errors"
	"strings"
	"testing"
)

var errFakePreview = errors.New("fake preview failure")

func TestParseVoiceCommand_EmptyArgMeansList(t *testing.T) {
	action, name := parseVoiceCommand("")
	if action != voiceActionList {
		t.Errorf("expected empty arg to produce voiceActionList, got %v", action)
	}
	if name != "" {
		t.Errorf("expected no name for list action, got %q", name)
	}
}

func TestParseVoiceCommand_WhitespaceOnlyMeansList(t *testing.T) {
	action, _ := parseVoiceCommand("   ")
	if action != voiceActionList {
		t.Errorf("expected whitespace-only arg to produce voiceActionList, got %v", action)
	}
}

func TestParseVoiceCommand_ListKeywordCaseInsensitive(t *testing.T) {
	for _, in := range []string{"list", "List", "LIST", "  list  "} {
		action, _ := parseVoiceCommand(in)
		if action != voiceActionList {
			t.Errorf("input %q: expected voiceActionList, got %v", in, action)
		}
	}
}

func TestParseVoiceCommand_NameMeansSet(t *testing.T) {
	action, name := parseVoiceCommand("Kore")
	if action != voiceActionSet {
		t.Errorf("expected voiceActionSet, got %v", action)
	}
	if name != "Kore" {
		t.Errorf("expected name Kore, got %q", name)
	}
}

func TestParseVoiceCommand_TrimsWhitespaceAroundName(t *testing.T) {
	_, name := parseVoiceCommand("  Zephyr  ")
	if name != "Zephyr" {
		t.Errorf("expected trimmed name Zephyr, got %q", name)
	}
}

func TestParseVoiceCommand_PreviewMeansPreviewAction(t *testing.T) {
	action, name := parseVoiceCommand("preview Kore")
	if action != voiceActionPreview {
		t.Errorf("expected voiceActionPreview, got %v", action)
	}
	if name != "Kore" {
		t.Errorf("expected preview name Kore, got %q", name)
	}
}

func TestParseVoiceCommand_PreviewCaseInsensitiveKeyword(t *testing.T) {
	for _, in := range []string{"Preview Zephyr", "PREVIEW Zephyr", "  preview   Zephyr  "} {
		action, name := parseVoiceCommand(in)
		if action != voiceActionPreview {
			t.Errorf("input %q: expected voiceActionPreview, got %v", in, action)
		}
		if name != "Zephyr" {
			t.Errorf("input %q: expected name Zephyr, got %q", in, name)
		}
	}
}

func TestParseVoiceCommand_PreviewWithNoNameIsEmpty(t *testing.T) {
	action, name := parseVoiceCommand("preview")
	if action != voiceActionPreview {
		t.Errorf("expected voiceActionPreview for bare 'preview', got %v", action)
	}
	if name != "" {
		t.Errorf("expected empty name for bare 'preview', got %q", name)
	}
}

func TestParseVoiceCommand_NameStartingWithPreviewIsNotMisparsed(t *testing.T) {
	// A voice name that merely starts with "preview" (none currently exist,
	// but guard the parser logic) must not be treated as the preview keyword
	// unless followed by a word boundary. "previewer" has no boundary after
	// "preview", so it should fall through to voiceActionSet.
	action, name := parseVoiceCommand("previewer")
	if action != voiceActionSet {
		t.Errorf("expected voiceActionSet for 'previewer', got %v", action)
	}
	if name != "previewer" {
		t.Errorf("expected name previewer, got %q", name)
	}
}

func TestVoiceListMessage_MarksCurrentVoice(t *testing.T) {
	msg := voiceListMessage("Kore")
	if !containsAll(msg, "Kore", "(current)", "Zephyr", "Usage:") {
		t.Errorf("expected voice list message to mention Kore as current and list others, got:\n%s", msg)
	}
}

func TestVoiceListMessage_DefaultsWhenCurrentEmpty(t *testing.T) {
	msg := voiceListMessage("")
	if !containsAll(msg, "Iapetus", "(current)") {
		t.Errorf("expected empty current voice to fall back to default Iapetus, got:\n%s", msg)
	}
}

func TestVoiceUnknownMessage_MentionsName(t *testing.T) {
	msg := voiceUnknownMessage("Bogus")
	if !containsAll(msg, "Bogus", "/voice list") {
		t.Errorf("expected unknown-voice message to mention the bad name and hint at /voice list, got:\n%s", msg)
	}
}

func TestVoiceSetMessage_MentionsName(t *testing.T) {
	msg := voiceSetMessage("Kore")
	if !containsAll(msg, "Kore") {
		t.Errorf("expected set message to mention the new voice name, got:\n%s", msg)
	}
}

func TestVoicePreviewUsageMessage_MentionsUsage(t *testing.T) {
	msg := voicePreviewUsageMessage()
	if !containsAll(msg, "/voice preview") {
		t.Errorf("expected preview usage message to mention '/voice preview', got:\n%s", msg)
	}
}

func TestVoicePreviewStartMessage_MentionsName(t *testing.T) {
	msg := voicePreviewStartMessage("Kore")
	if !containsAll(msg, "Kore") {
		t.Errorf("expected preview start message to mention the voice name, got:\n%s", msg)
	}
}

func TestVoicePreviewErrorMessage_MentionsNameAndError(t *testing.T) {
	msg := voicePreviewErrorMessage("Kore", errFakePreview)
	if !containsAll(msg, "Kore", "fake preview failure") {
		t.Errorf("expected preview error message to mention name and underlying error, got:\n%s", msg)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
