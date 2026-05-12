package tracker

import "strings"

type Activity struct {
	App   string
	Title string
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

	return &Activity{
		App:   cleanApp,
		Title: cleanTitle,
	}
}

// New is our generic constructor, defined in os-specific files
