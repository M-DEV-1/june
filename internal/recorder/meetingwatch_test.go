package recorder

import (
	"os"
	"slices"
	"testing"
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
