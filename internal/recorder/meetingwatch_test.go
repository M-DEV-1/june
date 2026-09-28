package recorder

import (
	"os"
	"slices"
	"strings"
	"testing"

	"june/internal/db"
)

// TestMicUsers covers micUsers's shape rules through one table: a stream needs a name and to be running to count at all, June's own streams are dropped by either the "June "-prefixed app name or the "june" process, and a stream that only captures (no matching running output) is quiet rather than a call.
func TestMicUsers(t *testing.T) {
	realDump, err := os.ReadFile("testdata/pw-dump.json")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		dump      []byte
		wantCalls []string
		wantQuiet []string
	}{
		{
			// The fixture is a real pw-dump from this machine, cut down to the two input streams it contained: the system's own echo canceller, which is always present and names no application, and a recorder that was actually capturing. A stream with no name is dropped outright rather than shown blank.
			name:      "real fixture: the nameless echo canceller is dropped, the named capture-only stream is quiet",
			dump:      realDump,
			wantCalls: nil,
			wantQuiet: []string{"pw-cat"},
		},
		{
			// June holds the microphone itself for the whole of a recording, so without this it would see its own stream, decide a meeting had started, and ask about the meeting it is already recording.
			name: "June's own recorder stream is dropped by its app-name prefix",
			dump: []byte(`[
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"June meeting recorder"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Google Chrome"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"Google Chrome"}}}
			]`),
			wantCalls: []string{"Google Chrome"},
			wantQuiet: nil,
		},
		{
			// June's voice assistant holds the microphone for as long as the user is talking to it, and live voice mode speaks back through the same process, so it looks exactly like a call unless both of its streams — by app-name prefix and by process binary — are excluded.
			name: "June's voice-mode streams are dropped by either app name or process binary",
			dump: []byte(`[
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"june","application.process.binary":"/home/user/Desktop/Code/projects/june/june"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"June voice","application.process.binary":"./june"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"June voice","application.process.binary":"./june"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}}
			]`),
			wantCalls: []string{"Discord"},
			wantQuiet: nil,
		},
		{
			// Handy is a push-to-talk dictation tool: it opens the microphone for each dictation and plays nothing back. A call plays the other side back through the same application that captures, so an application that only captures is dictating or recording, not in a call.
			name: "a call plays back and a dictation does not",
			dump: []byte(`[
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"PipeWire ALSA [handy]"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Google Chrome","application.process.binary":"chrome"}}},
			  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"Google Chrome","application.process.binary":"chrome"}}},
			  {"info":{"state":"idle","props":{"media.class":"Stream/Output/Audio","application.name":"PipeWire ALSA [handy]"}}}
			]`),
			wantCalls: []string{"Chrome"},
			wantQuiet: []string{"PipeWire ALSA [handy]"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, quiet := micUsers(c.dump)
			if !slices.Equal(calls, c.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, c.wantCalls)
			}
			if !slices.Equal(quiet, c.wantQuiet) {
				t.Errorf("capture-only = %v, want %v", quiet, c.wantQuiet)
			}
		})
	}
}

// A recording June started on its own has to end on its own, the moment it started is asked about once and left alone, and a recording the user started from the tray is never one June may stop. Each case below is one meetingWatch driven through a sequence of polls, checking what it decides to ask or to stop at each step.
func TestMeetingWatch(t *testing.T) {
	chrome := []string{"Google Chrome"}
	type step struct {
		start     bool // call w.started() immediately before this poll
		users     []string
		recording bool
		wantAsk   bool
		wantStop  bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{
			// A meeting is asked about once and then left alone: a second prompt during the same call is worse than no prompt at all. The wait before asking is what separates a call from a two-second voice note, and releasing the microphone is what ends one call and allows the next to be asked about.
			name: "asks once per call, then again for the next one",
			steps: []step{
				{users: chrome, wantAsk: false},
				{users: chrome, wantAsk: true},
				{users: chrome, wantAsk: false},
				{users: chrome, wantAsk: false},
				{users: nil},
				{users: chrome, wantAsk: false},
				{users: chrome, wantAsk: true},
			},
		},
		{
			// While June is recording there is nothing to ask, and the state must come back clean so the next call is asked about normally once the recording ends.
			name: "silent while already recording, resumes asking once it stops",
			steps: []step{
				{users: chrome, recording: true},
				{users: chrome, recording: true},
				{users: chrome, recording: true},
				{users: chrome, recording: true},
				{users: nil},
				{users: chrome, wantAsk: false},
				{users: chrome, wantAsk: true},
			},
		},
		{
			// The evidence that ends a call is the evidence that started it — the call's audio streams going away — so the same number of consecutive quiet polls stops the recording, and a poll that finds it already stopped must not report stopping it again.
			name: "stops the recording it started when the call ends",
			steps: []step{
				{users: chrome},
				{users: chrome, wantAsk: true},
				{start: true, users: chrome, recording: true},
				{users: nil, recording: true},
				{users: nil, recording: true, wantStop: true},
				{users: nil, recording: true},
			},
		},
		{
			// The recording June started is stopped from the tray, and the user then starts one of their own. That one is not June's to end: what makes a recording the watcher's is having started it, and the last one it started is over.
			name: "forgets its recording once that recording has ended",
			steps: []step{
				{start: true, users: nil},
				{users: nil, recording: true},
				{users: nil, recording: true},
				{users: nil, recording: true},
				{users: nil, recording: true},
			},
		},
		{
			// A call that drops one poll's worth of streams — a browser reopening its capture on a device change — must not end the recording, so the quiet polls have to be consecutive.
			name: "a call that comes back resets the quiet count",
			steps: []step{
				{start: true, users: nil, recording: true},
				{users: chrome, recording: true},
				{users: nil, recording: true},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var w meetingWatch
			for i, s := range c.steps {
				if s.start {
					w.started()
				}
				ask, stop := w.step(s.users, s.recording)
				if ask != s.wantAsk {
					t.Errorf("step %d: ask = %v, want %v", i, ask, s.wantAsk)
				}
				if stop != s.wantStop {
					t.Errorf("step %d: stop = %v, want %v", i, stop, s.wantStop)
				}
			}
		})
	}
}

// A recording the user started from the tray is theirs to stop: they may be recording something that never opens a call stream at all, and having it end itself under them is worse than a recording left running. This is covered above by "forgets its recording once that recording has ended", which reaches the same !ours guard by way of a recording June did start and then lost.

// windowFor and describe are read off the same real window titles this machine has recorded, so one table covers both: the process/window matching windowFor does, and the trimming and length cap describe adds on top of it.
func TestWindowFor_PrefersTheWindowOverTheProcess(t *testing.T) {
	// A call in a browser tab is the case the process name cannot describe: the process is "chrome" whether the tab is a meeting, a spreadsheet or a video. The window title is the only thing that says which, and June already records it every couple of seconds. Reading the most recent one matters because an app can have more than one window on record: the wrong pick here would report a stale screen instead of the meeting itself.
	eps := []db.Episode{
		{App: "Google Chrome", Title: "Inbox (12)"},
		{App: "Google Chrome", Title: "Calendar | Trelvo Kordis | Microsoft Teams"},
	}
	if got := windowFor("Chrome", eps); got != "Calendar | Trelvo Kordis | Microsoft Teams" {
		t.Errorf("windowFor = %q, want the most recent window for that process", got)
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		name  string
		users []string
		eps   []db.Episode
		check func(t *testing.T, got []string)
	}{
		{
			// A window title runs as long as the page wants and a notification has one line, so it is cut. Discord has no browser window behind it and keeps its own name.
			name:  "uses windows where there are any, falls back to the process name, and trims",
			users: []string{"Chrome", "Discord"},
			eps: []db.Episode{
				{App: "Google Chrome", Title: "Calendar | Trelvo Kordis | Microsoft Teams - High memory usage - 1.2 GB"},
			},
			check: func(t *testing.T, got []string) {
				if len(got) != 2 {
					t.Fatalf("describe returned %d names, want 2", len(got))
				}
				if len([]rune(got[0])) > maxNameRunes {
					t.Errorf("name is %d runes, want it cut to %d: %q", len([]rune(got[0])), maxNameRunes, got[0])
				}
				if !strings.HasPrefix(got[0], "Calendar | Trelvo Kordis") {
					t.Errorf("got[0] = %q, want the window title", got[0])
				}
				if got[1] != "Discord" {
					t.Errorf("got[1] = %q, want the process name when June saw no window", got[1])
				}
			},
		},
		{
			// A browser appends its own status to the end of a window title, after " - ": the microphone indicator, the memory warning, its own name. Both titles here are ones June recorded on this machine. Without this trim, a bug that stopped withoutBrowserStatus from doing anything would still pass a prefix check, so this pins the exact trimmed value.
			name:  "cuts the browser's own status off the end",
			users: []string{"Brave", "Chrome"},
			eps: []db.Episode{
				{App: "Brave", Title: "Meet – abc-defg-hij - Microphone recording - Brave"},
				{App: "Chrome", Title: "Calendar | Vexil Quorin | Microsoft Teams - High memory usage - 852 MB"},
			},
			check: func(t *testing.T, got []string) {
				if got[0] != "Meet – abc-defg-hij" {
					t.Errorf("got[0] = %q, want the meeting without the browser's status", got[0])
				}
				if got[1] != "Calendar | Vexil Quorin | Microsoft Teams" {
					t.Errorf("got[1] = %q, want the meeting without the memory warning", got[1])
				}
			},
		},
		{
			// The two edges of the name cap: a name of exactly maxNameRunes is shown whole and unmarked, and a name one rune longer is cut and ends in an ellipsis so the reader can see it was shortened. A cut that silently drops the last character without a marker reads as the window's real title.
			name:  "cuts only what is too long, and always marks the cut",
			users: []string{strings.Repeat("a", maxNameRunes), strings.Repeat("b", maxNameRunes+1)},
			eps:   nil,
			check: func(t *testing.T, got []string) {
				exact := strings.Repeat("a", maxNameRunes)
				if got[0] != exact {
					t.Errorf("a name of exactly %d runes came back as %q (%d runes), want it unchanged", maxNameRunes, got[0], len([]rune(got[0])))
				}
				if !strings.HasSuffix(got[1], "…") {
					t.Errorf("a name of %d runes came back as %q, want it to end in an ellipsis", maxNameRunes+1, got[1])
				}
				if n := len([]rune(got[1])); n > maxNameRunes {
					t.Errorf("cut name is %d runes, want at most %d", n, maxNameRunes)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, describe(c.users, c.eps))
		})
	}
}
