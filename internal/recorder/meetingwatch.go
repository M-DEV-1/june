package recorder

import (
	"context"
	"encoding/json"
	"june/internal/config"
	"june/internal/db"
	"june/internal/proactive"
	"june/internal/tracker"
	"june/internal/util"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// micPollInterval is how often the microphone is checked. A full PipeWire graph dump measures 0.01s and 6.8MB of peak memory on this machine, so five seconds is a 0.2% duty cycle and the cost is not what sets this — the wait before the question is.
	micPollInterval = 5 * time.Second
	// asksAfterPolls is how many consecutive polls must find the microphone held before June asks. Two is the fewest that can tell a sustained hold from a momentary one, since a single poll catches a two-second voice note as readily as a call.
	// Two polls five seconds apart puts the question on screen between five and ten seconds after the microphone opens, depending on where in the cycle it opened, and that window is the meeting's opening that goes unrecorded. Ten seconds is short enough that what is lost is people saying hello.
	asksAfterPolls = 2
	// juneStreamPrefix is how June's own capture streams name themselves to the audio server: "June meeting recorder" while recording a call, "June voice" while the user is talking to the assistant. The trailing space keeps it from matching an unrelated application whose name merely starts with those four letters.
	juneStreamPrefix = "June "
	// juneBinary is this program's own executable name. It is compared against the last element of the stream's process path, not the whole path: the audio library reports application.process.binary as os.Args[0], which is "/home/user/.../june" or "./june" and never the bare word — so comparing the raw value matched nothing, and June's voice assistant holding the microphone read as somebody else being in a call.
	juneBinary = "june"
	// maxNameRunes is how much of a window title the prompt shows. A page names its own window and some run very long, while the prompt is one line.
	maxNameRunes = 60
	// windowLookback is how far back June looks for the window belonging to whatever took the microphone.
	// Half an hour rather than a minute, because the tracker only records a window when its title changes: a meeting window opened and then left alone writes one episode and nothing more. On 2026-09-01 a call was named eight minutes before June asked about it, and a one-minute lookback found nothing and fell back to calling the meeting "Brave".
	// The risk of reaching too far back is small, because only windows belonging to the application currently holding the microphone are considered — the worst case is naming the last thing that application was showing, which still says more than its process name does.
	windowLookback = 30 * time.Minute
	// windowLookbackRows caps the episodes read for that lookup, and is sized for the lookback above rather than for how often the tracker writes.
	windowLookbackRows = 400
	// notifyWait is how long the meeting question is left on screen before June stops waiting for an answer. Fifteen seconds: the question is only worth asking while the call is starting, and a card that outlives its own waiter is a button that does nothing. A prompt nobody answered is not a no — the next poll simply finds the call still running and the state already marked as asked, so it stays quiet.
	notifyWait = 15 * time.Second
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

// micUsers returns the applications capturing from a microphone right now, excluding June's own recorder, split into those that are in a call and those that are not.
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
		if strings.HasPrefix(p.AppName, juneStreamPrefix) || strings.EqualFold(filepath.Base(p.Binary), juneBinary) {
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

// windowFor returns the title of the window June last saw for a capturing application, or empty when it saw none.
//
// Input: the application's process name, and the episodes June recorded recently, oldest first. Output: the most recent window title belonging to that application.
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

// micHolder is one application holding the microphone. proc is its process name, the one thing its windows can be matched on (see windowFor); name is what a person calls it, which the prompt falls back to when June has seen no window of it. On Linux the two are the same word. On Windows they can be unrelated strings: Teams is the package MSTeams, runs as ms-teams.exe and is called Microsoft Teams, and matching windows on the package name found none, so the prompt read "MSTeams is using your microphone" with the call's window on screen.
type micHolder struct{ proc, name string }

// holderProcs is the process name of each holder, in order.
func holderProcs(users []micHolder) []string {
	procs := make([]string, len(users))
	for i, u := range users {
		procs[i] = u.proc
	}
	return procs
}

// describe names each capturing application the way a person would recognise it: by the window it has open, falling back to the application's own name when June has seen no window for it.
//
// Input: the applications holding the microphone, and the episodes June recorded recently. Output: one display name each, cut to one line's worth.
func describe(users []micHolder, eps []db.Episode) []string {
	named := make([]string, 0, len(users))
	for _, u := range users {
		name := windowFor(u.proc, eps)
		if name == "" {
			name = u.name
		} else {
			name = withoutBrowserStatus(name)
		}
		// A name over the cap keeps one rune of the budget for the ellipsis, and is trimmed before it so the marker does not follow a space.
		if len([]rune(name)) > maxNameRunes {
			name = strings.TrimSpace(util.Runes(name, maxNameRunes-1)) + "…"
		}
		named = append(named, name)
	}
	return named
}

// consentEntry is one app under Windows' microphone consent store (HKCU\...\CapabilityAccessManager\ConsentStore\microphone): its subkey name and the LastUsedTimeStart and LastUsedTimeStop stamps Windows keeps for it. It lives here rather than in the Windows file so its rule is tested on Linux.
type consentEntry struct {
	// key is a package family name for a packaged app, or for a desktop app (under NonPackaged) the exe's full path with # in place of each backslash.
	key      string
	packaged bool
	// start and stop are FILETIMEs: 100-nanosecond ticks since 1601 UTC.
	start, stop uint64
	// proc and name are what the Windows reader resolved the app to, for a holder only: the executable its windows run under (a package's Application Executable, ms-teams for MSTeams), and the name Windows itself shows for it (the package's DisplayName, or the exe's FileDescription, which is what Task Manager lists a process as). Either may be empty, and consentUsers then derives it from the key.
	proc, name string
}

// wslBridgeName is what the prompt calls WSLg's msrdc.exe. Its FileDescription is "Remote Desktop", which is true of the program and says nothing to a person who never opened a remote desktop: what it carries is the microphone of a Linux app running under WSL.
const wslBridgeName = "WSL"

// wslBridgeMinHold is how long WSLg's msrdc.exe has to have held the microphone before it counts as a call. It is the RDP client WSLg draws Linux apps' windows and sound through, and it opens the Windows microphone whenever anything in WSL reads WSLg's audio source, not only for a call: on 2026-10-03 at 05:13:47 it held it for 5.8 s, two seconds after a dictation ended, with nothing in WSL in a call, and that was asked about as a meeting. A Linux call app under WSLg holds it for the length of the call, so the bridge is counted once its hold has outlasted that kind of grab, and a real call there is asked about half a minute late.
const wslBridgeMinHold = 30 * time.Second

// isWSLBridge reports whether a NonPackaged consent key is WSLg's msrdc.exe: C:\Program Files\WSL\msrdc.exe for WSL from its MSI, or the same exe inside the Store package's folder under WindowsApps. A Remote Desktop client's own msrdc.exe lives elsewhere and is a real remote session, so it is left alone.
func isWSLBridge(key string) bool {
	k := strings.ToLower(key)
	return strings.HasSuffix(k, "#msrdc.exe") && (strings.Contains(k, "#wsl#") || strings.Contains(k, "windowssubsystemforlinux"))
}

// fromGoBuild reports whether a NonPackaged consent key is a binary the go command built: `go test` and `go run` leave them under a go-build directory, in the temp directory or the build cache. That is a program being worked on, never a meeting app, and on the machines June is developed on it is June's own tree: internal/audio's tests open the microphone, and a `go run` of the daemon holds it the way the daemon does, under a name the june/junew rule cannot see.
func fromGoBuild(key string) bool {
	return strings.Contains(strings.ToLower(key), "#go-build")
}

// fileTime turns a consent-store stamp into a time. Input: a FILETIME, 100-nanosecond ticks since 1601-01-01 UTC. Output: the same instant.
func fileTime(ft uint64) time.Time {
	// 116444736000000000 is the number of ticks between 1601-01-01 and 1970-01-01.
	return time.Unix(0, (int64(ft)-116444736000000000)*100)
}

// packageShortName is a package's Name without its publisher's prefix, "WhatsAppDesktop" for 5319275A.WhatsAppDesktop and "Slack" for 91750D7E.Slack: the closest the family name comes to the app's own name, for when its manifest could not be read.
func packageShortName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// consentUsers returns the apps holding the microphone according to the consent store, excluding June itself. An app holds it when it has started using it and not stopped since: a non-zero start stamp and a zero stop stamp.
// Input: the entries under microphone and microphone\NonPackaged, and the time now, which WSLg's bridge's hold is measured against. Output: one holder per app, in order and without repeats by name; a holder's proc and name are the ones the Windows reader resolved, or else the exe or package name from the key.
// ponytail: every holder counts as a call, so a dictation tool on Windows gets asked about the way Handy was on Linux; telling them apart needs the render sessions (IAudioSessionManager2, GetProcessId) to see which holders also play audio back. An app that crashes mid-call leaves its stop stamp at 0, so it reads as holding the microphone until it next runs; cross-checking that the process still exists would fix that.
func consentUsers(entries []consentEntry, now time.Time) []micHolder {
	var users []micHolder
	seen := map[string]bool{}
	for _, e := range entries {
		if e.start == 0 || e.stop != 0 {
			continue
		}
		bin := e.key[strings.LastIndex(e.key, "#")+1:]
		if e.packaged {
			bin, _, _ = strings.Cut(e.key, "_")
		}
		// The Windows package ships the daemon twice, june.exe with a console and junew.exe without one for the login entry; either can be the process holding the microphone for voice or a recording.
		if base := strings.TrimSuffix(bin, ".exe"); base == "" || strings.EqualFold(base, juneBinary) || strings.EqualFold(base, juneBinary+"w") {
			continue
		}
		if !e.packaged && fromGoBuild(e.key) {
			continue
		}
		proc, name := e.proc, e.name
		if !e.packaged && isWSLBridge(e.key) {
			if now.Sub(fileTime(e.start)) < wslBridgeMinHold {
				continue
			}
			name = wslBridgeName
		}
		fallback := streamName(bin, "")
		if e.packaged {
			fallback = packageShortName(bin)
		}
		if proc == "" {
			proc = fallback
		}
		if name == "" {
			name = fallback
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		users = append(users, micHolder{proc: proc, name: name})
	}
	return users
}

// meetingWatch remembers enough between polls to ask about a call once, then leave it alone, and then end the recording it started when the call goes away. The zero value is ready to use.
type meetingWatch struct {
	// mu guards ours, which the poll loop and a press on the notice's own button both write.
	mu    sync.Mutex
	held  int
	asked bool
	// ours is true while a recording this watcher started is still running, which is the only kind it may stop. A recording the user started from the tray is theirs to stop, since it may be recording something that never opens a call stream at all.
	ours bool
	// gone counts the consecutive polls that have found no call while a recording of ours is running.
	gone int
}

// started records that the watcher's own Start succeeded, which is what makes the recording one this watcher may stop again. It takes the lock because a press on the notice's own button starts a recording from the HTTP goroutine while the poll loop is reading the same field.
func (w *meetingWatch) started() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ours = true
}

// step folds one poll into the watch and reports what to do about it.
//
// Input: the applications holding the microphone, and whether June is already recording. Output: ask, true exactly once per call, on the poll where the microphone has been held long enough to be a call and no question has been asked yet; and stop, true once for a recording this watcher started after asksAfterPolls consecutive polls with no call.
//
// Releasing the microphone is what ends a call, so the next one is asked about again — and it is also what ends a recording June started on its own. The evidence is the same in both directions, and so is the number of polls: one poll without a call stream is a browser reopening its capture, and two is the call being over.
func (w *meetingWatch) step(users []string, recording bool) (ask, stop bool) {
	if recording || len(users) == 0 {
		w.held, w.asked = 0, false
	}
	if !recording {
		// Nothing is recording, so the watcher has no recording of its own left to stop — whatever starts next is the user's until this watcher starts one itself.
		w.gone, w.ours = 0, false
		if len(users) == 0 {
			return false, false
		}
		w.held++
		if w.held < asksAfterPolls || w.asked {
			return false, false
		}
		w.asked = true
		return true, false
	}
	if !w.ours || len(users) > 0 {
		w.gone = 0
		return false, false
	}
	w.gone++
	if w.gone < asksAfterPolls {
		return false, false
	}
	w.ours, w.gone = false, 0
	return false, true
}

// askBody is the line under the prompt's title. It names the application holding the microphone, because "Chrome is using your microphone" is a question a person can answer and "in a meeting?" on its own is a guess.
func askBody(users []string) string {
	return strings.Join(users, ", ") + " is using your microphone."
}

// recordNoticeKind is the kind the meeting question is raised under, on the window's card and in the answer registry alike.
const recordNoticeKind = "meeting"

// askToRecord puts the question to the user and reports whether they said to record. It goes to the desktop window's own card when a window is listening and to a desktop notification only when none is, which is the rule every other question June asks already followed — this one posted a GNOME banner even with the window open, so the answer sat on a surface the settled design does not use.
func askToRecord(ctx context.Context, users []string) bool {
	n := proactive.Notice{
		Title:   "In a meeting?",
		Body:    askBody(users),
		Kind:    recordNoticeKind,
		Actions: []proactive.Action{{Key: "record", Label: "Start recording"}},
	}
	// apply is nil: the watcher registers what Start recording does for as long as it is running, which outlives this one question and is what lets a press land after it has timed out.
	return proactive.Ask(n, notifyWait, proactive.NotifySendAsk, nil) == "record"
}

// setupNudgeEvery is how often a call may bring up the "set up meeting transcripts" notice in place of the question, when transcripts are not installed. Once a day: the microphone opens for Discord, games and voice notes as readily as for meetings, and a user who has not set transcripts up yet should hear about it, not be asked on every call.
const setupNudgeEvery = 24 * time.Hour

// WatchForMeetings asks whether to record whenever an application other than June holds the microphone for long enough to be a call, and starts recording if the answer is yes. It returns when ctx is cancelled.
//
// The microphone is the signal rather than a meeting app's window, because it is the one thing every call has in common: it needs no list of which applications count, and it does not fire for a meeting tab that is merely open. Playback from the same application is what tells a call from dictation or a voice note. What the two together cannot tell is a call from, say, a voice message being listened to and answered, which is why the default is to ask rather than to record.
//
// With autoRecord set the question is skipped and recording starts on the same signal. Whether to ask at all is read from the config on every call rather than once, so turning the offer off in Settings takes effect on the next call. Nothing is offered or recorded while meeting transcripts are not installed; the call brings up a notice pointing at Local features instead, at most once a day.
func WatchForMeetings(ctx context.Context, rec *Recorder, autoRecord bool) {
	if rec == nil {
		return
	}
	ticker := time.NewTicker(micPollInterval)
	defer ticker.Stop()

	var w meetingWatch
	// The notice's "Start recording" button works whether or not askToRecord is still waiting on it: a press that lands after the question timed out, after a daemon restart, or on a second surface runs this instead, and the recording it starts is still one this watcher will stop when the call ends. Registered only while the watcher is running, so nothing starts a recording there is nobody left to stop.
	proactive.SetNoticeAction(recordNoticeKind, "record", func() error {
		if rec.Active() {
			return nil
		}
		if err := rec.Start(); err != nil {
			return err
		}
		w.started()
		slog.Info("started the meeting recording from the notice's own button")
		return nil
	})
	defer proactive.SetNoticeAction(recordNoticeKind, "record", nil)

	// nudged is when the set-up notice last went up, kept for the watcher's lifetime; a daemon restart shows it again at the next call, which is rare enough not to nag.
	var nudged time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			users := readMicUsers(ctx)
			procs := holderProcs(users)
			ask, stop := w.step(procs, rec.Active())
			if stop {
				slog.Info("the call June was recording has released the microphone, stopping the recording")
				if _, err := rec.StopAndProcess(ctx); err != nil {
					slog.Error("failed to stop the meeting recording June started", "error", err)
				}
				continue
			}
			if !ask {
				continue
			}
			// ReadConfig, not LoadConfig: this runs on the watcher's goroutine, and LoadConfig would write a default file or set aside one the user is half-way through saving. A read that fails is the defaults, with the offer on.
			if cfg, _ := config.ReadConfig(); !autoRecord && !cfg.Meetings.OfferEnabled() {
				slog.Debug("something else is holding the microphone, and the meeting offer is turned off", "processes", procs)
				continue
			}
			// The prompt names the window rather than the process wherever one can be found, since a call in a browser tab is "chrome" and so is everything else in that browser.
			// The desktop is asked before June's own history, because history is always behind here: the microphone opens as the call is joined and the tracker does not record the window for another minute or so, which is well after the question has been asked and answered.
			var eps []db.Episode
			if recent, err := rec.store.EpisodesInWindow(ctx, time.Now().Add(-windowLookback), time.Now(), windowLookbackRows); err == nil {
				eps = recent
			}
			for _, p := range procs {
				if t := tracker.WindowTitleFor(ctx, p); t != "" {
					eps = append(eps, db.Episode{App: p, Title: t})
				}
			}
			names := describe(users, eps)
			slog.Info("something else is holding the microphone", "apps", names, "processes", procs, "auto", autoRecord)
			if !rec.TranscriptionReady() {
				if time.Since(nudged) >= setupNudgeEvery {
					nudged = time.Now()
					rec.notifyAt(setupTitle, askBody(names)+" To have June record and summarise calls, set up meeting transcripts in Settings → Local features.", setupPlace, setupID)
				}
				continue
			}
			if !autoRecord && !askToRecord(ctx, names) {
				continue
			}
			if err := rec.Start(); err != nil {
				slog.Error("failed to start the meeting recording June offered", "error", err)
				continue
			}
			// Only a recording that actually started is one this watcher will stop again when the call goes away.
			w.started()
		}
	}
}
