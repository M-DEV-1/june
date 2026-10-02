package memory

import (
	"strings"
	"testing"
)

// ParseScreenSight reads the vision model's JSON reply, and keeps a prose reply as the summary.
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

	// A reply that is not JSON is kept as the summary rather than lost.
	if s := ParseScreenSight("just a paragraph about the screen"); s.Summary != "just a paragraph about the screen" || s.UserActivity != "" {
		t.Fatalf("prose fallback = %+v", s)
	}
}

// ComposeMoment strips U+FFFC from the structured-moment path: AT-SPI reports every image, video, and icon as an object replacement character, and ComposeMoment's output overrides the Normalize-cleaned text in db.WriteEpisode — so without stripping here, a screenful of thumbnails is stored and embedded as a run of U+FFFC that then outranks real memories. A capture that is nothing but object replacement characters must compose to "" rather than to a run of U+FFFC, so callers can tell there was no content.
func TestComposeMoment_StripsObjectReplacementChars(t *testing.T) {
	got := ComposeMoment("watching ￼ a video", []string{"￼￼￼", "the actual caption ￼"}, "")
	if strings.ContainsRune(got, '￼') {
		t.Errorf("expected no U+FFFC in composed moment, got %q", got)
	}
	if !strings.Contains(got, "the actual caption") {
		t.Errorf("expected real text to survive stripping, got %q", got)
	}
	if strings.Contains(got, "\n\n") || strings.HasPrefix(got, "\n") {
		t.Errorf("expected lines emptied by stripping to be dropped, got %q", got)
	}

	if got := ComposeMoment("￼￼", []string{"￼", "￼￼￼"}, ""); got != "" {
		t.Errorf("a capture that is nothing but object replacement characters composed to %q, want empty", got)
	}

	// The structured activity and visible text win over the raw fallback.
	if got := ComposeMoment("editing compiler.go", []string{"func Ingest"}, "raw a11y dump"); !strings.Contains(got, "func Ingest") || strings.Contains(got, "raw a11y") {
		t.Errorf("ComposeMoment with structured input = %q, want it without the fallback", got)
	}
}
