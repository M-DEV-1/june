// Package text holds the small string helpers shared across packages: capping output length for display or budget reasons, and matching text against a list of substrings.
package text

import (
	"strings"
	"unicode/utf8"
)

// Runes returns s unchanged if it has n runes or fewer. Otherwise it returns the first n runes of s, with nothing appended. Input: a string and a rune count cap. Output: s itself, or its first n runes; a cap of zero or less gives the empty string.
func Runes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
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
