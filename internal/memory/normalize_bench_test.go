package memory

import (
	"strings"
	"testing"
)

// benchCapture is one accessibility read of a busy window: a hundred thousand characters of lines, some of them the browser chrome Normalize drops.
var benchCapture = strings.Repeat("Back Forward Reload\nthe user was reading about cuda out of memory errors and what causes them\n\nAddress bar\n", 1200)

// BenchmarkNormalize measures the whole clean-up every screen capture runs through before it is stored and embedded.
func BenchmarkNormalize(b *testing.B) {
	b.SetBytes(int64(len(benchCapture)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Normalize("Brave", "cuda out of memory - Search", benchCapture)
	}
}

// TestWordCountMatchesFields pins the counter against the strings.Fields it replaces, over the whitespace shapes an accessibility capture actually carries, so the cheaper count can never disagree with the split it stands in for.
func TestWordCountMatchesFields(t *testing.T) {
	for _, s := range []string{
		"", " ", "\t\n\r ",
		"one",
		"  leading and trailing  ",
		"tabs\tand\nnewlines\r\nmixed",
		"unicode nbsp emspace",
		strings.Repeat("word ", 1000),
		string([]byte{0xff, 0xfe, 0x20, 0xc3, 0x28}),
		strings.Repeat("\U0001F600 ", 100),
	} {
		if got, want := wordCount(s), len(strings.Fields(s)); got != want {
			t.Errorf("wordCount(%.30q) = %d, want %d (len(strings.Fields))", s, got, want)
		}
	}
}

// TestExtractSignalCapsWithoutASeparateFlatteningPass pins that capping to a word budget still returns single-space-separated words, which is what lets the separate whitespace-collapsing pass ahead of it go.
func TestExtractSignalCapsWithoutASeparateFlatteningPass(t *testing.T) {
	got, _ := extractSignal("alpha   beta\n\n\tgamma\r\ndelta", "title", 3)
	if got != "alpha beta gamma" {
		t.Errorf("extractSignal = %q, want %q", got, "alpha beta gamma")
	}
	whole, _ := extractSignal("alpha   beta\n\n\tgamma\r\ndelta", "title", 0)
	if whole != "alpha beta gamma delta" {
		t.Errorf("extractSignal with no word cap = %q, want %q", whole, "alpha beta gamma delta")
	}
}

// TestIsChromeLineJunkMatchIsCaseInsensitive pins the fixed junk lines isChromeLine drops, in the casings a real accessibility tree reports them in, so the allocation-free compare that replaces lowercasing every captured line keeps matching exactly the same lines.
func TestIsChromeLineJunkMatchIsCaseInsensitive(t *testing.T) {
	for _, line := range []string{"cookie", "COOKIE", "Cookie", "Accept all", "ACCEPT ALL", "Skip to main content", "skip to content"} {
		if !isChromeLine(line) {
			t.Errorf("isChromeLine(%q) = false, want true", line)
		}
	}
	for _, line := range []string{"cookies", "we use a cookie", "accept all of the terms", "the user read the docs"} {
		if isChromeLine(line) {
			t.Errorf("isChromeLine(%q) = true, want false", line)
		}
	}
}

// TestPreferVisionTailHeadWordCount pins that a capture whose head is longer than the word budget and whose tail is short vision prose still returns just the tail, which is the decision the head's word count drives.
func TestPreferVisionTailHeadWordCount(t *testing.T) {
	head := strings.Repeat("chrome navigation label\n", 400)
	tail := "The user is reading a page about cuda out of memory errors and what causes them."
	if got := preferVisionTail(head + "\n" + tail); got != tail {
		t.Errorf("preferVisionTail returned %d bytes, want just the %d-byte tail", len(got), len(tail))
	}
	short := "one two\n\nthree four"
	if got := preferVisionTail(short); got != short {
		t.Errorf("preferVisionTail(%q) = %q, want it unchanged", short, got)
	}
}
