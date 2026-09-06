// Package util holds the small general-purpose helpers more than one package needs and the standard library does not provide as one call: string capping and matching, an atomic file write, and small HTTP-response helpers.
package util

import (
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
