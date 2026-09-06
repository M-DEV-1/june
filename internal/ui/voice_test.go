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

// TestVoiceListMessage is a table over voiceListMessage's marking rule: it marks the given voice
// current and lists the others, and an empty current voice falls back to the default Iapetus.
func TestVoiceListMessage(t *testing.T) {
	cases := []struct {
		name    string
		current string
		want    []string
	}{
		{"marks the current voice", "Kore", []string{"Kore", "(current)", "Zephyr", "Usage:"}},
		{"defaults when current is empty", "", []string{"Iapetus", "(current)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := voiceListMessage(tc.current)
			if !containsAll(msg, tc.want...) {
				t.Errorf("expected voice list message to contain %v, got:\n%s", tc.want, msg)
			}
		})
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
