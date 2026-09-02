package memory

import (
	"strings"
	"testing"
)

func TestNormalize_ContentNeedsContext(t *testing.T) {
	obs := Normalize("Netflix", "Suits S6E12 The Painting", "Watching Suits, courtroom scene with Harvey.")
	if obs.Content == "" {
		t.Fatal("expected content")
	}
	if obs.Context.App != "Netflix" || obs.Context.Title == "" {
		t.Fatalf("context incomplete: %+v", obs.Context)
	}
	if obs.Context.Domain != DomainPersonal {
		t.Fatalf("domain=%q want personal", obs.Context.Domain)
	}
	if obs.Context.Kind != KindMoment {
		t.Fatalf("kind=%q want moment", obs.Context.Kind)
	}
	doc := obs.Document()
	if !strings.Contains(doc, "Netflix") || !strings.Contains(doc, "Suits") {
		t.Fatalf("Document missing context+content: %q", doc)
	}
	if !strings.Contains(doc, "courtroom") && !strings.Contains(doc, "Watching") {
		t.Fatalf("Document missing substance: %q", doc)
	}
}

func TestNormalize_StripsChromeAndCaps(t *testing.T) {
	raw := strings.Repeat("⣿⣿⣿⣿⣿⣿⣿⣿\n", 40) + "\nActually editing hybrid search in the editor.\n"
	obs := Normalize("Code", "main.go", raw)
	if strings.Contains(obs.Content, "⣿") {
		t.Fatalf("chrome leaked into content: %q", obs.Content)
	}
	if wordCount(obs.Content) > signalMaxWords {
		t.Fatalf("content not capped: %d words", wordCount(obs.Content))
	}
	if !strings.Contains(obs.Content, "hybrid") {
		t.Fatalf("lost real text: %q", obs.Content)
	}
}

func TestNormalize_TitleOnlyWhenEmpty(t *testing.T) {
	obs := Normalize("mpv", "movie.mkv", "")
	if obs.Content != "movie.mkv" {
		t.Fatalf("content=%q want title", obs.Content)
	}
	if obs.Context.SignalKind != SignalTitleOnly {
		t.Fatalf("signal_kind=%q", obs.Context.SignalKind)
	}
}

func TestNormalize_StripsObjectReplacement(t *testing.T) {
	obs := Normalize("firefox", "Article", "Hello \uFFFC world \uFFFC there from a long enough sentence about climate.")
	if strings.Contains(obs.Content, "\uFFFC") {
		t.Fatalf("U+FFFC remained: %q", obs.Content)
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

// The capped path is what summaries and the compiler read, where one short observation really is better than a dump. It must not change.
func TestNormalize_StillCapsForTheCompiler(t *testing.T) {
	raw := strings.Repeat("word ", 500)
	if got := len(strings.Fields(Normalize("Code", "x", raw).Content)); got > 120 {
		t.Errorf("capped content is %d words, want at most 120", got)
	}
}
