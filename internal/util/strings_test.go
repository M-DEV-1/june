package util

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestUTF8Bytes_SplitsExactlyOnMultiByteRuneBoundary verifies truncating mid multi-byte UTF-8 char (e.g. the 3-byte U+FFFC object-replacement char a11y capture is full of) backs off to the last complete rune instead of returning a broken half-character.
func TestUTF8Bytes_SplitsExactlyOnMultiByteRuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		s        string
		maxBytes int
		want     string
	}{
		{"under limit returns unchanged", "abc", 10, "abc"},
		// "ab" (2 bytes) + U+FFFC (3 bytes) == 5 bytes total. Cutting at byte 4 lands one byte into the 3-byte rune.
		{"cut lands inside a multi-byte rune", "ab￼", 4, "ab"},
		{"cut lands exactly on a rune boundary", "ab￼", 5, "ab￼"},
		{"a negative cap drops everything rather than panicking", "abc", -1, ""},
	} {
		got := UTF8Bytes(tc.s, tc.maxBytes)
		if got != tc.want {
			t.Errorf("%s: UTF8Bytes(%q, %d) = %q, want %q", tc.name, tc.s, tc.maxBytes, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: UTF8Bytes(%q, %d) = %q is not valid UTF-8", tc.name, tc.s, tc.maxBytes, got)
		}
	}
}

// TestOneLineAndRunesSurviveHugeAndInvalidUTF8 checks the two helpers every capture path funnels model output and screen text through do not panic or return broken UTF-8 for a megabyte of text or for bytes that are not UTF-8 at all.
func TestOneLineAndRunesSurviveHugeAndInvalidUTF8(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("word \n\t", 200000),
		string([]byte{0xff, 0xfe, 0x00, 0xc3, 0x28, 0x80}),
		strings.Repeat(string([]byte{0xff, 0x80}), 100000),
	} {
		one := OneLine(s)
		if !utf8.ValidString(one) && utf8.ValidString(s) {
			t.Errorf("OneLine turned valid UTF-8 into invalid UTF-8")
		}
		if got := RunesEllipsis(s, 10); utf8.RuneCountInString(got) > 11 {
			t.Errorf("RunesEllipsis(%d runes, 10) returned %d runes", utf8.RuneCountInString(s), utf8.RuneCountInString(got))
		}
		if got := Runes(s, 10); utf8.RuneCountInString(got) > 10 {
			t.Errorf("Runes(%d runes, 10) returned %d runes", utf8.RuneCountInString(s), utf8.RuneCountInString(got))
		}
	}
}
