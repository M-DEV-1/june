package util

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"under limit returns unchanged", "abc", 10, "abc"},
		{"exact limit returns unchanged", "abc", 3, "abc"},
		{"cuts on rune count, not byte count", "ab￼cd", 3, "ab￼"}, // U+FFFC is 3 bytes but counts as one rune
		{"empty string", "", 5, ""},
		{"n zero drops everything", "abc", 0, ""},
		{"a negative cap drops everything rather than panicking", "abc", -1, ""},
	} {
		if got := Runes(tc.s, tc.n); got != tc.want {
			t.Errorf("%s: Runes(%q, %d) = %q, want %q", tc.name, tc.s, tc.n, got, tc.want)
		}
	}
}

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

// TestOneLine pins the whitespace-collapse shape multiple packages once re-implemented as strings.Join(strings.Fields(s), " ").
func TestOneLine(t *testing.T) {
	for _, tc := range []struct{ name, s, want string }{
		{"already one line", "a b c", "a b c"},
		{"newlines and tabs collapse to single spaces", "a\n\tb\r\n  c", "a b c"},
		{"leading and trailing whitespace goes", "  a b  ", "a b"},
		{"whitespace-only becomes empty", " \n\t ", ""},
		{"empty stays empty", "", ""},
		{"unicode spaces count as whitespace", "a b", "a b"},
	} {
		if got := OneLine(tc.s); got != tc.want {
			t.Errorf("%s: OneLine(%q) = %q, want %q", tc.name, tc.s, got, tc.want)
		}
	}
}

// TestRunesEllipsis pins the cap-then-mark shape multiple packages once re-implemented, including the non-positive cap that must not append a marker to nothing.
func TestRunesEllipsis(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"under the cap is unchanged and unmarked", "abc", 10, "abc"},
		{"exactly at the cap is unchanged and unmarked", "abc", 3, "abc"},
		{"over the cap is cut and marked", "abcdef", 3, "abc…"},
		{"cuts on runes, not bytes", "ab￼cd", 3, "ab￼…"},
		{"a non-positive cap returns s unchanged rather than a bare marker", "abc", 0, "abc"},
		{"a negative cap returns s unchanged", "abc", -1, "abc"},
		{"empty stays empty", "", 5, ""},
	} {
		if got := RunesEllipsis(tc.s, tc.n); got != tc.want {
			t.Errorf("%s: RunesEllipsis(%q, %d) = %q, want %q", tc.name, tc.s, tc.n, got, tc.want)
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

// TestNonSpeechLine checks the shared pattern against whisper's bracketed and parenthesised markers, and against an ordinary line of speech that must not match.
func TestNonSpeechLine(t *testing.T) {
	for _, line := range []string{"[BLANK_AUDIO]", "(upbeat music)", "[SOUND]", "[ Silence ]"} {
		if !NonSpeechLine.MatchString(line) {
			t.Errorf("NonSpeechLine.MatchString(%q) = false, want true", line)
		}
	}
	if NonSpeechLine.MatchString("this is what someone actually said") {
		t.Error("NonSpeechLine matched a line of real speech")
	}
}

// TestContainsAny checks the case-insensitive substring match, including that no substring given never matches.
func TestContainsAny(t *testing.T) {
	if !ContainsAny("Hello World", "world") {
		t.Error("ContainsAny should match case-insensitively")
	}
	if ContainsAny("Hello World", "xyz") {
		t.Error("ContainsAny should not match an absent substring")
	}
	if ContainsAny("Hello World") {
		t.Error("ContainsAny with no substrings given should never match")
	}
}
