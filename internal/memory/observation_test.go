package memory

import (
	"strings"
	"testing"
)

// Normalize drops braille chrome and the U+FFFC that AT-SPI reports for every image, and caps the moment at signalMaxWords, without losing the real text.
func TestNormalize_StripsChromeAndCaps(t *testing.T) {
	raw := strings.Repeat("⣿⣿⣿⣿⣿⣿⣿⣿\n", 40) + "\nActually editing \uFFFC hybrid search in the editor.\n"
	obs := Normalize("Code", "main.go", raw)
	if strings.Contains(obs.Content, "⣿") || strings.Contains(obs.Content, "\uFFFC") {
		t.Fatalf("chrome leaked into content: %q", obs.Content)
	}
	if wordCount(obs.Content) > signalMaxWords {
		t.Fatalf("content not capped: %d words", wordCount(obs.Content))
	}
	if !strings.Contains(obs.Content, "hybrid") {
		t.Fatalf("lost real text: %q", obs.Content)
	}
}

// The 120-word cap exists so a multi-KB accessibility dump does not drown FTS and embeddings. With a local embedder and passage-level chunking neither of those costs applies any more, and the cap was discarding 99% of a long capture before anything could index it — a 96,061-character screen reached the vector index as roughly 700 characters. NormalizeFull does the same cleaning without the cap.
func TestNormalizeFull_KeepsTheWholeCleanedCapture(t *testing.T) {
	body := strings.Repeat("a real sentence about the work being done. ", 200) // ~8,400 chars
	raw := "Search\nSettings\n" + body

	capped := Normalize("Code", "search.go", raw).Document()
	full := NormalizeFull("Code", "search.go", raw).Document()

	if len(full) <= len(capped) {
		t.Fatalf("full document is %d chars and capped is %d — the cap should be the only difference", len(full), len(capped))
	}
	if !strings.Contains(full, "a real sentence") {
		t.Error("full document lost the content it is meant to keep")
	}
	// The cleaning itself is covered by the Normalize tests above; both paths share extractSignal and only the word cap differs.
	if strings.Contains(full, "\n\n") {
		t.Error("whitespace collapsing stopped applying — the cleaning must still run, only the cap is dropped")
	}
}
