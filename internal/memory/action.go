// Package memory — action.go is the action item: one thing somebody agreed to do, lifted out of a meeting's minutes so it can carry a status and a priority of its own. Minutes are a frozen record of what was said and must never be edited to say a task is finished; an action item is the live half, stored as its own notes row under kind 'action' and closed or re-prioritised by the user whenever they say so.
package memory

import (
	"slices"
	"strings"
	"time"
)

// The statuses an action item moves through. Open items are what the morning brief draws on; done and dropped are both closed, and differ only in whether the work happened.
const (
	StatusOpen    = "open"
	StatusDone    = "done"
	StatusDropped = "dropped"
)

// The priorities the user can set on an action item. Everything starts normal: the minutes say who agreed to what, never how much it matters, so a priority the user did not choose is a guess the brief would then present as fact.
const (
	PriorityHigh   = "high"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// ActionNoteKind is the notes.kind an action item is stored under, keeping owed work distinguishable from the memory compiler's facts and from meeting minutes.
const ActionNoteKind = "action"

// UnknownOwner is who an action item belongs to when the minutes never named anybody, matching the "owner unclear" the minutes prompt asks the model to write.
const UnknownOwner = "Owner unclear"

// maxOwnerWords and maxOwnerLen are how long the text before the dash may be and still be a person's name. A name is one to four words; anything longer is a sentence that happens to contain a dash, and filing it as a person would have the brief read a sentence out as if it were somebody who owed work.
const (
	maxOwnerWords = 4
	maxOwnerLen   = 40
)

// actionSection is the minutes heading whose bullets are action items, matching the "## Action items" section the recorder's minutes prompt asks the model to write.
const actionSection = "## Action items"

// ActionItem is one owed piece of work. Owner and Text come from the minutes and do not change; Status and Priority are the user's to set. Source names the meeting it was agreed in and Raised is when that meeting happened, which is what lets the brief ask about an item that has been sitting open for a while.
type ActionItem struct {
	// NoteID is the notes row this item is stored as, set when the store reads it back and zero for one just parsed out of minutes. It is what the user's corrections address.
	NoteID   int64
	Owner    string
	Text     string
	Status   string
	Priority string
	Source   string
	Raised   time.Time
}

// looksLikeOwner reports whether s is short enough to be somebody's name rather than the first half of a sentence.
func looksLikeOwner(s string) bool {
	return len(s) <= maxOwnerLen && len(strings.Fields(s)) <= maxOwnerWords
}

// ParseMinutesActions pulls the action items out of one meeting's minutes. Input: the minutes markdown, the meeting's name, and when it happened. Output: one ActionItem per bullet under the "## Action items" heading, every one open at normal priority — scanning stops at the next heading, so a later section's bullets are never mistaken for owed work. Minutes with no such section yield none.
func ParseMinutesActions(minutes, source string, raised time.Time) []ActionItem {
	var out []ActionItem
	inSection := false
	for _, line := range strings.Split(minutes, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inSection = strings.EqualFold(trimmed, actionSection)
			continue
		}
		if !inSection || !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		owner, text, ok := splitBullet(strings.TrimPrefix(trimmed, "- "))
		if !ok {
			continue
		}
		out = append(out, ActionItem{
			Owner:    owner,
			Text:     text,
			Status:   StatusOpen,
			Priority: PriorityNormal,
			Source:   source,
			Raised:   raised,
		})
	}
	return out
}

// splitBullet separates one action bullet into its owner and the work owed. Input: the bullet's text with its "- " marker already stripped, in the "**Owner** — what they agreed to do" shape the minutes prompt asks for. Output: the owner with its bold markers removed, the work, and false for a bullet carrying no owner separator at all.
func splitBullet(bullet string) (owner, text string, ok bool) {
	// The em dash is what the minutes prompt asks for and a colon is what models write anyway — both of today's recordings came back as "Alex: review his code", which parsed as nothing at all. Either separator is accepted, the em dash first so a bullet carrying both splits where it was told to.
	owner, text, ok = strings.Cut(bullet, "—")
	if !ok {
		owner, text, ok = strings.Cut(bullet, ":")
	}
	if !ok {
		return "", "", false
	}
	owner = strings.TrimSpace(strings.Trim(strings.TrimSpace(owner), "*"))
	text = strings.TrimSpace(text)
	if owner == "" || text == "" {
		return "", "", false
	}
	if !looksLikeOwner(owner) {
		// The dash separated two halves of one sentence rather than a name from the work, so the whole bullet is the work and nobody was named.
		return UnknownOwner, strings.TrimSpace(bullet), true
	}
	return owner, text, true
}

// raisedFormat is how an action note carries the date its meeting happened — the same local calendar-day key the diary and the dreaming loop use.
const raisedFormat = "2006-01-02"

// Note renders the action item as the single line stored in the notes table under kind 'action'. The status and priority lead so the state is readable at a glance and cheap to parse back; the owner and work read as an ordinary sentence, and the meeting and date close it so the brief can say where an item came from and how long it has been sitting. One line, because the FTS mirror and the vector index both store a note as one blob.
func (a ActionItem) Note() string {
	line := "[" + a.Status + "/" + a.Priority + "] " + a.Owner + " — " + a.Text
	if a.Raised.IsZero() {
		return line
	}
	if a.Source == "" {
		return line + " (" + a.Raised.Format(raisedFormat) + ")"
	}
	return line + " (" + a.Source + ", " + a.Raised.Format(raisedFormat) + ")"
}

// ParseAction reads one action note back into an ActionItem. Input: a note's content. Output: the item and true, or false for any content that is not an action note — an ordinary fact note, or a line whose status and priority prefix is missing or unrecognised. The work text may itself contain brackets, dashes and brackets of its own: only the leading prefix and the trailing "(source, date)" are treated as structure.
func ParseAction(content string) (ActionItem, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(content), "[")
	if !ok {
		return ActionItem{}, false
	}
	tags, rest, ok := strings.Cut(rest, "]")
	if !ok {
		return ActionItem{}, false
	}
	status, priority, ok := strings.Cut(tags, "/")
	if !ok || !ValidStatus(status) || !ValidPriority(priority) {
		return ActionItem{}, false
	}

	a := ActionItem{Status: status, Priority: priority}
	rest = strings.TrimSpace(rest)
	// The provenance suffix is the last parenthesised group at the very end, so work text carrying its own brackets survives.
	if strings.HasSuffix(rest, ")") {
		if i := strings.LastIndex(rest, " ("); i >= 0 {
			// Two shapes: "(meeting, date)" when the minutes named the meeting, "(date)" when they did not.
			inner := strings.TrimSuffix(rest[i+2:], ")")
			source, raised, found := strings.Cut(inner, ", ")
			if !found {
				source, raised = "", inner
			}
			if when, err := time.Parse(raisedFormat, raised); err == nil {
				a.Source, a.Raised, rest = source, when, strings.TrimSpace(rest[:i])
			}
		}
	}

	owner, text, ok := splitBullet(rest)
	if !ok {
		return ActionItem{}, false
	}
	a.Owner, a.Text = owner, text
	return a, true
}

// ValidStatus and ValidPriority keep a mistyped or model-invented tag from being stored as if it were a real state.
func ValidStatus(s string) bool {
	return s == StatusOpen || s == StatusDone || s == StatusDropped
}

func ValidPriority(p string) bool {
	return p == PriorityHigh || p == PriorityNormal || p == PriorityLow
}

// MinutesLabel names the meeting a set of minutes covers, for the provenance an action item carries. Input: the minutes markdown. Output: the meeting's name, or "" when the minutes open straight into their sections without a title line. It reads the first ordinary line before any section heading — the minutes prompt puts the meeting's name and time there — and keeps the part before the dash or comma that separates the name from when it happened.
func MinutesLabel(minutes string) string {
	for _, line := range strings.Split(minutes, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "##") {
				return "" // a section started, so there was no title line
			}
			continue
		}
		line = strings.TrimSpace(strings.Trim(line, "*"))
		if name, _, ok := strings.Cut(line, "—"); ok {
			line = name
		} else if name, _, ok := strings.Cut(line, ","); ok {
			line = name
		}
		return strings.TrimSpace(line)
	}
	return ""
}

// OwnedByUser reports whether owner names the user. Input: an action item's owner as the minutes wrote it, and the personal-context identity entry, which names the user and every form they are written down as ("Alex Rivera — goes by Alex") and names nobody else. Output: whether the two refer to the same person. Matching is on whole words, case-insensitively, so a shorter form of the user's name counts and a fragment of it does not. With no identity on file nothing is attributed, since guessing here would put another person's work on the user's list.
func OwnedByUser(owner, identity string) bool {
	owner = strings.TrimSpace(owner)
	if owner == "" || identity == "" || owner == UnknownOwner {
		return false
	}
	words := strings.Fields(strings.ToLower(identity))
	for i := range words {
		words[i] = strings.Trim(words[i], ".,;:—-")
	}
	matched := 0
	for _, part := range strings.Fields(strings.ToLower(owner)) {
		part = strings.Trim(part, ".,;:—-()[]")
		// A parenthetical qualifier is not part of anybody's name. The minutes prompt writes the recording person as "Their Name (recording)", and treating "(recording)" as a name that must appear in the identity entry made the user's own work look like somebody else's.
		if part == "" || part == "recording" {
			continue
		}
		if !slices.Contains(words, part) {
			return false
		}
		matched++
	}
	return matched > 0
}

// UserMeetingActions keeps a meeting's action items only when at least one of them is the user's. A meeting the user owes nothing in was not really their meeting — they sat in on it — and its items are somebody else's business. When they do owe something, every item is kept, including the ones other people took on: work the user is waiting for is work they care about, which is the difference between sitting in on a call and being in it. The minutes themselves always keep the full record either way.
func UserMeetingActions(items []ActionItem, identity string) []ActionItem {
	for _, a := range items {
		if OwnedByUser(a.Owner, identity) {
			return items
		}
	}
	return nil
}
