package tracker

import "strings"

type Activity struct {
	App          string
	Title        string
	ScreenText   string
	UserActivity string
	VisibleText  []string
	// ImageJPEG is the frame for the monitor the user is on, and ExtraJPEG holds one frame per other monitor at the same moment — a meeting on one screen while notes sit on the other is one activity, not two.
	// Nothing persists ExtraJPEG yet: internal/db writeEpisodeJPEG stores a single image per episode (internal/db/image.go), so the extra frames need a second column or a "{id}-b.jpg" naming rule before they reach disk or the vision call.
	ImageJPEG []byte
	ExtraJPEG [][]byte
}

// Sight is the structured vision extract for one capture.
type Sight struct {
	UserActivity string
	VisibleText  []string
	Summary      string
}

// Text is the searchable description for a vision capture: activity, then visible chunks, then summary. Never accessibility chrome — if vision ran, a11y is discarded.
func (s Sight) Text() string {
	var parts []string
	if s.UserActivity != "" {
		parts = append(parts, s.UserActivity)
	}
	parts = append(parts, s.VisibleText...)
	if s.Summary != "" && !strings.Contains(strings.Join(parts, "\n"), s.Summary) {
		parts = append(parts, s.Summary)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// defines standard interface for all os implementations
type Tracker interface {
	GetActiveWindow() (*Activity, error)
}

func Normalize(app, title string) *Activity {
	// memory scrubbing
	cleanApp := strings.TrimSpace(strings.ReplaceAll(app, "\x00", ""))
	cleanTitle := strings.TrimSpace(strings.ReplaceAll(title, "\x00", ""))

	if cleanApp == "" {
		cleanApp = "Unknown"
	}
	if cleanTitle == "" {
		cleanTitle = "Unknown"
	}

	// cleanApp = strings.TrimSuffix(cleanApp, ".exe")
	// we will decide later

	return &Activity{
		App:   cleanApp,
		Title: cleanTitle,
	}
}

// New is our generic constructor, defined in os-specific files
