// notify.go is how one of Ora's moments reaches the desktop as a notification the user can act on without opening anything: Open in Ora, Done, and three ways to be reminded later. The buttons are the reason this talks to org.freedesktop.Notifications itself rather than shelling out — notify-send can only offer buttons by blocking a whole process for as long as the notification is on screen, and everything here has to survive being answered hours later or not at all.
package proactive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"ora/internal/db"
	"ora/internal/memory"
)

// The keys the desktop reports back when a button is pressed. "default" is the one the notification body itself carries: clicking the banner rather than a button sends it.
const (
	actionOpen     = "default"
	actionDone     = "done"
	actionHour     = "hour"
	actionEvening  = "evening"
	actionTomorrow = "tomorrow"
)

// noticeActions are the buttons every notice carries, in the order they are offered. One list, so a snooze that comes back can be dealt with exactly like the first posting.
var noticeActions = []Action{
	{actionOpen, "Open in Ora"},
	{actionDone, "Done"},
	{actionHour, "In an hour"},
	{actionEvening, "This evening"},
	{actionTomorrow, "Tomorrow"},
}

// eveningHour and morningHour are the local clock hours "This evening" and "Tomorrow" bring a notice back at.
const (
	eveningHour = 18
	morningHour = 9
)

// Action is one button on a notification. Key is what the desktop reports back when it is pressed; Label is what the user reads on it.
type Action struct {
	Key   string
	Label string
}

// Notifier posts one desktop notification and reports what the user did with it.
type Notifier interface {
	// Notify shows the notification and returns as soon as the desktop has taken it. noticeKey identifies which notice this is ("kind|id", see noticeKey) so a later press on the window's own card can dismiss this exact banner through Close. chose is called at most once, later and from another goroutine, with the key of the button the user pressed or "" when the notification was dismissed without one.
	Notify(noticeKey, title, body string, actions []Action, chose func(buttonKey string)) error
	// Close dismisses whichever banner Notify most recently posted under noticeKey. A no-op when nothing is posted under that key — the window and the banner can race to deal with the same notice, and the loser here has nothing left to close.
	Close(noticeKey string) error
}

// noticeKey identifies one notice to a Notifier: kind and id together are what the window's rail line and its desktop banner both point at, and what ties a window press back to the exact banner it should also dismiss.
func noticeKey(n Notice) string {
	return n.Kind + "|" + n.ID
}

// The desktop notification service every Linux desktop provides, and the icon Ora's own moments are posted with.
const (
	notifyDest  = "org.freedesktop.Notifications"
	notifyIface = "org.freedesktop.Notifications"
	notifyPath  = dbus.ObjectPath("/org/freedesktop/Notifications")
	noticeIcon  = "x-office-calendar"
)

// BusNotifier posts notifications on the session bus and watches it for what the user pressed. Presses arrive as ActionInvoked and dismissals as NotificationClosed, both carrying the id the Notify call returned, which is what ties a press back to the notice that caused it.
type BusNotifier struct {
	conn *dbus.Conn
	// mu guards waiting and keyed, and is held across the Notify call itself, so a user quick enough to click before the id is recorded still finds a handler waiting for them.
	mu      sync.Mutex
	waiting map[uint32]func(string)
	// keyed maps a notice's own key to the D-Bus id Notify posted it under, so Close can find which banner a window press should also dismiss.
	keyed map[string]uint32
}

// NewNotifier returns the notifier the daemon should post with: the session bus when there is one, and notify-send when there is not. Input: a context that ends the bus listener. Output: a notifier, always.
func NewNotifier(ctx context.Context) Notifier {
	n, err := NewBusNotifier(ctx)
	if err != nil {
		slog.Warn("no session bus for notifications, falling back to notify-send", "error", err)
		return sendNotifier{}
	}
	return n
}

// NewBusNotifier connects to the session bus and starts listening for presses and dismissals. Input: a context that ends the listener. Output: the notifier, or an error when there is no session bus to post on.
func NewBusNotifier(ctx context.Context) (*BusNotifier, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	for _, member := range []string{"ActionInvoked", "NotificationClosed"} {
		if err := conn.AddMatchSignal(
			dbus.WithMatchObjectPath(notifyPath),
			dbus.WithMatchInterface(notifyIface),
			dbus.WithMatchMember(member),
		); err != nil {
			return nil, fmt.Errorf("subscribe %s: %w", member, err)
		}
	}
	n := &BusNotifier{conn: conn, waiting: map[uint32]func(string){}, keyed: map[string]uint32{}}
	sigCh := make(chan *dbus.Signal, 16)
	conn.Signal(sigCh)
	go n.listen(ctx, sigCh)
	return n, nil
}

// Notify posts one notification with the given buttons, at normal urgency and under the ora desktop entry, and never expires it: a snooze button nobody is there to press is worth nothing, so the banner stays until the user deals with it.
func (n *BusNotifier) Notify(noticeKey, title, body string, actions []Action, chose func(string)) error {
	flat := make([]string, 0, len(actions)*2)
	for _, a := range actions {
		flat = append(flat, a.Key, a.Label)
	}
	hints := map[string]dbus.Variant{
		"urgency":       dbus.MakeVariant(byte(1)),
		"desktop-entry": dbus.MakeVariant("ora"),
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var id uint32
	// The zero replaces_id posts a new notification rather than replacing an existing one; the zero expire_timeout means it never times out.
	if err := n.conn.Object(notifyDest, notifyPath).Call(notifyIface+".Notify", 0,
		"Ora", uint32(0), noticeIcon, title, body, flat, hints, int32(0)).Store(&id); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	n.waiting[id] = chose
	// A key with no id behind it ("meeting|", every package-level Notify) would make each such banner overwrite the last one's entry, so only a notice with a row of its own is remembered for Close.
	if noticeKey != "" && !strings.HasSuffix(noticeKey, "|") {
		n.keyed[noticeKey] = id
	}
	return nil
}

// Close dismisses the banner Notify most recently posted under noticeKey, through the same CloseNotification call the desktop uses to auto-expire one. A key nothing was posted under — the banner already gone, or this notifier never posted it — is not an error: the window and the banner can race to deal with the same notice, and the loser here simply has nothing left to close.
func (n *BusNotifier) Close(noticeKey string) error {
	n.mu.Lock()
	id, ok := n.keyed[noticeKey]
	if ok {
		delete(n.keyed, noticeKey)
	}
	n.mu.Unlock()
	if !ok {
		return nil
	}
	return n.conn.Object(notifyDest, notifyPath).Call(notifyIface+".CloseNotification", 0, id).Err
}

// listen turns the bus signals into calls on whoever is waiting for that notification, until ctx ends.
func (n *BusNotifier) listen(ctx context.Context, sigCh chan *dbus.Signal) {
	defer n.conn.RemoveSignal(sigCh)
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-sigCh:
			if !ok {
				return
			}
			switch sig.Name {
			case notifyIface + ".ActionInvoked":
				if len(sig.Body) < 2 {
					continue
				}
				id, _ := sig.Body[0].(uint32)
				key, _ := sig.Body[1].(string)
				n.finish(id, key)
			case notifyIface + ".NotificationClosed":
				if len(sig.Body) < 1 {
					continue
				}
				id, _ := sig.Body[0].(uint32)
				n.finish(id, "")
			}
		}
	}
}

// finish hands the press to whoever is waiting on that notification id and forgets it, so the NotificationClosed that follows an ActionInvoked for the same notification does nothing a second time. An id nobody is waiting on — another app's notification on the same bus — is ignored.
func (n *BusNotifier) finish(id uint32, key string) {
	n.mu.Lock()
	chose := n.waiting[id]
	delete(n.waiting, id)
	// The banner is gone, so the key that pointed at it goes too; otherwise the map grows for the life of the daemon and a later Close aims at a dead id.
	for k, v := range n.keyed {
		if v == id {
			delete(n.keyed, k)
		}
	}
	n.mu.Unlock()
	if chose != nil {
		chose(key)
	}
}

// sendNotifier is the fallback for a machine with no session bus to reach: notify-send offers the same buttons and prints back the key of whichever was pressed, at the cost of a process sitting there for as long as the notification is on screen.
type sendNotifier struct{}

// Notify posts through notify-send from its own goroutine, since that command does not return until the notification is answered or gone.
func (sendNotifier) Notify(noticeKey, title, body string, actions []Action, chose func(string)) error {
	labels := make([]string, 0, len(actions))
	for _, a := range actions {
		labels = append(labels, a.Key+"="+a.Label)
	}
	go func() {
		key, err := NotifySendAsk(title, body, labels)
		if err != nil {
			slog.Debug("notify-send could not offer a notice's buttons", "title", title, "error", err)
			return
		}
		chose(key)
	}()
	return nil
}

// Close is a no-op: notify-send is a blocking process per banner with no id this notifier can reach back into, so a window press cannot also dismiss the notify-send fallback's own banner. A machine on this fallback has no session bus, which is the same reason it has no other way to close one either.
func (sendNotifier) Close(string) error { return nil }

// noticeNotifier and noticeOpen are the desktop notifier and the "Open in Ora" callback the package-level Notify below posts through — wired by SetNotifier and SetOpenWindow alongside the Scheduler's own copies, and guarded by noticeMu, the same lock sendNotice uses. Nil means neither has been wired yet (or the daemon never calls SetNotifier), in which case Notify falls back to raw notify-send.
var (
	noticeNotifier Notifier
	noticeOpen     func()
)

// SetNotifier wires the desktop notifier every notice is posted through — cmd/daemon.go is the only production caller. Unset, notices fall back to the plain notification func New was built with, which carries no buttons. Also wires the package-level Notify below onto the same notifier, since a meeting's own moments reach this package outside the scheduler entirely.
func (s *Scheduler) SetNotifier(n Notifier) {
	s.notifier = n
	noticeMu.Lock()
	noticeNotifier = n
	noticeMu.Unlock()
}

// SetOpenWindow wires what "Open in Ora", and a click on the notification body itself, does: the same thing the tray's own Open Ora item does. Unset, those clicks do nothing. Also wires the package-level Notify below's own "Open in Ora" button onto the same func.
func (s *Scheduler) SetOpenWindow(fn func()) {
	s.openWindow = fn
	noticeMu.Lock()
	noticeOpen = fn
	noticeMu.Unlock()
}

// SetTaskDone wires the daemon's own task-done path, the one POST /tasks/{id}/done answers through, which "Done" on a task notice calls with that task's id. Unset, Done on a task notice only records the dismissal.
func (s *Scheduler) SetTaskDone(fn func(ctx context.Context, id string) error) {
	s.taskDone = fn
}

// post shows one notice on the desktop with the full set of buttons and applies whatever the user presses. Input: the notice. Output: nothing. Falls back to the plain notification func when no notifier is wired or the desktop refuses the notification, which loses the buttons but never the message.
//
// ponytail: the press is applied on a background context, because it arrives long after the tick that posted the notice has returned and there is no longer a request to be cancelled with.
func (s *Scheduler) post(n Notice) {
	if s.notifier != nil {
		if err := s.notifier.Notify(noticeKey(n), n.Title, n.Body, noticeActions, func(key string) {
			s.chose(context.Background(), n, key)
		}); err == nil {
			return
		} else {
			slog.Debug("could not post a notification with its buttons", "title", n.Title, "error", err)
		}
	}
	s.notify(n.Title, n.Body)
}

// chose applies the button the user pressed on a notice. Input: the notice it was posted for, and the pressed button's key — "" for a notification dismissed without pressing anything, which deliberately leaves nothing behind and lets the moment come round again on its own. Output: nothing; a failure is logged, since the notification it would be reported on is already gone.
// Done and the three snooze buttons go through Act — the exact path POST /notices/{kind}/{id}/action answers through — so a task closed or snoozed from the banner takes the same code, with the same banner-closing side effect, as one closed or snoozed from the window's own rail line. Open has no equivalent on the window's rail line (the window is already open) and a dismissal changes nothing, so both stay here.
func (s *Scheduler) chose(ctx context.Context, n Notice, key string) {
	switch key {
	case "":
		return
	case actionOpen:
		if s.openWindow != nil {
			s.openWindow()
		}
	case actionDone, actionHour, actionEvening, actionTomorrow:
		if err := s.Act(ctx, n.Kind, n.ID, n.Title, n.Body, key); err != nil {
			slog.Warn("could not apply a notification's own button", "kind", n.Kind, "id", n.ID, "key", key, "error", err)
		}
	default:
		slog.Debug("a notification came back with a button Ora does not offer", "key", key)
	}
}

// snooze puts a notice away until later and tells the window it has gone. Input: the notice and the pressed snooze button's key. Output: the store's error when the snooze could not be written, so the route that asked answers with a failure instead of a 200 that snoozed nothing; nil once it is stored.
func (s *Scheduler) snooze(ctx context.Context, n Notice, key string) error {
	due := snoozeUntil(s.now(), key)
	if _, err := s.store.AddSnooze(ctx, n.Kind, n.ID, n.Title, n.Body, due); err != nil {
		slog.Warn("could not snooze a notice", "kind", n.Kind, "id", n.ID, "error", err)
		return err
	}
	n.Action = "snoozed"
	n.Until = due.Format(time.RFC3339)
	sendNotice(n)
	return nil
}

// markDone applies "Done". A task notice names a task, so it goes through the daemon's own task-done path and is closed for real. Every other kind has nothing to complete — a routine notice or a morning brief is Ora reporting, not work owed — so all this records is that the user cleared it. Either way, any snooze still pending for this notice is cancelled, so a Done pressed while a snooze is in flight — from the original notice or from a re-fired one, both carrying the same kind and id — stops it firing again. Input: the notice. Output: whatever closing the task returned when that failed — ErrTaskGone when the id no longer names one — otherwise nil; cancelling the pending snooze is best-effort and only logged on failure, since the notice itself is already closed by then.
func (s *Scheduler) markDone(ctx context.Context, n Notice) error {
	if n.Kind == "task" && n.ID != "" && s.taskDone != nil {
		if err := s.taskDone(ctx, n.ID); err != nil {
			return err
		}
	}
	if _, err := s.store.CancelSnoozes(ctx, n.Kind, n.ID); err != nil {
		slog.Warn("could not cancel a done notice's pending snooze", "kind", n.Kind, "id", n.ID, "error", err)
	}
	slog.Info("notice cleared from its notification", "kind", n.Kind, "id", n.ID)
	n.Action = "done"
	sendNotice(n)
	return nil
}

// ErrBadNoticeAction is what Act returns for an action string that is none of the four buttons a notice offers.
var ErrBadNoticeAction = errors.New("not a notice action")

// ErrTaskGone is what Act returns for "done" on a task notice whose task no longer exists. The taskDone func wired with SetTaskDone (cmd/daemon.go's loopback call to its own POST /tasks/{id}/done) is what recognises the 404 that route answers with and wraps this in.
var ErrTaskGone = errors.New("the task this notice named no longer exists")

// Act applies one notice button exactly as pressing it on the desktop notification would. POST /notices/{kind}/{id}/action calls this directly, and chose (a D-Bus press) calls it too, for Done and the three snooze buttons — the one path both surfaces answer through, so a task closed or snoozed from either takes the same code. Input: kind and id name the notice ("" for one with no task or place behind it, such as a brief), title and body are what a snooze needs to re-fire the notice later, and action is "done", "hour", "evening" or "tomorrow". Output: ErrBadNoticeAction for any other action string, whatever markDone returned (ErrTaskGone included) for "done", the store's error when a snooze could not be written, else nil.
// Once the action is applied, this also closes the notice's own desktop banner, if any: without it, a done or snoozed task answered from the window would leave its notification sitting on screen asking the same question a second time. A D-Bus press closing its own already-closing banner a second time this way is harmless — Close is a no-op once the key is gone.
func (s *Scheduler) Act(ctx context.Context, kind, id, title, body, action string) error {
	n := Notice{Title: title, Body: body, Kind: kind, ID: id}
	var err error
	switch action {
	case actionDone:
		err = s.markDone(ctx, n)
	case actionHour, actionEvening, actionTomorrow:
		err = s.snooze(ctx, n, action)
	default:
		return ErrBadNoticeAction
	}
	if s.notifier != nil {
		if cerr := s.notifier.Close(noticeKey(n)); cerr != nil {
			slog.Debug("could not close a notice's own banner after a window press", "kind", kind, "id", id, "error", cerr)
		}
	}
	return err
}

// snoozeUntil is when a snoozed notice comes back. Input: the moment the button was pressed and its key. Output: one hour later for "hour"; today at eveningHour for "evening", or tomorrow's when that hour has already gone by; tomorrow at morningHour for "tomorrow"; the zero time for any other key.
func snoozeUntil(now time.Time, key string) time.Time {
	day := db.DayStart(now)
	switch key {
	case actionHour:
		return now.Add(time.Hour)
	case actionEvening:
		evening := atHour(day, eveningHour)
		if !now.Before(evening) {
			evening = atHour(day.AddDate(0, 0, 1), eveningHour)
		}
		return evening
	case actionTomorrow:
		return atHour(day.AddDate(0, 0, 1), morningHour)
	}
	return time.Time{}
}

// atHour returns that local clock hour on day's calendar date. Built with time.Date rather than by adding hours to midnight, so a day that gains or loses an hour to daylight saving still lands on the right wall clock.
func atHour(day time.Time, hour int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
}

// maybeSnoozes raises every snooze that has come due, with the same buttons as the first time so it can be pushed back again. Each is stamped as fired before it is raised, so a snooze can only ever come back once per pressing. It goes through say rather than post so a re-fired snooze lands on the same single surface a first-time notice does: the window's card when a window is up, the desktop banner when none is.
func (s *Scheduler) maybeSnoozes(ctx context.Context) {
	due, err := s.store.DueSnoozes(ctx, s.now())
	if err != nil {
		slog.Warn("snoozes: reading due snoozes failed", "error", err)
		return
	}
	for _, sn := range due {
		if err := s.store.MarkSnoozeFired(ctx, sn.ID); err != nil {
			slog.Warn("snoozes: could not stamp one as fired, skipping it", "id", sn.ID, "error", err)
			continue
		}
		s.say(Notice{Title: sn.Title, Body: sn.Body, ID: sn.NoticeID, Kind: sn.Kind})
	}
}

// taskNoticeWatermarkKind is the diary-table row maybeTaskNotices keeps purely as a marker, on the empty day the same way the understanding doc is: the highest action-item note id already turned into a task notice, so a daemon restart never re-announces work it has already surfaced.
const taskNoticeWatermarkKind = "task-notice-watermark"

// maxTaskNoticesPerMeeting caps how many task notices one meeting's newly-lifted action items raise at once. Five bullets from one meeting would otherwise be five banners in a row; the rest are folded into the last one's body as a count instead.
const maxTaskNoticesPerMeeting = 3

// maxTaskNoticesPerTick caps how many task notices one tick raises across every meeting, so a day of back-to-back meetings never turns into a wall of cards; whatever is over the cap waits, uncounted, for the next tick.
const maxTaskNoticesPerTick = 5

// maybeTaskNotices posts one task notice for each of the user's own action items a meeting has newly raised since the last tick, grouped by the meeting that raised them (Source), capped at maxTaskNoticesPerMeeting per meeting and maxTaskNoticesPerTick in all. Input: the tick's context. Output: nothing — a failed read is logged and retried next tick. The very first tick, with no watermark stored yet, announces nothing and only records the highest note id it sees, so a daemon meeting an old store does not raise a notice for every item already open. After that the watermark advances to the highest note id seen among the user's own items, whether or not it was announced, so a closed item is never rescanned; an item another person owned that is later handed to the user by hand (PATCH /tasks/{id}) is already under the watermark and is not announced either.
func (s *Scheduler) maybeTaskNotices(ctx context.Context) {
	mark, err := s.store.DiaryEntry(ctx, "", taskNoticeWatermarkKind)
	if err != nil {
		slog.Warn("task notices: reading the watermark failed", "error", err)
		return
	}
	watermark, _ := strconv.ParseInt(mark, 10, 64)
	seeding := mark == ""

	items, err := s.store.ActionItemsByOwner(ctx, memory.OwnerMe)
	if err != nil {
		slog.Warn("task notices: reading action items failed", "error", err)
		return
	}

	max := watermark
	var order []string
	byMeeting := map[string][]memory.ActionItem{}
	for _, a := range items {
		if a.NoteID > max {
			max = a.NoteID
		}
		if seeding || a.NoteID <= watermark || a.Status != memory.StatusOpen {
			continue
		}
		if _, ok := byMeeting[a.Source]; !ok {
			order = append(order, a.Source)
		}
		byMeeting[a.Source] = append(byMeeting[a.Source], a)
	}

	posted := 0
	for _, meeting := range order {
		group := byMeeting[meeting]
		extra := len(group) - maxTaskNoticesPerMeeting
		if extra > 0 {
			group = group[:maxTaskNoticesPerMeeting]
		}
		if room := maxTaskNoticesPerTick - posted; len(group) > room {
			// Over the tick's cap the rest of this meeting, and every meeting after it, waits: the watermark still moves, so they are not re-found next tick, which is the price of never flooding the desk.
			group = group[:room]
		}
		for i, a := range group {
			posted++
			body := a.Text
			if i == len(group)-1 && extra > 0 {
				body += fmt.Sprintf("\n\nand %d more in Tasks", extra)
			}
			s.say(Notice{Title: "New task from " + meeting, Body: body, Place: "tasks", ID: strconv.FormatInt(a.NoteID, 10), Kind: "task"})
		}
	}

	if max != watermark || seeding {
		if err := s.store.SetDiaryEntry(ctx, "", taskNoticeWatermarkKind, strconv.FormatInt(max, 10)); err != nil {
			slog.Warn("task notices: writing the watermark failed", "error", err)
		}
	}
}
