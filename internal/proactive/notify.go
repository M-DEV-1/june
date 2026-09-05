// notify.go is how one of Ora's moments reaches the desktop as a notification the user can act on without opening anything: Open in Ora, Done, and three ways to be reminded later. The buttons are the reason this talks to org.freedesktop.Notifications itself rather than shelling out — notify-send can only offer buttons by blocking a whole process for as long as the notification is on screen, and everything here has to survive being answered hours later or not at all.
package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"ora/internal/db"
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
	// Notify shows the notification and returns as soon as the desktop has taken it. chose is called at most once, later and from another goroutine, with the key of the button the user pressed or "" when the notification was dismissed without one.
	Notify(title, body string, actions []Action, chose func(key string)) error
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
	// mu guards waiting and is held across the Notify call itself, so a user quick enough to click before the id is recorded still finds a handler waiting for them.
	mu      sync.Mutex
	waiting map[uint32]func(string)
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
	n := &BusNotifier{conn: conn, waiting: map[uint32]func(string){}}
	sigCh := make(chan *dbus.Signal, 16)
	conn.Signal(sigCh)
	go n.listen(ctx, sigCh)
	return n, nil
}

// Notify posts one notification with the given buttons, at normal urgency and under the ora desktop entry, and never expires it: a snooze button nobody is there to press is worth nothing, so the banner stays until the user deals with it.
func (n *BusNotifier) Notify(title, body string, actions []Action, chose func(string)) error {
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
	return nil
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
	n.mu.Unlock()
	if chose != nil {
		chose(key)
	}
}

// sendNotifier is the fallback for a machine with no session bus to reach: notify-send offers the same buttons and prints back the key of whichever was pressed, at the cost of a process sitting there for as long as the notification is on screen.
type sendNotifier struct{}

// Notify posts through notify-send from its own goroutine, since that command does not return until the notification is answered or gone.
func (sendNotifier) Notify(title, body string, actions []Action, chose func(string)) error {
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

// SetNotifier wires the desktop notifier every notice is posted through — cmd/daemon.go is the only production caller. Unset, notices fall back to the plain notification func New was built with, which carries no buttons.
func (s *Scheduler) SetNotifier(n Notifier) {
	s.notifier = n
}

// SetOpenWindow wires what "Open in Ora", and a click on the notification body itself, does: the same thing the tray's own Open Ora item does. Unset, those clicks do nothing.
func (s *Scheduler) SetOpenWindow(fn func()) {
	s.openWindow = fn
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
		if err := s.notifier.Notify(n.Title, n.Body, noticeActions, func(key string) {
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
func (s *Scheduler) chose(ctx context.Context, n Notice, key string) {
	switch key {
	case "":
		return
	case actionOpen:
		if s.openWindow != nil {
			s.openWindow()
		}
	case actionDone:
		s.markDone(ctx, n)
	case actionHour, actionEvening, actionTomorrow:
		s.snooze(ctx, n, key)
	default:
		slog.Debug("a notification came back with a button Ora does not offer", "key", key)
	}
}

// snooze puts a notice away until later and tells the window it has gone. Input: the notice and the pressed snooze button's key. Output: nothing.
func (s *Scheduler) snooze(ctx context.Context, n Notice, key string) {
	due := snoozeUntil(s.now(), key)
	if _, err := s.store.AddSnooze(ctx, n.Kind, n.ID, n.Title, n.Body, due); err != nil {
		slog.Warn("could not snooze a notice", "kind", n.Kind, "id", n.ID, "error", err)
		return
	}
	n.Action = "snoozed"
	n.Until = due.Format(time.RFC3339)
	sendNotice(n)
}

// markDone applies "Done". A task notice names a task, so it goes through the daemon's own task-done path and is closed for real. Every other kind has nothing to complete — a routine notice or a morning brief is Ora reporting, not work owed — so all this records is that the user cleared it. Input: the notice. Output: nothing.
func (s *Scheduler) markDone(ctx context.Context, n Notice) {
	if n.Kind == "task" && n.ID != "" && s.taskDone != nil {
		if err := s.taskDone(ctx, n.ID); err != nil {
			slog.Warn("could not close a task from its notification", "id", n.ID, "error", err)
			return
		}
	}
	slog.Info("notice cleared from its notification", "kind", n.Kind, "id", n.ID)
	n.Action = "done"
	sendNotice(n)
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

// maybeSnoozes posts every snooze that has come due, with the same buttons as the first time so it can be pushed back again. Each is stamped as fired before it is posted, so a snooze can only ever come back once per pressing.
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
		s.post(Notice{Title: sn.Title, Body: sn.Body, ID: sn.NoticeID, Kind: sn.Kind})
	}
}
