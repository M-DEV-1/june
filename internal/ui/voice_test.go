package ui

import (
	"strings"
	"testing"
)

// parseVoiceCommand turns everything after "/voice" into an action plus a voice name.
func TestParseVoiceCommand(t *testing.T) {
	cases := []struct {
		in         string
		wantAction voiceAction
		wantName   string
	}{
		{"", voiceActionList, ""},
		{"   ", voiceActionList, ""},
		{"list", voiceActionList, ""},
		{"List", voiceActionList, ""},
		{"LIST", voiceActionList, ""},
		{"  list  ", voiceActionList, ""},
		{"Kore", voiceActionSet, "Kore"},
		{"  Zephyr  ", voiceActionSet, "Zephyr"},
		{"preview Kore", voiceActionPreview, "Kore"},
		{"Preview Zephyr", voiceActionPreview, "Zephyr"},
		{"PREVIEW Zephyr", voiceActionPreview, "Zephyr"},
		{"  preview   Zephyr  ", voiceActionPreview, "Zephyr"},
		{"preview", voiceActionPreview, ""},
		// A name that merely starts with "preview" must not be read as the keyword: "previewer" has no word boundary after it, so it falls through to a set.
		{"previewer", voiceActionSet, "previewer"},
	}
	for _, tc := range cases {
		action, name := parseVoiceCommand(tc.in)
		if action != tc.wantAction || name != tc.wantName {
			t.Errorf("parseVoiceCommand(%q) = %v, %q; want %v, %q", tc.in, action, name, tc.wantAction, tc.wantName)
		}
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

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
