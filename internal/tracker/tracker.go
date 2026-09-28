package tracker

import (
	"regexp"
	"strings"
	"sync/atomic"
)

type Activity struct {
	App          string
	Title        string
	ScreenText   string
	UserActivity string
	VisibleText  []string
	// ImageJPEG is the frame for the monitor the user is on, and ExtraJPEG holds one frame per other monitor at the same moment — a meeting on one screen while notes sit on the other is one activity, not two.
	// Both persist: writeEpisodeJPEG stores the primary as frames/{id}.jpg and each extra as frames/{id}-b.jpg, {id}-c.jpg (internal/db/image.go), read back with Store.EpisodeExtraImages. The vision call sees the whole canvas, not the primary monitor: tieredCapture in internal/tracker/daemon.go passes visionFn the one PNG grabScreen returned, which spans every monitor, and screenFrames only splits that same PNG per monitor afterwards for storage. What is captured is gated by the blocklist and by Ora's own window, not by which monitor a window sits on — skipReason runs before any capture starts, so it covers the vision tier and the accessibility tier alike.
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

// blocklist is the app blocklist the daemon in this process was built with. It is package-level because a read outside the tracker has no daemon to ask: the /context handler answers the hotkey from a live window read (internal/ipc/reads.go), and that read has to refuse the same applications the capture loop refuses or the hotkey hands the model a window the episode store would never hold.
// ponytail: one process runs one tracker, so a package variable is the whole of what this needs. Give the Daemon's blocklist a home the ipc server can be constructed with if a second tracker ever exists.
var blocklist atomic.Pointer[[]string]

// SetBlocklist records the app blocklist every reader in this process should apply. Input: the list of application names to refuse, which may be nil. Output: none. NewDaemon calls it, so ordinary callers never have to.
func SetBlocklist(list []string) { blocklist.Store(&list) }

// Blocklisted reports whether an application is one this machine refuses to record. Input: an application name. Output: true when the tracker would drop that window, false when it would keep it or when no blocklist has been set. It matches the same way the capture loop does, so the two cannot drift apart.
func Blocklisted(app string) bool {
	list := blocklist.Load()
	return list != nil && MatchesBlocklist(app, *list)
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
