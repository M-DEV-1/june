package tracker

import (
	"regexp"
	"strings"
)

type Activity struct {
	App          string
	Title        string
	ScreenText   string
	UserActivity string
	VisibleText  []string
	// ImageJPEG is the frame for the monitor the user is on, and ExtraJPEG holds one frame per other monitor at the same moment — a meeting on one screen while notes sit on the other is one activity, not two.
	// Both persist: writeEpisodeJPEG stores the primary as frames/{id}.jpg and each extra as frames/{id}-b.jpg, {id}-c.jpg (internal/db/image.go), read back with Store.EpisodeExtraImages. The vision call still only sees the primary — tieredCapture in internal/tracker/daemon.go passes one PNG to visionFn.
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

// IsOraWindow reports whether a window is Ora's own desktop window: the app is named ora, or it is the XWayland frame process (mutter-x11-frames) carrying the exact title "Ora". A terminal titled "ora" because it sits in the repo is not the window, so the title alone never decides.
// Input: an application name and a window title. Output: true when the window is Ora itself.
// Ora looking at Ora is never the user's activity, so this is the one rule the tracker, the capture loop and the /context reads all filter by.
func IsOraWindow(app, title string) bool {
	app = strings.TrimSpace(app)
	return strings.EqualFold(app, "ora") || (app == "mutter-x11-frames" && strings.TrimSpace(title) == "Ora")
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

// meetingWindow matches the window title of a call in progress. Only the three apps this machine has actually recorded a meeting in are listed: Google Meet writes "Meet - <code>", Teams titles its window "Microsoft Teams (PWA) - Chat | <person>", Zoom names itself. Matching on the title covers a browser tab and a desktop app alike without needing to know which.
// ponytail: a hand-written list of app names, which is the wrong shape for the question — it has to be edited every time a meeting happens somewhere new, and it cannot see a call in an app it has never heard of. The signal that does not need a list is the audio server: a meeting app holds a capture stream on the microphone for the whole call and nothing else on a desktop does, so asking PulseAudio which process is recording identifies the call without naming anything. Ora already speaks that protocol for the recording itself (internal/audio/meeting_linux.go); GetSourceOutputInfoList carries application.process.id. Replace this when a meeting happens in an app the list misses.
var meetingWindow = regexp.MustCompile(`(?i)\b(google meet|meet [–—-] |microsoft teams|zoom meeting|zoom\.us)`)

// IsMeetingWindow reports whether a window is a call in progress rather than whatever else is on screen. Everything that claims to name a participant is checked against this first: a screen during a meeting is mostly not the meeting.
func IsMeetingWindow(app, title string) bool {
	return meetingWindow.MatchString(title) || meetingWindow.MatchString(app)
}
