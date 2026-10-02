package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"june/internal/act"
	"june/internal/db"
	"june/internal/memory"
	"june/internal/tracker"
	"june/internal/util"
)

// maxToolRows caps how many rows any one tool result may carry. A tool result is prompt text, and action_items reads from a store that grows without bound — open action items never expire by design. query_memory has queryMemoryHits for the same reason.
const maxToolRows = 40

func shellName() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "sh"
}

// toolError renders a failed tool call as one plain sentence. Every failure path in executeTool goes through it, so a Go error string (a time-parse dump, a type name, a wrapped sqlite message) never reaches the model and, from there, never gets read out loud. The real error goes to the log instead.
// Input: a plain-words sentence saying what went wrong and, where the model can fix it, what to try instead. Output: the tool result string, prefixed "error: " — which is what the model and resultSummary both read as "this call failed".
func toolError(msg string) string {
	return "error: " + msg
}

// storeUnavailable is what every failed memory read says. The model can only ever do one thing about it, so naming the store's own error would add nothing it can act on.
const storeUnavailable = "I couldn't reach your memory just now, try that again in a moment"

// hasArg reports whether name was supplied with a value that isn't an empty string. A non-string value counts as supplied — the model meant something by it, and treating it as absent is how a filter gets dropped without anyone noticing.
func hasArg(args map[string]any, name string) bool {
	v, present := args[name]
	if !present {
		return false
	}
	s, ok := v.(string)
	return !ok || strings.TrimSpace(s) != ""
}

// stringArg returns args[name] as a string. A missing argument is "" with no error; one present but not a string is an error, since silently ignoring it is how a wrong-typed date turned into "the start of today".
func stringArg(args map[string]any, name string) (string, error) {
	v, present := args[name]
	if !present {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", name, v)
	}
	return s, nil
}

// checkArgs names every argument that isn't in valid, listing the ones that are so the model can correct itself on the next call. Returns "" when every argument is known.
// This exists because the model invents parameters — a real trace called recall with query_memory's "query" argument, which the recall handler ignored, then answered from the timeline branch with since defaulted to the start of today.
func checkArgs(args map[string]any, valid ...string) string {
	var unknown []string
	for name := range args {
		if !slices.Contains(valid, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	slices.Sort(unknown)
	return fmt.Sprintf("I don't take %s here, what I do take is %s",
		strings.Join(unknown, ", "), strings.Join(valid, ", "))
}

// parseRef splits a "[note#12]" or "note#12" style reference — the shape query_memory, recall and action_items hand back — into its kind and numeric id, so revise can dispatch on it without the model having to know which table backs a result.
// Input: the ref as a result showed it, brackets optional. Output: the kind ("note", "thread" or "task") and the id, or an error naming the shape a ref must have.
func parseRef(ref string) (string, int64, error) {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "[")
	ref = strings.TrimSuffix(ref, "]")
	kind, idStr, ok := strings.Cut(ref, "#")
	if !ok {
		return "", 0, fmt.Errorf(`ref must look like "note#12", "thread#3" or "task#5"`)
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf(`ref must look like "note#12", "thread#3" or "task#5"`)
	}
	return kind, id, nil
}

// optionalWindow parses since/until when either is given, for tools where no dates means no time filter at all (unlike recall, whose timeline branch defaults to today — see recallBounds).
// Output: the bounds, whether any filtering should happen, and a parse error. A zero bound means unbounded on that side.
func optionalWindow(args map[string]any, now time.Time) (time.Time, time.Time, bool, error) {
	sinceStr, sinceErr := stringArg(args, "since")
	untilStr, untilErr := stringArg(args, "until")
	if err := errors.Join(sinceErr, untilErr); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	var since, until time.Time
	var err error
	if strings.TrimSpace(sinceStr) != "" {
		if since, err = parseInstant(sinceStr, now, false); err != nil {
			return time.Time{}, time.Time{}, false, err
		}
	}
	if strings.TrimSpace(untilStr) != "" {
		if until, err = parseInstant(untilStr, now, true); err != nil {
			return time.Time{}, time.Time{}, false, err
		}
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return time.Time{}, time.Time{}, false, errSinceAfterUntil
	}
	return since, until, !since.IsZero() || !until.IsZero(), nil
}

// queryMemoryHits is how many hits query_memory shows the model. The since/until window is enforced inside HybridSearchWindow (SQL-side, before top-k), so a windowed call asks for the same limit as a plain one.
const queryMemoryHits = 10

// sensitivePathSubstrings/sensitivePathSuffixes are the paths read_file refuses — credentials, SSH/GPG/cloud keys, and june's own IPC token, all of which the model could otherwise read and ship to the Gemini API with zero user involvement. Matched against the path as given plus its absolute form, so both a relative "id_rsa" and "~/.ssh/id_rsa" (which filepath.Abs can't expand "~" in, but still contains the ".ssh/" substring literally) get caught.
var sensitivePathSubstrings = []string{".ssh/", ".gnupg/", ".aws/", ".env", "id_rsa", "id_ed25519", "credentials", "shadow", "june-db/ipc-token"}
var sensitivePathSuffixes = []string{".pem", ".key"}

// isSensitivePath reports whether path matches one of the patterns above.
func isSensitivePath(path string) bool {
	// Both forms are compared with forward slashes, so a Windows path such as C:\Users\me\.aws\config meets the same patterns.
	candidates := []string{filepath.ToSlash(path)}
	if abs, err := filepath.Abs(path); err == nil {
		candidates = append(candidates, filepath.ToSlash(abs))
	}
	for _, c := range candidates {
		for _, sub := range sensitivePathSubstrings {
			if strings.Contains(c, sub) {
				return true
			}
		}
		for _, suffix := range sensitivePathSuffixes {
			if strings.HasSuffix(c, suffix) {
				return true
			}
		}
	}
	return false
}

// openURLCommand builds the command that hands a url to the desktop's browser, per platform. A var so a test can swap it and never launch a real browser. Input: the url, already checked to be http or https. Output: the command, not yet started.
var openURLCommand = func(url string) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return exec.Command("open", url)
	default:
		return exec.Command("xdg-open", url)
	}
}

// approvalGatedTools are the tools that would need a person to approve a call before it runs. Nothing in June can ask for that approval, so they are kept out of the tool list the live session declares (liveToolsFor) and out of the ask's list (askAllowedTools), and read_file refuses a sensitive path rather than reading it.
var approvalGatedTools = map[string]bool{"read_file": true}

// refuseApproval is what a tool returns instead of asking for an approval nobody is listening for. Input: what the tool wanted to do, for the log. Output: one refusal sentence for the model.
func refuseApproval(what string) string {
	slog.Warn("refusing a tool that needs approval: nothing in this session can ask for one", "what", what)
	return toolError("that needs your say-so and " + refuseApprovalMark + ", do it yourself, or tell me exactly what to do instead")
}

// ExecuteTool is just executeTool but exported, so eval tests outside this package can call the real tool (query_memory, recall, etc) the same way the model does.
func (a *Agent) ExecuteTool(ctx context.Context, name string, args map[string]any) string {
	return a.executeTool(ctx, name, args)
}

// brainRequiredTools are the tool names whose handler reads or writes through a.brain, the memory store. Widening the set of tools the ask gate can reach (askAllowedTools) opened a path where an agent built without a brain — a nil ContextReader, the shape both a not-yet-connected daemon and a test can produce — hit a.brain.SomeMethod(...) and panicked on the nil interface's own method dispatch. Checked once, up front, so every one of them fails the same way every other executeTool error path already does: a plain "error: ..." string, never a panic.
var brainRequiredTools = map[string]bool{
	"query_memory": true, "query_store": true, "recall": true,
	"save_note": true, "add_task": true, "personal_context": true, "revise": true, "action_items": true,
}

// executeTool runs a tool and returns the result as a string
// maybe this can be seperated into /agent/tools altogether later and be compiled with OS specific code?
// executeTool runs one tool and files what it did. Input: the tool's name and arguments. Output: the tool's result string, exactly as runTool produced it.
// Every path that runs a tool comes through here — the ask loop, the sub-task loop, the computer-use job and the live voice session — which is why the record is written at this one point rather than at each of them. Before this the live session recorded nothing at all, so the tools it used most were invisible to anything reading the store.
func (a *Agent) executeTool(ctx context.Context, name string, args map[string]any) string {
	start := time.Now()
	var result string
	if busy := a.screenBusy(ctx, name); busy != "" {
		result = toolError(busy)
	} else {
		result = a.runTool(ctx, name, args)
	}
	outcome := toolOutcome(result)
	if outcome == OutcomeOK {
		a.holdScreen(ctx, name)
	}
	if rec := recorderFrom(ctx); rec != nil {
		rec(ToolRecord{
			Name:     name,
			Args:     toolActivitySummary(name, args),
			Outcome:  outcome,
			Result:   resultSummary(name, result),
			Output:   util.UTF8Bytes(result, toolOutputCap),
			TurnID:   turnIDFrom(ctx),
			Duration: time.Since(start),
			Offered:  offeredFrom(ctx),
		})
	}
	return result
}

// runTool is the tool switch itself: what each tool name does, with no recording or timing around it.
func (a *Agent) runTool(ctx context.Context, name string, args map[string]any) string {
	if a.brain == nil && brainRequiredTools[name] {
		return toolError(storeUnavailable)
	}
	switch name {
	case "read_file":
		path, ok := args["path"].(string)
		if !ok {
			return toolError("read_file needs a path")
		}
		if isSensitivePath(path) {
			return refuseApproval("read file: " + path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("read_file failed", "path", path, "error", err)
			return toolError("I couldn't read that file, check the path")
		}
		return util.RunesNote(string(data), 4000, "... (truncated, file too large)")

	case "list_files":
		path, _ := args["path"].(string)
		if path == "" {
			path = "."
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			slog.Warn("list_files failed", "path", path, "error", err)
			return toolError("I couldn't list that folder, check the path")
		}
		var lines []string
		for _, e := range entries {
			prefix := "[file]"
			if e.IsDir() {
				prefix = "[dir] "
			}
			lines = append(lines, fmt.Sprintf("%s %s", prefix, filepath.Join(path, e.Name())))
		}
		if len(lines) > 100 {
			lines = lines[:100]
			lines = append(lines, "... (truncated, too many entries)")
		}
		return strings.Join(lines, "\n")

	case "observe_screen":
		app, title, nodes, err := a.observe(ctx)
		if err != nil {
			slog.Warn("observe_screen failed", "error", err)
			return toolError("could not look at the screen: " + err.Error())
		}
		items := act.Filter(nodes)
		// Read before the new snapshot is stored, since the answer is this look against the one before it.
		previous := a.lastScreen(ctx)
		if len(items) == 0 {
			a.rememberScreen(ctx, items, screenSnapshot{app: app, title: title})
			return app + " · " + title + "\n(nothing actionable is showing)" + frameOnlyHint
		}
		lines := strings.Split(act.Format(items), "\n")
		a.rememberScreen(ctx, items, screenSnapshot{app: app, title: title, lines: lines})
		return observeResult(app, title, lines, previous)

	case "look":
		if a.capture == nil {
			return toolError("this session cannot see the screen")
		}
		shot, err := a.capture(ctx)
		if err != nil {
			slog.Warn("look failed", "error", err)
			return toolError("could not take a picture of the screen: " + err.Error())
		}
		// There is no count of looks: a video, a loading page or a window redrawing itself changes with nothing done in between, and a screen nobody touched does not, so the only look turned away is one at the very picture the model already has.
		if !recordLook(ctx, shot) {
			return unchangedLookMarker + ", so the picture you already have is what is showing; act on it, or wait_for a change"
		}
		// A channel that cannot be handed a picture with the tool result is given a road of its own here: the Live voice session takes its tool results as text, so without this it took a picture it was never shown and every coordinate it named afterwards was refused. Nothing changes for a text ask, which has no sender and collects the picture through takeLook instead.
		deliverPicture(ctx, shot)
		return fmt.Sprintf("here is the picture, %d wide and %d high. Its top-left corner is %d,%d on the screen and one of its pixels is %.2f screen pixels, so point at things in it with its own coordinates and draw will put them back on the screen for you.", shot.W, shot.H, shot.X, shot.Y, shot.Scale)

	case "point_at":
		it, errText := a.seenItem(ctx, args)
		if errText != "" {
			return errText
		}
		if a.Point == nil {
			return toolError("this session cannot draw on the screen")
		}
		it, errText = a.stillThere(ctx, it)
		if errText != "" {
			return errText
		}
		x, y, w, h, errText := a.freshRect(ctx, it)
		if errText != "" {
			return errText
		}
		label, _ := args["label"].(string)
		remembered, hadRemembered := a.screenTarget()
		if err := a.Point(x, y, w, h, label); err != nil {
			return toolError(err.Error())
		}
		a.rememberTarget(ScreenTarget{Label: it.Label, Role: it.Role, Window: a.currentWindow(ctx), HasRect: true, X: x, Y: y, W: w, H: h})
		note := ""
		if hadRemembered {
			note = targetMismatchNote(questionFrom(ctx), &remembered, it.Label)
		}
		return fmt.Sprintf("ringed [%d] %s %q", it.N, it.Role, it.Label) + note

	case "show_marks":
		items := a.seen(ctx)
		if items == nil {
			return toolError("call observe_screen first, then show_marks")
		}
		if a.Marks == nil {
			return toolError("this session cannot draw on the screen")
		}
		total := len(items)
		if total > 40 {
			items = items[:40]
		}
		// The marks are drawn where each element is now, not where the listing said it was: after a scroll the numbers would otherwise sit over whatever moved into those rectangles, and the numbers are what the model then clicks by. An element that is no longer showing gets no mark, the same rule point_at follows.
		fresh := make([]act.Item, 0, len(items))
		gone := 0
		for _, it := range items {
			x, y, w, h, errText := a.freshRect(ctx, it)
			if errText != "" {
				gone++
				continue
			}
			it.X, it.Y, it.W, it.H = x, y, w, h
			fresh = append(fresh, it)
		}
		if err := a.Marks(fresh); err != nil {
			return toolError(err.Error())
		}
		switch {
		case gone > 0:
			return fmt.Sprintf("marked %d of %d elements on the screen; %d are no longer showing", len(fresh), total, gone)
		case len(fresh) < total:
			return fmt.Sprintf("marked %d of %d elements on the screen", len(fresh), total)
		}
		return fmt.Sprintf("marked %d element(s) on the screen", len(fresh))

	case "draw":
		if a.Draw == nil {
			return toolError("this session cannot draw on the screen")
		}
		shapes, errText := drawShapeList(args)
		remembered, hadRemembered := a.screenTarget()
		var drawn, refused []string
		var last *ScreenTarget
		// Shapes the stream already resolved — drawn or refused — are reported but never acted on again, and they are always the leading ones, because the stream hands them over in the order the call lists them; a refused one still holds its place so the shapes after it are not shifted onto the wrong entries.
		if early := streamDrawnFrom(ctx); len(early) > 0 {
			for _, d := range early {
				if d.Err != "" {
					refused = append(refused, fmt.Sprintf("shape %d: %s", len(drawn)+len(refused)+1, strings.TrimPrefix(d.Err, "error: ")))
					continue
				}
				drawn = append(drawn, d.Phrase)
				if d.Target != nil {
					last = d.Target
				}
			}
			if len(early) >= len(shapes) {
				shapes = nil
			} else {
				shapes = shapes[len(early):]
			}
		}
		// Read after the early block rather than returned bare, so shapes the stream already drew are still reported even when the finished call as a whole is over the cap or carries a bad entry.
		if errText != "" {
			refused = append(refused, strings.TrimPrefix(errText, "error: "))
		}
		// One name for the whole call, so the overlay keeps its shapes on screen together; when the stream already inked the leading ones it is their name, so the two halves of the call stay one drawing.
		group := drawGroupFor(ctx)
		for _, shape := range shapes {
			phrase, target, errText := a.drawOne(ctx, group, shape)
			if errText != "" {
				// One bad shape does not lose the rest of the drawing: the others are drawn and the model is told which entry failed, so it can send that one again rather than the whole diagram.
				refused = append(refused, fmt.Sprintf("shape %d: %s", len(drawn)+len(refused)+1, strings.TrimPrefix(errText, "error: ")))
				continue
			}
			drawn = append(drawn, phrase)
			if target != nil {
				last = target
			}
		}
		// Only the last shape drawn around a numbered element is remembered, so a later bare "draw a circle around it" resolves to the last thing this call drew around rather than to whichever entry happened to come first.
		note := ""
		if last != nil {
			a.rememberTarget(*last)
			if hadRemembered {
				note = targetMismatchNote(questionFrom(ctx), &remembered, last.Label)
			}
		}
		switch {
		case len(drawn) == 0:
			return toolError(strings.Join(refused, "; "))
		case len(drawn) == 1 && len(refused) == 0:
			return "drew " + drawn[0] + note
		default:
			result := fmt.Sprintf("drew %d shapes: %s", len(drawn), strings.Join(drawn, "; ")) + note
			if len(refused) > 0 {
				result += "\nnot drawn: " + strings.Join(refused, "; ")
			}
			return result
		}

	case "click":
		return a.click(ctx, args)

	// click_at is no longer declared to the model — click itself takes a bare point now — but it is still answered, so a live session holding the older tool list does not find one of its tools missing mid-task.
	case "click_at":
		return a.clickPoint(ctx, args)

	case "wait_for":
		kind, _ := args["kind"].(string)
		value, _ := args["value"].(string)
		return a.waitFor(ctx, act.Check{Kind: kind, Value: value}, waitTimeout(args))

	case "scroll_to":
		it, errText := a.seenItem(ctx, args)
		if errText != "" {
			return errText
		}
		// The number came off a list that may be several rounds old, so the element behind it is checked to still be the one the list named, exactly as click and point_at do: a toolkit that has recycled the object path would otherwise have this scroll reported as a scroll to something it never touched.
		it, errText = a.stillThere(ctx, it)
		if errText != "" {
			return errText
		}
		if err := a.scrollTo(ctx, it.Ref); err != nil {
			return toolError(fmt.Sprintf("could not scroll to [%d] %s %q: %v", it.N, it.Role, it.Label, err))
		}
		return fmt.Sprintf("scrolled to [%d] %s %q; call observe_screen to see the page now", it.N, it.Role, it.Label)

	case "type_text":
		text, ok := args["text"].(string)
		if !ok || text == "" {
			return toolError("type_text needs text")
		}
		enter, _ := args["enter"].(bool)
		// A newline, tab or backspace inside the text is a real Enter, Tab or BackSpace keystroke once it reaches the keyboard: the newline submits a chat box halfway through the message, and the tab moves the rest of the text into whatever field comes next. The enter argument above stays the one way to press Enter on purpose.
		if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 }) {
			return toolError("type_text will not type control characters; use press_key for Enter, Tab or Backspace, or the enter argument to press Enter after the text")
		}
		// The field the last successful click acted on stands in for the one the keyboard is in — "click the field first" is the documented way to reach type_text anyway — and keyboardField then reads who really holds the keys, so the checks below are put against the field the text will actually reach. Before any click, this is the zero act.Item, which matches neither check below.
		focused, known := a.focus(ctx)
		landed := ""
		window := a.currentWindow(ctx)
		if known {
			focused, known, landed = a.keyboardField(ctx, focused)
		} else {
			// A click at a point, or a focus-moving key, left the click itself saying nothing about the field, so the tree is asked who holds the keyboard now. A readable place to type is the field; a readable control that is no place to type is refused by name below; nothing readable at all (a window without a tree) types on the click alone, and the press check and the next look are what verify it, which is the same unknown keyboardField goes ahead on.
			if holder, ok := keyboardHolder(ctx); ok {
				focused, known = act.Item{Role: holder.Role, Label: holder.Label, Ref: holder.Ref}, typingPlaces[holder.Role]
			} else {
				slog.Warn("nothing readable holds the keyboard after a point click, so typing goes ahead where the focus is")
				known = true
			}
		}
		if !known && !blindConsent(questionFrom(ctx)) && !goAllowed(ctx) {
			// press_key is named because it is the only way in that the model itself can take. The other two ways past this line — the phrase in the question and the go field on the request — are both the user's, and a model that has hit this mid-task cannot reach either. On 2026-09-20 a GNOME Clocks run was refused here twice, told to ask for something it could not ask for, and found press_key on its own eighty seconds later.
			return fmt.Sprintf("Stopped before typing: the keyboard is held by %s %q in %q, which is no place to type. Click the field first. If this window draws its own fields and publishes none of them, send the characters one at a time with press_key instead.", focused.Role, focused.Label, window)
		}
		if secretField(focused, window) {
			return fmt.Sprintf("Stopped before typing into %s %q in %q, I never type passwords, card numbers or other secrets, so say it yourself once the field is focused.", focused.Role, focused.Label, window)
		}
		if irreversible(focused, window, true) && !consented(questionFrom(ctx), matchedVerb(focused, window)) && !goAllowed(ctx) {
			return fmt.Sprintf("Stopped before typing into %s %q in %q. %s", focused.Role, focused.Label, window, consentPrompt(matchedVerb(focused, window)))
		}
		if enter {
			text += "\n"
		}
		// The text goes in through the portal keyboard, the same session press_key uses, into whatever has focus: the field the checks above looked at. There is no toolkit path to set a field's text on Wayland, so this is the one way in, opened on first use.
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		if err := dev.TypeText(text); err != nil {
			return toolError("could not type: " + err.Error())
		}
		return fmt.Sprintf("typed %d characters%s; call observe_screen to see the result", len([]rune(text)), landed)

	case "press_key":
		keys, _ := args["keys"].(string)
		if keys == "" {
			return toolError("press_key needs keys, like \"Enter\" or \"Ctrl+L\"")
		}
		if pressesFocused(keys) {
			// Enter, Space and the Send chords press whatever has keyboard focus, which is a click by another name, so they go through the same stop line the click tool does — against the control keyboardField says the keys will reach, which is the one the last click focused unless the accessibility read names another.
			focused, known := a.focus(ctx)
			if known {
				focused, known, _ = a.keyboardField(ctx, focused)
			} else if holder, ok := keyboardHolder(ctx); ok {
				// After a point click the tree says who holds the keyboard, and the stop line below is put against that control, whatever it is.
				focused, known = act.Item{Role: holder.Role, Label: holder.Label, Ref: holder.Ref}, true
			}
			window := a.currentWindow(ctx)
			if !known {
				// Nothing readable holds the keyboard and the last click was at a point: the window publishes no tree, so the press goes ahead on the click, checked against the window's own title below.
				slog.Warn("nothing readable holds the keyboard after a point click, so the key press goes ahead where the focus is", "keys", keys)
			}
			if irreversible(focused, window, false) && !consented(questionFrom(ctx), matchedVerb(focused, window)) && !goAllowed(ctx) {
				return fmt.Sprintf("Stopped before pressing %s on %s %q in %q. %s", keys, focused.Role, focused.Label, window, consentPrompt(matchedVerb(focused, window)))
			}
		}
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		if err := dev.PressKey(keys); err != nil {
			return toolError(fmt.Sprintf("could not press %s: %v", keys, err))
		}
		// Tab, Shift+Tab and the arrows move the keyboard off whatever the last click focused, and nothing here can say where to, so the next Enter or Space has to be refused rather than checked against a control this session can no longer vouch for.
		if movesFocus(keys) {
			a.focusLost(ctx)
		}
		return fmt.Sprintf("pressed %s; call observe_screen to see what it did", keys)

	case "scroll_at":
		x, y, errText := a.picturePoint(ctx, args)
		if errText != "" {
			return errText
		}
		dy, ok := args["dy"].(float64)
		if !ok || dy == 0 {
			return toolError("scroll_at needs dy, how many steps to scroll: positive is down")
		}
		dev, errText := a.inputDevice(ctx)
		if errText != "" {
			return errText
		}
		// Same order as clickPoint: the tap indicator is drawn at the point the camera is about to photograph, so it must land before the camera starts rather than racing it.
		a.tapAt(x, y, "")
		before := a.beforePress(ctx)
		if err := dev.ScrollAt(float64(x), float64(y), int32(dy)); err != nil {
			return toolError(fmt.Sprintf("could not scroll at %d,%d: %v", x, y, err))
		}
		if missed := a.pressCheck(ctx, before, x, y); missed != "" {
			return missed
		}
		return fmt.Sprintf("scrolled %d steps at %d,%d; look or call observe_screen to see the page now", int(dy), x, y)

	// switch_window is no longer declared: open_app raises an already-running window before it starts anything, so it was open_app minus the working launch. Still answered, so a live session holding the older tool list is not left with a tool that returns nothing.
	case "switch_window", "open_app":
		app, _ := args["app"].(string)
		return a.openApp(ctx, strings.TrimSpace(app))

	case "open_url":
		raw, ok := args["url"].(string)
		if !ok {
			return toolError("open_url needs a url")
		}
		// Only http and https ever reach the opener. The url in a tool call is routinely copied out of screen text or a page the model just read, and the opener is a shell command: file:///home/…/.ssh/id_rsa, a .desktop path, a smb:// share or a javascript: link would all be acted on. ipc.Open makes the same check for the same reason.
		if parsed, err := neturl.Parse(raw); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			slog.Warn("open_url refused: not an http(s) link", "url", raw)
			return toolError("I can only open http and https links")
		}
		cmd := openURLCommand(raw)
		if err := cmd.Start(); err != nil {
			slog.Warn("open_url failed", "url", raw, "error", err)
			return toolError("I couldn't open that link")
		}
		// Reaped in the background: the opener exits in milliseconds, and an unwaited child stays a zombie in the daemon's process table for the life of the daemon.
		go func() { _ = cmd.Wait() }()
		// Opened is not showing: the shell keeps the new tab behind the window in front unless the browser is raised from inside the shell, so the result says which of the two happened rather than leaving the model to read "opened" as "in front".
		if how := a.raiseBrowser(ctx); how != "" {
			return fmt.Sprintf("opened %s in browser and brought the browser to the front (%s)", raw, how)
		}
		return fmt.Sprintf("opened %s in browser; the browser window was not brought to the front, so it may be behind the window that was in front, switch_window to it before reading the page", raw)

	case "query_memory":
		if msg := checkArgs(args, "query", "domain", "app", "since", "until", "kind"); msg != "" {
			return toolError(msg)
		}
		query, ok := args["query"].(string)
		if !ok {
			return toolError("query_memory needs something to search for")
		}
		if kind, _ := args["kind"].(string); kind == "meeting" {
			since, until, _, err := optionalWindow(args, time.Now())
			if err != nil {
				return toolError(dateHint)
			}
			return a.listMeetingNotes(ctx, since, until)
		}
		// domain is optional: a missing or wrong-typed arg silently becomes "" (search everything, weighted toward the current domain) rather than erroring — since/until below are stricter since a mis-parsed date changes which day the answer comes from.
		domain, _ := args["domain"].(string)
		since, until, timed, err := optionalWindow(args, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return toolError("that range runs backwards, the start has to come before the end")
		}
		if err != nil {
			slog.Warn("query_memory: unreadable date", "error", err)
			return toolError(dateHint)
		}
		slog.Debug("querying long-term memory", "query", query, "domain", domain, "since", since, "until", until)

		// HybridSearchWindow (FTS5 + vector, fused via reciprocal rank fusion) covers episodes/summaries/notes/threads in one fused, domain-aware ranking, with the since/until window enforced store-side — inside the SQL and the vector candidate pool, before any top-k — so a sparse window still yields its items instead of the old over-fetch-and-post-filter returning nothing.
		hits, err := a.brain.HybridSearchWindow(ctx, query, domain, since, until, queryMemoryHits)
		if err != nil {
			slog.Error("query_memory: search failed", "error", err)
			return toolError(storeUnavailable)
		}
		found := len(hits)
		app, _ := args["app"].(string)
		if strings.TrimSpace(app) != "" {
			hits = filterHitsByApp(hits, app)
		}
		if len(hits) == 0 {
			// "Nothing exists" and "the filters removed everything" are different answers and the model has to be able to tell them apart — answering the second as the first is how a question about episodes watched today got a flat no while the rows sat in the store.
			desc := filterDescription(app, since, until)
			if desc == "" {
				return "no memory matches"
			}
			total := found
			if total == 0 && timed {
				// The window emptied the search inside the store, so what exists outside it takes one unwindowed call to see.
				if unfiltered, uerr := a.brain.HybridSearchWindow(ctx, query, domain, time.Time{}, time.Time{}, queryMemoryHits); uerr == nil {
					total = len(unfiltered)
				}
			}
			if total > 0 {
				return fmt.Sprintf("%d matches, none %s", total, desc)
			}
			return "no memory matches"
		}
		lines := make([]string, 0, len(hits))
		// A minute-sampled screen produces runs of byte-identical captures, and ten copies of one row crowd nine real answers out of the result. Dedupe on the content itself (not the whole line — the same text five minutes apart is still the same information).
		seen := make(map[string]bool, len(hits))
		for _, h := range hits {
			if key := strings.TrimSpace(h.Content); key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			// Content is excerpted via db.FormatHit/FormatNoteHit like every other read path — an unformatted hit can inject tens of KB from a single oversized row (see RetrieveRelevant/RecallSubject, which already do this).
			// Notes are the only source revise can follow up on, so they're the only hits that carry their ref_id in the surfaced line — the model needs it in hand to act on a correction.
			// The *WithSource variants append a {"source":{...}} tag ask.go's Evidence extraction reads back out — a caller can then show which stored row an answer leaned on, instead of a paraphrase nobody can trace.
			if h.Source == "note" {
				lines = append(lines, db.FormatNoteHitWithSource(h, 0))
			} else {
				lines = append(lines, db.FormatHitWithSource(h, 0))
			}
		}
		return withReviseHint(strings.Join(lines, "\n"))

	case "query_store":
		if msg := checkArgs(args, "query"); msg != "" {
			return toolError(msg)
		}
		query, ok := args["query"].(string)
		if !ok || strings.TrimSpace(query) == "" {
			return toolError("query_store needs a SQL statement to run")
		}
		slog.Info("running query_store", "query", query)
		result, err := a.brain.QueryStore(ctx, query, maxToolRows)
		if err != nil {
			// Every other tool hides its raw error behind toolError's fixed wording, because a Go error string means nothing to a model that can't fix it. Here the error IS the fix — "no such column: titel" or "only one SQL statement is allowed per call" tells the model exactly what to change and try again, so it's passed through instead of hidden.
			slog.Warn("query_store: rejected or failed", "query", query, "error", err)
			return toolError(err.Error())
		}
		return result

	case "recall":
		if msg := checkArgs(args, "subject", "since", "until", "app"); msg != "" {
			return toolError(msg)
		}
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			slog.Info("recalling subject", "subject", subject)
			lines, err := a.brain.RecallSubject(ctx, subject, recallSubjectLimit)
			if err != nil {
				slog.Error("recall: subject lookup failed", "subject", subject, "error", err)
				return toolError(storeUnavailable)
			}
			if len(lines) == 0 {
				return "no memory of that subject"
			}
			// RecallSubject takes only a subject and a limit — it has no app filter and no date window — so combining subject with either can't be honored. Erroring taught the model nothing (it made the same call twice in one session) and cost the turn; the answer plus a line saying which filter was dropped is what it was after.
			var ignored []string
			if hasArg(args, "since") || hasArg(args, "until") {
				ignored = append(ignored, "the date window")
			}
			if hasArg(args, "app") {
				ignored = append(ignored, "the app filter")
			}
			if len(ignored) > 0 {
				lines = append(lines, fmt.Sprintf("(note: subject recall ignores %s)", strings.Join(ignored, " and ")))
			}
			return withReviseHint(strings.Join(lines, "\n"))
		}

		sinceStr, sinceErr := stringArg(args, "since")
		untilStr, untilErr := stringArg(args, "until")
		if err := errors.Join(sinceErr, untilErr); err != nil {
			slog.Warn("recall: date argument was not text", "error", err)
			return toolError(dateHint)
		}
		since, until, err := recallBounds(sinceStr, untilStr, time.Now())
		if errors.Is(err, errSinceAfterUntil) {
			return toolError("that range runs backwards, the start has to come before the end")
		}
		if err != nil {
			slog.Warn("recall: unreadable date", "error", err)
			return toolError(dateHint)
		}
		app, _ := args["app"].(string)
		slog.Info("recalling timeline window", "since", since, "until", until, "app", app)

		// One more than the cap, so overflow is detectable: a window whose episodes all fit is answered from them raw, and a bigger window climbs to the summary tier instead of silently returning whichever slice of itself is newest.
		episodes, err := a.brain.ListEpisodes(ctx, db.EpisodeQuery{Since: since, Until: until, App: app, Limit: recallEpisodeCap + 1, NewestFirst: true})
		if err != nil {
			slog.Error("recall: timeline read failed", "error", err)
			return toolError(storeUnavailable)
		}
		if len(episodes) == 0 {
			return "no episodes in that window"
		}
		// Summaries carry no app attribution, so an app-filtered recall stays on episodes whatever the size.
		if len(episodes) > recallEpisodeCap && app == "" {
			if lines := a.summaryTimeline(ctx, since, until); len(lines) > 0 {
				return strings.Join(lines, "\n")
			}
		}
		if len(episodes) > recallEpisodeCap {
			episodes = episodes[:recallEpisodeCap]
		}
		return strings.Join(formatEpisodeTimeline(episodes, func(e db.Episode) string {
			return oneLineExcerpt(e.ScreenText)
		}), "\n")

	case "branch":
		task, ok := args["task"].(string)
		if !ok || strings.TrimSpace(task) == "" {
			return toolError("branch needs the question to work on")
		}
		if !takeBranch(ctx) {
			return toolError(fmt.Sprintf("I have already run %d background searches for this; answer from what they found, or use query_memory or recall", maxBranchesPerAsk))
		}
		// webSearch calls a real search engine directly (Exa, falling back to Tavily) — the cheapest and fastest path. When neither is configured or both fail, the task goes to whichever brain the router says has a web search of its own, the same way a typed ask does, instead of telling the model the web is out of reach.
		result, err := a.webSearch(ctx, task)
		if err != nil {
			slog.Warn("branch: web search failed, handing the task to a brain with its own web search", "error", err)
			result, err = a.webAsk(ctx, task)
		}
		if err != nil {
			slog.Error("branch: failed", "error", err)
			return toolError("that background search didn't come back, use query_memory or recall instead")
		}
		return result

	case "do":
		goal, _ := args["goal"].(string)
		goal = strings.TrimSpace(goal)
		if goal == "" {
			return toolError("do needs the whole job in one goal, in the user's own words")
		}
		if a.RunJob == nil {
			return toolError("this session cannot start a job, so work the screen a step at a time instead")
		}
		slog.Info("do: handing a chain of work to the job runner", "goal", goal)
		said, err := a.RunJob(ctx, goal)
		if err != nil {
			slog.Warn("do: the job runner would not take it", "goal", goal, "error", err)
			return toolError(fmt.Sprintf("that job did not start: %v", err))
		}
		return said

	case "add_task":
		title, _ := args["title"].(string)
		title = strings.TrimSpace(title)
		if title == "" {
			return toolError("add_task needs the task's title")
		}
		// A conversation per task, as POST /tasks does it, so the row on the Tasks screen opens somewhere to work on it. A conversation that cannot be opened is not fatal: the task itself is what the user asked for, and it is filed against no conversation rather than not at all.
		convID, err := a.brain.CreateConversation(ctx, title, "")
		if err != nil {
			slog.Warn("add_task: could not open a conversation for the task, filing it without one", "error", err)
		}
		id, err := a.brain.AddUserTask(ctx, title, convID)
		if err != nil {
			slog.Error("add_task: write failed", "error", err)
			return toolError("that task didn't save, tell the user it is not on their list")
		}
		// The ref goes back with the title so that a correction in the next breath has something to aim at: revise takes "task#N", and without the N the model has to guess, which on 2026-09-12 it did, at a note belonging to something else.
		return fmt.Sprintf("added to the task list as task#%d: %s", id, title)

	case "save_note":
		content, ok := args["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return toolError("save_note needs the fact to remember")
		}
		if _, err := a.brain.LogNote(ctx, content, "fact"); err != nil {
			slog.Error("save_note: write failed", "error", err)
			return toolError("that didn't save, try saying it again")
		}
		return "saved"

	case "personal_context":
		action, _ := args["action"].(string)
		subject, _ := args["subject"].(string)
		content, _ := args["content"].(string)
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "view":
			entries, err := a.brain.PersonalContext(ctx)
			if err != nil {
				slog.Error("personal_context: read failed", "error", err)
				return toolError(storeUnavailable)
			}
			if len(entries) == 0 {
				return "nothing in personal context yet"
			}
			var b strings.Builder
			for _, e := range entries {
				fmt.Fprintf(&b, "%s: %s\n", e.Subject, e.Content)
			}
			return strings.TrimRight(b.String(), "\n")
		case "set":
			if strings.TrimSpace(subject) == "" {
				return toolError("personal_context needs a subject to file this under, like \"identity\" or the person's name")
			}
			if strings.TrimSpace(content) == "" {
				return toolError("personal_context needs the content to save under that subject")
			}
			if err := a.brain.SetPersonalContext(ctx, subject, content); err != nil {
				slog.Error("personal_context: write failed", "subject", subject, "error", err)
				return toolError("that didn't save, try saying it again")
			}
			return "saved"
		case "delete":
			if strings.TrimSpace(subject) == "" {
				return toolError("personal_context needs the subject to remove, view it first to see which ones there are")
			}
			if err := a.brain.DeletePersonalContext(ctx, subject); err != nil {
				slog.Error("personal_context: delete failed", "subject", subject, "error", err)
				return toolError("nothing was removed, view it first to see which subjects there are")
			}
			return "deleted"
		default:
			return toolError("personal_context takes view, set or delete")
		}

	case "revise":
		if msg := checkArgs(args, "ref", "content", "state", "priority", "remove"); msg != "" {
			return toolError(msg)
		}
		ref, ok := args["ref"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return toolError("revise needs a ref, the \"note#N\", \"thread#N\" or \"task#N\" a result showed you")
		}
		kind, id, err := parseRef(ref)
		if err != nil {
			return toolError(err.Error())
		}
		content, hasContent := args["content"].(string)
		hasContent = hasContent && strings.TrimSpace(content) != ""
		state, hasState := args["state"].(string)
		hasState = hasState && strings.TrimSpace(state) != ""
		priority, hasPriority := args["priority"].(string)
		hasPriority = hasPriority && strings.TrimSpace(priority) != ""
		remove, _ := args["remove"].(bool)
		if remove && (hasContent || hasState || hasPriority) {
			return toolError("revise can't remove and change something in the same call, pick one")
		}
		if !remove && !hasContent && !hasState && !hasPriority {
			return toolError("revise needs content, a state, a priority, or remove, say what changed")
		}

		switch kind {
		case "note":
			if remove {
				if err := a.brain.DeleteNote(ctx, id); err != nil {
					slog.Error("revise: delete failed", "id", id, "error", err)
					return toolError("nothing was there to delete, look it up again with query_memory and use the id it shows")
				}
				return "deleted"
			}
			// An action item's state lives as a "[state/priority]" prefix on the same notes row a plain note uses — SetActionStatus rewrites just that prefix and leaves the rest of the line alone.
			// Priority rides the same "[state/priority]" prefix as the status and is set the same way, so "make that one high priority" has somewhere to land.
			if hasPriority {
				p := strings.TrimSpace(priority)
				if !memory.ValidPriority(p) {
					return toolError("priority must be high, normal, or low")
				}
				if err := a.brain.SetActionPriority(ctx, id, p); err != nil {
					slog.Error("revise: priority write failed", "id", id, "error", err)
					return toolError("that priority didn't stick, look the item up again and use the id it shows")
				}
				if !hasState && !hasContent {
					return "updated"
				}
			}
			if hasState {
				s := strings.TrimSpace(state)
				if !memory.ValidStatus(s) {
					return toolError("state must be open, done, or dropped")
				}
				if err := a.brain.SetActionStatus(ctx, id, s); err != nil {
					slog.Error("revise: status write failed", "id", id, "error", err)
					return toolError("nothing was updated, look it up again with query_memory and use the id it shows")
				}
			}
			if hasContent {
				// An action item's content is a rendered "[state/priority] Owner: work (Meeting, date)" line, so its text is corrected through SetActionText, which re-renders the line: writing the model's prose straight over it would strip the prefix and drop the item out of every read that goes through ParseAction. An id that names an ordinary note is not an action item, and that one is written whole.
				err := a.brain.SetActionText(ctx, id, content)
				if errors.Is(err, db.ErrNotActionItem) {
					err = a.brain.UpdateNote(ctx, id, content)
				}
				if err != nil {
					slog.Error("revise: content write failed", "id", id, "error", err)
					return toolError("nothing was updated, look it up again with query_memory and use the id it shows")
				}
			}
			return "updated"
		case "thread":
			if remove {
				return toolError("a thread can't be removed, correct it with content instead")
			}
			if hasState || hasPriority {
				return toolError("a thread has no state or priority, those only apply to an action item")
			}
			if err := a.brain.UpdateThreadState(ctx, id, content); err != nil {
				slog.Error("revise: thread write failed", "id", id, "error", err)
				return toolError("nothing was fixed, look the thread up again with query_memory and use the id it shows")
			}
			return "fixed"
		case "task":
			// A row on the Tasks screen is either there or not: user_tasks has a done flag and nothing else to set, so "dropped" means the same thing as remove, and there is no priority to write.
			if hasPriority {
				return toolError("a task on the list has no priority, that only applies to an action item")
			}
			if remove || strings.TrimSpace(state) == "dropped" {
				if err := a.brain.DeleteUserTask(ctx, id); err != nil {
					slog.Error("revise: task delete failed", "id", id, "error", err)
					return toolError("nothing was there to delete, read user_tasks with query_store and use the id it shows")
				}
				return "deleted"
			}
			if hasState {
				s := strings.TrimSpace(state)
				if s != "done" && s != "open" {
					return toolError("a task on the list is done, open, or dropped")
				}
				if err := a.brain.SetUserTaskDone(ctx, id, s == "done"); err != nil {
					slog.Error("revise: task state write failed", "id", id, "error", err)
					return toolError("nothing was updated, read user_tasks with query_store and use the id it shows")
				}
			}
			if hasContent {
				if err := a.brain.SetUserTaskTitle(ctx, id, content); err != nil {
					slog.Error("revise: task content write failed", "id", id, "error", err)
					return toolError("nothing was updated, read user_tasks with query_store and use the id it shows")
				}
			}
			return "updated"
		default:
			return toolError(fmt.Sprintf("revise only handles note, thread and task refs, not %q", kind))
		}

	case "action_items":
		items, err := a.brain.OpenActionItems(ctx)
		if err != nil {
			slog.Error("action_items: read failed", "error", err)
			return toolError("could not read the action items")
		}
		if len(items) == 0 {
			return "nothing outstanding, no open action items"
		}
		var b strings.Builder
		// Open items never expire by design — "an owed task does not stop being owed" — so the list only grows, and it is prompt text like any other tool result.
		if len(items) > maxToolRows {
			items = items[:maxToolRows]
		}
		for _, it := range items {
			// The id leads so revise can close one without a second lookup, and the source meeting trails so the model can say where a task came from.
			fmt.Fprintf(&b, "[note#%d] %s\n", it.NoteID, it.Note())
		}
		return strings.TrimRight(b.String(), "\n")

	case "delegate":
		return delegateHandler(ctx, a, args)

	default:
		slog.Warn("unknown tool called", "tool", name)
		return toolError("there's no tool by that name")
	}
}

// toolArgSummaryRunes bounds how much of an argument the UI's tool line shows. A shell command or a note body runs arbitrarily long, and the live status row is a single line.
const toolArgSummaryRunes = 48

// quoteArg renders an argument value as a quoted display literal, cut to toolArgSummaryRunes with an ellipsis when it is longer.
// Input: the raw argument string. Output: the quoted, possibly-truncated literal, e.g. `"ls -la"`.
func quoteArg(s string) string {
	return fmt.Sprintf("%q", util.RunesEllipsis(s, toolArgSummaryRunes))
}

// toolActivitySummary pre-formats a tool call's primary argument into a short display literal for the UI (e.g. `"Riddler puzzles"` for query_memory), so the UI never needs to know each tool's arg-shape — that knowledge already lives here, next to executeTool/toolDefinitions.
// Unknown tools and no-arg tools summarize to "".
func toolActivitySummary(name string, args map[string]any) string {
	switch name {
	case "query_memory":
		if q, ok := args["query"].(string); ok {
			return quoteArg(q)
		}
	case "recall":
		if subject, ok := args["subject"].(string); ok && strings.TrimSpace(subject) != "" {
			return quoteArg(subject)
		}
		since, _ := args["since"].(string)
		until, _ := args["until"].(string)
		switch {
		case since != "" && until != "":
			return fmt.Sprintf("since %s until %s", since, until)
		case since != "":
			return fmt.Sprintf("since %s", since)
		case until != "":
			return fmt.Sprintf("until %s", until)
		}
	case "read_file":
		if path, ok := args["path"].(string); ok {
			return quoteArg(path)
		}
	case "list_files":
		if path, ok := args["path"].(string); ok && path != "" {
			return quoteArg(path)
		}
	case "open_url":
		if url, ok := args["url"].(string); ok {
			return quoteArg(url)
		}
	case "save_note":
		if content, ok := args["content"].(string); ok {
			return quoteArg(content)
		}
	case "revise":
		if ref, ok := args["ref"].(string); ok {
			return quoteArg(ref)
		}
	case "personal_context":
		action, _ := args["action"].(string)
		if subject, ok := args["subject"].(string); ok && subject != "" {
			return fmt.Sprintf("%s %s", action, quoteArg(subject))
		}
		return quoteArg(action)
	case "branch":
		if task, ok := args["task"].(string); ok {
			return quoteArg(task)
		}
	case "do":
		if goal, ok := args["goal"].(string); ok {
			return quoteArg(goal)
		}
	case "query_store":
		if query, ok := args["query"].(string); ok {
			return quoteArg(query)
		}
	case "click", "scroll_to", "point_at":
		if n, ok := args["n"].(float64); ok {
			return fmt.Sprintf("element %d", int(n))
		}
	case "switch_window", "open_app":
		if app, ok := args["app"].(string); ok {
			return quoteArg(app)
		}
	case "type_text":
		if text, ok := args["text"].(string); ok {
			return quoteArg(text)
		}
	case "click_at":
		x, xok := args["x"].(float64)
		y, yok := args["y"].(float64)
		if xok && yok {
			return fmt.Sprintf("%d,%d", int(x), int(y))
		}
	case "scroll_at":
		x, xok := args["x"].(float64)
		y, yok := args["y"].(float64)
		dy, dyok := args["dy"].(float64)
		if xok && yok && dyok {
			return fmt.Sprintf("%d steps at %d,%d", int(dy), int(x), int(y))
		}
	}
	return ""
}

// resultSummary condenses a tool's raw result string into a short status word for the UI's transcript log line and for a "tool" event's Detail — "N hits" for the search-shaped tools, "failed" on any error result (executeTool always prefixes errors with "error"), "0 hits" for the known empty-result sentinels, the window line for observe_screen and the window a click landed on (never the accessibility listing itself, only that one line — see toolLogDetail for the separate rule that keeps screen content out of the server's own log file), "done" for any other success.
func resultSummary(name, result string) string {
	if strings.HasPrefix(result, "error") {
		return "failed"
	}
	if strings.HasPrefix(result, "Stopped before ") {
		return "stopped"
	}
	switch result {
	case "no memory matches", "no memory of that subject", "no episodes in that window", "no rows matched":
		return "0 hits"
	case "saved":
		return "saved"
	case "updated":
		return "updated"
	case "deleted":
		return "deleted"
	case "fixed":
		return "fixed"
	}
	if name == "query_memory" || name == "recall" {
		return fmt.Sprintf("%d hits", strings.Count(result, "\n")+1)
	}
	if name == "observe_screen" {
		line, _, _ := strings.Cut(result, "\n")
		return line
	}
	if name == "click" {
		if _, rest, ok := strings.Cut(result, `the window is now "`); ok {
			if title, _, ok := strings.Cut(rest, `"`); ok {
				return fmt.Sprintf("window now %q", title)
			}
		}
	}
	if name == "click_at" || name == "scroll_at" {
		if line, _, ok := strings.Cut(result, ";"); ok {
			return line
		}
	}
	if name == "draw" {
		if label, ok := drawnItemLabel(result); ok {
			return fmt.Sprintf("drew around %q", label)
		}
	}
	return "done"
}

// drawnItemLabel reads the item name off a draw result that resolved a numbered item — "drew a circle around [3] push button \"Reload\"" (see the draw case's "box", "circle" branch) — so resultSummary can surface what was actually drawn around instead of the generic "done" a caller reading only the summary would otherwise see; a live "tool" event and a screen eval both read the summary, never the full result text. Output: the label and true, or "" and false for a draw that named no item — an arrow, a line, a path, or a box/circle drawn from a raw rectangle rather than "on" a numbered one.
func drawnItemLabel(result string) (string, bool) {
	_, rest, ok := strings.Cut(result, "] ")
	if !ok {
		return "", false
	}
	_, rest, ok = strings.Cut(rest, `"`)
	if !ok {
		return "", false
	}
	label, _, ok := strings.Cut(rest, `"`)
	return label, ok
}

// filterDescription names the post-filters query_memory applied, in the words the model can repeat back: "in slack since Aug 28 00:00". Returns "" when no filter was set, so the caller can fall back to the plain no-matches answer.
func filterDescription(app string, since, until time.Time) string {
	var parts []string
	if strings.TrimSpace(app) != "" {
		parts = append(parts, "in "+strings.TrimSpace(app))
	}
	if !since.IsZero() {
		parts = append(parts, "since "+since.In(time.Local).Format("Jan 2 15:04"))
	}
	if !until.IsZero() {
		parts = append(parts, "until "+until.In(time.Local).Format("Jan 2 15:04"))
	}
	return strings.Join(parts, " ")
}

// filterHitsByApp keeps episode hits whose App contains filter (case-insensitive) and drops other sources. A missing App on an episode hit is dropped rather than guessed.
func filterHitsByApp(hits []db.MemoryHit, app string) []db.MemoryHit {
	app = strings.ToLower(strings.TrimSpace(app))
	if app == "" {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if h.Source != "episode" {
			continue
		}
		if strings.Contains(strings.ToLower(h.App), app) {
			out = append(out, h)
		}
	}
	return out
}

// listMeetingNotes renders every meeting-minutes note whose creation time falls in [since, until] (a zero bound is open), newest first, each as "[note#ID] date: excerpt". It reads the notes table directly rather than ranking, because minutes are the answer to "what was the meeting about" and no query word reliably ranks them above the screens of the user reading them.
// maxMeetingNotesListed caps how many meetings query_memory kind=meeting lists in one answer; the rest are counted, and a narrower window or a real query reaches them.
const maxMeetingNotesListed = 30

// latestMeetingNotesListed is how many meetings are listed when no window was given. A question with no window is "the latest meeting", and listing thirty sets of minutes for it cost one round 178k input tokens on 2026-09-10; the count line says how many older ones a window would reach.
const latestMeetingNotesListed = 3

func (a *Agent) listMeetingNotes(ctx context.Context, since, until time.Time) string {
	notes, err := a.brain.NotesOfKindSince(ctx, "meeting", since)
	if err != nil {
		slog.Error("query_memory: reading meeting notes failed", "error", err)
		return toolError(storeUnavailable)
	}
	limit := maxMeetingNotesListed
	if since.IsZero() && until.IsZero() {
		limit = latestMeetingNotesListed
	}
	var lines []string
	left := 0
	for _, n := range notes {
		if !until.IsZero() && n.CreatedAt.After(until) {
			continue
		}
		if len(lines) == limit {
			left++
			continue
		}
		lines = append(lines, fmt.Sprintf("[note#%d] %s: %s", n.ID, n.CreatedAt.Format("Mon Jan 2 15:04"), db.FormatNoteHitWithSource(db.MemoryHit{Source: "note", RefID: n.ID, Content: n.Content, CreatedAt: n.CreatedAt}, 0)))
	}
	if len(lines) == 0 {
		if desc := filterDescription("", since, until); desc != "" {
			return "no meeting minutes " + desc
		}
		return "no meeting minutes saved yet"
	}
	if left > 0 {
		lines = append(lines, fmt.Sprintf("and %d more, older; narrow the window or ask about one", left))
	}
	return strings.Join(lines, "\n")
}

// typingPlaces are the roles of a control text can legitimately land in, so a field holding the keyboard while another was clicked is somewhere the keys have a place to go rather than proof they would be lost. A password box is one of them on purpose: it is the secret stop line, not this one, that refuses typing into it, and leaving it out here would answer with the wrong refusal. A combo box is not one, because only a combo box holding a box to type in can be typed into and the tracker's read resolves such a one to that child (see tracker.FocusedElement).
var typingPlaces = map[string]bool{"entry": true, "text": true, "password text": true, "search box": true}

// keyboardHolder reads which element of the window in front holds the keyboard. The tracker's read in production, a stand-in in the tests; it is a package variable rather than a field on Agent because the stop lines it serves are the package's, not one session's.
var keyboardHolder = tracker.FocusedElement

// keyboardField says which control the keys are about to reach and whether this session can vouch for the answer. Input: the call's context and the item the last numbered click acted on. Output: the control to put the stop lines against, false when the keyboard is provably somewhere the keys have no place to land, and a note naming where they are going when that is not the clicked control.
// The clicked control still holding the keyboard is the ordinary case and focusHeld answers it, a descendant of it holding the keyboard included, which is how Chromium and Electron publish a focused text input. Only when that read says a definite no is the window in front asked who does hold the keys: another box to type in is a legitimate landing place — a page that moved the focus into its own search box is the case this refused on the user's screen — and a readable holder that is no place to type is the proof this stop line exists for.
func (a *Agent) keyboardField(ctx context.Context, it act.Item) (act.Item, bool, string) {
	if a.focusHeld(ctx, it) {
		return it, true, ""
	}
	holder, ok := keyboardHolder(ctx)
	if !ok {
		// Nothing readable holds the keyboard, which is not the same as the keys going astray: the window may publish no tree at all. This is the same unknown the failed read of the clicked field is, and it goes ahead on the remembered click.
		slog.Warn("nothing readable holds the keyboard in the window in front, so typing goes ahead on the remembered click", "ref", it.Ref)
		return it, true, ""
	}
	got := act.Item{Role: holder.Role, Label: holder.Label, Ref: holder.Ref}
	if !typingPlaces[holder.Role] {
		return got, false, ""
	}
	return got, true, fmt.Sprintf(" into the %s %q, which holds the keyboard, not the %s %q that was clicked", got.Role, got.Label, it.Role, it.Label)
}
