// hotkey.go holds the shortcut that shows and hides June's hover: reading one, checking whether another program already holds it, and getting it into effect. GET /hotkey/check, POST /hotkey/status and the hotkey half of GET and POST /settings are answered from here.
// Both systems answer the same questions the same way, and only the work underneath differs: on Windows the window registers the shortcut itself and the daemon tests a combination by registering it for a moment (hotkey_windows.go); on GNOME the desktop runs June's own custom keybinding, which the daemon writes and checks against every other binding the desktop holds (hotkey_gnome.go).
package ipc

import (
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"june/internal/config"
	"june/internal/util"
)

// The words GET /settings reports in hotkey_status.
const (
	HotkeyOK          = "ok"
	HotkeyTaken       = "taken"
	HotkeyPending     = "pending"
	HotkeyUnsupported = "unsupported"
	HotkeyUnknown     = "unknown"
)

// The words GET /hotkey/check reports in reason. "current" is the shortcut June already holds, which a test would otherwise read as taken, since June is the program holding it.
const (
	checkOK          = "ok"
	checkTaken       = "taken"
	checkInvalid     = "invalid"
	checkCurrent     = "current"
	checkUnsupported = "unsupported"
)

// hotkeyGOOS is runtime.GOOS, indirected so a test can drive the Windows or the GNOME path on any machine.
var hotkeyGOOS = runtime.GOOS

// registerProbe tests a shortcut by registering it for a moment (see probeHotkey). Output: true when it is free; an error when the test itself could not run. A var so a test can stand in for Windows.
var registerProbe = probeHotkey

// Accel is one shortcut: the modifiers held and the one key pressed with them. Key is a canonical key name, one of hotkeyKeys.
type Accel struct {
	Ctrl, Alt, Shift, Super bool
	Key                     string
}

// hotkeyKey is what each system calls one key: the Windows virtual-key code RegisterHotKey takes, and the GTK key name a GNOME binding is written with.
type hotkeyKey struct {
	vk     uint32
	keysym string
}

// namedKeys are the keys other than letters, digits and F-keys a shortcut may use, by canonical name. The names are the ones the window's shortcut plugin parses too (global-hotkey's parse_key), so the window registers exactly the text this file hands it. The punctuation codes are the US layout's, which is what RegisterHotKey's VK_OEM codes mean.
var namedKeys = map[string]hotkeyKey{
	"Space":        {0x20, "space"},
	"Enter":        {0x0D, "Return"},
	"Tab":          {0x09, "Tab"},
	"Escape":       {0x1B, "Escape"},
	"Backspace":    {0x08, "BackSpace"},
	"Delete":       {0x2E, "Delete"},
	"Insert":       {0x2D, "Insert"},
	"Home":         {0x24, "Home"},
	"End":          {0x23, "End"},
	"PageUp":       {0x21, "Page_Up"},
	"PageDown":     {0x22, "Page_Down"},
	"Left":         {0x25, "Left"},
	"Up":           {0x26, "Up"},
	"Right":        {0x27, "Right"},
	"Down":         {0x28, "Down"},
	"Comma":        {0xBC, "comma"},
	"Period":       {0xBE, "period"},
	"Minus":        {0xBD, "minus"},
	"Equal":        {0xBB, "equal"},
	"Semicolon":    {0xBA, "semicolon"},
	"Slash":        {0xBF, "slash"},
	"Backslash":    {0xDC, "backslash"},
	"Quote":        {0xDE, "apostrophe"},
	"Backquote":    {0xC0, "grave"},
	"BracketLeft":  {0xDB, "bracketleft"},
	"BracketRight": {0xDD, "bracketright"},
}

// keyAliases are other spellings of a named key that a caller may send: the browser's KeyboardEvent.code names and the short forms people type. Keyed upper-case.
var keyAliases = map[string]string{
	"ESC": "Escape", "RETURN": "Enter", "SPACEBAR": "Space", "DEL": "Delete", "INS": "Insert",
	"PGUP": "PageUp", "PGDN": "PageDown", "PAGE_UP": "PageUp", "PAGE_DOWN": "PageDown",
	"ARROWLEFT": "Left", "ARROWUP": "Up", "ARROWRIGHT": "Right", "ARROWDOWN": "Down",
	"APOSTROPHE": "Quote", "GRAVE": "Backquote", "BACK_SPACE": "Backspace",
}

// hotkeyKeys maps every spelling ParseHotkey accepts, upper-case, to its canonical name, and canonical names to what each system calls the key.
var hotkeyKeys, hotkeyNames = func() (map[string]hotkeyKey, map[string]string) {
	keys := map[string]hotkeyKey{}
	names := map[string]string{}
	add := func(name string, k hotkeyKey, spellings ...string) {
		keys[name] = k
		names[strings.ToUpper(name)] = name
		names[strings.ToUpper(k.keysym)] = name
		for _, s := range spellings {
			names[s] = name
		}
	}
	for c := 'A'; c <= 'Z'; c++ {
		add(string(c), hotkeyKey{uint32(c), strings.ToLower(string(c))}, "KEY"+string(c))
	}
	for c := '0'; c <= '9'; c++ {
		add(string(c), hotkeyKey{uint32(c), string(c)}, "DIGIT"+string(c))
	}
	for n := 1; n <= 24; n++ {
		add("F"+strconv.Itoa(n), hotkeyKey{uint32(0x6F + n), "F" + strconv.Itoa(n)})
	}
	for name, k := range namedKeys {
		add(name, k)
	}
	for alias, name := range keyAliases {
		names[alias] = name
	}
	return keys, names
}()

// hotkeyError is a sentence the window can show as it is, saying why some text is not a shortcut June can use.
type hotkeyError string

func (e hotkeyError) Error() string { return string(e) }

const (
	errHotkeyEmpty      = hotkeyError("Press the keys you want to use.")
	errHotkeyNoKey      = hotkeyError("Add a key after Ctrl, Alt or Shift.")
	errHotkeyUnknownKey = hotkeyError("June can't use that key. Try a letter, a number, Space or an F key.")
	errHotkeyNoModifier = hotkeyError("Add Ctrl or Alt, like Ctrl+Shift+Space.")
)

// setModifier records one modifier named by token. Input: a modifier's name in any of the spellings people, browsers and GNOME use. Output: false when token names no modifier.
func (a *Accel) setModifier(token string) bool {
	switch strings.ToUpper(strings.TrimSpace(token)) {
	case "CTRL", "CONTROL", "PRIMARY":
		a.Ctrl = true
	case "ALT", "OPTION", "MOD1":
		a.Alt = true
	case "SHIFT":
		a.Shift = true
	case "SUPER", "WIN", "WINDOWS", "META", "CMD", "COMMAND", "MOD4":
		a.Super = true
	default:
		return false
	}
	return true
}

// ParseHotkey reads a shortcut. Input: text like "Ctrl+Alt+Space" in any letter case and with any of the modifier and key spellings above, or a GNOME accelerator like "<Control><Alt>space". Output: the shortcut, or a hotkeyError saying in plain words what is wrong with it. At least one of Ctrl, Alt and Super is required, so the shortcut cannot swallow ordinary typing; the exceptions are F1–F24 with Shift alone and F13–F24 on their own, which no one types.
func ParseHotkey(text string) (Accel, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Accel{}, errHotkeyEmpty
	}
	var mods []string
	var key string
	if strings.HasPrefix(text, "<") {
		rest := text
		for strings.HasPrefix(rest, "<") {
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return Accel{}, errHotkeyUnknownKey
			}
			mods = append(mods, rest[1:end])
			rest = rest[end+1:]
		}
		key = strings.TrimSpace(rest)
	} else {
		parts := strings.Split(text, "+")
		key = strings.TrimSpace(parts[len(parts)-1])
		mods = parts[:len(parts)-1]
	}
	var a Accel
	for _, m := range mods {
		if !a.setModifier(m) {
			return Accel{}, errHotkeyUnknownKey
		}
	}
	if key == "" || (&Accel{}).setModifier(key) {
		return Accel{}, errHotkeyNoKey
	}
	name, ok := hotkeyNames[strings.ToUpper(key)]
	if !ok {
		return Accel{}, errHotkeyUnknownKey
	}
	a.Key = name
	if !a.Ctrl && !a.Alt && !a.Super {
		f := a.fKey()
		if !(a.Shift && f >= 1) && !(!a.Shift && f >= 13) {
			return Accel{}, errHotkeyNoModifier
		}
	}
	return a, nil
}

// usableHotkey reads a shortcut the user asks June to hold (see ParseHotkey). Only a shortcut that is not one, or that another program already holds, is refused: the user asked to be able to set any key they want, so a combination apps use inside their own windows (see everyday) is allowed and only warned about.
func usableHotkey(text string) (Accel, error) {
	return ParseHotkey(text)
}

// everyday reports whether apps use the combination for work people do all day. RegisterHotKey and GNOME's list of bindings only know about other global shortcuts, so both call Ctrl+C free, and taking it breaks copy everywhere — which the check's note warns about before the user saves. Output: true for Ctrl or Ctrl+Shift with any key but Space or an F key, and Ctrl+F4; Ctrl+Alt with a letter, a digit or punctuation (AltGr on many keyboards), Tab or Delete; Alt with Tab, F4, Left or Right; and Shift+F10. A combination with Super is left to the availability check.
func (a Accel) everyday() bool {
	if a.Super {
		return false
	}
	f := a.fKey()
	switch {
	case a.Ctrl && !a.Alt:
		return (a.Key != "Space" && f == 0) || f == 4
	case a.Ctrl:
		// Canonical letter and digit names are the only one-character ones, and the punctuation keys the only ones with VK_OEM codes, which start at 0xBA.
		punctuation := hotkeyKeys[a.Key].vk >= 0xBA
		return len(a.Key) == 1 || punctuation || a.Key == "Tab" || a.Key == "Delete"
	case a.Alt:
		return a.Key == "Tab" || f == 4 || a.Key == "Left" || a.Key == "Right"
	}
	return a.Shift && f == 10
}

// CanonicalHotkey is the canonical text of a shortcut, or "" when text is not one.
func CanonicalHotkey(text string) string {
	a, err := ParseHotkey(text)
	if err != nil {
		return ""
	}
	return a.String()
}

// fKey is the number of the F-key the shortcut uses, 0 when its key is not one.
func (a Accel) fKey() int {
	if len(a.Key) < 2 || a.Key[0] != 'F' {
		return 0
	}
	n, err := strconv.Atoi(a.Key[1:])
	if err != nil {
		return 0
	}
	return n
}

// String is the shortcut's canonical text: Ctrl, Alt, Shift and Super in that order, then the key, joined by "+".
func (a Accel) String() string {
	var parts []string
	for _, m := range []struct {
		on   bool
		name string
	}{{a.Ctrl, "Ctrl"}, {a.Alt, "Alt"}, {a.Shift, "Shift"}, {a.Super, "Super"}} {
		if m.on {
			parts = append(parts, m.name)
		}
	}
	return strings.Join(append(parts, a.Key), "+")
}

// GNOME is the shortcut as a GNOME binding is written, such as "<Control><Alt>space".
func (a Accel) GNOME() string {
	var b strings.Builder
	for _, m := range []struct {
		on   bool
		name string
	}{{a.Ctrl, "<Control>"}, {a.Alt, "<Alt>"}, {a.Shift, "<Shift>"}, {a.Super, "<Super>"}} {
		if m.on {
			b.WriteString(m.name)
		}
	}
	b.WriteString(hotkeyKeys[a.Key].keysym)
	return b.String()
}

// The sentences hotkey_note and a check's note carry. Each is one plain idea; the window shows them as they are.

// takenNote is the note for a shortcut another program holds.
func takenNote(accel string) string {
	return accel + " is used by another app. Choose your own shortcut."
}

// freeNote is the note for a shortcut June could take.
func freeNote(accel string) string {
	return accel + " is free to use."
}

// everydayNote is the note for a free shortcut that other apps use inside their own windows.
func everydayNote(accel string) string {
	return accel + " is free, but other apps use it too. June will take it over everywhere."
}

// currentNote is the note for the shortcut June holds already.
func currentNote(accel string) string {
	return "June already uses " + accel + "."
}

// pendingNote is the note while the window is still registering a shortcut.
func pendingNote(accel string) string {
	return "June is setting up " + accel + "."
}

// WindowOffNote is the note when June runs without its window. Exported so `june doctor` can tell this reason apart from a desktop June cannot set a shortcut on, whose fix differs.
const WindowOffNote = "The shortcut needs June's window, which is turned off."

const (
	sideBySideNote   = "Another June on this computer keeps the shortcut."
	otherSystemNote  = "June can't set a shortcut on this computer."
	applyFailedNote  = "June couldn't set the shortcut. Try again."
	noReportNote     = "June couldn't check the shortcut. Try setting it again."
	probeFailedNote  = "June couldn't check this shortcut. Try again."
	readFailedNote   = "June couldn't read this computer's shortcuts. Try again."
	badSavedNote     = "The saved shortcut isn't valid. Choose a new one."
	cannotChangeNote = "June can't change the shortcut right now."
)

// HotkeyCheck is GET /hotkey/check's answer.
type HotkeyCheck struct {
	Hotkey    string `json:"hotkey"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Note      string `json:"note"`
}

// windowReportWait is how long a shortcut handed to the window may stay pending before June stops waiting to hear whether it worked. Vars so a test can shorten them.
var windowReportWait = 30 * time.Second

// windowStartReportWait is windowReportWait for the shortcut handed over as the daemon starts, which the window registers only once it is up; a first WebView2 start at sign-in can take most of a minute.
var windowStartReportWait = 3 * time.Minute

// settleWait is how long POST /settings waits for the window to say whether a new shortcut took, so the answer the Settings screen gets is usually the final one; past it the answer says pending and the "hotkey" event that follows carries the rest.
var settleWait = 2 * time.Second

// Hotkeys is the daemon's one record of the shortcut: what is in effect, whether it works, and the plain sentence that goes with it. Every change goes through applyMu, so two changes asked for at once cannot interleave their checks and writes; the state itself is read under mu, which nothing holds while it waits on another process.
type Hotkeys struct {
	cfg    *LiveConfig
	server *Server
	// window is whether June runs its window. The shortcut shows and hides the window's hover, so without one there is nothing for it to do.
	window bool
	// handsOff is set for a second daemon run beside the real one (see HandsOff). Set before Start and only read after.
	handsOff bool

	applyMu sync.Mutex
	// stopped is set once Stop has let go of the shortcut, so a change or a start still under way cannot take it again behind a quit. Guarded by applyMu.
	stopped bool

	mu     sync.Mutex
	active string // the shortcut in effect, or being registered while pending; "" for none
	status string
	note   string
	// asked counts the shortcuts handed to the window, so the give-up timer of an older one cannot settle a newer one.
	asked uint64
	// settled is closed when a pending shortcut stops being pending, for a POST /settings that is waiting to say how it went. nil while nothing is pending.
	settled chan struct{}
}

// NewHotkeys makes the daemon's shortcut record. Input: the live config the wanted shortcut is read from and written to, the server whose event stream the window hears about changes on (nil sends nothing), and whether June runs its window. Output: the record, which says "unknown" until Start has run.
func NewHotkeys(cfg *LiveConfig, server *Server, window bool) *Hotkeys {
	return &Hotkeys{cfg: cfg, server: server, window: window, status: HotkeyUnknown}
}

// Start puts the wanted shortcut into effect as the daemon comes up. On Windows that is the window's job once it starts, so the shortcut is pending until it says how it went; on GNOME the daemon writes June's binding itself, off the start path since it runs gsettings a dozen times. Input: none. Output: none.
func (h *Hotkeys) Start() {
	switch {
	case h.handsOff:
		h.set("", HotkeyUnsupported, sideBySideNote)
	case !h.window:
		h.set("", HotkeyUnsupported, WindowOffNote)
	case hotkeyGOOS == "windows":
		wanted, err := usableHotkey(h.cfg.Get().HotkeyWanted())
		if err != nil {
			h.set("", HotkeyUnknown, badSavedNote)
			return
		}
		h.askWindow(wanted.String(), windowStartReportWait)
	case hotkeyGOOS == "linux":
		if !gnomeDesktop() {
			h.set("", HotkeyUnsupported, manualNote())
			return
		}
		h.set("", HotkeyPending, "")
		go func() {
			h.applyMu.Lock()
			defer h.applyMu.Unlock()
			h.startGNOME()
		}()
	default:
		h.set("", HotkeyUnsupported, otherSystemNote)
	}
}

// HandsOff makes the record leave the desktop's shortcut alone, for a second daemon run beside the real one on a port of its own (a test, a dry run). The window the user sees talks to the real one, and writing June's GNOME binding from here would point it at this build. Such a daemon reports the shortcut unsupported and refuses to change it. Call before Start.
func (h *Hotkeys) HandsOff() { h.handsOff = true }

// Stop lets go of the shortcut as June quits or the user logs off, so the keys go back to other apps, as they do on Windows, where the shortcut ends with the window's process. On GNOME that takes June's binding out of the desktop's list, after keeping any change the user made to it in GNOME's own settings; the next start writes it again. A restart does not call this, since the June that follows keeps the shortcut. Input: none. Output: none.
func (h *Hotkeys) Stop() {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()
	h.stopped = true
	if h.handsOff || !h.window || hotkeyGOOS != "linux" || !gnomeDesktop() {
		return
	}
	h.stopGNOME()
}

// View is the shortcut as GET /settings reports it. Output: the shortcut in effect as canonical text ("" for none; the one being set up while pending), its status, and its note. A nil record reports nothing known.
func (h *Hotkeys) View() (accel, status, note string) {
	if h == nil {
		return "", HotkeyUnknown, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active, h.status, h.note
}

// set records a new state (see record) about the shortcut in effect. Input: that shortcut, its status and its note.
func (h *Hotkeys) set(accel, status, note string) { h.record(accel, status, note, accel) }

// lost records that June has no shortcut (see record), naming in the event the one it could not have. Input: that shortcut, the status and the note.
func (h *Hotkeys) lost(accel, status, note string) { h.record("", status, note, accel) }

// record stores a new state and, for any state but pending, tells every window about it as {"type":"hotkey","text":status,"detail":shortcut} so an open Settings screen changes with it. Input: the shortcut in effect ("" for none), the status, the note, and the shortcut the event names.
func (h *Hotkeys) record(active, status, note, about string) {
	h.mu.Lock()
	h.storeLocked(active, status, note)
	h.mu.Unlock()
	h.publish(status, about)
}

// storeLocked is record's change to the state, for a caller holding mu: it stores the state and, once nothing is pending, wakes a POST /settings waiting to say how its change went.
func (h *Hotkeys) storeLocked(active, status, note string) {
	h.active, h.status, h.note = active, status, note
	if status != HotkeyPending && h.settled != nil {
		close(h.settled)
		h.settled = nil
	}
}

// publish is record's word to the windows, sent outside mu.
func (h *Hotkeys) publish(status, about string) {
	if status != HotkeyPending && h.server != nil {
		h.server.Publish(Event{ID: "hotkey", Type: "hotkey", Text: status, Detail: about})
	}
}

// Check answers whether text is a shortcut June could use. Input: the shortcut as the user pressed it. Output: the check, with its canonical text and a plain note.
func (h *Hotkeys) Check(text string) HotkeyCheck {
	a, err := usableHotkey(text)
	if err != nil {
		return HotkeyCheck{Hotkey: strings.TrimSpace(text), Reason: checkInvalid, Note: err.Error()}
	}
	reason, note := h.availability(a)
	if reason == checkOK && a.everyday() {
		note = everydayNote(a.String())
	}
	return HotkeyCheck{Hotkey: a.String(), Available: reason == checkOK || reason == checkCurrent, Reason: reason, Note: note}
}

// availability is Check's answer for a shortcut that parsed. Output: the reason and its note.
func (h *Hotkeys) availability(a Accel) (reason, note string) {
	accel := a.String()
	switch {
	case h.handsOff:
		return checkUnsupported, sideBySideNote
	case !h.window:
		return checkUnsupported, WindowOffNote
	}
	switch hotkeyGOOS {
	case "windows":
		// The window's own shortcut cannot be test-registered: the window is the program holding it, and the test would call it taken.
		if active, status, _ := h.View(); accel == active && (status == HotkeyOK || status == HotkeyPending) {
			return checkCurrent, currentNote(accel)
		}
		free, err := registerProbe(a)
		switch {
		case err != nil:
			slog.Warn("could not test a shortcut", "hotkey", accel, "error", err)
			return checkTaken, probeFailedNote
		case !free:
			return checkTaken, takenNote(accel)
		}
		return checkOK, freeNote(accel)
	case "linux":
		if !gnomeDesktop() {
			return checkUnsupported, manualNote()
		}
		binds, err := gnomeBindings()
		if err != nil {
			slog.Warn("could not read the desktop's shortcuts", "error", err)
			return checkUnsupported, readFailedNote
		}
		return gnomeAvailability(binds, accel)
	}
	return checkUnsupported, otherSystemNote
}

// Change is the hotkey half of POST /settings: it checks text again exactly as GET /hotkey/check does and refuses a shortcut June cannot have without saving anything; otherwise it saves the choice and puts it into effect. Input: the shortcut as text, "" for back to config.DefaultHotkey. Output: 0 when the change went through, else the status to answer with, the error word and a plain message.
func (h *Hotkeys) Change(text string) (code int, word, message string) {
	if h == nil {
		return http.StatusServiceUnavailable, "unavailable", cannotChangeNote
	}
	text = strings.TrimSpace(text)
	wanted := text
	if wanted == "" {
		wanted = config.DefaultHotkey
	}
	a, err := usableHotkey(wanted)
	if err != nil {
		return http.StatusBadRequest, checkInvalid, err.Error()
	}
	accel := a.String()
	saved := accel
	if text == "" {
		saved = ""
	}

	h.applyMu.Lock()
	if h.stopped {
		h.applyMu.Unlock()
		return http.StatusServiceUnavailable, "unavailable", cannotChangeNote
	}
	reason, note := h.availability(a)
	switch reason {
	case checkOK, checkCurrent:
	case checkUnsupported:
		h.applyMu.Unlock()
		return http.StatusConflict, checkUnsupported, note
	default:
		h.applyMu.Unlock()
		return http.StatusConflict, checkTaken, note
	}
	if h.cfg.Get().Hotkey != saved {
		if err := h.cfg.Update(func(c *config.JuneConfig) { c.Hotkey = saved }); err != nil {
			h.applyMu.Unlock()
			return http.StatusInternalServerError, "not_saved", "June couldn't save the shortcut. Try again."
		}
	}
	var settled chan struct{}
	switch {
	case hotkeyGOOS != "windows":
		// Even when June's binding holds it already, as after the user set it in GNOME's own settings, so the record says so too; writeJuneBinding skips a write GNOME does not need.
		h.applyGNOME(a)
	case reason != checkCurrent:
		settled = h.askWindow(accel, windowReportWait)
	}
	h.applyMu.Unlock()
	if settled != nil {
		select {
		case <-settled:
		case <-time.After(settleWait):
		}
	}
	return 0, "", ""
}

// askWindow hands the window a shortcut to register: it says pending until the window reports, tells the window on the event stream, and gives up after wait. Input: the canonical shortcut and how long to wait for the window. Output: a channel closed once the window has reported, or the wait has been given up.
func (h *Hotkeys) askWindow(accel string, wait time.Duration) chan struct{} {
	h.mu.Lock()
	h.asked++
	asked := h.asked
	h.active, h.status, h.note = accel, HotkeyPending, pendingNote(accel)
	if h.settled == nil {
		h.settled = make(chan struct{})
	}
	settled := h.settled
	h.mu.Unlock()
	if h.server != nil {
		// The window reads the shortcut to register from the saved config, which already names this one.
		h.server.Tell("hotkey")
	}
	time.AfterFunc(wait, func() {
		h.mu.Lock()
		stale := h.asked != asked || h.status != HotkeyPending
		h.mu.Unlock()
		if !stale {
			slog.Warn("the window never said whether the shortcut works", "hotkey", accel, "waited", wait)
			h.lost(accel, HotkeyUnknown, noReportNote)
		}
	})
	return settled
}

// Report records what the window found when it registered a shortcut (POST /hotkey/status). While another shortcut is pending, a report about this one is ignored: the window says again what it holds each time its event stream connects, and such a word about the old shortcut, arriving just after a new one was handed over, would otherwise answer the POST /settings waiting on the new one with the old keys. The new one's own report, or the give-up timer, settles it. Input: the shortcut, whether it registered, and the system's reason when it did not. Output: an error for text that is not a shortcut.
func (h *Hotkeys) Report(text string, ok bool, reason string) error {
	a, err := ParseHotkey(text)
	if err != nil {
		return err
	}
	accel := a.String()
	active, status, note := accel, HotkeyOK, ""
	if !ok {
		active, status, note = "", HotkeyTaken, takenNote(accel)
		// The window refuses a hand-edited shortcut June would not let the user pick, and no other app is to blame for that.
		if _, err := usableHotkey(accel); err != nil {
			note = badSavedNote
		}
	}
	h.mu.Lock()
	if h.status == HotkeyPending && h.active != accel {
		waiting := h.active
		h.mu.Unlock()
		slog.Info("the window spoke about a shortcut other than the one being set up; still waiting", "hotkey", accel, "ok", ok, "waiting_for", waiting)
		return nil
	}
	h.storeLocked(active, status, note)
	h.mu.Unlock()
	if ok {
		slog.Info("the window holds the shortcut", "hotkey", accel)
	} else {
		slog.Warn("the window could not register the shortcut", "hotkey", accel, "reason", reason)
	}
	h.publish(status, accel)
	return nil
}

// CheckRoute handles GET /hotkey/check?hotkey=<shortcut>, answering HotkeyCheck. A "+" sent unencoded arrives as a space, so text with spaces and no "+" is read with the spaces as "+".
func (h *Hotkeys) CheckRoute(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("hotkey")
	if !strings.Contains(text, "+") && strings.Contains(strings.TrimSpace(text), " ") {
		text = strings.Join(strings.Fields(text), "+")
	}
	util.WriteJSON(w, h.Check(text))
}

// StatusRoute handles POST /hotkey/status {"hotkey": string, "ok": bool, "error": string}: the window saying whether it could register a shortcut. Answers 204, or 400 for a body that is not one.
func (h *Hotkeys) StatusRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hotkey string `json:"hotkey"`
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if err := h.Report(req.Hotkey, req.OK, req.Error); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
