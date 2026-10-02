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
