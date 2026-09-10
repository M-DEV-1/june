// Package proactive is the daemon's initiative seam: a per-minute scheduler that, once per day, writes Ora's diary after the evening close hour and delivers a short brief when the user first shows up after the morning hour. Everything here is best-effort — a failed store read or brain call is logged and simply retried on the next tick, never fatal.
package proactive

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
)

// activityWindow is how recent the newest episode must be for the user to count as present at the keyboard. The brief waits for this so it lands when the user actually sits down, not while the machine idles overnight.
const activityWindow = 10 * time.Minute

// briefMinutesWindow is how far back the brief looks for meeting minutes whose action items may still be open.
const briefMinutesWindow = 3 * 24 * time.Hour

// dayFormat is the local calendar-day key used for diary rows.
const dayFormat = "2006-01-02"

// dutyTimeoutFor is how long one duty may take, given the hard limit on a single brain call. Input: the configured brain timeout. Output: room for the two calls the evening close makes plus two minutes for the store reads and the prompt assembly around them.
// A duty needs a deadline at all because a call that dies silently — a laptop suspend, a Wi-Fi drop mid-TLS — used to leave the close blocked forever and every duty after it never ran again until the daemon was restarted. The number is derived rather than fixed because a flat ten minutes was exactly two calls at the default 300-second ceiling with nothing left over, and said nothing at all about a machine that raised that ceiling.
func dutyTimeoutFor(brainCall time.Duration) time.Duration {
	return 2*brainCall + 2*time.Minute
}

// defaultDutyTimeout bounds one tick duty on a machine that has not set its own brain timeout. See SetBrainTimeout for the one that has.
var defaultDutyTimeout = dutyTimeoutFor(config.DefaultBrainTimeoutSeconds * time.Second)

// routineTimeout bounds one routine run, which goes through the whole tool loop and so is allowed considerably longer than a duty's own single brain call.
const routineTimeout = 30 * time.Minute

// firstBackoff and laterBackoff are how long a failed close or brief waits before it is attempted again: five minutes after the first failure in a row, thirty after every one after that. Without them a broken brain cost one counted request, one warning line and one usage tally every minute for the rest of the day.
const (
	firstBackoff = 5 * time.Minute
	laterBackoff = 30 * time.Minute
)

// Store is the slice of *db.Store the scheduler needs: the diary rows both duties key their once-per-day state off, the action items the brief chases, and the day's material the evening close writes from. Declared here rather than taking *db.Store whole, so this package states its entire data dependency in one place and widening it is a deliberate edit instead of an accident.
type Store interface {
	// Both duties key their once-per-day state off the diary table itself, which is why a daemon restart never repeats or loses a delivery.
	DiaryEntry(ctx context.Context, day, kind string) (string, error)
	SetDiaryEntry(ctx context.Context, day, kind, content string) error

	// The open action items the brief names, and the status/priority edits a one-click answer applies to one.
	OpenActionItems(ctx context.Context) ([]memory.ActionItem, error)
	SetActionStatus(ctx context.Context, noteID int64, status string) error
	SetActionPriority(ctx context.Context, noteID int64, priority string) error
	// ActionItemsByOwner is what maybeTaskNotices (see notify.go) reads to find the user's own action items, oldest first and note id included, so it can tell a meeting's newly-lifted items from ones already announced.
	ActionItemsByOwner(ctx context.Context, owner string) ([]memory.ActionItem, error)

	// The day's material the close writes from, plus the store's own notion of "now".
	SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error)
	GetLiveThreads(ctx context.Context, limit int) ([]memory.Thread, error)
	NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]db.Note, error)
	PersonalContext(ctx context.Context) ([]db.PersonalEntry, error)
	MemoryAsOf(ctx context.Context, source string) (time.Time, error)

	// Routines and SetRoutineRun back the routine runner (see routine.go): every routine to check, and where each due one's result is recorded. TryStart/Finish are the in-flight guard shared with POST /routines/{id}/run so the two never run the same routine at once.
	Routines(ctx context.Context) ([]db.Routine, error)
	SetRoutineRun(ctx context.Context, id int64, when time.Time, answer string) error
	TryStart(id int64) bool
	Finish(id int64)

	// The notices the user pushed to later from a notification's own buttons, and the per-minute check that brings the due ones back. See notify.go.
	AddSnooze(ctx context.Context, kind, noticeID, title, body string, due time.Time) (int64, error)
	DueSnoozes(ctx context.Context, now time.Time) ([]db.Snooze, error)
	MarkSnoozeFired(ctx context.Context, id int64) error
	CancelSnoozes(ctx context.Context, kind, noticeID string) (int, error)
}

// Scheduler owns the two daily proactive duties. Construct with New and run with Run; both duties key their once-per-day state off the diary table itself (the close is done when today's kind='day' row exists, the brief when today's kind='brief' row does), so a daemon restart never repeats or loses a delivery.
type Scheduler struct {
	// ask posts the one-click question about a stale action item and blocks until the user answers. Nil disables asking.
	ask    func(title, body string, actions []string) (string, error)
	store  Store
	brain  brain.Brain
	notify func(title, body string)
	// briefHour and closeHour are local hours; negative disables that seam. Resolved from config in New.
	briefHour int
	closeHour int
	// now is the clock, replaceable in tests. Day boundaries and hour checks are all local time.
	now func() time.Time
	// weeklyStudy runs the Sunday-only weekly system log + distillation study pass; unset (the zero value) disables the trigger entirely. See SetWeeklyStudy.
	weeklyStudy func(ctx context.Context, now time.Time) error
	// routineAsk puts a due routine's instruction to the daemon's own ask path; unset disables the routine runner entirely. See SetRoutineAsk.
	routineAsk func(ctx context.Context, question string) (string, error)
	// notifier posts a notice as a desktop notification carrying the Open/Done/snooze buttons, and calls back with whichever the user pressed; unset falls back to notify, which has none. See SetNotifier and notify.go.
	notifier Notifier
	// openWindow is what "Open in Ora" and a click on the notification body do; unset, they do nothing. See SetOpenWindow.
	openWindow func()
	// taskDone closes one task through the daemon's own task-done path when "Done" is pressed on a task notice; unset, Done only records the dismissal. See SetTaskDone.
	taskDone func(ctx context.Context, id string) error
	// askWait is how long the stale-item question waits on the window's card before giving up on it. Set to notifyWait in New, which is the same hour the notify-send fallback is given, and shortened by tests.
	askWait time.Duration
	// dutyTimeout is how long one duty on a tick may take before its context is cancelled. Set from defaultDutyTimeout in New and shortened by tests.
	dutyTimeout time.Duration
	// retryAfter is the earliest moment a duty that failed may be attempted again, keyed by duty name, and retries counts its consecutive failures, which is what picks the backoff step. Both are read and written only on the tick goroutine, so neither needs a lock.
	retryAfter map[string]time.Time
	retries    map[string]int
	// taskMark is the last task-notice watermark this process computed, kept so a SetDiaryEntry that failed does not make the same items be announced again on the next tick. Tick goroutine only.
	taskMark int64
	// lastRun remembers when each routine actually last ran, so a store that will not record the run does not make the routine re-ask every minute. Guarded by lastRunMu, since a routine finishes on its own goroutine while a later tick reads this.
	lastRun   map[int64]time.Time
	lastRunMu sync.Mutex
	// running counts the routine runs a tick has started and not yet finished, so tests can wait for them. Nothing in production waits on it.
	running sync.WaitGroup
}

// New builds a Scheduler from the store, a one-shot brain, a desktop-notification func (NotifySend in production), and the proactive config, whose zero hours resolve to the defaults.
func New(store Store, b brain.Brain, notify func(title, body string), cfg config.ProactiveConfig) *Scheduler {
	briefHour, closeHour := cfg.Hours()
	return &Scheduler{
		store: store, brain: b, notify: notify,
		briefHour: briefHour, closeHour: closeHour, now: time.Now,
		askWait:     notifyWait,
		dutyTimeout: defaultDutyTimeout,
		retryAfter:  map[string]time.Time{},
		retries:     map[string]int{},
		lastRun:     map[int64]time.Time{},
	}
}

// SetAsk wires the one-click question the brief asks about an item that has gone quiet. Unset, the brief still names stale items in its text and simply never asks about one — cmd/daemon.go is the only production caller. The func posts the question and blocks until the user answers or dismisses it, returning the action they chose ("" for a dismissal).
func (s *Scheduler) SetAsk(fn func(title, body string, actions []string) (string, error)) {
	s.ask = fn
}

// SetBrainTimeout sizes the per-duty deadline from the hard limit one brain call is allowed. Input: the configured brain timeout (config.BrainConfig.TimeoutSeconds, as a duration) — cmd/daemon.go is the only production caller. Output: nothing; unset, the deadline is sized for config.DefaultBrainTimeoutSeconds.
func (s *Scheduler) SetBrainTimeout(brainCall time.Duration) {
	s.dutyTimeout = dutyTimeoutFor(brainCall)
}

// SetWeeklyStudy wires the Sunday-only weekly system log + distillation study pass — cmd/daemon.go is the only production caller. Unset, the trigger never fires.
func (s *Scheduler) SetWeeklyStudy(fn func(ctx context.Context, now time.Time) error) {
	s.weeklyStudy = fn
}

// Run checks the brief and close conditions once a minute until ctx ends. A condition that is not yet met, or a duty that failed, is simply re-checked on the next tick.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.tick(ctx)
		case <-ctx.Done():
			slog.Debug("proactive scheduler stopped")
			return
		}
	}
}

// tick runs every duty's condition check once. Split from Run so tests can drive the schedule directly.
// The two store-only duties go first: they are a sqlite read each, and running them after the brain-calling ones made a "remind me in an hour" land ten minutes late whenever a close or a brief was slow.
func (s *Scheduler) tick(ctx context.Context) {
	s.duty(ctx, s.maybeSnoozes)
	s.duty(ctx, s.maybeTaskNotices)
	// Not wrapped in duty: maybeClose can close two days on one tick, and it gives each of them its own deadline rather than making four brain calls share one.
	s.maybeClose(ctx)
	s.duty(ctx, s.maybeBrief)
	s.duty(ctx, s.maybeWeeklyStudy)
	// Not wrapped in duty: maybeRoutines runs each due routine on its own goroutine, which outlives this call and so must not be handed a context that is cancelled when it returns. It gives each run its own routineTimeout instead.
	s.maybeRoutines(ctx)
}

// duty runs one tick duty under its own deadline. Input: the tick's context and the duty. Output: nothing. The deadline is per duty, so a brain call that never returns costs that one duty this tick rather than freezing every duty after it for the life of the daemon.
func (s *Scheduler) duty(ctx context.Context, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(ctx, s.dutyTimeout)
	defer cancel()
	fn(ctx)
}

// backedOff reports whether a duty is still inside the wait its last failure earned. Input: the duty's name and the tick's moment. Output: true when it must not be attempted yet.
func (s *Scheduler) backedOff(duty string, now time.Time) bool {
	return now.Before(s.retryAfter[duty])
}

// failed records that a duty just failed and sets when it may be tried again: firstBackoff after the first failure in a row, laterBackoff after any further one. Input: the duty's name and the tick's moment. Output: nothing.
func (s *Scheduler) failed(duty string, now time.Time) {
	s.retries[duty]++
	wait := firstBackoff
	if s.retries[duty] > 1 {
		wait = laterBackoff
	}
	s.retryAfter[duty] = now.Add(wait)
}

// succeeded clears whatever backoff a duty was under, so its next failure starts again at firstBackoff. Input: the duty's name. Output: nothing.
func (s *Scheduler) succeeded(duty string) {
	delete(s.retries, duty)
	delete(s.retryAfter, duty)
}

// waitRoutines blocks until every routine run this scheduler has started has finished. Only tests call it: production starts a run and lets the tick return.
func (s *Scheduler) waitRoutines() {
	s.running.Wait()
}

// notifyWait is how long a notify-send call that waits for a button is given before the child process is killed: past an hour nobody is coming to press it, and on a machine with no session bus every unanswered notice otherwise pinned a goroutine and a process until the daemon exited.
const notifyWait = time.Hour

// notifySendTimeout bounds a notify-send call that is not waiting for anything, so a wedged notification daemon cannot hold up whatever goroutine posted the notice.
const notifySendTimeout = 30 * time.Second

// NotifySendAsk posts a notification carrying one button per action and blocks until the user picks one or dismisses it, returning the chosen action's key ("" for a dismissal). Each action is "key=Label"; notify-send prints the key of whatever was clicked. --action implies --wait, so this call is as long-lived as the notification on screen, up to notifyWait.
func NotifySendAsk(title, body string, actions []string) (string, error) {
	args := []string{"-a", "Ora"}
	for _, a := range actions {
		args = append(args, "--action="+a)
	}
	args = append(args, title, body)
	ctx, cancel := context.WithTimeout(context.Background(), notifyWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "notify-send", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// weeklyStudyDuty is the backoff key the Sunday study is tracked under, so a failed pass waits like a failed close instead of being retried on every tick.
const weeklyStudyDuty = "weekly-study"

// maybeWeeklyStudy fires the Sunday-only weekly system log + distillation study pass, once the brief hour has passed and the user's first activity of the day shows up — the same first-activity gate maybeBrief uses, so it rides the Claude workday window rather than firing overnight. Disabled when no weeklyStudy func is wired (SetWeeklyStudy never called) or the brief hour itself is disabled.
// Today's kind='weekly-study' diary row is the once-per-Sunday marker, and it is written only when the pass returned nil — either because it did the work or because it deliberately had nothing to read (see cmd/daemon.go). A pass that failed is backed off and tried again, since it now runs under a deadline and one cut off mid-call used to be recorded as done and not attempted again until next Sunday.
func (s *Scheduler) maybeWeeklyStudy(ctx context.Context) {
	if s.weeklyStudy == nil || s.briefHour < 0 {
		return
	}
	now := s.now()
	if now.Weekday() != time.Sunday || now.Hour() < s.briefHour {
		return
	}
	if s.backedOff(weeklyStudyDuty, now) {
		return
	}
	day := now.Format(dayFormat)
	existing, err := s.store.DiaryEntry(ctx, day, "weekly-study")
	if err != nil {
		slog.Warn("weekly study: reading marker failed", "error", err)
		return
	}
	if existing != "" {
		return
	}
	last, err := s.store.MemoryAsOf(ctx, "episode:recent")
	if err != nil || last.IsZero() || s.now().Sub(last) > activityWindow {
		return
	}
	if err := s.weeklyStudy(ctx, now); err != nil {
		s.failed(weeklyStudyDuty, s.now())
		slog.Warn("weekly study failed, backing off", "error", err)
		return
	}
	if err := s.store.SetDiaryEntry(ctx, day, "weekly-study", "weekly system log and distillation study ran"); err != nil {
		slog.Warn("weekly study: writing marker failed", "error", err)
		return
	}
	s.succeeded(weeklyStudyDuty)
}

// readAction is the notify-send action key for the button that opens a long body in full.
const readAction = "read"

// longBodyRunes is the body length past which GNOME's banner has already cut the text off. GNOME shows a few lines of a notification and hides the rest, which is how every morning brief and meeting prep went unread past its first sentences.
const longBodyRunes = 200

// notifyArgs builds the notify-send arguments for one notification: the app name and icon, then for a body longer than longBodyRunes a "Read in full" action, then the title and body.
func notifyArgs(icon, title, body string) []string {
	args := []string{"-a", "Ora", "-i", icon}
	if len([]rune(body)) > longBodyRunes {
		args = append(args, "--action="+readAction+"=Read in full")
	}
	return append(args, title, body)
}

// Notice is one moment Ora has something to say about, drawn by the desktop window as its own card instead of being handed to GNOME. Title is the card's first line and Body the few lines under it. Place and ID say what a click on the card opens — Place names one of the app window's screens ("tasks", "days") and ID the row to select there, both empty when the moment points at nothing in particular. Kind names the moment: "brief", "close", "meeting", "task", "routine", "stale" or "note". Action and Until are empty on the notice itself and set only when the user has since dealt with it from its own notification: Action is "snoozed" or "done", and Until is the RFC 3339 moment a snoozed notice comes back, so the window can say "snoozed until 18:00" instead of drawing the card again.
type Notice struct {
	Title  string
	Body   string
	Place  string
	ID     string
	Kind   string
	Action string
	Until  string
	// Actions is the notice's own buttons, empty for all but a notice that asked a question of its own — the stale-item "Still open", so far. The window draws exactly these in place of the buttons its kind implies, and POSTs the pressed one's Key back on /notices/{kind}/{id}/action, where Act hands it to the goroutine waiting on the answer (see awaitAnswer in notify.go).
	Actions []Action
	// Expires is the RFC 3339 moment a question stops being answerable, set by Ask and empty on every notice that asked nothing. Past it the goroutine waiting on the answer has gone, so the button would 400; the window counts down to it and takes the card off screen rather than leaving a button on screen that no longer does anything.
	Expires string
}

// noticeSend is the desktop window's notice channel, wired once at startup by cmd/daemon.go and read from whichever goroutine a notification happens to be on, which is what noticeMu guards. Nil means nothing has been wired and every notification goes to notify-send.
var (
	noticeMu   sync.Mutex
	noticeSend func(Notice) bool
)

// SetNoticeSender wires the channel every notification in this package prefers over notify-send. Input: a func that offers one notice to the desktop window and reports whether a window was there to draw it — the daemon passes one that asks the ipc hub whether any window has been reading the event stream lately — or nil to send everything through notify-send again. Output: nothing.
func SetNoticeSender(send func(Notice) bool) {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	noticeSend = send
}

// sendNotice offers one moment to the desktop window. Input: the notice. Output: true when a window took it, and false when no sender is wired or no window is listening, in which case the caller falls back to notify-send.
func sendNotice(n Notice) bool {
	noticeMu.Lock()
	send := noticeSend
	noticeMu.Unlock()
	if send == nil {
		return false
	}
	// The card draws exactly the buttons its notice names, so a notice that names none offers only Open. Done and the three snoozes belong to a task and to the stale-task question, both of which name their own; a routine's report, a meeting moment or a transcription has nothing to complete and nowhere to push to later, and the card that offered them those answered "Could not do that" on every press.
	if len(n.Actions) == 0 {
		n.Actions = openOnlyActions
	}
	return send(n)
}

// say delivers one of the scheduler's own moments to whichever single surface is there to show it: the desktop window's own card when a window is reading the event stream, and otherwise the desktop notification carrying the full set of buttons. Input: the notice. Output: nothing.
// One surface, not both: the window's card offers the same Done and snooze buttons through POST /notices/{kind}/{id}/action, so posting a banner as well asked the user the same question twice. This is the rule the package-level Notify below already followed for a meeting's own moments.
func (s *Scheduler) say(n Notice) {
	if sendNotice(n) {
		return
	}
	s.post(n)
}

// Say is say for callers outside this package. Input: the notice. Output: nothing. The daemon hands it to internal/ipc so a routine run started from the window's own Run button reaches the user the same way the scheduler's tick on the same routine would, desktop-notification fallback included.
func (s *Scheduler) Say(n Notice) { s.say(n) }

// noticeKinds names the moment behind a notification from the icon it was posted with, which is the only thing such a call carries that says what it is about: the recorder posts everything it has to say about a meeting under the microphone icon, its "Before you join" prep included, and this package's own daily moments carry the calendar.
var noticeKinds = map[string]string{"audio-input-microphone": "meeting", "x-office-calendar": "day"}

// noticeKind is the kind a package-level Notify sends its notice under. Input: the icon the notification was posted with. Output: the moment's name, or "note" for an icon this package has no moment for.
func noticeKind(icon string) string {
	if kind, ok := noticeKinds[icon]; ok {
		return kind
	}
	return "note"
}

// openOnlyActions is the single button a moment posted outside the scheduler's own notices carries: a meeting's "Before you join" or "Recording saved" has nothing to complete and nowhere to push to later, only somewhere to open.
var openOnlyActions = []Action{{actionOpen, "Open in Ora"}}

// Notify posts a desktop notification through the same wired notifier the scheduler's own notices use — carrying an "Open in Ora" button — falling back to bare notify-send only when no notifier has been wired (SetNotifier never called, or its own post failed). A long body also gets a "Read in full" button that opens the whole text in a zenity window, because GNOME's banner truncates it; that path is unaffected, since a notification already open in zenity has somewhere to read the whole thing and does not need the bus notifier's button too. Failure is logged and ignored: a missing notification must never take down the work that produced it.
// The long form waits in the background for the button, up to an hour, so the caller never blocks on it.
// The desktop window gets first refusal: it draws the same text as a card of Ora's own, which is not cut off after two lines and can be clicked through to what it is about, so the notifier (and notify-send) are only reached when no window is listening.
func Notify(icon, title, body string) {
	NotifyAt(icon, title, body, "", "")
}

// NotifyAt is Notify for a notice whose card opens somewhere in the window. Input: as Notify, plus the place ("chats", "tasks", "days", "meetings", "routines") and the row id the card's Open goes to, both "" to open the window and nothing in particular.
func NotifyAt(icon, title, body, place, id string) {
	// The notice carries its one button rather than leaving the window to guess: nothing posted this way has a task behind it, so Done and the snoozes would have nothing to act on.
	n := Notice{Title: title, Body: body, Place: place, ID: id, Kind: noticeKind(icon), Actions: openOnlyActions}
	if sendNotice(n) {
		return
	}
	if len([]rune(body)) <= longBodyRunes {
		if notifyThroughBus(n) {
			return
		}
		args := notifyArgs(icon, title, body)
		ctx, cancel := context.WithTimeout(context.Background(), notifySendTimeout)
		defer cancel()
		if err := exec.CommandContext(ctx, "notify-send", args...).Run(); err != nil {
			slog.Debug("notify-send failed", "title", title, "error", err)
		}
		return
	}
	longArgs := notifyArgs(icon, title, body)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyWait)
		defer cancel()
		out, err := exec.CommandContext(ctx, "notify-send", longArgs...).Output()
		if err != nil {
			slog.Debug("notify-send failed", "title", title, "error", err)
			return
		}
		if strings.TrimSpace(string(out)) != readAction {
			return
		}
		f, err := os.CreateTemp("", "ora-*.md")
		if err != nil {
			return
		}
		f.WriteString(body)
		f.Close()
		if err := exec.Command("zenity", "--text-info", "--title="+title, "--filename="+f.Name(), "--width=720", "--height=560").Run(); err != nil {
			slog.Debug("zenity failed, opening the text with the default app", "error", err)
			exec.Command("xdg-open", f.Name()).Run()
		}
	}()
}

// notifyThroughBus offers a short notice to the wired bus notifier with a single "Open in Ora" button — for moments outside the scheduler's own (meeting prep, "Recording saved") that have nothing to complete and nowhere to snooze to. Input: the notice. Output: whether the notifier took it; false when none is wired or the desktop refused it, in which case the caller falls back to notify-send.
func notifyThroughBus(n Notice) bool {
	noticeMu.Lock()
	notifier, open := noticeNotifier, noticeOpen
	noticeMu.Unlock()
	if notifier == nil {
		return false
	}
	err := notifier.Notify(noticeKey(n), n.Title, n.Body, openOnlyActions, func(key string) {
		if key == actionOpen && open != nil {
			open()
		}
	})
	if err != nil {
		slog.Debug("could not post a notification through the bus, falling back to notify-send", "title", n.Title, "error", err)
		return false
	}
	return true
}

// NotifySend posts a calendar-icon notification; the recorder uses Notify with its own microphone icon.
func NotifySend(title, body string) {
	Notify("x-office-calendar", title, body)
}
