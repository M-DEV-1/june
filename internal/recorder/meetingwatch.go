package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"ora/internal/db"
	oratext "ora/internal/text"
	"ora/internal/tracker"
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
	// oraStreamPrefix is how Ora's own capture streams name themselves to the audio server: "Ora meeting recorder" while recording a call, "Ora voice" while the user is talking to the assistant. The trailing space keeps it from matching an unrelated application whose name merely starts with those three letters.
	oraStreamPrefix = "Ora "
	// oraBinary is this program's own executable name. It is compared against the last element of the stream's process path, not the whole path: the audio library reports application.process.binary as os.Args[0], which is "/home/user/.../ora" or "./ora" and never the bare word — so comparing the raw value matched nothing, and Ora's voice assistant holding the microphone read as somebody else being in a call.
	oraBinary = "ora"
	// maxNameRunes is how much of a window title the prompt shows. A page names its own window and some run very long, while the prompt is one line.
	maxNameRunes = 60
	// windowLookback is how far back Ora looks for the window belonging to whatever took the microphone.
	// Half an hour rather than a minute, because the tracker only records a window when its title changes: a meeting window opened and then left alone writes one episode and nothing more. On 2026-09-01 a call was named eight minutes before Ora asked about it, and a one-minute lookback found nothing and fell back to calling the meeting "Brave".
	// The risk of reaching too far back is small, because only windows belonging to the application currently holding the microphone are considered — the worst case is naming the last thing that application was showing, which still says more than its process name does.
	windowLookback = 30 * time.Minute
	// windowLookbackRows caps the episodes read for that lookup, and is sized for the lookback above rather than for how often the tracker writes.
	windowLookbackRows = 400
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

// micUsers returns the applications capturing from a microphone right now, excluding Ora's own recorder, split into those that are in a call and those that are not.
//
// Input: the JSON pw-dump writes. Output: the names of the applications that capture and also play audio back, then the names of those that only capture; each list is in the order the dump listed them and without repeats, since a browser opens a separate stream per tab.
//
// A stream counts only when it is running and names an application. The audio server's own echo canceller is always present as an input stream and names no application, so requiring a name is what keeps it out; requiring "running" is what distinguishes a microphone being read from one merely being open.
//
// A call plays the other side back through the same application that captures, so an application with a running input stream and a running output stream is in a call. One that only captures is dictating, recording or transcribing: Handy, a push-to-talk dictation tool, opens the microphone for every dictation and plays nothing back, and on 2026-09-03 it was asked about as a meeting five times in fifteen minutes.
func micUsers(dump []byte) (calls, captureOnly []string) {
	var nodes []pwNode
	if err := json.Unmarshal(dump, &nodes); err != nil {
		return nil, nil
	}
	playing := map[string]bool{}
	for _, n := range nodes {
		p := n.Info.Props
		if p.MediaClass == "Stream/Output/Audio" && n.Info.State == "running" {
			playing[streamName(p.Binary, p.AppName)] = true
		}
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		p := n.Info.Props
		if p.MediaClass != "Stream/Input/Audio" || n.Info.State != "running" {
			continue
		}
		if strings.HasPrefix(p.AppName, oraStreamPrefix) || strings.EqualFold(filepath.Base(p.Binary), oraBinary) {
			continue
		}
		name := streamName(p.Binary, p.AppName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if playing[name] {
			calls = append(calls, name)
		} else {
			captureOnly = append(captureOnly, name)
		}
	}
	return calls, captureOnly
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

// statusSeparator is what a browser puts between a page's title and its own running commentary about that page — the microphone indicator, the memory warning, the browser's name. Everything after the first one is the browser talking about itself.
const statusSeparator = " - "

// withoutBrowserStatus trims a browser's own status off the end of a window title.
//
// Input: a window title. Output: the part before the browser's first appended status, or the whole title when it appends none.
//
// "Meet - abc-defg-hij - Microphone recording - Brave" becomes "Meet - abc-defg-hij", and a Teams window loses its memory warning and megabyte count. This is where a browser appends rather than which words it appends, so it needs no list of them and does not care what a browser adds next.
// Trimming any further would mean dropping the part that says which call this is, which is the one thing the name is for.
func withoutBrowserStatus(title string) string {
	if head, _, found := strings.Cut(title, statusSeparator); found && strings.TrimSpace(head) != "" {
		return strings.TrimSpace(head)
	}
	return title
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
		} else {
			name = withoutBrowserStatus(name)
		}
		// A name over the cap keeps one rune of the budget for the ellipsis, and is trimmed before it so the marker does not follow a space.
		if len([]rune(name)) > maxNameRunes {
			name = strings.TrimSpace(oratext.Runes(name, maxNameRunes-1)) + "…"
		}
		named = append(named, name)
	}
	return named
}

// readMicUsers asks the audio server which applications are in a call, and logs any that hold the microphone without playing anything back so the rule can be checked against the log. It returns nothing when pw-dump is missing or fails, which is also what a machine with no PipeWire reports, so the watcher simply never fires there.
func readMicUsers(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "pw-dump").Output()
	if err != nil {
		return nil
	}
	calls, captureOnly := micUsers(out)
	if len(captureOnly) > 0 {
		slog.Debug("holding the microphone without playing anything back, not a call", "processes", captureOnly)
	}
	return calls
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
// The microphone is the signal rather than a meeting app's window, because it is the one thing every call has in common: it needs no list of which applications count, and it does not fire for a meeting tab that is merely open. Playback from the same application is what tells a call from dictation or a voice note. What the two together cannot tell is a call from, say, a voice message being listened to and answered, which is why the default is to ask rather than to record.
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
			// The prompt names the window rather than the process wherever one can be found, since a call in a browser tab is "chrome" and so is everything else in that browser.
			// The desktop is asked before Ora's own history, because history is always behind here: the microphone opens as the call is joined and the tracker does not record the window for another minute or so, which is well after the question has been asked and answered.
			var eps []db.Episode
			if recent, err := rec.store.EpisodesInWindow(ctx, time.Now().Add(-windowLookback), time.Now(), windowLookbackRows); err == nil {
				eps = recent
			}
			for _, u := range users {
				if t := tracker.WindowTitleFor(ctx, u); t != "" {
					eps = append(eps, db.Episode{App: u, Title: t})
				}
			}
			names := describe(users, eps)
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
