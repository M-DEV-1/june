// Package memory — action.go is the action item: one thing somebody agreed to do, lifted out of a meeting's minutes so it can carry a status and a priority of its own. Minutes are a frozen record of what was said and must never be edited to say a task is finished; an action item is the live half, stored as its own notes row under kind 'action' and closed or re-prioritised by the user whenever they say so.
package memory

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
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

// MeOwner is who an action item belongs to when the minutes prompt identifies it as the [me] speaker's own, matching the "Me" the minutes prompt asks the model to write for the user's own bullets.
const MeOwner = "Me"

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
	// DoneSource names the evidence that closed this item — the meeting, day or note whose text said the work had happened. Empty for an item the user closed themselves and for one still open.
	DoneSource string
	// Created is when the notes row was written, set when the store reads the item back and zero for one just parsed out of minutes. It is the date the window shows, since an item parsed out of minutes has a raised date only when the minutes named one.
	Created time.Time
	// OwnerOverride is the class the user picked by hand on the Tasks screen, overriding whatever OwnerClass would otherwise read from Owner and Text. Empty when nobody has corrected it, set when the store reads the item back from its owner_class column.
	OwnerOverride string
}

// The three answers to "whose task is this?". They are worked out from the owner the minutes named and from who the user is, not stored: every action row already carries an owner name, so classifying it on read means old rows and new ones are read the same way and nothing has to be rewritten.
const (
	OwnerMe      = "me"
	OwnerThem    = "them"
	OwnerUnclear = "unclear"
)

// selfOwners are the ways an item's owner can say "the person recording" without naming them. The minutes prompt asks for "Me"; models write "You" or "I" anyway when they slip into addressing the reader.
var selfOwners = []string{"me", "you", "i", "i'll", "myself"}

// OwnerClass says whose task this is. Input: the personal-context identity entry, which names the user and every form they are written down as ("Alex Rivera — goes by Alex") and names nobody else. Output: "me" when the owner is the user, "them" when it is somebody else, "unclear" when nobody can be made out.
// An item the minutes left unowned is read a second time, from its own text: a name in front of "will", "to" or "should", or a "Person — work" prefix the bullet parser could not split, all name somebody who is not the user. With no identity on file the user's own name reads as somebody else's, which is deliberate — guessing here would put another person's work on his list.
// A class the user picked by hand always wins: hearing about a thing in a meeting does not make it his, and the user is the one person who actually knows whose work it is.
func (a ActionItem) OwnerClass(identity string) string {
	if a.OwnerOverride != "" {
		return a.OwnerOverride
	}
	owner := strings.TrimSpace(a.Owner)
	if slices.Contains(selfOwners, strings.ToLower(owner)) || OwnedByUser(owner, identity) {
		return OwnerMe
	}
	unowned, qualified := unownedOwner(owner)
	if !unowned {
		return OwnerThem
	}
	if qualified || namesSomebodyElse(a.Text) {
		// "Owner unclear (workstream lead)" says the work went to a side of the call, which is somebody, just not a named one.
		return OwnerThem
	}
	return OwnerUnclear
}

// unownedOwner reads an owner that names nobody. Input: the owner as the minutes wrote it. Output: whether it is one of those ("Owner unclear", or blank), and whether it carries a qualifier saying who it went to anyway ("Owner unclear (workstream lead)").
func unownedOwner(owner string) (unowned, qualified bool) {
	if owner == "" {
		return true, false
	}
	rest, ok := strings.CutPrefix(strings.ToLower(owner), strings.ToLower(UnknownOwner))
	if !ok {
		return false, false
	}
	return true, strings.TrimSpace(rest) != ""
}

// subjectVerbs are the words a name sits in front of when a sentence says who owes the work. Only "will" and "should" count at the start of the sentence: a sentence's first word is capitalised whether or not it is a name, and "Continue to research ..." would otherwise read as somebody called Continue.
var (
	subjectVerbs   = []string{"will", "to", "should"}
	firstWordVerbs = []string{"will", "should"}
)

// namesSomebodyElse reports whether an unowned item's own text names the person who owes it. Input: the work text. Output: true for a capitalised name in front of "will", "to" or "should", or a bolded "Person — work" prefix the bullet parser left in place because it was too long to read as a name. The first word is skipped: a sentence starts with a capital whether or not it is a name.
func namesSomebodyElse(text string) bool {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(text, "**"); ok {
		if prefix, _, found := strings.Cut(rest, "**"); found && strings.TrimSpace(prefix) != "" {
			return true
		}
	}
	words := strings.Fields(text)
	for i := 0; i < len(words)-1; i++ {
		word := strings.Trim(words[i], ".,;:—-()[]*`\"'")
		if !capitalisedName(word) {
			continue
		}
		verbs := subjectVerbs
		if i == 0 {
			verbs = firstWordVerbs
		}
		if slices.Contains(verbs, strings.ToLower(strings.Trim(words[i+1], ".,;:—-()[]*`"))) {
			return true
		}
	}
	return false
}

// capitalisedName reports whether a word could be somebody's name: at least two letters, all of them letters, and the first one upper case. An acronym in caps ("PR", "TCFD") is left in, since the check that matters is the verb that follows it.
func capitalisedName(word string) bool {
	if len([]rune(word)) < 2 {
		return false
	}
	for i, r := range word {
		if i == 0 && !unicode.IsUpper(r) {
			return false
		}
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// Mine reports whether this action item is the user's own to answer for. Input: none beyond the item itself. Output: true when the owner is "Me" (however the model cased or spaced it) or is unnamed ("Owner unclear"), since an item nobody was named for might still be the user's; false for anybody else's name.
// Kept for the paths that have no identity entry to hand; anything that can read personal context should use OwnerClass instead, which tells the user's own name from somebody else's.
func (a ActionItem) Mine() bool {
	owner := strings.TrimSpace(a.Owner)
	return owner == UnknownOwner || strings.EqualFold(owner, MeOwner)
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
	// The em dash is what the minutes prompt asks for, and everything else here is what models write anyway — both of today's recordings came back as "Alex: review his code", which parsed as nothing at all, and a plain hyphen is common enough that the window's own renderer already accepts it.
	// They are tried in this order so a bullet carrying more than one splits where the prompt told it to, and the hyphen is matched with spaces around it so a hyphenated name is not cut in half.
	for _, sep := range []string{"—", "–", " - ", ":"} {
		if owner, text, ok = strings.Cut(bullet, sep); ok {
			break
		}
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

// doneVia separates an action note's priority from the evidence that closed it, as in "[done/normal via Daily AI Sprint Standup 2026-09-04]". A phrase rather than a symbol so a hand-edited line still reads as a sentence.
const doneVia = " via "

// raisedFormat is how an action note carries the date its meeting happened — the same local calendar-day key the diary and the dreaming loop use.
const raisedFormat = "2006-01-02"

// Note renders the action item as the single line stored in the notes table under kind 'action'. The status and priority lead so the state is readable at a glance and cheap to parse back; the owner and work read as an ordinary sentence, and the meeting and date close it so the brief can say where an item came from and how long it has been sitting. One line, because the FTS mirror and the vector index both store a note as one blob.
func (a ActionItem) Note() string {
	tags := a.Status + "/" + a.Priority
	if a.DoneSource != "" {
		// The evidence that closed an item rides in the tag prefix rather than the trailing provenance, which already means the meeting the work was agreed in and must keep meaning that.
		tags += doneVia + strings.ReplaceAll(a.DoneSource, "]", "")
	}
	line := "[" + tags + "] " + a.Owner + " — " + a.Text
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
	if !ok {
		return ActionItem{}, false
	}
	priority, doneSource, _ := strings.Cut(priority, doneVia)
	if !ValidStatus(status) || !ValidPriority(priority) {
		return ActionItem{}, false
	}

	a := ActionItem{Status: status, Priority: priority, DoneSource: strings.TrimSpace(doneSource)}
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

// ValidOwnerClass reports whether class is one of the three answers to "whose task is this?" — the only values PATCH /tasks/{id} and OwnerOverride may ever hold.
func ValidOwnerClass(class string) bool {
	return class == OwnerMe || class == OwnerThem || class == OwnerUnclear
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

// completionVerbs are the words that say a piece of work actually happened. "Deploy" and "review" are not among them: they name the work, not its end.
var completionVerbs = []string{"done", "finished", "completed", "complete", "merged", "pushed", "deployed", "shipped", "landed", "closed", "resolved", "delivered", "sent", "fixed", "wrote", "ran"}

// evidenceStopwords are words too common to count as naming a particular task.
var evidenceStopwords = []string{"the", "and", "with", "that", "this", "from", "into", "then", "than", "when", "what", "which", "have", "been", "will", "should", "would", "about", "over", "only", "more", "some", "other", "their", "them", "they", "there", "before", "after", "today", "work"}

// referenceNumber matches a pull request, issue or ticket number as people write them in a meeting, "#5632".
var referenceNumber = regexp.MustCompile(`#\d+`)

// sentenceBreak splits evidence into the sentences a completion verb has to share with the work it closes, so "merged X; Y is still pending" cannot close Y.
var sentenceBreak = regexp.MustCompile(`[.;\n!?]+`)

// EvidenceCloses reports whether a piece of later writing says this item's work is finished. Input: the item, and the text of a meeting summary, a day's page or a compiled note. Output: true when one sentence of it both names the work and says it happened.
// Naming the work means either the same pull request or ticket number the item names, or at least half of the item's distinctive words, never fewer than two. A sentence that names the work without a completion verb, or carries a completion verb about something else, closes nothing.
func EvidenceCloses(a ActionItem, evidence string) bool {
	want := distinctiveWords(a.Text)
	refs := referenceNumber.FindAllString(a.Text, -1)
	if len(want) < 2 && len(refs) == 0 {
		return false
	}
	for _, sentence := range sentenceBreak.Split(evidence, -1) {
		words := strings.Fields(strings.ToLower(sentence))
		for i := range words {
			words[i] = strings.Trim(words[i], ".,;:—-()[]*`\"'")
		}
		if !containsAnyWord(words, completionVerbs) {
			continue
		}
		for _, ref := range refs {
			if strings.Contains(sentence, ref) {
				return true
			}
		}
		matched := 0
		for _, w := range want {
			if slices.Contains(words, w) {
				matched++
			}
		}
		if matched >= 2 && matched*2 >= len(want) {
			return true
		}
	}
	return false
}

// containsAnyWord reports whether any of wanted appears in words as a whole word.
func containsAnyWord(words, wanted []string) bool {
	for _, w := range words {
		if slices.Contains(wanted, w) {
			return true
		}
	}
	return false
}

// distinctiveWords reduces a piece of work to the words that identify it: lower case, four letters or more, punctuation stripped, common words and duplicates dropped.
func distinctiveWords(text string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ".,;:—-()[]*`\"'&#")
		if len([]rune(w)) < 4 || slices.Contains(evidenceStopwords, w) || slices.Contains(out, w) {
			continue
		}
		out = append(out, w)
	}
	return out
}
