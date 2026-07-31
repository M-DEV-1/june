package memory

import (
	"strings"
	"testing"
	"time"
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

func TestFormatLine_Moment(t *testing.T) {
	at := time.Date(2026, 7, 24, 20, 53, 0, 0, time.UTC)
	line := FormatLine(KindMoment, DomainPersonal, at, "Netflix", "Suits", "Season 6 Ep 12", 200)
	if !strings.Contains(line, "moment") || !strings.Contains(line, "personal") {
		t.Fatalf("line=%q", line)
	}
	if !strings.Contains(line, "Netflix") || !strings.Contains(line, "Season 6") {
		t.Fatalf("missing context/content: %q", line)
	}
}

func TestKindOf(t *testing.T) {
	if KindOf("episode") != KindMoment {
		t.Fatal()
	}
	if KindOf("note") != KindFact {
		t.Fatal()
	}
	if KindOf("digest") != KindPeriod {
		t.Fatal()
	}
	if KindOf("thread") != KindArc {
		t.Fatal()
	}
}
