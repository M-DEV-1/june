// Package util holds the small general-purpose helpers more than one package needs and the standard library does not provide as one call: string capping and matching, a slice tail, file checks and an atomic file write, small HTTP helpers, and starting and stopping a local server child.
package util

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ContainsAny reports whether s contains any of the given substrings, compared without case. Input: any string, and the substrings to look for, which must already be lower case — s is lowercased here, they are not. Output: true on the first substring found, false when none of them is there or none was given.
func ContainsAny(s string, subs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// NonSpeechLine matches whisper's marker for a stretch with no words in it — "[BLANK_AUDIO]", "(upbeat music)", "[SOUND]". A quiet stream, whether a dictation into a silent room or the microphone side of a call, is otherwise nothing but these.
var NonSpeechLine = regexp.MustCompile(`^[\[(][^)\]]*[)\]]$`)

// Runes returns s unchanged if it has n runes or fewer. Otherwise it returns the first n runes of s, with nothing appended. Input: a string and a rune count cap. Output: s itself, or its first n runes; a cap of zero or less gives the empty string.
func Runes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// UTF8Bytes returns s unchanged if it is maxBytes bytes or fewer. Otherwise it returns the longest prefix of s that is at most maxBytes bytes long and does not split a multi-byte UTF-8 rune in half. Input: a string and a byte count cap. Output: s itself, or a UTF-8-safe byte prefix of it; a cap of zero or less gives the empty string.
func UTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// OneLine collapses every run of whitespace in s to a single space and trims the ends, so text captured off a screen or returned by a model prints as one row instead of carrying its own layout. Input: any string. Output: the same words separated by single spaces; whitespace-only input gives the empty string.
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// RunesEllipsis returns s unchanged when it has n runes or fewer, and otherwise its first n runes followed by a single "…" so the reader can see the text was cut. Input: a string and a rune count cap. Output: s, or its first n runes plus an ellipsis; a cap of zero or less returns s unchanged rather than a bare ellipsis.
func RunesEllipsis(s string, n int) string {
	if n <= 0 {
		return s
	}
	cut := Runes(s, n)
	if cut == s {
		return s
	}
	return cut + "…"
}

// RunesNote cuts s to n runes and, when it cut anything, puts note on a line of its own after it so the reader knows text is missing. Input: the text, the most runes to keep, and the note. Output: s unchanged when it fits, else its first n runes, a newline and note.
func RunesNote(s string, n int, note string) string {
	cut := Runes(s, n)
	if cut == s {
		return s
	}
	return cut + "\n" + note
}

// LogHead is the first 300 runes of s on one line, which is as much of a command's error output as belongs in one log line. Input: any string, including one that is not ASCII. Output: s with its whitespace collapsed, cut on a rune boundary with an ellipsis when it was longer than 300 runes.
func LogHead(s string) string {
	return RunesEllipsis(OneLine(s), 300)
}

// FirstLine returns the first non-empty line of s, trimmed. Input: any string. Output: that line, or "" when s is blank.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// LastN keeps the newest n entries of a slice. Input: any slice and how many of its last entries to keep. Output: the last n, or the whole slice when it is already that short or shorter.
func LastN[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}

// DesktopLine names the machine the session runs on, for a prompt: the desktop environment and display server from the session's own variables, and the distribution from /etc/os-release. Nothing is written in; a variable that is not set is left out. Output: one line such as "Ubuntu 24.04.4 LTS, desktop ubuntu:GNOME on wayland", or "Linux" when nothing is known.
func DesktopLine() string {
	var parts []string
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				parts = append(parts, strings.Trim(v, `"`))
			}
		}
	}
	if d := os.Getenv("XDG_CURRENT_DESKTOP"); d != "" {
		line := "desktop " + d
		if s := os.Getenv("XDG_SESSION_TYPE"); s != "" {
			line += " on " + s
		}
		parts = append(parts, line)
	}
	if len(parts) == 0 {
		return "Linux"
	}
	return strings.Join(parts, ", ")
}
