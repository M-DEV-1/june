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
