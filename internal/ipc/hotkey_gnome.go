// hotkey_gnome.go is the GNOME half of the shortcut: June's own custom keybinding, written and read through gsettings, and the clash check against every other shortcut the desktop holds. It carries no build tag so a test can drive it with a fake gsettings on any machine; hotkeyGOOS decides when it runs.
package ipc

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"june/internal/config"
)

// gsettingsTimeout bounds each gsettings call, so a wedged dconf backend cannot stall a request.
const gsettingsTimeout = 2 * time.Second

// gsettingsRunner runs `gsettings <args...>` and returns its trimmed stdout, or an error. A package variable so the test can swap in a fake instead of touching the real desktop.
var gsettingsRunner = runGsettings

// runGsettings is gsettingsRunner's real implementation.
func runGsettings(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gsettingsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gsettings", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mediaKeysSchema is the GNOME schema custom keybindings are registered under.
const mediaKeysSchema = "org.gnome.settings-daemon.plugins.media-keys"

// customKeybindingSchemaPrefix, plus a keybinding's own D-Bus path, names the per-keybinding schema its command and binding live under.
const customKeybindingSchemaPrefix = mediaKeysSchema + ".custom-keybinding:"

// customKeybindingsDir is where a new custom keybinding's path is made, as GNOME's own keyboard settings make theirs.
const customKeybindingsDir = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/"

// windowToggleCommand is the command older hand-made wirings of June's shortcut ran; a binding running it is still June's.
const windowToggleCommand = "june-window-toggle"

// gnomeKeybindingSchemas hold the shortcuts GNOME itself answers: window management, the shell, mutter, and the media keys. Every key in them is a shortcut or a list of them, and one on the same keys as June's would leave which of the two runs to chance.
var gnomeKeybindingSchemas = []string{
	"org.gnome.desktop.wm.keybindings",
	"org.gnome.shell.keybindings",
	"org.gnome.mutter.keybindings",
	"org.gnome.mutter.wayland.keybindings",
	mediaKeysSchema,
}

// gnomeDesktop reports whether this session is GNOME with gsettings to reach it, the one desktop June can set a shortcut on by itself. A var so a test can say yes.
var gnomeDesktop = func() bool {
	gnome := false
	for _, d := range strings.Split(os.Getenv("XDG_CURRENT_DESKTOP"), ":") {
		if strings.EqualFold(d, "GNOME") {
			gnome = true
		}
	}
	if !gnome {
		return false
	}
	_, err := exec.LookPath("gsettings")
	return err == nil
}

// gnomeBinding is one shortcut the desktop holds: where it is set, the shortcut as canonical text, and whether it is June's own.
type gnomeBinding struct {
	where string
	accel string
	ours  bool
}

// customBinding is one custom keybinding as GNOME's keyboard settings list it.
type customBinding struct {
	path    string
	command string
	accel   string
	ours    bool
}

// customKeybindings reads every custom keybinding. Output: each one's path, command and shortcut ("" for none June can read), or an error when the list itself cannot be read.
func customKeybindings() ([]customBinding, error) {
	list, err := gsettingsRunner("get", mediaKeysSchema, "custom-keybindings")
	if err != nil {
		return nil, err
	}
	var out []customBinding
	for _, path := range gvariantStrings(list) {
		schema := customKeybindingSchemaPrefix + path
		command, _ := gsettingsRunner("get", schema, "command")
		binding, _ := gsettingsRunner("get", schema, "binding")
		c := customBinding{path: path, command: gvariantString(command), accel: CanonicalHotkey(gvariantString(binding))}
		c.ours = isToggleCommand(c.command)
		out = append(out, c)
	}
	return out, nil
}

// gnomeBindings reads every shortcut the desktop holds that June could also ask for. A schema this GNOME does not have is skipped; a binding June cannot read as a shortcut (a lone Super_L, a media key) is skipped too, since June can never ask for it. Output: the shortcuts, or an error when not even the custom keybindings can be read.
func gnomeBindings() ([]gnomeBinding, error) {
	var out []gnomeBinding
	for _, schema := range gnomeKeybindingSchemas {
		listing, err := gsettingsRunner("list-recursively", schema)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(listing, "\n") {
			fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
			if len(fields) < 3 {
				continue
			}
			for _, text := range gvariantQuoted(fields[2]) {
				if accel := CanonicalHotkey(text); accel != "" {
					out = append(out, gnomeBinding{where: fields[0] + " " + fields[1], accel: accel})
				}
			}
		}
	}
	customs, err := customKeybindings()
	if err != nil {
		return nil, err
	}
	for _, c := range customs {
		if c.accel != "" {
			out = append(out, gnomeBinding{where: c.path, accel: c.accel, ours: c.ours})
		}
	}
	return out, nil
}

// gnomeAvailability is whether accel is free among binds. Output: taken when any binding but June's own holds it, current when only June's does, ok otherwise; each with its note.
func gnomeAvailability(binds []gnomeBinding, accel string) (reason, note string) {
	ours := false
	for _, b := range binds {
		if b.accel != accel {
			continue
		}
		if !b.ours {
			return checkTaken, takenNote(accel)
		}
		ours = true
	}
	if ours {
		return checkCurrent, currentNote(accel)
	}
	return checkOK, freeNote(accel)
}

// startGNOME puts the wanted shortcut into effect as the daemon starts: a shortcut the user gave June's binding in GNOME's own keyboard settings (see adoptGNOME), else the one chosen in June. Called with applyMu held.
func (h *Hotkeys) startGNOME() {
	if h.stopped {
		return
	}
	kept := ""
	if customs, err := customKeybindings(); err == nil {
		kept = h.adoptGNOME(customs)
	}
	var a Accel
	var err error
	if kept != "" {
		// The user may have picked it in GNOME itself, so it is held even where June's own editor would have refused it.
		a, err = ParseHotkey(kept)
	} else {
		a, err = usableHotkey(h.cfg.Get().HotkeyWanted())
	}
	if err != nil {
		h.set("", HotkeyUnknown, badSavedNote)
		return
	}
	h.applyGNOME(a)
}

// adoptGNOME keeps the shortcut June's binding holds, which differs from June's choice when the user changed it in GNOME's own keyboard settings: that shortcut is saved as June's choice, so the start that follows writes it back rather than June's older one. An empty binding is not adopted: June empties its own binding itself when another binding holds its keys, so an empty one cannot be told from the user turning it off, and the next start writes June's choice again, as the window registers it at every start on Windows. Input: the custom keybindings as read. Output: the shortcut June's binding holds, as canonical text, or "" when there is no such binding or it holds nothing June can read. Called with applyMu held.
func (h *Hotkeys) adoptGNOME(customs []customBinding) string {
	own, ok := ownBinding(customs)
	if !ok || own.accel == "" {
		return ""
	}
	if own.accel == CanonicalHotkey(h.cfg.Get().HotkeyWanted()) {
		return own.accel
	}
	saved := own.accel
	if saved == CanonicalHotkey(config.DefaultHotkey) {
		saved = ""
	}
	if err := h.cfg.Update(func(c *config.JuneConfig) { c.Hotkey = saved }); err != nil {
		slog.Warn("could not save the shortcut set for June in GNOME's keyboard settings", "hotkey", own.accel, "error", err)
	} else {
		slog.Info("kept the shortcut set for June in GNOME's keyboard settings", "hotkey", own.accel)
	}
	return own.accel
}

// stopGNOME takes June's binding out of the desktop's list as June quits, so its keys go back to other apps and a binding that runs June is not left behind once June is gone (packaging/uninstall.sh quits June before it removes it). A change the user made to it in GNOME's own keyboard settings is saved first. Called with applyMu held.
func (h *Hotkeys) stopGNOME() {
	customs, err := customKeybindings()
	if err != nil {
		slog.Warn("could not read the desktop's shortcuts to let go of June's", "error", err)
		return
	}
	h.adoptGNOME(customs)
	if err := removeJuneBinding(customs); err != nil {
		slog.Warn("could not take June's shortcut out of the desktop's keybindings", "error", err)
		return
	}
	slog.Info("let go of the shortcut")
}

// applyGNOME makes a the shortcut June's binding runs on, and records how that went. A shortcut another binding holds is not written: two bindings on one shortcut leave which one runs to chance, so June's binding is emptied instead and the user is asked to choose. Called with applyMu held.
func (h *Hotkeys) applyGNOME(a Accel) {
	accel := a.String()
	binds, err := gnomeBindings()
	if err != nil {
		slog.Warn("could not read the desktop's shortcuts", "error", err)
		h.lost(accel, HotkeyUnknown, readFailedNote)
		return
	}
	if reason, note := gnomeAvailability(binds, accel); reason == checkTaken {
		if err := clearJuneBinding(); err != nil {
			slog.Warn("could not take June's shortcut off the keys another binding holds", "hotkey", accel, "error", err)
		}
		h.lost(accel, HotkeyTaken, note)
		return
	}
	if err := writeJuneBinding(a); err != nil {
		slog.Warn("could not write June's shortcut into the desktop's keybindings", "hotkey", accel, "error", err)
		h.lost(accel, HotkeyUnknown, applyFailedNote)
		return
	}
	h.set(accel, HotkeyOK, "")
}

// writeJuneBinding sets June's custom keybinding to a, making the binding when there is none yet. Its name, command and shortcut are written before it joins the list, so GNOME never reads a half-made binding. Output: the first gsettings error.
func writeJuneBinding(a Accel) error {
	customs, err := customKeybindings()
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(customs)+1)
	for _, c := range customs {
		paths = append(paths, c.path)
	}
	own, listed := ownBinding(customs)
	// Every start puts the shortcut into effect again, and a write GNOME did not need still makes it let go of every key and grab them again.
	if listed && own.accel == a.String() && own.command == toggleCommand() {
		return nil
	}
	path := own.path
	if !listed {
		path = freeCustomPath(paths)
	}
	schema := customKeybindingSchemaPrefix + path
	for _, kv := range [][2]string{{"name", "June"}, {"command", toggleCommand()}, {"binding", a.GNOME()}} {
		if _, err := gsettingsRunner("set", schema, kv[0], gvariantQuote(kv[1])); err != nil {
			return err
		}
	}
	if listed {
		return nil
	}
	_, err = gsettingsRunner("set", mediaKeysSchema, "custom-keybindings", gvariantList(append(paths, path)))
	return err
}

// clearJuneBinding empties the shortcut of June's binding (see ownBinding), keeping the binding itself so the next change finds it. Output: the first gsettings error.
func clearJuneBinding() error {
	customs, err := customKeybindings()
	if err != nil {
		return err
	}
	own, ok := ownBinding(customs)
	if !ok {
		return nil
	}
	_, err = gsettingsRunner("set", customKeybindingSchemaPrefix+own.path, "binding", gvariantQuote(""))
	return err
}

// ownBinding is the binding June keeps its shortcut in: the first custom keybinding whose command shows June's hover. Any others are the user's own and are left as they are. Output: the binding, and false when there is none.
func ownBinding(customs []customBinding) (customBinding, bool) {
	for _, c := range customs {
		if c.ours {
			return c, true
		}
	}
	return customBinding{}, false
}

// removeJuneBinding takes June's binding (see ownBinding) out of the desktop's list of custom keybindings, which is what makes GNOME let go of its keys, and then resets its keys, as GNOME's own keyboard settings do when one is removed. Input: the custom keybindings as read. Output: the first gsettings error.
func removeJuneBinding(customs []customBinding) error {
	own, ok := ownBinding(customs)
	if !ok {
		return nil
	}
	keep := make([]string, 0, len(customs))
	for _, c := range customs {
		if c.path != own.path {
			keep = append(keep, c.path)
		}
	}
	if _, err := gsettingsRunner("set", mediaKeysSchema, "custom-keybindings", gvariantList(keep)); err != nil {
		return err
	}
	_, err := gsettingsRunner("reset-recursively", customKeybindingSchemaPrefix+own.path)
	return err
}

// freeCustomPath is a custom keybinding path no binding in paths uses: .../june/, else .../june2/, and so on.
func freeCustomPath(paths []string) string {
	used := map[string]bool{}
	for _, p := range paths {
		used[p] = true
	}
	for n := 1; ; n++ {
		name := "june"
		if n > 1 {
			name += strconv.Itoa(n)
		}
		if p := customKeybindingsDir + name + "/"; !used[p] {
			return p
		}
	}
}

// isToggleCommand reports whether a custom keybinding's command is one that shows June's hover: `june --toggle`, which June writes, or the pid-file signal and helper script hand-made wirings ran before it.
func isToggleCommand(command string) bool {
	return strings.Contains(command, windowToggleCommand) ||
		strings.Contains(command, "window.pid") ||
		(strings.Contains(command, "june") && strings.Contains(command, "--toggle"))
}

// juneProgram is the june program a shortcut should run. A `go run` build lives in a temporary folder that is gone once it exits, so from one the installed june on PATH is used instead. Output: the path, and whether it is the june PATH finds, which can then be named by its name alone.
func juneProgram() (path string, onPath bool) {
	exe, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
	}
	found, lookErr := exec.LookPath("june")
	if lookErr == nil {
		if real, err := filepath.EvalSymlinks(found); err == nil {
			found = real
		}
	}
	if err != nil || strings.HasPrefix(exe, os.TempDir()) {
		if lookErr != nil {
			return "june", true
		}
		return found, true
	}
	return exe, lookErr == nil && found == exe
}

// toggleCommand is the command June's binding runs: june --toggle, which asks the running June to show or hide its hover.
func toggleCommand() string {
	path, _ := juneProgram()
	return shellQuote(path) + " --toggle"
}

// manualNote is hotkey_note on a desktop June cannot set a shortcut on, with the command to bind by hand.
func manualNote() string {
	command := "june --toggle"
	if path, onPath := juneProgram(); !onPath {
		command = path + " --toggle"
	}
	return "This desktop doesn't let June set a shortcut. In your keyboard settings, add one that runs: " + command
}

// shellQuote quotes s as one argument for the shell-like parsing GNOME gives a keybinding's command (g_shell_parse_argv). A plain path is left as it is.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gvariantQuote writes s as a GVariant string literal, which is what gsettings set takes.
func gvariantQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// gvariantList writes items as a GVariant string-array literal.
func gvariantList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = gvariantQuote(it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// gvariantQuoted pulls every string out of a GVariant literal as gsettings prints one, a lone string or an array of them, in either quote and with backslash escapes. Input: a literal like "['<Super>Up', '<Alt>F10']". Output: the strings, unquoted.
func gvariantQuoted(literal string) []string {
	var out []string
	for i := 0; i < len(literal); i++ {
		q := literal[i]
		if q != '\'' && q != '"' {
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(literal) && literal[j] != q; j++ {
			if literal[j] == '\\' && j+1 < len(literal) {
				j++
			}
			b.WriteByte(literal[j])
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

// gvariantStrings splits a GVariant string-array literal, as gsettings prints one, into its elements. Input: a literal like "['a', 'b']" or the empty array's "@as []" / "[]". Output: the quoted elements, unquoted. ponytail: plain split-and-trim, not a real GVariant parser — every value gsettings hands back here is a D-Bus object path, which never itself contains a comma or a quote, so this holds; a command or binding string with an embedded comma would break it.
func gvariantStrings(literal string) []string {
	literal = strings.TrimSpace(literal)
	literal = strings.TrimPrefix(literal, "@as")
	literal = strings.TrimSpace(literal)
	literal = strings.TrimPrefix(literal, "[")
	literal = strings.TrimSuffix(literal, "]")
	literal = strings.TrimSpace(literal)
	if literal == "" {
		return nil
	}
	parts := strings.Split(literal, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, gvariantString(p))
	}
	return out
}

// gvariantString unquotes a string value as gsettings prints one: in single quotes, or in double quotes when the value itself holds a single quote, as June's own command does when its path has a space in it ("'/home/a b/june' --toggle"), with backslash escapes either way. Input: a literal like "'june --toggle'". Output: the value alone, or the input trimmed if it carries no quotes.
func gvariantString(literal string) string {
	if values := gvariantQuoted(literal); len(values) > 0 {
		return values[0]
	}
	return strings.TrimSpace(literal)
}
