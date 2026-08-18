package memory

import (
	"strings"
	"testing"
)

func TestParseScreenSight_JSON(t *testing.T) {
	s := ParseScreenSight(`{"user_activity":"watching Suits","visible_text":["Harvey: object","Donna: noted"],"summary":"courtroom"}`)
	if s.UserActivity != "watching Suits" {
		t.Fatalf("activity=%q", s.UserActivity)
	}
	if len(s.VisibleText) != 2 || s.VisibleText[0] != "Harvey: object" {
		t.Fatalf("visible=%v", s.VisibleText)
	}
	if s.Summary != "courtroom" {
		t.Fatalf("summary=%q", s.Summary)
	}
}

func TestParseScreenSight_ProseFallback(t *testing.T) {
	s := ParseScreenSight("just a paragraph about the screen")
	if s.Summary != "just a paragraph about the screen" || s.UserActivity != "" {
		t.Fatalf("%+v", s)
	}
}

func TestParseScreenSight_Empty(t *testing.T) {
	s := ParseScreenSight("  ")
	if s.UserActivity != "" || s.Summary != "" || len(s.VisibleText) != 0 {
		t.Fatalf("expected zero, got %+v", s)
	}
}

func TestComposeMoment_PrefersStructured(t *testing.T) {
	got := ComposeMoment("editing compiler.go", []string{"func Ingest", "wordFlushLimit"}, "raw a11y dump")
	if !strings.Contains(got, "editing compiler.go") || !strings.Contains(got, "func Ingest") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "raw a11y") {
		t.Fatalf("fallback leaked: %q", got)
	}
}

func TestComposeMoment_FallbackWhenEmpty(t *testing.T) {
	if got := ComposeMoment("", nil, "Netflix · Suits"); got != "Netflix · Suits" {
		t.Fatalf("got %q", got)
	}
}
