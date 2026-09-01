package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"ora/internal/db"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// micPollInterval is how often the microphone is checked. A full PipeWire graph dump measures 0.01s and 6.8MB of peak memory on this machine, so five seconds is a 0.2% duty cycle and the cost is not what sets this — the wait before the question is.
	micPollInterval = 5 * time.Second
	// asksAfterPolls is how many consecutive polls must find the microphone held before Ora asks. Two is the fewest that can tell a sustained hold from a momentary one, since a single poll catches a two-second voice note as readily as a call.
	// Two polls five seconds apart puts the question on screen between five and ten seconds after the microphone opens, depending on where in the cycle it opened, and that window is the meeting's opening that goes unrecorded. Ten seconds is short enough that what is lost is people saying hello.
	asksAfterPolls = 2
	// oraStreamPrefix is how Ora's own capture streams name themselves to the audio server, set in audio.StartMeetingCapture. Ora holds the microphone for the whole of a recording, so without this it would find its own stream and ask about the meeting it is already recording.
	oraStreamPrefix = "Ora meeting"
	// oraBinary is this program's own executable name, checked alongside oraStreamPrefix because the process behind Ora's stream is called "ora" and says nothing about the stream being a recording of Ora's own making.
	oraBinary = "ora"
	// maxNameRunes is how much of a window title the prompt shows. A page names its own window and some run very long, while the prompt is one line.
	maxNameRunes = 60
	// windowLookback is how far back Ora looks for the window belonging to whatever took the microphone. The tracker records the focused window every couple of seconds, so a minute covers the call being joined and the user then switching away to something else.
	windowLookback = time.Minute
	// windowLookbackRows caps the episodes read for that lookup. The tracker writes one every couple of seconds at most, so a minute cannot fill this.
	windowLookbackRows = 100
	// notifyWait is how long a prompt is left on screen before Ora stops waiting for an answer. A prompt nobody answered is not a no — the next poll simply finds the call still running and the state already marked as asked, so it stays quiet.
	notifyWait = 10 * time.Minute
)

// pwNode is the part of a PipeWire node that says whether it is an application capturing audio. pw-dump prints every object in the graph; everything not described here is ignored.
type pwNode struct {
	Info struct {
		State string `json:"state"`
		Props struct {
			MediaClass string `json:"media.class"`
			AppName    string `json:"application.name"`
			Binary     string `json:"application.process.binary"`
		} `json:"props"`
	} `json:"info"`
}

// micUsers returns the applications capturing from a microphone right now, excluding Ora's own recorder.
//
// Input: the JSON pw-dump writes. Output: one name per application, in the order the dump listed them and without repeats, since a browser opens a separate stream per tab.
//
// A stream counts only when it is running and names an application. The audio server's own echo canceller is always present as an input stream and names no application, so requiring a name is what keeps it out; requiring "running" is what distinguishes a microphone being read from one merely being open.
func micUsers(dump []byte) []string {
	var nodes []pwNode
	if err := json.Unmarshal(dump, &nodes); err != nil {
		return nil
	}
	var users []string
	seen := map[string]bool{}
	for _, n := range nodes {
		p := n.Info.Props
		if p.MediaClass != "Stream/Input/Audio" || n.Info.State != "running" {
			continue
		}
		if strings.HasPrefix(p.AppName, oraStreamPrefix) || strings.EqualFold(p.Binary, oraBinary) {
			continue
		}
		name := streamName(p.Binary, p.AppName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		users = append(users, name)
	}
	return users
}

// streamName is the name to show a person for one capturing stream.
//
// Input: the stream's process binary and the name the application gave itself. Output: a name a person recognises, or empty when the stream identifies itself as nothing at all.
//
// The process is the answer rather than one of several hints, because what an application calls itself is not its name: Discord registers as "WEBRTC VoiceEngine" and a browser as "Brave input", one naming the audio library and the other the direction the audio flows. Neither can be cleaned up by pattern, and every application that opens a microphone through the audio server's compatibility layer reports its process, so there is nothing to clean up. A process name is lowercase by convention and is capitalised for reading.
//
// The name the application gave itself is used only when there is no process behind the stream at all, which is what a tool speaking the audio server's native protocol looks like. Whatever such a stream calls itself is then shown verbatim: there is nothing better to say about it, and inventing a rule to tidy it would be guessing at a shape no application has been observed to use.
func streamName(binary, appName string) string {
	if binary != "" {
		name := strings.TrimSuffix(filepath.Base(binary), ".exe")
		if name == strings.ToLower(name) {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		return name
	}
	return appName
}

// windowFor returns the title of the window Ora last saw for a capturing application, or empty when it saw none.
//
// Input: the application's process name, and the episodes Ora recorded recently, oldest first. Output: the most recent window title belonging to that application.
//
// This is what the process name cannot say. A call in a browser tab is "chrome" whether it is a meeting, a spreadsheet or a video, and a web app installed as its own window is "chrome" too — so Google Meet, Teams and a shopping tab are one name. The window title is the only thing on the machine that tells them apart, and the tracker already writes it down every couple of seconds.
//
// The two names are matched loosely because they are never written the same way: the process is "chrome" where the window says "Google Chrome", and "brave" where it says "Brave Browser".
func windowFor(app string, eps []db.Episode) string {
	if app == "" {
		return ""
	}
	a := strings.ToLower(app)
	for i := len(eps) - 1; i >= 0; i-- {
		e := strings.ToLower(eps[i].App)
		if e == "" || (!strings.Contains(e, a) && !strings.Contains(a, e)) {
			continue
		}
		if t := strings.TrimSpace(eps[i].Title); t != "" {
			return t
		}
	}
	return ""
}

// describe names each capturing application the way a person would recognise it: by the window it has open, falling back to the process when Ora has seen no window for it.
//
// Input: the process names holding the microphone, and the episodes Ora recorded recently. Output: one display name each, cut to one line's worth.
func describe(users []string, eps []db.Episode) []string {
	named := make([]string, 0, len(users))
	for _, u := range users {
		name := windowFor(u, eps)
		if name == "" {
			name = u
		}
		if r := []rune(name); len(r) > maxNameRunes {
			name = strings.TrimSpace(string(r[:maxNameRunes-1])) + "\u2026"
		}
		named = append(named, name)
	}
	return named
}

// readMicUsers asks the audio server which applications are capturing. It returns nothing when pw-dump is missing or fails, which is also what a machine with no PipeWire reports, so the watcher simply never fires there.
func readMicUsers(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "pw-dump").Output()
	if err != nil {
		return nil
	}
	return micUsers(out)
}

// meetingWatch remembers enough between polls to ask about a call once and then leave it alone. The zero value is ready to use.
type meetingWatch struct {
	held  int
	asked bool
}

// step folds one poll into the watch and reports whether this is the moment to ask about a meeting.
//
// Input: the applications holding the microphone, and whether Ora is already recording. Output: true exactly once per call, on the poll where the microphone has been held long enough to be a call and no question has been asked yet.
//
// Releasing the microphone is what ends a call, so the next one is asked about again.
func (w *meetingWatch) step(users []string, recording bool) bool {
	if recording || len(users) == 0 {
		w.held, w.asked = 0, false
		return false
	}
	w.held++
	if w.held < asksAfterPolls || w.asked {
		return false
	}
	w.asked = true
	return true
}

// askBody is the line under the prompt's title. It names the application holding the microphone, because "Chrome is using your microphone" is a question a person can answer and "in a meeting?" on its own is a guess.
func askBody(users []string) string {
	return strings.Join(users, ", ") + " is using your microphone."
}

// askToRecord puts the question on screen with a button on it and reports whether the button was pressed. notify-send prints the name of the action the user chose, and prints nothing when the prompt is dismissed or times out.
func askToRecord(ctx context.Context, users []string) bool {
	ctx, cancel := context.WithTimeout(ctx, notifyWait)
	defer cancel()

	cmd := exec.CommandContext(ctx, "notify-send",
		"--app-name=Ora",
		"--action=record=Start recording",
		"In a meeting?",
		askBody(users))
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(bytes.TrimSpace(out))) == "record"
}

// WatchForMeetings asks whether to record whenever an application other than Ora holds the microphone for long enough to be a call, and starts recording if the answer is yes. It returns when ctx is cancelled.
//
// The microphone is the signal rather than a meeting app's window, because it is the one thing every call has in common: it needs no list of which applications count, and it does not fire for a meeting tab that is merely open. What it cannot tell on its own is a call from any other use of the microphone, which is why the default is to ask rather than to record.
//
// With autoRecord set the question is skipped and recording starts on the same signal.
func WatchForMeetings(ctx context.Context, rec *Recorder, autoRecord bool) {
	if rec == nil {
		return
	}
	ticker := time.NewTicker(micPollInterval)
	defer ticker.Stop()

	var w meetingWatch
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			users := readMicUsers(ctx)
			if !w.step(users, rec.Active()) {
				continue
			}
			// The prompt names the window rather than the process wherever Ora has seen one, since a call in a browser tab is "chrome" and so is everything else in that browser.
			names := users
			if eps, err := rec.store.EpisodesInWindow(ctx, time.Now().Add(-windowLookback), time.Now(), windowLookbackRows); err == nil {
				names = describe(users, eps)
			}
			slog.Info("something else is holding the microphone", "apps", names, "processes", users, "auto", autoRecord)
			if !autoRecord && !askToRecord(ctx, names) {
				continue
			}
			if err := rec.Start(); err != nil {
				slog.Error("failed to start the meeting recording Ora offered", "error", err)
			}
		}
	}
}
