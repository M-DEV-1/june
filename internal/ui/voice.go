package ui

import (
	"fmt"
	"strings"

	"ora/internal/config"
)

// voiceAction is the result of parsing a "/voice ..." input: either show the list of available voices, or attempt to set a specific one.
type voiceAction int

const (
	voiceActionList voiceAction = iota
	voiceActionSet
	voiceActionPreview
)

// parseVoiceCommand splits the text after "/voice" into an action: empty or "list" shows the voices, "preview <name>" plays a sample without changing anything, anything else is a candidate name to set. Bare "/voice" is handled separately in ui.go as the Voice-Only mode switch, so this only ever sees "/voice <something>".
func parseVoiceCommand(arg string) (action voiceAction, name string) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.EqualFold(arg, "list") {
		return voiceActionList, ""
	}
	if rest, ok := cutFold(arg, "preview"); ok {
		return voiceActionPreview, strings.TrimSpace(rest)
	}
	return voiceActionSet, arg
}

// cutFold reports whether s starts with the word prefix (case-insensitively) followed by nothing or whitespace, and if so returns the remainder.
func cutFold(s, prefix string) (rest string, ok bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	if len(s) == len(prefix) {
		return "", true
	}
	if s[len(prefix)] != ' ' && s[len(prefix)] != '\t' {
		return "", false
	}
	return s[len(prefix)+1:], true
}

// voiceListMessage renders the available voices with the current one marked.
func voiceListMessage(current string) string {
	if current == "" {
		current = config.DefaultVoice
	}
	var b strings.Builder
	b.WriteString("Available voices:\n")
	for _, v := range config.AvailableVoices {
		if strings.EqualFold(v, current) {
			b.WriteString("  * " + v + " (current)\n")
		} else {
			b.WriteString("    " + v + "\n")
		}
	}
	b.WriteString("\nUsage: /voice <name> to switch, /voice list to see this again.")
	return b.String()
}

// voiceUnknownMessage is shown when the user asks to set a voice that isn't in config.AvailableVoices.
func voiceUnknownMessage(name string) string {
	return fmt.Sprintf("Unknown voice: %q. Use /voice list to see available voices.", name)
}

// voiceSetMessage confirms a successful voice change.
func voiceSetMessage(name string) string {
	return fmt.Sprintf("Voice set to %s — reconnecting to apply...", name)
}

// voicePreviewUsageMessage is shown for "/voice preview" with no name given.
func voicePreviewUsageMessage() string {
	return "Usage: /voice preview <name> (e.g. /voice preview Kore)"
}

// voicePreviewStartMessage is shown right before the sample plays.
func voicePreviewStartMessage(name string) string {
	return fmt.Sprintf("Playing preview of %s...", name)
}

// voicePreviewErrorMessage is shown when the preview TTS call or playback fails.
func voicePreviewErrorMessage(name string, err error) string {
	return fmt.Sprintf("Failed to preview %s: %v", name, err)
}
