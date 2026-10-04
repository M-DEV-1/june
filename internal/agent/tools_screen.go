package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"june/internal/act"
	"june/internal/tracker"
	"june/internal/window"
)

// screenSnapshot is one observe_screen answer kept for the next one to be compared against: the window it was of and the numbered lines it listed. It is only ever used to shorten what the model is told; the list the numbers resolve against is the ask's own seen items, which a fresh walk rewrites on every look.
type screenSnapshot struct {
	app, title string
	lines      []string
}

// ScreenTarget is the screen item a point_at, click or draw(on) call last acted on or pointed at: its label and role off the observe_screen listing that named it, the window it was in, and its rectangle in screen pixels when the hop had one. It is kept on the agent across every ask this session answers, so a bare "it" or "that" in a later ask can be resolved against what was actually done rather than a fresh screen read.
type ScreenTarget struct {
	Label, Role, Window string
	// HasRect is false for a target recorded from click, whose result text (frozen by an existing test) carries no rectangle.
	HasRect    bool
	X, Y, W, H int
}

// screenTarget returns the ScreenTarget this session last pointed at or acted on (see rememberTarget). Output: the target and true, or the zero value and false when nothing has been pointed at or acted on yet this session.
func (a *Agent) screenTarget() (ScreenTarget, bool) {
	t, ok := a.lastTarget.Load().(ScreenTarget)
	return t, ok
}

// rememberTarget stores t as the newest screen target, for a later ask's bare "it" to resolve against. Input: the target just acted on or pointed at. Output: none.
func (a *Agent) rememberTarget(t ScreenTarget) {
	a.lastTarget.Store(t)
}

// bareReferenceWords are the endings a follow-up on the same screen uses to mean "the thing I just pointed at or acted on" rather than naming something of its own: the question ends, trailing punctuation aside, in one of these — "ring it", "draw a circle around it", "do that again" — never "click the address bar", which already names its own noun after the pronoun or instead of one.
var bareReferenceWords = []string{"it", "that", "this", "again", "the same one"}

// isBareReference reports whether question is a follow-up naming nothing of its own to act on: it ends, once trailing punctuation is stripped, in one of bareReferenceWords. Deliberately simple — a question that names a noun after its pronoun ("this button") fails the check and gets no reminder, which only ever costs a helpful sentence, never a wrong one.
func isBareReference(question string) bool {
	q := strings.ToLower(strings.TrimRight(strings.TrimSpace(question), ".?! "))
	for _, w := range bareReferenceWords {
		if q == w || strings.HasSuffix(q, " "+w) {
			return true
		}
	}
	return false
}

// targetMismatchNote is the one line a point_at or draw(on) result adds when a bare follow-up ("ring it") resolved to a different item than the one remembered from before, so the model can correct itself within the same ask rather than the wrong item standing as what was drawn or pointed at. Input: the question, the target remembered before this call (nil when there was none) and the label the number just resolved to. Output: the note, with its own leading newline so a caller can concatenate it straight onto its result, or "" when there is nothing to flag — the question named its own target, there was nothing remembered yet, or the labels already agree.
func targetMismatchNote(question string, remembered *ScreenTarget, gotLabel string) string {
	if remembered == nil || !isBareReference(question) || remembered.Label == gotLabel {
		return ""
	}
	return fmt.Sprintf("\n(you were asked about %q, this is %q)", remembered.Label, gotLabel)
}

// unchangedScreenMarker opens the answer a repeat look gets. It is matched, not just printed: sameScreenAgain in ask.go reads it to know that a look which returned this text saw the screen it had already seen, so a round spent on it does not spend a step.
const unchangedScreenMarker = "(unchanged since the last look"

// sameTreeHint is appended to the answer a repeat observe_screen gets when nothing in the listing changed. What did not change is the accessibility tree, which is all observe_screen reads; a window that draws its own controls publishes none of them, so it reads identical whether the last action worked or not.
//
// On 2026-09-20 a GNOME Clocks run was told "(unchanged since the last look" six times while setting a timer. The timer's digits are drawn by the app and are in no tree, so every click on them came back looking like a click that did nothing, and the model clicked the same coordinate four times and typed "15" twice. The listing was telling the truth about the tree and the model read it as the truth about the screen.
const sameTreeHint = "\n(this is the accessibility tree, which is all observe_screen reads — a window that draws its own controls looks identical here whether or not it changed, so call look to see whether the last action landed)"

// frameOnlyHint is appended to an observe_screen result when the window published nothing but its own frame buttons, so the model knows to fall back to look instead of reporting an empty screen.
const frameOnlyHint = "\n(this window publishes none of its content to the accessibility tree; call look to see the page as a picture, and click_at with points from that picture)"

// jobFrameOnlyHint and jobSameTreeHint are what a computer-use job is told in place of frameOnlyHint and sameTreeHint. Both of those send the model to look, and inside a job look and every click at a point are refused (see jobSeesNoPictures), so a text-only brain was told to take an action certain to be refused, and every refusal was a failed step towards the stuck question.
const (
	jobFrameOnlyHint = "\n(this window publishes none of its content to the accessibility tree, and this job reads that tree alone, so nothing in it can be acted on from here; if the goal needs it, say so with ask)"
	jobSameTreeHint  = "\n(this is the accessibility tree, which is all observe_screen reads — a window that draws its own controls looks identical here whether or not it changed, so write a screen_changed check for an action whose effect this list cannot show)"
)

// frameOnlyNote and sameTreeNote are the two hints for whoever is reading the listing: a job, which cannot be shown a picture, or anything else. Input: whether the call is a job's (see inJob). Output: the hint.
func frameOnlyNote(job bool) string {
	if job {
		return jobFrameOnlyHint
	}
	return frameOnlyHint
}

func sameTreeNote(job bool) string {
	if job {
		return jobSameTreeHint
	}
	return sameTreeHint
}

// frameOnlyLabels are the window-frame button labels that, alone, mean a window's own content never reached the accessibility bus. This happens with Chromium-family browsers run without --force-renderer-accessibility: the window manager's minimize/maximize/restore/close controls still publish, but the page itself does not.
var frameOnlyLabels = map[string]bool{"Minimize": true, "Maximize": true, "Restore": true, "Close": true}

// reviseHint is appended to a memory read whose hits include a note, because a note is the only thing revise can change or remove and the ref it needs is right there in the line above.
//
// The system prompt already says to fix memory in the same turn, and it lost. On 2026-09-12 the user said "you can delete note"; June called query_memory, got "[note#312]" back, said "Understood, I've deleted that note about the supplement then", and never called revise. The note is still in the store. A rule sitting in nine thousand tokens of system prompt competes with everything else in it; a line attached to the result the model is reading at the moment it decides does not.
//
// ponytail: this makes the next action salient, it cannot make a model take it. What would actually close the gap is noticing that June claimed a change no tool call made and feeding that back as a lesson. That needs the spoken text and the turn's tool calls compared in one place, which is a bigger piece of work than this line.
const reviseHint = "\n(any [note#N] above can be corrected or removed right now with revise — if they asked you to change or forget one, call it in this turn; saying you did without calling it leaves the wrong fact in memory.)"

// noteRefPattern matches the "[note#N]" ref db.FormatNoteHit puts on a note hit, which is the only hit shape revise takes.
var noteRefPattern = regexp.MustCompile(`\[note#\d+\]`)

// withReviseHint appends reviseHint to a memory read's result when that result actually holds a note ref. Input: the result as query_memory or recall built it. Output: the same result with the hint on the end, or unchanged when there is no note in it — a search that returned only screen summaries has nothing revise could act on, so it does not pay for the line.
func withReviseHint(result string) string {
	if !noteRefPattern.MatchString(result) {
		return result
	}
	return result + reviseHint
}

// browserKeysHint is appended to an observe_screen result when the window in front is a browser. A browser's tab strip and address bar are chrome, not page content, so the Linux walk never lists them (the Windows walk lists the address bar, which is why the hint says "may not" rather than "never": a hint that contradicts the lines above it is one the model stops trusting), and a model that only knows what the listing holds reads that absence as "there is nothing here to close a tab with" and says out loud that it cannot. On 2026-09-12 that happened three times in one session — refusing to switch a tab, refusing to close tabs, refusing to open a link — with press_key available the whole time.
//
// It names what can be done and the key that does it, because the gap is not that the model lacks press_key; it is that nothing connects "close a tab" to "Ctrl+W". The keys are the ones the session actually needed, not every shortcut a browser has: a longer list costs every browser look more tokens to carry guesses nobody asked for.
const browserKeysHint = "\n(this is a browser: its tabs and address bar are chrome, not page content, so the list above may not show them — reach them with press_key. Ctrl+W closes the tab in front, Ctrl+Tab moves to the next tab, Ctrl+T opens a new one, Ctrl+L selects the address bar.)"

// browserMarkers are the lowercase fragments that identify a browser in the app name observe reports ("Brave Browser", "Chromium", "Google Chrome", "Firefox"). Fragments rather than whole names because the same browser reports itself differently depending on how it was started — snap Brave comes through as "Brave Browser", and an X11 window activated through its mutter frame reports the frame's own name (see the note in internal/act) — and "chrom" covers Chromium and Chrome together.
var browserMarkers = []string{"brave", "chrom", "firefox", "edge", "safari", "vivaldi", "opera"}

// isBrowser reports whether an app name observe returned is a browser, so observeResult knows whether browserKeysHint is worth its tokens. Input: the app name, in whatever case it arrived. Output: true when any browserMarkers fragment is in it.
func isBrowser(app string) bool {
	lower := strings.ToLower(app)
	for _, marker := range browserMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// frameOnly reports whether lines, an observe_screen listing already split one item per line, contains nothing but window-frame buttons: every line is a push button whose label is Minimize, Maximize, Restore or Close. An empty listing counts too, since a window with no readable content shows the same nothing either way. Input: the numbered listing lines, in act.Format's `[n] role "label" (x,y)` shape. Output: true when the listing carries no content of its own to act on.
func frameOnly(lines []string) bool {
	for _, line := range lines {
		isFrameButton := false
		for label := range frameOnlyLabels {
			if strings.Contains(line, `push button "`+label+`"`) {
				isFrameButton = true
				break
			}
		}
		if !isFrameButton {
			return false
		}
	}
	return true
}

// pageUnreadHint and jobPageUnreadHint are appended to a browser's listing that holds its own controls and nothing of the page (see pageListed). Chromium (Chrome, Edge and every Electron app) builds a page's accessibility tree only once a client asks for it and fills it in over the following second, so the first read of a window nobody had read listed the tab strip, Back, Refresh and the address bar and none of the page. The Windows walk reads such a window a second time after a pause, but the page can still be missing after it, and without a word on it the model took the toolbar for the whole page. frameOnlyHint does not fire there, because the toolbar is more than the window's frame buttons.
const (
	pageUnreadHint    = "\n(nothing of the page itself is in this list, only the browser's own controls: a browser can take a moment to publish a page nothing had read before, so call observe_screen once more, and if the page is still missing, call look to see it as a picture)"
	jobPageUnreadHint = "\n(nothing of the page itself is in this list, only the browser's own controls: a browser can take a moment to publish a page nothing had read before, so call observe_screen once more, and if the page is still missing, this job cannot reach it, so say so with ask)"
)

// pageUnreadNote is the hint for a browser listing with none of the page in it, for a job or anything else (see frameOnlyNote). Input: whether the call is a job's. Output: the hint.
func pageUnreadNote(job bool) string {
	if job {
		return jobPageUnreadHint
	}
	return pageUnreadHint
}

// pageRoles are the roles only a page lists: a browser's own tab strip, toolbar and address bar are page tabs, buttons and an entry, and a page with anything on it has a run of text or a link at the least.
var pageRoles = map[string]bool{"text": true, "link": true, "list item": true, "table cell": true, "tree item": true, "check box": true, "radio button": true, "combo box": true, "slider": true, "spin button": true}

// pageListed reports whether a listing holds anything of a page. Input: the numbered listing lines, in act.Format's `[n] role "label"` shape. Output: true when any line's role is one of pageRoles.
func pageListed(lines []string) bool {
	for _, line := range lines {
		_, rest, ok := strings.Cut(line, "] ")
		if !ok {
			continue
		}
		if role, _, _ := strings.Cut(rest, ` "`); pageRoles[role] {
			return true
		}
	}
	return false
}

// maxChangedLines is how many lines may differ before a look is sent as a whole list instead of as the lines that changed. Twelve, because past that the page has moved rather than ticked over, and a model piecing a page together out of a dozen scattered corrections is worse off than one reading the page.
const maxChangedLines = 12

// observeResult renders one look at the screen against the look before it, so a page that has not moved is not paid for twice. Input: the window's app and title, the numbered lines this look produced, the snapshot of the look before (zero value when there was none), and whether a job is reading it, which picks the hints (see frameOnlyNote). Output: the whole numbered list for a first look, a different window, a list of a different length, or a page that changed too much to state line by line; a one-line "unchanged" answer when nothing moved; otherwise the window line and only the lines that changed.
// The numbers are the same numbers either way: the shorter answers are only ever sent when the list is the same length in the same order, so a number the model already has still points at the same row.
func observeResult(app, title string, lines []string, previous screenSnapshot, job bool) string {
	window := app + " · " + title
	full := window + "\n" + strings.Join(lines, "\n")
	if frameOnly(lines) {
		full += frameOnlyNote(job)
	} else if isBrowser(app) && !pageListed(lines) {
		full += pageUnreadNote(job)
	}
	// Only on the full listing, never on the two short answers below. Those exist to save tokens on a window already described, and this hint is stable for the app, so the copy sent with that window's first full listing is still in the conversation. A window not described yet always renders full, which is where the hint is needed.
	if isBrowser(app) {
		full += browserKeysHint
	}
	if previous.app != app || previous.title != title || len(previous.lines) != len(lines) {
		return full
	}
	var changed []string
	for i, line := range lines {
		if line != previous.lines[i] {
			changed = append(changed, line)
		}
	}
	switch {
	case len(changed) == 0:
		return fmt.Sprintf("%s\n%s the same %d items, and their numbers still stand)%s", window, unchangedScreenMarker, len(lines), sameTreeNote(job))
	case len(changed) <= maxChangedLines:
		return fmt.Sprintf("%s\n(the same %d items as the last look, with these changed:)\n%s", window, len(lines), strings.Join(changed, "\n"))
	}
	return full
}

// unchangedLookMarker opens what look answers when the screen is the very picture the model already has, so the model is told there is nothing new to see instead of being sent the same picture again.
const unchangedLookMarker = "the screen has not changed since your last look"

// lookTokenCost estimates what an image of this size costs the model to read. Input: the picture's width and height in pixels. Output: the token count, at one token per 750 pixels, which is what Anthropic documents and close to what the other two charge. No provider reports its input broken down by part, so this is an estimate on purpose — it is here so a turn that sent two screenfuls does not look, in the ledger, exactly like one that sent none.
func lookTokenCost(w, h int) int {
	return w * h / 750
}

// askLookState is what one ask's looks and screen tools leave behind: the newest picture, taken so draw can map coordinates the model reads off it back onto the screen; whether that picture has been handed to the model yet; what the pictures are estimated to have cost; the list the last observe_screen produced and the answer it gave; and which control the last click focused. It is carried on the ask's own context (see withAskLookState), not on the shared Agent, so two asks running at once — a routine and a typed question — never share or clobber one screenshot, one numbered list, or one stop-line check.
type askLookState struct {
	mu sync.Mutex
	// look is the newest picture this ask took.
	look *tracker.Capture
	// lookUndelivered is true between a look being taken and the picture being handed to the model, so each picture is sent exactly once, with the tool result that produced it.
	lookUndelivered bool
	// lookTokens is what this ask's pictures are estimated to have cost.
	lookTokens int
	// items is the []act.Item the last observe_screen listed, which a number from the model resolves against, and snap is the answer that look produced, so a repeat look can say what changed instead of sending the whole list again.
	items []act.Item
	snap  screenSnapshot
	// clicked is the item the last numbered click acted on, standing in for "the control that now has keyboard focus" — there is no accessibility read for actual focus, and click it first is the documented way to reach type_text anyway. focusUnknown says that stand-in cannot be trusted: a click at a bare coordinate or a key that moves focus has left this session unable to name what the keyboard is pointing at.
	clicked      act.Item
	focusUnknown bool
	// beforeShot is the picture a screen_changed check keeps before the action, for wait_for to compare against (see CheckHolds and waitFor).
	beforeShot tracker.Capture
	// branches is how many background searches this ask has run: a search costs a call against a monthly search allowance, not this turn's tokens.
	branches int
	// acted is the window the last numbered click was seen to leave in front, read straight after it, and "" once observe_screen has looked again. A click can retitle the window it lands in (a chat opening the conversation clicked), and the keys sent right after it in the same burst are going where that click put them, not into some other window (see keysWindow).
	acted string
	// job marks the state of a computer-use job (see NewScreenScope) rather than of an ask: its brain reads text only, so no picture can be shown to it, and its own prompt offers it branch, which an ask is not offered (see jobAllowedTools).
	job bool
}

// maxBranchesPerAsk is how many background web searches one ask may run. The old cap went when the Gemini-subtask engine it guarded was retired, and nothing replaced it, so an agentic loop could call branch as often as it liked. It no longer spends the daily Gemini allowance, but it does spend a real search allowance — Tavily's free tier is a thousand calls a month, which one determined session could make a dent in.
const maxBranchesPerAsk = 6

// takeBranch records one background search against this ask's allowance. Input: the ask's context. Output: true when there was one left to take.
func takeBranch(ctx context.Context) bool {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.branches >= maxBranchesPerAsk {
		return false
	}
	s.branches++
	return true
}

// streamDrawn is one shape that was resolved while the model was still writing the call it belongs to, whether it drew or not. The Codex stream hands over each shape the moment its object closes, so the ink starts before the round has finished rather than after it (see parseCodexStream); the phrase and the target are what the draw tool would have produced had it drawn the shape itself. Err carries the tool error when the shape failed instead — a shape refused during the stream still has to occupy its place in this slice, or the shapes it precedes in the finished call would be shifted onto the wrong entries.
type streamDrawn struct {
	Phrase string
	Target *ScreenTarget
	Err    string
}

// streamDrawnKey is the unexported context key withStreamDrawn stores one call's already-drawn shapes, and the group they were drawn under, at.
type streamDrawnKey struct{}

// streamDrawnState is what a draw call inherits from the stream that drew part of it: the shapes already on screen, and the group name they went out under, which the rest of the call has to reuse or the overlay treats the two halves as separate drawings.
type streamDrawnState struct {
	group string
	drawn []streamDrawn
}

// withStreamDrawn carries the shapes a draw call already drew off the stream into that call's own execution, so the tool draws only what is left rather than drawing everything a second time. Input: the ask's context, the group those shapes were drawn under, and the shapes themselves in the order the call listed them. Output: a context for that one tool call.
func withStreamDrawn(ctx context.Context, group string, drawn []streamDrawn) context.Context {
	if len(drawn) == 0 {
		return ctx
	}
	return context.WithValue(ctx, streamDrawnKey{}, streamDrawnState{group: group, drawn: drawn})
}

// streamDrawnFrom returns the leading shapes of this draw call that the stream already drew. Output: those shapes in call order, or nil when the call drew nothing early — which is every provider but Codex, and every Codex round whose backend sent no argument deltas.
func streamDrawnFrom(ctx context.Context) []streamDrawn {
	st, _ := ctx.Value(streamDrawnKey{}).(streamDrawnState)
	return st.drawn
}

// drawGroupFor names the draw call now running, so every shape of it lands on the overlay as one drawing. Output: the group the stream already drew this call's leading shapes under, or a fresh name when nothing was drawn early.
func drawGroupFor(ctx context.Context) string {
	if st, ok := ctx.Value(streamDrawnKey{}).(streamDrawnState); ok && st.group != "" {
		return st.group
	}
	return nextDrawGroup()
}

// askLookStateKey is the unexported context key withAskLookState stores the per-ask look state under.
type askLookStateKey struct{}

// withAskLookState attaches a fresh, empty look state to ctx, one per ask, so the picture draw maps coordinates against and what the pictures cost belong to the ask now starting rather than to whatever ask ran before it. Input: the ask's own context. Output: a context carrying the new state, to use for every tool call the ask makes.
func withAskLookState(ctx context.Context) context.Context {
	return context.WithValue(ctx, askLookStateKey{}, &askLookState{})
}

// NewScreenScope gives one caller its own screen state — the numbered list observe_screen produced, the picture look took and what the pictures cost, and which control the last click focused — in place of the agent-wide state a directly driven tool call would otherwise read and write. A long-running computer-use job (internal/actjob) calls it once and makes every tool call of that job with the context it returns, so two jobs never resolve a number against each other's window or each other's pictures. Input: the job's own context. Output: a context carrying fresh screen state.
// The state is marked as a job's (see inJob). A job's brain is text in, text out (actjob.Model), so a look it takes is a picture nobody can show it: look said "here is the picture" and then every point read off it was refused, a round apiece. Marked, look and a click at a point say at once that this brain cannot see pictures.
func (a *Agent) NewScreenScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, askLookStateKey{}, &askLookState{job: true})
}

// inJob reports whether a tool call belongs to a computer-use job, whose screen state NewScreenScope made. Input: the call's context. Output: true for a job's call, false for an ask's or a directly driven one.
func inJob(ctx context.Context) bool {
	s, ok := ctx.Value(askLookStateKey{}).(*askLookState)
	return ok && s.job
}

// jobSeesNoPictures is what look and a click at a point say inside a job: the job's brain reads text only, so there is no picture it could be shown and no point it could read off one. It names the path that works, the numbered list, rather than leaving the model to look again.
const jobSeesNoPictures = "this job's brain reads text only, so no picture can be shown to it and no point read off one; use observe_screen and act on an item by its number, and when the list shows nothing for what you need, say so with ask"

// lookStateFrom reads the look state withAskLookState attached to ctx. Output: that state, or a throwaway empty one when ctx carries none, which only happens when a tool is driven directly rather than through an ask.
func lookStateFrom(ctx context.Context) *askLookState {
	if s, ok := ctx.Value(askLookStateKey{}).(*askLookState); ok {
		return s
	}
	return &askLookState{}
}

// askState is the screen state this tool call reads and writes: the one withAskLookState attached to ctx, or the agent's own when a tool is driven directly rather than through an ask, which is what a test and the eval entry points do. Input: the call's context. Output: the state, never nil.
func (a *Agent) askState(ctx context.Context) *askLookState {
	if s, ok := ctx.Value(askLookStateKey{}).(*askLookState); ok {
		return s
	}
	return &a.askScreen
}

// seen returns the items the last observe_screen of this ask listed, which is what a number the model gives resolves against. Output: nil when this ask has not observed the screen yet.
func (a *Agent) seen(ctx context.Context) []act.Item {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items
}

// rememberScreen stores what an observe_screen call just produced: the items its numbers resolve against and the snapshot the next look is compared to. Observing also restores a known focus, since after it the model is naming elements again rather than pixels. Input: the call's context, the listed items and the snapshot. Output: none.
func (a *Agent) rememberScreen(ctx context.Context, items []act.Item, snap screenSnapshot) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items, s.snap, s.focusUnknown, s.acted = items, snap, false, ""
}

// rememberActed records the window a numbered click was seen to leave in front, and actedIn reads it back: "" when observe_screen has looked since, or nothing was clicked. Input: the call's context and the window in "app · title" form.
func (a *Agent) rememberActed(ctx context.Context, window string) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acted = window
}

func (a *Agent) actedIn(ctx context.Context) string {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acted
}

// lastScreen returns the snapshot of this ask's last observe_screen answer, the zero value when it has not observed yet.
func (a *Agent) lastScreen(ctx context.Context) screenSnapshot {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// focus returns this session's stand-in for the control the keyboard is pointing at: the item the last numbered click acted on. Output: the item, and false when nothing can be vouched for — a click at a bare coordinate or a focus-moving key has happened since, and the caller must refuse rather than check the stale item.
func (a *Agent) focus(ctx context.Context) (act.Item, bool) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clicked, !s.focusUnknown
}

// focusHeld reports whether the control the last click focused still has the keyboard, read from the accessibility focused state. Input: the call's context and the remembered item. Output: held, false only when the read says definitely not focused — a dialog that opened over the field, or an application that moved the focus itself — and true otherwise, including when nothing has been wired to read focus, the item carries no reference or the read fails, where the remembered click is all there is; and read, true only when the read itself said the element is focused, which is what lets keysWindow take the field's window for the one last listed without reading it again.
func (a *Agent) focusHeld(ctx context.Context, it act.Item) (held, read bool) {
	// Nothing has been clicked yet: there is no field to read the state on, and the zero item this stands for matches no stop-line check either, so this is the same "typing where the focus is" it has always been.
	if a.focused == nil || it.Ref == "" {
		return true, false
	}
	held, err := a.focused(ctx, it.Ref)
	if err != nil {
		// The read said nothing rather than saying no: the bus timed out, the element has gone, or its toolkit does not publish the focused bit on the node the walk listed, which Chromium and Electron often do not. Refusing on that stops typing into fields that are perfectly focused, so the remembered click stands and the guard bites only on a definite not-focused.
		slog.Warn("could not read whether the field still holds the keyboard, so typing goes ahead on the remembered click", "ref", it.Ref, "error", err)
		return true, false
	}
	return held, held
}

// rememberClick records the item a numbered click just acted on as the control the keyboard is now pointing at. Input: the call's context and the clicked item. Output: none.
func (a *Agent) rememberClick(ctx context.Context, it act.Item) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicked, s.focusUnknown = it, false
}

// focusLost marks this session unable to say what the keyboard is pointing at, which a click at a bare coordinate and a focus-moving key both leave behind. type_text and a focused key press refuse while it stands. Input: the call's context. Output: none.
func (a *Agent) focusLost(ctx context.Context) {
	s := a.askState(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicked, s.focusUnknown = act.Item{}, true
}

// recordLook stores the picture a look just took: it becomes the one draw maps coordinates against, it is queued to be handed to the model, and its cost is added to the ask's. Input: the ask's context and the capture. Output: false, recording nothing, when this is the exact picture the last look took — the screen has not changed since, so there is nothing new to send, and a picture not yet handed over still goes out once with the result that took it.
func recordLook(ctx context.Context, c tracker.Capture) bool {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.look != nil && bytes.Equal(s.look.Data, c.Data) {
		return false
	}
	s.look, s.lookUndelivered = &c, true
	s.lookTokens += lookTokenCost(c.W, c.H)
	return true
}

// markLookDelivered records that the model has been shown the newest picture, for a channel that had to push it out of band rather than return it with the tool result (see deliverPicture). Input: the ask's context. Output: none.
func markLookDelivered(ctx context.Context) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookUndelivered = false
}

// takeLook hands the newest picture to whichever brain is assembling the request, once. Output: the capture and true the first time it is called after a look, false every other time — the picture goes to the model with the tool result that produced it and never again.
func takeLook(ctx context.Context) (tracker.Capture, bool) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.look == nil || !s.lookUndelivered {
		return tracker.Capture{}, false
	}
	s.lookUndelivered = false
	return *s.look, true
}

// lookSeen returns the newest picture this ask took and actually showed the model, which is the one draw maps coordinates against, plus the reason a refusal happened. Delivery is the test, not the taking: a channel with no way to carry an image — the Live voice session, which takes tool results as text — leaves every picture undelivered, and a point read off a picture nobody saw is a guess like any other. Output: the capture and true when a picture was taken and handed over; otherwise false, with blind=true when a picture was taken but this channel could not carry it to the model, which no number of further looks will change.
func lookSeen(ctx context.Context) (capture tracker.Capture, ok bool, blind bool) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.look == nil {
		return tracker.Capture{}, false, false
	}
	if s.lookUndelivered {
		return tracker.Capture{}, false, true
	}
	return *s.look, true, false
}

// lookTokensSpent is what this ask's pictures are estimated to have cost, for the turn trace.
func lookTokensSpent(ctx context.Context) int {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookTokens
}

// needLookFirst is what draw says when it is given coordinates with no picture behind them. It names the tool to call, because the refusal is only useful if the model's next move is the look it should have made.
const needLookFirst = "I need to look at the screen first, call look, then give me points in that picture's own coordinates"

// cannotSeePictures is what draw says when the picture was taken but this session had no way to show it. A Live voice session takes tool results as text, so every look it makes is undelivered and no further look will change that: telling it to look again sends it round the same loop until it gives up and reports that its screen tools are broken, which is what happened on 2026-09-07 against an Electron window. The refusal therefore names the path that does work — the numbered list — and says plainly that this one never will.
const cannotSeePictures = "I took the picture but this session cannot show it to me, so I have no coordinates to work from and looking again will not help. Use observe_screen and act on an item by its number instead; if observe_screen lists nothing, this window does not expose its contents and I should say so rather than keep trying."

// toScreen turns a point the model read off the last look into the point on the screen it names. Input: the point in the picture's own pixels. Output: the screen point, or a tool error when this ask has not looked or the point is outside the picture, which is the shape a guessed coordinate takes.
func (a *Agent) toScreen(ctx context.Context, x, y int) (int, int, string) {
	if inJob(ctx) {
		return 0, 0, toolError(jobSeesNoPictures)
	}
	c, ok, blind := lookSeen(ctx)
	if !ok {
		if blind {
			return 0, 0, toolError(cannotSeePictures)
		}
		return 0, 0, toolError(needLookFirst)
	}
	if !c.Holds(x, y) {
		return 0, 0, toolError(fmt.Sprintf("%d,%d is not inside the picture the last look took, which is %d wide and %d high, give me a point in it", x, y, c.W, c.H))
	}
	sx, sy := c.ToScreen(x, y)
	return sx, sy, ""
}

// toScreenForDraw turns a point the model worked out from the last look into the point on the screen it names, for drawing only. Input: the call's context and the point in the picture's own pixels. Output: the screen point, or a tool error when no look has been taken or the point is outside the picture.
// It differs from toScreen in one way: it maps the point even when the picture was never delivered to the model. A live voice session takes tool results as text, so it never sees the image — but it is told the frame in words ("here is the picture, 1280 wide and 698 high … one of its pixels is 1.50 screen pixels"), which is everything needed to place a shape. The rule toScreen enforces, that a point read off a picture nobody saw is a guess, is right for clicking and wrong for drawing: a guessed click presses the wrong control, where a guessed line lands in the wrong place and nothing happens. Refusing both is why a session asked to draw an octopus on 2026-09-07 sent a nine-point path, was refused, and told the user it could not draw at all.
func (a *Agent) toScreenForDraw(ctx context.Context, x, y int) (int, int, string) {
	c, ok := lookFrame(ctx)
	if !ok {
		return 0, 0, toolError(needLookFirst)
	}
	if !c.Holds(x, y) {
		return 0, 0, toolError(fmt.Sprintf("%d,%d is not inside the picture the last look took, which is %d wide and %d high, give me a point in it", x, y, c.W, c.H))
	}
	sx, sy := c.ToScreen(x, y)
	return sx, sy, ""
}

// lookFrame returns the newest picture this session took, whether or not the model was ever shown it. Output: the capture and true when a look has been taken, the zero value and false when none has.
// Only the frame is used — the size, the origin and the scale — so a session that cannot be shown images can still place a shape by the description it was given in words.
func lookFrame(ctx context.Context) (tracker.Capture, bool) {
	s := lookStateFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.look == nil {
		return tracker.Capture{}, false
	}
	return *s.look, true
}

// InputDevice is the keyboard and pointer press_key, click_at and scroll_at drive, and the one type_text falls back to when a field will not take text through the accessibility path: an *input.Session in production (see UsePortalInput), a fake in tests, which is the whole reason it is an interface — a test must record what was sent rather than press keys on the user's own screen.
type InputDevice interface {
	PressKey(name string) error
	TypeText(text string) error
	ClickAt(x, y float64) error
	RightClickAt(x, y float64) error
	ScrollAt(x, y float64, dy int32) error
}

// onceInput wraps an opener so the session behind it is opened at most once, on the first tool call that actually needs it. The portal puts a dialog on the user's screen asking them to allow remote control the first time a session opens, so opening at daemon start would ask someone who has not requested anything, and opening per call would ask again. Only a session that actually opened is remembered: the open blocks on that dialog, so an ask that gave up waiting for it must not cost this session its keyboard until the daemon restarts, and the next call asks again. The device handed back also reopens once, without a dialog thanks to the saved restore token, when the portal says the session it holds is gone (see sessionGone), and replays the action on the new one. Input: the opener. Output: an opener that hands back the one open session, retrying the open after every failure.
func onceInput(open func(context.Context) (InputDevice, error)) func(context.Context) (InputDevice, error) {
	r := &reopeningInput{open: open}
	return func(ctx context.Context) (InputDevice, error) {
		if _, err := r.device(ctx); err != nil {
			return nil, err
		}
		return &boundInput{r: r, ctx: ctx}, nil
	}
}

// sessionGone reports whether the portal answered with its stale-handle error. Mutter closes a RemoteDesktop session when the screencast stream behind it ends (a monitor unplugged or reconfigured, the screen locked, the machine asleep), and every call on the old handle fails this way until a new session is opened.
func sessionGone(err error) bool {
	if err == nil {
		return false
	}
	// "not allowed to call Notify..." is a session the portal opened without the keyboard or the pointer in it, which is what a consent dialog answered with nothing granted leaves behind (2026-09-08, after the saved token was lost); it never becomes useful, so it is dropped and the next press opens a new one, and the dialog, again.
	return strings.Contains(err.Error(), "Invalid session") || strings.Contains(err.Error(), "not allowed to call Notify")
}

// reopeningInput is the one cached session and the opener that made it. do runs one action on it and, when the portal says that session is gone, opens a new one and runs the action again.
type reopeningInput struct {
	open func(context.Context) (InputDevice, error)
	mu   sync.Mutex
	dev  InputDevice
}

// device hands back the cached session, opening one if there is none. Input: the context bounding the open. Output: the session, or the open's error, in which case nothing is cached and the next call opens again.
func (r *reopeningInput) device(ctx context.Context) (InputDevice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dev == nil {
		opened, err := r.open(ctx)
		if err != nil {
			return nil, err
		}
		r.dev = opened
	}
	return r.dev, nil
}

// do runs act on the cached session. If the portal answers that the session is gone, the cached one is forgotten, a new one opened under ctx, and act run once more on it; any other error comes back as is.
func (r *reopeningInput) do(ctx context.Context, act func(InputDevice) error) error {
	dev, err := r.device(ctx)
	if err != nil {
		return err
	}
	if err = act(dev); !sessionGone(err) {
		return err
	}
	slog.Info("the portal input session is gone, opening a new one")
	r.mu.Lock()
	if r.dev == dev {
		r.dev = nil
	}
	r.mu.Unlock()
	if dev, err = r.device(ctx); err != nil {
		return err
	}
	return act(dev)
}

// boundInput is the InputDevice a tool call drives: the shared reopening session, bound to that call's context so a reopen is bounded the same way the first open was.
type boundInput struct {
	r   *reopeningInput
	ctx context.Context
}

// keysByContext is an InputDevice whose typing and key presses stop between two keys once a context ends, as *input.Session's do on every platform. boundInput hands such a device the tool call's own context, so a job or an ask that is stopped stops typing within a character rather than after the whole text has gone out; a test's device has none, and types the whole text as it always did.
type keysByContext interface {
	PressKeyContext(ctx context.Context, name string) error
	TypeTextContext(ctx context.Context, text string) error
}

func (b *boundInput) PressKey(name string) error {
	return b.r.do(b.ctx, func(d InputDevice) error {
		if k, ok := d.(keysByContext); ok {
			return k.PressKeyContext(b.ctx, name)
		}
		return d.PressKey(name)
	})
}

func (b *boundInput) TypeText(text string) error {
	return b.r.do(b.ctx, func(d InputDevice) error {
		if k, ok := d.(keysByContext); ok {
			return k.TypeTextContext(b.ctx, text)
		}
		return d.TypeText(text)
	})
}

func (b *boundInput) ClickAt(x, y float64) error {
	return b.r.do(b.ctx, func(d InputDevice) error { return d.ClickAt(x, y) })
}

func (b *boundInput) RightClickAt(x, y float64) error {
	return b.r.do(b.ctx, func(d InputDevice) error { return d.RightClickAt(x, y) })
}

func (b *boundInput) ScrollAt(x, y float64, dy int32) error {
	return b.r.do(b.ctx, func(d InputDevice) error { return d.ScrollAt(x, y, dy) })
}

// WindowRaiser brings an already-running window of another application to the front, which switch_window tries before it falls back to driving the shell's own search from the keyboard: a *window.Raiser in production, talking to the bundled GNOME Shell extension over D-Bus (see internal/window), a fake in tests. Available says whether that extension is loaded and enabled in the running shell right now; List reports every open window with its pid, WM_CLASS, title and focus state; ByPid, ByTitle and ByWmClass each activate a window by one key and report whether they found and raised one.
type WindowRaiser interface {
	Available(ctx context.Context) (bool, error)
	List(ctx context.Context) ([]window.Window, error)
	ByPid(ctx context.Context, pid uint32) (bool, error)
	ByTitle(ctx context.Context, substring string) (bool, error)
	ByWmClass(ctx context.Context, wmClass string) (bool, error)
}

// windowByWindow is a WindowRaiser that can also activate the very window List reported, as *window.Raiser can. On Windows many windows share one pid — every Store app's frame belongs to ApplicationFrameHost, every File Explorer window to explorer.exe, every browser window to its browser — so raising a matched window by its pid brought whichever of that process's windows was nearest the front: "switch to Calculator" with Settings in front raised Settings again and reported it found.
type windowByWindow interface {
	ByWindow(ctx context.Context, w window.Window) (bool, error)
}

// raiseListed activates one window from the raiser's List: that very window when the raiser can do it, otherwise its process's by pid, which is all a test's raiser offers. Input: the call's context and the window. Output: whether it came to the front, and the raiser's error.
func (a *Agent) raiseListed(ctx context.Context, w window.Window) (bool, error) {
	if r, ok := a.raiser.(windowByWindow); ok {
		return r.ByWindow(ctx, w)
	}
	return a.raiser.ByPid(ctx, w.Pid)
}

// UseWindowRaiser gives this agent a way to raise another application's window through the bundled GNOME Shell extension. Input: the raiser, which switch_window asks first and falls back from when the extension is not installed, not enabled, or matched nothing.
func (a *Agent) UseWindowRaiser(r WindowRaiser) { a.raiser = r }

// UsePortalInput gives this agent a keyboard and pointer through the desktop portal, opened on the first tool call that needs one. Input: the directory the portal's restore token is kept in, so the user is asked to allow remote control once rather than on every restart.
func (a *Agent) UsePortalInput(dataDir string) {
	a.input = onceInput(func(ctx context.Context) (InputDevice, error) { return openPortalInput(ctx, dataDir) })
}

// tapLead is how long the real press waits after the overlay's tap is sent, so the pointer on screen is seen arriving before the click lands rather than after it. The overlay's flight is bounded to a few hundred milliseconds whatever the distance; a variable so tests cost no wall time.
var tapLead = 350 * time.Millisecond

// tapAt shows the press that is about to happen at a screen point, when this session can draw, and gives the overlay time to fly there. Input: the point in screen pixels and the label of what is there, "" when unknown. Output: none; a tap that reached no window is not an error, the press still goes ahead unseen.
func (a *Agent) tapAt(x, y int, label string) {
	if a.Tap == nil {
		return
	}
	if err := a.Tap(x, y, label); err != nil {
		return
	}
	time.Sleep(tapLead)
}

// inputDevice hands back this session's keyboard and pointer, opening it if this is the first call that needs it. Output: the device, or a tool error naming why there is none — nothing wired one up, or the portal refused, which is what a declined consent dialog looks like from here.
func (a *Agent) inputDevice(ctx context.Context) (InputDevice, string) {
	if a.input == nil {
		return nil, toolError("this session cannot reach the keyboard or the pointer")
	}
	dev, err := a.input(ctx)
	if err != nil {
		return nil, toolError("could not reach the keyboard and pointer: " + err.Error())
	}
	return dev, ""
}

// pressesFocused reports whether a key press acts on the control that has keyboard focus rather than moving around or editing text. Input: the key or chord press_key was given. Output: true for Enter or Return with any modifiers or none, and for a bare Space, all of which press what is focused and so go through the click's stop line; false for everything else.
// Enter carrying a modifier counts because Ctrl+Enter is Send in Slack, Teams and Gmail, which are the applications this engine is most likely to be driving; a chord this is wrong about costs a consent question, and one it misses sends a message nothing checked.
func pressesFocused(keys string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(keys)), "+")
	switch strings.TrimSpace(parts[len(parts)-1]) {
	case "enter", "return":
		return true
	case "space":
		return len(parts) == 1
	}
	return false
}

// movesFocus reports whether a key press moves keyboard focus off whatever the last click focused: Tab, Shift+Tab and the arrows, whatever modifiers they carry. Input: the key or chord press_key was given. Output: true for those keys, which leave this session unable to say what is focused, false for everything else.
func movesFocus(keys string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(keys)), "+")
	switch strings.TrimSpace(parts[len(parts)-1]) {
	case "tab", "up", "down", "left", "right":
		return true
	}
	return false
}

// picturePoint reads the x and y a click_at or scroll_at was given and turns them into the point on the screen they name. Input: the tool's arguments, whose x and y are coordinates in the picture the last look took. Output: the screen point, or a tool error when either is missing or no look stands behind them.
func (a *Agent) picturePoint(ctx context.Context, args map[string]any) (int, int, string) {
	x, xOK := args["x"].(float64)
	y, yOK := args["y"].(float64)
	if !xOK || !yOK {
		return 0, 0, toolError("this tool needs x and y, read off the picture the last look took")
	}
	return a.toScreen(ctx, int(x), int(y))
}

// frontWindow names the window in front right now, read fresh rather than from the last observe_screen answer, because a click at a coordinate is checked against the window's own title and looks and clicks can have moved the front window on since that list was made. Output: the "app · title" form currentWindow uses, falling back to the last observed window when the walk fails.
func (a *Agent) frontWindow(ctx context.Context) string {
	app, title, _, err := a.observe(ctx)
	if err != nil || (app == "" && title == "") {
		return a.currentWindow(ctx)
	}
	if title == "" {
		return app
	}
	return app + " · " + title
}

// seenItem resolves the "n" argument of point_at, click and scroll_to to the item observe_screen listed under that number. Input: the tool's arguments. Output: the item, or "" and a tool error when n is missing, nothing has been observed yet, or the number is not in the last list.
func (a *Agent) seenItem(ctx context.Context, args map[string]any) (act.Item, string) {
	n, ok := args["n"].(float64)
	if !ok {
		return act.Item{}, toolError("this tool needs n, the element's number from observe_screen")
	}
	items := a.seen(ctx)
	return itemByNumber(items, int(n))
}

// itemAt finds the item the last observe_screen listed whose rectangle covers a point on the screen, so a click at a bare coordinate can be checked against the control it actually lands on. Input: the listed items, whose rectangles are in screen pixels, and the screen point. Output: the smallest item covering the point and true, the smallest so a button wins over the panel it sits in, or the zero item and false when nothing listed covers it — a canvas, a video, or a control the accessibility walk never saw.
func itemAt(items []act.Item, x, y int) (act.Item, bool) {
	var found act.Item
	ok := false
	for _, it := range items {
		if x < it.X || y < it.Y || x >= it.X+it.W || y >= it.Y+it.H {
			continue
		}
		if !ok || it.W*it.H < found.W*found.H {
			found, ok = it, true
		}
	}
	return found, ok
}

// itemByNumber resolves n to the item observe_screen listed under that number, the lookup seenItem and draw's from/to both use. Input: the items from the latest observe_screen list (nil when observe_screen has not run) and the number named. Output: the item, or "" and a tool error when items is nil or n is not in the list.
func itemByNumber(items []act.Item, n int) (act.Item, string) {
	if items == nil {
		return act.Item{}, toolError("call observe_screen first, then name one of its numbers")
	}
	if n < 1 || n > len(items) {
		return act.Item{}, toolError(fmt.Sprintf("there is no element %d in the last observe_screen list", n))
	}
	return items[n-1], ""
}

// drawGroupSeq numbers draw calls so every shape of one drawing carries the same name. A counter rather than a clock: two calls in the same millisecond must not share a name, or the second drawing joins the first instead of replacing it.
var drawGroupSeq atomic.Uint64

// nextDrawGroup names one draw call. Output: a name no other call on this daemon will use.
func nextDrawGroup() string {
	return "d" + strconv.FormatUint(drawGroupSeq.Add(1), 10)
}

// MaxDrawShapes bounds one draw call. The list comes from the model and every entry is resolved against the screen and broadcast to the overlay on its own, so it is worth a ceiling; thirty-two is far above any real diagram.
// It is exported because it is also the size of the largest burst one action can put on the event stream, which is what a client's buffer has to be able to hold — see clientBufferSize in internal/ipc.
const MaxDrawShapes = 32

// maxDrawShapes is the unexported spelling the rest of this package already uses.
const maxDrawShapes = MaxDrawShapes

// drawShapeList returns the shapes one draw call is asking for. Input: the tool's arguments. Output: the entries of "shapes" when it holds at least one, otherwise the arguments themselves as a single shape, or a tool error when shapes holds more than maxDrawShapes or an entry that is not an object. A call carrying both forms is read as the batch, because refusing it would cost a round to say something the batch already answers.
func drawShapeList(args map[string]any) ([]map[string]any, string) {
	raw, ok := args["shapes"].([]any)
	if !ok || len(raw) == 0 {
		// shapes was sent but is unusable as a batch — empty, or not an array at all — and the fallback below would read args itself as the one shape it names, which is only right when args actually carries "shape". Otherwise the fallback ends up refusing over a field ("shape") the model never meant to send, rather than the one it did.
		if _, present := args["shapes"]; present {
			if _, hasShape := args["shape"]; !hasShape {
				return nil, toolError("shapes must be a non-empty array of shape objects")
			}
		}
		return []map[string]any{args}, ""
	}
	if len(raw) > maxDrawShapes {
		return nil, toolError(fmt.Sprintf("draw takes at most %d shapes in one call, got %d", maxDrawShapes, len(raw)))
	}
	shapes := make([]map[string]any, len(raw))
	for i, entry := range raw {
		shape, ok := entry.(map[string]any)
		if !ok {
			return nil, toolError(fmt.Sprintf("shapes[%d] must be an object carrying shape and its own points, rect, on or from/to", i))
		}
		shapes[i] = shape
	}
	return shapes, ""
}

// drawOne draws one shape and says what it drew. Input: the ask's context and one shape's arguments — shape, then from/to, points, on or rect as that shape needs, and an optional label. Output: the phrase naming what was drawn, for the caller to put after "drew "; the target to remember when the shape was drawn around a numbered element, nil for every other form since there is nothing to resolve a later "it" against; and a tool error when the shape cannot be drawn, in which case nothing was drawn.
func (a *Agent) drawOne(ctx context.Context, group string, args map[string]any) (string, *ScreenTarget, string) {
	shape, _ := args["shape"].(string)
	label, _ := args["label"].(string)
	switch shape {
	case "arrow", "line":
		points, errText := a.drawPoints(ctx, args, true, 2)
		if errText != "" {
			return "", nil, errText
		}
		if err := a.Draw(group, shape, points, 0, 0, 0, 0, label); err != nil {
			return "", nil, toolError(err.Error())
		}
		return shape + fmt.Sprintf(" through %d point(s)", len(points)) + labelNote(label), nil, ""
	case "path":
		points, errText := a.drawPoints(ctx, args, false, 3)
		if errText != "" {
			return "", nil, errText
		}
		if err := a.Draw(group, shape, points, 0, 0, 0, 0, label); err != nil {
			return "", nil, toolError(err.Error())
		}
		return fmt.Sprintf("a path through %d point(s)", len(points)) + labelNote(label), nil, ""
	case "box", "circle":
		x, y, w, h, it, errText := a.drawRect(ctx, args)
		if errText != "" {
			return "", nil, errText
		}
		if err := a.Draw(group, shape, nil, x, y, w, h, label); err != nil {
			return "", nil, toolError(err.Error())
		}
		// it is nil for the "rect" form (a point read off a picture, naming no observe_screen item), so there is nothing to remember or check a mismatch against — only the "on" form draws around a numbered item.
		if it == nil {
			return fmt.Sprintf("a %s around %d,%d %dx%d", shape, x, y, w, h) + labelNote(label), nil, ""
		}
		target := ScreenTarget{Label: it.Label, Role: it.Role, Window: a.currentWindow(ctx), HasRect: true, X: x, Y: y, W: w, H: h}
		return fmt.Sprintf("a %s around [%d] %s %q", shape, it.N, it.Role, it.Label) + labelNote(label), &target, ""
	case "":
		return "", nil, toolError("draw needs shape (arrow, line, path, box or circle), or shapes for several at once")
	default:
		return "", nil, toolError("draw needs shape to be arrow, line, path, box or circle")
	}
}

// drawPoints resolves the draw tool's arguments into the screen points to draw through, for shapes arrow, line and path. Input: the tool's arguments, carrying either "points" ([[x,y],...] read off the picture the last look took, at least min) or, when allowFromTo is set, "from" and "to" (element numbers from the latest observe_screen list, mapped to their rectangles' centres). Output: the points in screen coordinates, or nil and a tool error when points has fewer than min well-formed [x,y] entries, no look has been taken this turn, a point falls outside that picture, neither form is present or allowed, or an element number is not in the last list.
// The two forms are the only two ways a point can be known rather than guessed: an element the accessibility tree listed, or a place in a picture the model has actually seen. Inside a video, an image, a canvas or a game there are no elements, and on 2026-09-05 that is exactly where a run drew two labelled arrows at coordinates it had made up.
func (a *Agent) drawPoints(ctx context.Context, args map[string]any, allowFromTo bool, min int) ([][2]int, string) {
	if raw, ok := args["points"].([]any); ok {
		if len(raw) < min {
			return nil, toolError(fmt.Sprintf("draw needs at least %d points", min))
		}
		points := make([][2]int, len(raw))
		for i, p := range raw {
			pair, ok := p.([]any)
			if !ok || len(pair) != 2 {
				return nil, toolError("each point must be [x, y]")
			}
			x, xOK := pair[0].(float64)
			y, yOK := pair[1].(float64)
			if !xOK || !yOK {
				return nil, toolError("each point must be [x, y]")
			}
			sx, sy, errText := a.toScreenForDraw(ctx, int(x), int(y))
			if errText != "" {
				return nil, errText
			}
			points[i] = [2]int{sx, sy}
		}
		return points, ""
	}

	if !allowFromTo {
		return nil, toolError(fmt.Sprintf("draw needs points (at least %d)", min))
	}
	from, fromOK := args["from"].(float64)
	to, toOK := args["to"].(float64)
	if !fromOK || !toOK {
		return nil, toolError("draw needs from and to (element numbers from observe_screen), or points")
	}
	items := a.seen(ctx)
	fromItem, errText := itemByNumber(items, int(from))
	if errText != "" {
		return nil, errText
	}
	toItem, errText := itemByNumber(items, int(to))
	if errText != "" {
		return nil, errText
	}
	fromPoint, errText := a.livePoint(ctx, fromItem)
	if errText != "" {
		return nil, errText
	}
	toPoint, errText := a.livePoint(ctx, toItem)
	if errText != "" {
		return nil, errText
	}
	return [][2]int{fromPoint, toPoint}, ""
}

// livePoint returns the centre of an element as it is on the screen right now, after checking the number still points at the element observe_screen listed. Input: a context and the listed item. Output: the point to draw through, or the tool error to hand back when the element has moved out from under its number or cannot be read, in which case nothing is drawn — the same rule point_at follows, because a line drawn to where an element used to be points at whatever has taken its place.
func (a *Agent) livePoint(ctx context.Context, it act.Item) ([2]int, string) {
	it, errText := a.stillThere(ctx, it)
	if errText != "" {
		return [2]int{}, errText
	}
	x, y, w, h, errText := a.freshRect(ctx, it)
	if errText != "" {
		return [2]int{}, errText
	}
	return [2]int{x + w/2, y + h/2}, ""
}

// drawRect resolves the draw tool's arguments into the rectangle to draw around, for shapes box and circle. Input: the tool's arguments, carrying either "on" (an element number from the latest observe_screen list) or "rect" ({"x":,"y":,"w":,"h":} read off the picture the last look took). Output: the rectangle in screen pixels, the item it was resolved from when "on" named one (nil for the "rect" form, which names no item), or all zero and a tool error when neither is present, the element number is not in the last list, rect is missing a numeric field, no look has been taken this turn, or the rectangle falls outside that picture.
func (a *Agent) drawRect(ctx context.Context, args map[string]any) (x, y, w, h int, it *act.Item, errText string) {
	if on, ok := args["on"].(float64); ok {
		items := a.seen(ctx)
		resolved, errText := itemByNumber(items, int(on))
		if errText != "" {
			return 0, 0, 0, 0, nil, errText
		}
		resolved, errText = a.stillThere(ctx, resolved)
		if errText != "" {
			return 0, 0, 0, 0, nil, errText
		}
		x, y, w, h, errText := a.freshRect(ctx, resolved)
		return x, y, w, h, &resolved, errText
	}
	rect, ok := args["rect"].(map[string]any)
	if !ok {
		return 0, 0, 0, 0, nil, toolError(`box and circle say where with rect {"x":..,"y":..,"w":..,"h":..} in the last look's coordinates, or with on (an element number from observe_screen); points is only for arrow, line and path`)
	}
	rx, xOK := rect["x"].(float64)
	ry, yOK := rect["y"].(float64)
	rw, wOK := rect["w"].(float64)
	rh, hOK := rect["h"].(float64)
	if !xOK || !yOK || !wOK || !hOK {
		return 0, 0, 0, 0, nil, toolError("rect needs x, y, w and h")
	}
	farX, farY, errText := a.toScreenForDraw(ctx, int(rx)+int(rw), int(ry)+int(rh))
	if errText != "" {
		return 0, 0, 0, 0, nil, errText
	}
	// Both corners are mapped, so a rectangle that starts inside the picture and runs off its edge is refused rather than drawn half over something the model never saw.
	x, y, errText = a.toScreenForDraw(ctx, int(rx), int(ry))
	if errText != "" {
		return 0, 0, 0, 0, nil, errText
	}
	return x, y, farX - x, farY - y, nil, ""
}

// labelNote formats the label suffix a draw result line carries, or "" when the model gave no label.
func labelNote(label string) string {
	if label == "" {
		return ""
	}
	return fmt.Sprintf(" labelled %q", label)
}

// stopBeforeClick refuses to click an element that trips the stop-line rule (irreversible) without the user's explicit go-ahead for this exact step. It rings the element first when this session can draw, so the user sees exactly what would have been clicked before being asked to say go — and the ring goes around where the element is now, since that ring is the whole of what the user is answering. Input: a context, the observed item that tripped irreversible, and the window it sits in. Output: a result beginning "Stopped before " naming the control and window plus the one-line question to unlock it; when the element cannot be read the refusal is replaced by the error telling the model to look again, and either way the click does not happen.
func (a *Agent) stopBeforeClick(ctx context.Context, it act.Item, window string) string {
	unseen := ""
	if a.Point != nil {
		x, y, w, h, errText := a.freshRect(ctx, it)
		if errText != "" {
			return errText
		}
		if err := a.Point(x, y, w, h, it.Label); err != nil {
			// The refusal stands either way — the click is what matters — but the question below asks the user to look at a ring, so say when there was none to look at.
			unseen = " I could not ring it: " + err.Error() + "."
		}
	}
	return fmt.Sprintf("Stopped before clicking [%d] %s %q in %q.%s %s", it.N, it.Role, it.Label, window, unseen, consentPrompt(matchedVerb(it, window)))
}

// stillThere runs the staleness check on an element the model named by number, before anything is drawn around it or done to it. Input: a context and the item observe_screen listed. Output: the item to act on, and "" when the element is still what the list showed, or the tool error to hand back when it is not.
// An element that is only renamed comes back with its new name rather than a refusal. A toggle renames itself the moment it is pressed — Play becomes Pause, Mute becomes Unmute — so the second press on the same number was refused for the element having done exactly what the first press asked. The log has the model spending three rounds and a full re-listing on one of these to press a button whose reference never changed. The name still matters, because the stop line is judged on it, so it is the new name the guards downstream see.
func (a *Agent) stillThere(ctx context.Context, it act.Item) (act.Item, string) {
	if a.verify == nil {
		return it, ""
	}
	err := a.verify(ctx, it.Ref, it.Role, it.Label, it.X, it.Y, it.W, it.H)
	if err == nil {
		return it, ""
	}
	var renamed *tracker.Relabelled
	if errors.As(err, &renamed) {
		it.Label = renamed.Now
		return it, ""
	}
	return it, toolError(fmt.Sprintf("[%d] %s %q is not what observe_screen listed there, look again: %v", it.N, it.Role, it.Label, err))
}

// freshRect reads where an element is on the screen right now, so a ring goes around the element rather than around the rectangle it occupied when observe_screen made its list; the accessibility reference stays valid while the page scrolls under it, so the remembered rectangle can by now be over something else entirely. Input: a context and the item observe_screen listed. Output: the element's current rectangle, or an empty rectangle and the tool error to hand back when it cannot be read or has no size on screen, in which case nothing is drawn at all — a ring in the wrong place is what the user reads before saying go.
func (a *Agent) freshRect(ctx context.Context, it act.Item) (x, y, w, h int, errText string) {
	if a.extents == nil {
		return it.X, it.Y, it.W, it.H, ""
	}
	x, y, w, h, err := a.extents(ctx, it.Ref)
	if err != nil {
		return 0, 0, 0, 0, toolError(fmt.Sprintf("could not read where [%d] %s %q is on the screen, so I won't ring it; look again with observe_screen: %v", it.N, it.Role, it.Label, err))
	}
	if w <= 0 || h <= 0 {
		return 0, 0, 0, 0, toolError(fmt.Sprintf("[%d] %s %q is not showing on the screen now, so there is nothing to ring; look again with observe_screen", it.N, it.Role, it.Label))
	}
	return x, y, w, h, ""
}

// mentionsWindow reports whether question names the given window, so a click may follow the user across a window switch they actually asked for rather than one that just happened to grab focus. Input: the ask's own question text, and the front window's app and title. Output: true when the question contains the app name, or a title word of at least four letters, case-insensitively; false for an empty question, which never names anything.
func mentionsWindow(question, app, title string) bool {
	q := strings.ToLower(question)
	if q == "" {
		return false
	}
	if app != "" && strings.Contains(q, strings.ToLower(app)) {
		return true
	}
	return mentionsTitle(q, title, "")
}

// mentionsTitle reports whether question names a window by its title: a word of the title of at least four letters is in it, case-insensitively, leaving out the words the other title has too. Input: the question, the window's title, and the title of the window it has to be told from ("" for none). Output: true when such a word is in the question.
// The other title's words are left out because a word two windows share names neither of them: a second Notepad window is "notes.txt - Notepad" beside "Untitled - Notepad", and "type it in notepad" names no one of the two.
func mentionsTitle(question, title, other string) bool {
	q := strings.ToLower(question)
	if q == "" {
		return false
	}
	trim := func(word string) string { return strings.ToLower(strings.Trim(word, ".,:;·-\"'")) }
	shared := map[string]bool{}
	for _, word := range strings.Fields(other) {
		shared[trim(word)] = true
	}
	for _, word := range strings.Fields(title) {
		word = trim(word)
		if len(word) >= 4 && !shared[word] && strings.Contains(q, word) {
			return true
		}
	}
	return false
}

// frontWindowNow reads the window in front right now in the "app · title" form currentWindow uses, for a check that has to be live rather than read off the last screen listing. Output: "" when the front cannot be read.
func (a *Agent) frontWindowNow(ctx context.Context) string {
	app, title, _, err := a.observe(ctx)
	if err != nil || app == "" {
		return ""
	}
	if title == "" {
		return app
	}
	return app + " · " + title
}

// How the window switch is paced. switchVerifyFor is how long the front window is watched for the raise to show up in the accessibility tree, which lags the shell extension that did the raising. raiserTimeout bounds the extension's three D-Bus calls on their own, because they run inside gnome-shell and a busy shell must cost this tool two seconds rather than the whole turn. Both are variables so a test can set them to zero and cost no wall time.
var (
	switchVerifyFor = 2 * time.Second
	raiserTimeout   = 2 * time.Second
)

// windowSep is what currentWindow puts between a window's application and its title, and what namesApp splits on to match one side without the other.
const windowSep = " · "

// appWords splits text into its lowercase words, breaking on everything that is not a letter or a digit, so "brave-browser" is two words and a name can be matched a whole word at a time. Input: any text. Output: its words, lowercased.
func appWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// namesApp reports whether a piece of text names an application, case-insensitively and by whole words. Input: the text — a request, or a window read as "app · title" — and the application name, which may itself be several words. Output: false for an empty name, else whether the name's words appear in a row in the text, and in a window's reading in its application or in its title but never across the two.
// Whole words rather than any substring, and the two halves apart rather than blended: "gmail · Inbox" does not name Mail, and "Barcode Scanner" does not name Code, both of which used to read as the asked-for window already being in front.
func namesApp(text, app string) bool {
	want := appWords(app)
	if len(want) == 0 {
		return false
	}
	for _, part := range strings.Split(text, windowSep) {
		have := appWords(part)
		for i := 0; i+len(want) <= len(have); i++ {
			if slices.Equal(have[i:i+len(want)], want) {
				return true
			}
		}
	}
	return false
}

// frontIsApp reports whether a window reading of the form "app · title" is a window of the named application, judged on the application half alone. The title half is the document or the page, which can be named after anything at all: a Chromium window showing the Spotify web player reads as "Chromium · Spotify Premium", and reading that as Spotify is what made open_app report Spotify as already in front and start nothing. Input: the window reading and the application name. Output: whether the application half names it.
func frontIsApp(front, app string) bool {
	return namesApp(frontApp(front), app)
}

// frameHost is the program that owns the window of every Store app on Windows (Calculator, Settings, Photos), which runs in a process of its own behind it. The tracker reads such a window under the app's own executable, "CalculatorApp · Calculator" or "SystemSettings · Settings", and only while the app is suspended or resuming under the frame's, "ApplicationFrameHost · Calculator"; judged on either half no Store app was ever in front, so open_app reported a Calculator it had just raised as "not Calculator".
const frameHost = "applicationframehost"

// frontApp is the application half of a window reading, or for a Store app's window the title half, which is what such a window is titled with: the Store app's own name. Which executables are Store apps is the tracker's to say (tracker.IsStoreApp), since it is the one that looked inside the frame, so this does not hang on how it names them. Input: a reading of the form "app · title". Output: the half that names the application.
func frontApp(front string) string {
	appHalf, title, _ := strings.Cut(front, windowSep)
	if (strings.EqualFold(appHalf, frameHost) || tracker.IsStoreApp(appHalf)) && title != "" {
		return title
	}
	return appHalf
}

// raiseWindow asks the GNOME Shell extension to bring the application's window forward. The window List reports is the most exact key there is, so List is read first and any open window whose class or title already names the app is raised itself (by its pid where the raiser can do no better, see raiseListed); failing that (no extension, nothing in the list matched, or the pid activation itself did not land) it falls back to the looser text matches the extension does itself, WM_CLASS before title since a title is often a document name and not the app. Input: a context bounding the D-Bus calls and the application name. Output: true and the key that found it ("pid 1234", `wm_class "brave-browser"` or `title "Brave"`) when a window was raised; false and "" when there is no extension wired up, it is not loaded in the running shell, it matched nothing, or every call failed — every one of which means the keyboard path is what is left.
func (a *Agent) raiseWindow(ctx context.Context, app string) (bool, string) {
	if a.raiser == nil {
		return false, ""
	}
	// The extension answers from inside gnome-shell, so these calls get a bound of their own rather than the whole turn's: a shell busy redrawing costs this tool a couple of seconds and then the keyboard path, not the ask.
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	if ok, err := a.raiser.Available(ctx); err != nil || !ok {
		return false, ""
	}
	if windows, err := a.raiser.List(ctx); err == nil {
		// Two passes, WM_CLASS before title, because a window's class is its application and its title is only what it happens to be showing. One pass over both let a Chromium window titled "Spotify Premium" be raised as Spotify while the real Spotify window sat further down the list.
		for _, byClass := range []bool{true, false} {
			for _, w := range windows {
				if byClass != namesApp(w.WmClass, app) {
					continue
				}
				if !byClass && !namesApp(w.Title, app) {
					continue
				}
				if ok, err := a.raiseListed(ctx, w); err == nil && ok {
					return true, fmt.Sprintf("pid %d", w.Pid)
				}
			}
		}
	}
	if ok, err := a.raiser.ByWmClass(ctx, app); err == nil && ok {
		return true, fmt.Sprintf("wm_class %q", app)
	}
	if ok, err := a.raiser.ByTitle(ctx, app); err == nil && ok {
		return true, fmt.Sprintf("title %q", app)
	}
	return false, ""
}

// browserFromProgID turns the ProgId Windows records for the default browser into the browser's executable stem, which is what raiseBrowser matches against a window's class there. Input: a ProgId such as "BraveHTML", "MSEdgeHTM" or "FirefoxURL-308046B0AF4A39CB". Output: the lowercased stem ("brave", "msedge", "firefox"), or the lowercased ProgId up to its first dot or dash when it carries none of the usual suffixes.
func browserFromProgID(progID string) string {
	id := strings.ToLower(progID)
	if i := strings.IndexAny(id, ".-"); i >= 0 {
		id = id[:i]
	}
	for _, suffix := range []string{"bhtml", "html", "htm", "url", "stable"} {
		if stem, ok := strings.CutSuffix(id, suffix); ok && stem != "" {
			return stem
		}
	}
	return id
}

// raiseBrowser brings the default browser's window to the front through the shell extension, after open_url has put a page in it: xdg-open hands the browser no activation token, so the shell keeps the new tab behind whatever was in front, and observe_screen then reads the old window and the model opens the page again. The browser's window is the one whose class shares a word with the browser's desktop id, which is how brave_brave.desktop finds wm_class brave-browser and org.mozilla.firefox.desktop finds firefox without a table of browsers. Input: a context bounding the D-Bus calls. Output: how it was raised ("pid 1234"), or "" when there is no extension, no known browser, no window of it open yet, or the raise did not land.
func (a *Agent) raiseBrowser(ctx context.Context) string {
	if a.raiser == nil {
		return ""
	}
	// Windows can name the browser two ways, its ProgId's browser and then its handler's executable (see defaultBrowserID there), and they are tried in that order, so a launcher's stem is only ever matched once the browser's own name has matched nothing.
	ids := strings.Fields(defaultBrowserID())
	if len(ids) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, raiserTimeout)
	defer cancel()
	if ok, err := a.raiser.Available(ctx); err != nil || !ok {
		return ""
	}
	windows, err := a.raiser.List(ctx)
	if err != nil {
		return ""
	}
	for _, id := range ids {
		idWords := appWords(strings.TrimSuffix(id, ".desktop"))
		for _, w := range windows {
			classWords := appWords(w.WmClass)
			if !slices.ContainsFunc(classWords, func(word string) bool { return len(word) > 2 && slices.Contains(idWords, word) }) {
				continue
			}
			if ok, err := a.raiseListed(ctx, w); err == nil && ok {
				return fmt.Sprintf("pid %d", w.Pid)
			}
		}
	}
	return ""
}

// switchOutcome reads which window is in front after a switch was attempted and says what happened in the words the model reads back. Input: a context, the application asked for, the window that was in front before, the key that raised it ("pid 1234" and so on, from raiseWindow) or "" when the keyboard did the switching, and the keyboard the search was typed on — nil when the extension did the raising and no search was ever opened. Output: the switch's result line, or a tool error when the front window cannot be read at all.
// A switch that did not land can leave the shell's search sitting over everything, so it is closed again with one Escape — but only when a live read says it is still up, since the same key sent at the user's own window discards whatever was in it. Whether that press lands changes nothing about the report, which is about the switch.
func (a *Agent) switchOutcome(ctx context.Context, app, before, how string, dev InputDevice) string {
	after := a.frontWindowAfterSwitch(ctx, app)
	if frontIsApp(after, app) {
		if how != "" {
			return fmt.Sprintf("switched to %q, raised by %s; call observe_screen to see it", after, how)
		}
		return fmt.Sprintf("switched to %q; call observe_screen to see it", after)
	}
	switch {
	case after == "":
		return toolError("I could not read which window is in front after that, so I cannot say whether the switch worked")
	case after == before:
		return fmt.Sprintf("the front window is still %q; nothing called %q came forward", before, app)
	default:
		return fmt.Sprintf("the front window is now %q, not %q", after, app)
	}
}

// frontWindowAfterSwitch polls the window in front until it names the application asked for or switchVerifyFor runs out, since the shell takes a moment to raise a window after Enter. Input: a context and the application name. Output: the window in front in the "app · title" form, which is the last reading taken whether it matched or not.
func (a *Agent) frontWindowAfterSwitch(ctx context.Context, app string) string {
	deadline := time.Now().Add(switchVerifyFor)
	for {
		front := a.frontWindowNow(ctx)
		if frontIsApp(front, app) || !time.Now().Before(deadline) {
			return front
		}
		select {
		case <-time.After(waitPollEvery):
		case <-ctx.Done():
			return front
		}
	}
}

// frontWindowChanged refuses a click that would land in whatever grabbed focus since observe_screen built the list a number is being resolved against — a popup, a desktop overview, another application — unless the question that started this turn actually named where it should act (see WithQuestion, mentionsWindow). A title change alone in the same application does not count: measured 2026-09-09 03:19, a YouTube tab's title grew "- Audio playing" the moment Play was pressed, and the same click was refused though the same page and button were still there; that case is left for stillThere to verify by role, label and rectangle. Input: a context carrying the ask's own question. Output: "" when nothing has been observed yet (seenItem already refuses that case), the same application is still in front, or the question names the window now in front; otherwise the tool error to hand back, naming both windows.
func (a *Agent) frontWindowChanged(ctx context.Context) string {
	last := a.lastScreen(ctx)
	if last.app == "" && last.title == "" {
		return ""
	}
	app, title, _, err := a.observe(ctx)
	if err != nil {
		// The real error, if there is one, surfaces from stillThere or the click itself right after this.
		return ""
	}
	if app == last.app {
		return ""
	}
	if mentionsWindow(questionFrom(ctx), app, title) {
		return ""
	}
	front, from := app, last.app
	if title != "" {
		front = app + " · " + title
	}
	if last.title != "" {
		from = last.app + " · " + last.title
	}
	return toolError(fmt.Sprintf("the window in front is now %q, not %q where observe_screen listed this element, call observe_screen again, or say which window to use", front, from))
}
