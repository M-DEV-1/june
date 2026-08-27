package memory

import (
	"encoding/json"
	"strings"
)

// ScreenSight is the structured vision extract for one moment: what the user is doing, plus the visible text worth remembering as separate chunks — not one capped prose blob.
type ScreenSight struct {
	UserActivity string   `json:"user_activity"`
	VisibleText  []string `json:"visible_text"`
	Summary      string   `json:"summary"`
}

// ParseScreenSight decodes a vision JSON object. Unknown/empty input yields a zero value, not an error — callers fall back to accessibility text.
func ParseScreenSight(raw string) ScreenSight {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ScreenSight{}
	}
	var s ScreenSight
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ScreenSight{Summary: raw}
	}
	s.UserActivity = strings.TrimSpace(s.UserActivity)
	s.Summary = strings.TrimSpace(s.Summary)
	cleaned := make([]string, 0, len(s.VisibleText))
	for _, line := range s.VisibleText {
		if line = strings.TrimSpace(line); line != "" {
			cleaned = append(cleaned, line)
		}
	}
	s.VisibleText = cleaned
	return s
}

// ComposeMoment builds the searchable screen_text for a moment. Activity first, then visible chunks, else fallback (title or a11y). Capped at signalMaxWords so FTS/embeddings stay short.
// Object replacement characters are stripped from every part and parts left empty by that are dropped: AT-SPI renders each image, video, and icon as U+FFFC, and this output overrides Normalize's cleaned text in db.WriteEpisode, so anything not stripped here is stored and embedded as-is.
func ComposeMoment(activity string, visible []string, fallback string) string {
	var parts []string
	if a := StripObjectChars(activity); a != "" {
		parts = append(parts, a)
	}
	for _, line := range visible {
		if line = StripObjectChars(line); line != "" {
			parts = append(parts, line)
		}
	}
	if len(parts) == 0 {
		return StripObjectChars(fallback)
	}
	if wordCount(strings.Join(parts, " ")) <= signalMaxWords {
		return strings.Join(parts, "\n")
	}
	var kept []string
	n := 0
	for _, p := range parts {
		w := wordCount(p)
		if n+w > signalMaxWords {
			break
		}
		kept = append(kept, p)
		n += w
	}
	if len(kept) == 0 {
		return capWords(parts[0], signalMaxWords)
	}
	return strings.Join(kept, "\n")
}

// VisibleTextJSON serializes visible chunks for the episodes.visible_text column. Empty slice becomes "".
func VisibleTextJSON(visible []string) string {
	if len(visible) == 0 {
		return ""
	}
	b, err := json.Marshal(visible)
	if err != nil {
		return ""
	}
	return string(b)
}
