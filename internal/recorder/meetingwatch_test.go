package recorder

import (
	"ora/internal/db"
	"os"
	"strings"
	"testing"
)

// The fixture is a real pw-dump from this machine, cut down to the two input streams it contained: the system's own echo canceller, which is always present and names no application, and a recorder that was actually capturing. Anything that reads a microphone appears in this same shape. The recorder plays nothing back, so it is reported as holding the microphone without being in a call.
func TestMicUsers_OnlyRunningStreamsThatNameAnApp(t *testing.T) {
	dump, err := os.ReadFile("testdata/pw-dump.json")
	if err != nil {
		t.Fatal(err)
	}

	calls, quiet := micUsers(dump)

	if len(calls) != 0 {
		t.Fatalf("calls = %v, want none: a recorder that plays nothing back is not a call", calls)
	}
	if len(quiet) != 1 || quiet[0] != "pw-cat" {
		t.Fatalf("capture-only = %v, want [pw-cat]", quiet)
	}
}

// Ora holds the microphone itself for the whole of a recording, so without this it would see its own stream, decide a meeting had started, and ask about the meeting it is already recording.
func TestMicUsers_IgnoresOraItself(t *testing.T) {
	dump := []byte(`[
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Ora meeting recorder"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Google Chrome"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"Google Chrome"}}}
	]`)

	got, _ := micUsers(dump)

	if len(got) != 1 || got[0] != "Google Chrome" {
		t.Fatalf("micUsers = %v, want [Google Chrome] with Ora's own stream dropped", got)
	}
}

// A meeting is asked about once and then left alone: a second prompt during the same call is worse than no prompt at all. The wait before asking is what separates a call from a two-second voice note, and releasing the microphone is what ends one call and allows the next to be asked about.
func TestMeetingWatch_AsksOncePerCall(t *testing.T) {
	var w meetingWatch
	chrome := []string{"Google Chrome"}

	if w.step(chrome, false) {
		t.Error("asked on the first poll, which cannot tell a call from a voice note")
	}
	if !w.step(chrome, false) {
		t.Fatal("did not ask once the microphone had been held across polls")
	}
	if w.step(chrome, false) || w.step(chrome, false) {
		t.Error("asked twice about the same call")
	}

	// The call ends, and a later one is a new question.
	w.step(nil, false)
	w.step(chrome, false)
	if !w.step(chrome, false) {
		t.Error("did not ask about a second call after the microphone was released")
	}
}

// While Ora is recording there is nothing to ask, and the state must come back clean so the next call is asked about normally.
func TestMeetingWatch_SilentWhileRecording(t *testing.T) {
	var w meetingWatch
	chrome := []string{"Google Chrome"}

	for i := 0; i < 4; i++ {
		if w.step(chrome, true) {
			t.Fatal("asked while already recording")
		}
	}
	w.step(nil, false)
	w.step(chrome, false)
	if !w.step(chrome, false) {
		t.Error("stopped asking after a recording ended")
	}
}

// The prompt names the application holding the microphone so the answer is an informed one: "Chrome is using your microphone" is a question a person can answer, where "in a meeting?" is a guess.
func TestAskBody_NamesTheApp(t *testing.T) {
	if body := askBody([]string{"Google Chrome"}); !strings.Contains(body, "Google Chrome") {
		t.Errorf("askBody = %q, want it to name the application", body)
	}
}

// Every value here was read off this machine. Discord names the audio library rather than itself, and the browsers name the stream's direction, so the application property is the wrong one to show a person — the process behind it is the right one.
func TestMicUsers_NamesTheProcessNotTheAudioEngine(t *testing.T) {
	dump := []byte(`[
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Brave Input","application.process.binary":"brave"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"Brave","application.process.binary":"brave"}}}
	]`)

	got, _ := micUsers(dump)

	if len(got) != 2 || got[0] != "Discord" || got[1] != "Brave" {
		t.Fatalf("micUsers = %v, want [Discord Brave]", got)
	}
}

// A browser calls its stream "Brave input" and Discord calls its "WEBRTC VoiceEngine", so no amount of tidying the application's own name produces the program's. The process is what is shown, and these two differ only in that one has a process and the other does not.
func TestMicUsers_ShowsTheProcessAndOtherwiseTheNameVerbatim(t *testing.T) {
	dump := []byte(`[
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Brave input","application.process.binary":"brave"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"pw-cat"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"Brave","application.process.binary":"brave"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"pw-cat"}}}
	]`)

	got, _ := micUsers(dump)

	if len(got) != 2 || got[0] != "Brave" || got[1] != "pw-cat" {
		t.Fatalf("micUsers = %v, want [Brave pw-cat]", got)
	}
}

// Ora's own stream has to stay excluded now that the process name is preferred, since the process is called "ora" and says nothing about the stream being Ora's own recording.
func TestMicUsers_IgnoresOraByEitherName(t *testing.T) {
	dump := []byte(`[
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Ora meeting recorder","application.process.binary":"ora"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}}
	]`)

	got, _ := micUsers(dump)

	if len(got) != 1 || got[0] != "Discord" {
		t.Fatalf("micUsers = %v, want [Discord] with Ora's own stream dropped", got)
	}
}

// A call in a browser tab is the case the process name cannot describe: the process is "chrome" whether the tab is a meeting, a spreadsheet or a video. The window title is the only thing that says which, and Ora already records it every couple of seconds. The Teams title here is one Ora actually logged on this machine.
func TestWindowFor_PrefersTheWindowOverTheProcess(t *testing.T) {
	eps := []db.Episode{
		{App: "Google Chrome", Title: "Inbox (12)"},
		{App: "Google Chrome", Title: "Calendar | Karan Mehta | Microsoft Teams"},
	}

	if got := windowFor("Chrome", eps); got != "Calendar | Karan Mehta | Microsoft Teams" {
		t.Errorf("windowFor = %q, want the most recent window for that process", got)
	}
}

// The process name and the window's application name are never written the same way — "chrome" against "Google Chrome", "brave" against "Brave Browser" — so they are matched loosely rather than compared.
func TestWindowFor_MatchesLooselyAndOnlyTheRightApp(t *testing.T) {
	eps := []db.Episode{
		{App: "Brave Browser", Title: "Project sync - Google Meet"},
		{App: "Code", Title: "meetingwatch.go"},
	}

	if got := windowFor("brave", eps); got != "Project sync - Google Meet" {
		t.Errorf("windowFor(brave) = %q, want the Brave window", got)
	}
	if got := windowFor("Discord", eps); got != "" {
		t.Errorf("windowFor(Discord) = %q, want empty when Ora saw no window for it", got)
	}
}

// A window title runs as long as the page wants and a notification has one line, so it is cut. Discord has no browser window behind it and keeps its own name.
func TestDescribe_UsesWindowsWhereThereAreAnyAndTrims(t *testing.T) {
	eps := []db.Episode{
		{App: "Google Chrome", Title: "Calendar | Karan Mehta | Microsoft Teams - High memory usage - 1.2 GB"},
	}

	got := describe([]string{"Chrome", "Discord"}, eps)

	if len(got) != 2 {
		t.Fatalf("describe returned %d names, want 2", len(got))
	}
	if len([]rune(got[0])) > maxNameRunes {
		t.Errorf("name is %d runes, want it cut to %d: %q", len([]rune(got[0])), maxNameRunes, got[0])
	}
	if !strings.HasPrefix(got[0], "Calendar | Karan Mehta") {
		t.Errorf("got[0] = %q, want the window title", got[0])
	}
	if got[1] != "Discord" {
		t.Errorf("got[1] = %q, want the process name when Ora saw no window", got[1])
	}
}

// A browser appends its own status to the end of a window title, after " - ": the microphone indicator, the memory warning, its own name. Both titles here are ones Ora recorded on this machine.
func TestDescribe_CutsTheBrowsersOwnStatusOffTheEnd(t *testing.T) {
	eps := []db.Episode{
		{App: "Brave", Title: "Meet – abc-defg-hij - Microphone recording - Brave"},
		{App: "Chrome", Title: "Calendar | Priya Shah | Microsoft Teams - High memory usage - 852 MB"},
	}

	got := describe([]string{"Brave", "Chrome"}, eps)

	if got[0] != "Meet – abc-defg-hij" {
		t.Errorf("got[0] = %q, want the meeting without the browser's status", got[0])
	}
	if got[1] != "Calendar | Priya Shah | Microsoft Teams" {
		t.Errorf("got[1] = %q, want the meeting without the memory warning", got[1])
	}
}

// Ora's voice assistant holds the microphone for as long as the user is talking to it, and the audio library names a stream after the running binary by default: application.name "ora", application.process.binary the full path it was launched from. Comparing that path against "ora" never matched, so Ora noticed itself talking and offered to record the conversation.
func TestMicUsers_IgnoresOrasOwnVoiceMode(t *testing.T) {
	dump := []byte(`[
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"ora","application.process.binary":"/home/user/Desktop/Code/projects/ora/ora"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Ora voice","application.process.binary":"./ora"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"WEBRTC VoiceEngine","application.process.binary":"Discord"}}}
	]`)

	got, _ := micUsers(dump)

	if len(got) != 1 || got[0] != "Discord" {
		t.Fatalf("micUsers = %v, want [Discord] with both of Ora's own streams dropped", got)
	}
}

// Handy is a push-to-talk dictation tool: it opens the microphone for each dictation and plays nothing back, and on 2026-09-03 Ora asked "In a meeting?" for five dictations in fifteen minutes. A call plays the other side back through the same application that captures, so an application that only captures is dictating or recording, not in a call. The Handy stream here is the one pw-dump reported on this machine: it speaks through the ALSA layer and names no process.
func TestMicUsers_ACallPlaysBackAndADictationDoesNot(t *testing.T) {
	dump := []byte(`[
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"PipeWire ALSA [handy]"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.name":"Google Chrome","application.process.binary":"chrome"}}},
	  {"info":{"state":"running","props":{"media.class":"Stream/Output/Audio","application.name":"Google Chrome","application.process.binary":"chrome"}}},
	  {"info":{"state":"idle","props":{"media.class":"Stream/Output/Audio","application.name":"PipeWire ALSA [handy]"}}}
	]`)

	calls, quiet := micUsers(dump)

	if len(calls) != 1 || calls[0] != "Chrome" {
		t.Fatalf("calls = %v, want [Chrome]: the browser both captures and plays back", calls)
	}
	if len(quiet) != 1 || quiet[0] != "PipeWire ALSA [handy]" {
		t.Fatalf("capture-only = %v, want [PipeWire ALSA [handy]]: a dictation tool plays nothing back", quiet)
	}
}

// TestDescribe_CutsOnlyWhatIsTooLongAndAlwaysMarksTheCut pins the two edges of the name cap: a name of exactly maxNameRunes is shown whole and unmarked, and a name one rune longer is cut and ends in an ellipsis so the reader can see it was shortened. A cut that silently drops the last character without a marker reads as the window's real title.
func TestDescribe_CutsOnlyWhatIsTooLongAndAlwaysMarksTheCut(t *testing.T) {
	exact := strings.Repeat("a", maxNameRunes)
	if got := describe([]string{exact}, nil); got[0] != exact {
		t.Errorf("a name of exactly %d runes came back as %q (%d runes), want it unchanged", maxNameRunes, got[0], len([]rune(got[0])))
	}

	over := strings.Repeat("b", maxNameRunes+1)
	got := describe([]string{over}, nil)[0]
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a name of %d runes came back as %q, want it to end in an ellipsis", maxNameRunes+1, got)
	}
	if n := len([]rune(got)); n > maxNameRunes {
		t.Errorf("cut name is %d runes, want at most %d", n, maxNameRunes)
	}
}
