package memory

import (
	"strings"
	"testing"
	"time"
)

// realMinutes is the shape the recorder's minutes prompt asks for, with the action bullets copied from a real meeting note so the parser is tested against what the model actually writes.
const realMinutes = `# Meeting minutes

## Decisions
- Keep JSONB as the storage approach.

## Action items
- **Zemna Braxen** — wire up Route Planning Activity Generation on Basics and implement the small RQLD generation module.
- **Zemna Braxen** — finish the local ` + "`acme-basics`" + ` setup: restore the DB dump, resolve the asdf/` + "`.tool-versions`" + ` Node mismatch.
- **Owner unclear** — clean up the mixed lockfile situation in the frontend; raised but not assigned.

## Open questions
- Nothing further.
`

func TestParseMinutesActions(t *testing.T) {
	raised := time.Date(2026, 8, 28, 14, 17, 0, 0, time.UTC)
	got := ParseMinutesActions(realMinutes, "acme basics setup", raised)

	if len(got) != 3 {
		t.Fatalf("want 3 action items, got %d: %+v", len(got), got)
	}
	if got[0].Owner != "Zemna Braxen" {
		t.Errorf("owner = %q, want %q", got[0].Owner, "Zemna Braxen")
	}
	if got[0].Text != "wire up Route Planning Activity Generation on Basics and implement the small RQLD generation module." {
		t.Errorf("text = %q", got[0].Text)
	}
	if got[2].Owner != "Owner unclear" {
		t.Errorf("unowned item owner = %q", got[2].Owner)
	}
	for i, a := range got {
		if a.Status != StatusOpen {
			t.Errorf("item %d status = %q, want open", i, a.Status)
		}
		if a.Priority != PriorityNormal {
			t.Errorf("item %d priority = %q, want normal", i, a.Priority)
		}
		if a.Source != "acme basics setup" || !a.Raised.Equal(raised) {
			t.Errorf("item %d provenance = %q/%v", i, a.Source, a.Raised)
		}
	}
}

func TestParseAction(t *testing.T) {
	cases := []struct {
		name  string
		note  string
		check func(t *testing.T, got ActionItem, ok bool)
	}{
		{
			name: "a hand-edited note still parses",
			note: "[done/high] Zemna Braxen — finish the acme-basics setup. (essentials setup, 2026-08-28)",
			check: func(t *testing.T, got ActionItem, ok bool) {
				if !ok {
					t.Fatal("refused a hand-edited action note")
				}
				if got.Status != StatusDone || got.Priority != PriorityHigh {
					t.Errorf("status/priority = %q / %q", got.Status, got.Priority)
				}
			},
		},
		{
			name: "an ordinary fact note is not an action item",
			note: "the user prefers dark mode",
			check: func(t *testing.T, got ActionItem, ok bool) {
				if ok {
					t.Fatal("parsed an ordinary fact note as an action item")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseAction(c.note)
			c.check(t, got, ok)
		})
	}
}

// MinutesLabel names the meeting for an action item's provenance line, taken from the title line the minutes open with. The three shapes below are real: two carry a bold title, one has no title line at all.
func TestMinutesLabel(t *testing.T) {
	for _, tc := range []struct{ name, minutes, want string }{
		{"bold title", "# Meeting minutes\n\n**vq x zb tool — Google Meet, Fri 28 Aug 2026, 21:36–23:08 IST**\n\n## Attendees\n", "vq x zb tool"},
		{"bold title with comma", "# Meeting minutes\n\n**route planning sync — Fri 28 Aug 2026, 14:03–14:41 (38m)**\n\n## Attendees\n", "route planning sync"},
		{"no title line", "# Meeting minutes\n\n## Attendees\n- Zemna Braxen\n", ""},
		{"empty", "", ""},
	} {
		if got := MinutesLabel(tc.minutes); got != tc.want {
			t.Errorf("%s: MinutesLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Minutes with no title line leave an item with no meeting name. The date still has to survive, and the line must not read as if the name were blank.
func TestActionItem_NoteWithoutSource(t *testing.T) {
	a := ActionItem{Owner: "Zemna Braxen", Text: "finish the setup.", Status: StatusOpen, Priority: PriorityNormal, Raised: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)}
	note := a.Note()
	if strings.Contains(note, ", 2026") {
		t.Errorf("note renders an empty meeting name: %q", note)
	}
	got, ok := ParseAction(note)
	if !ok {
		t.Fatalf("refused its own output: %q", note)
	}
	if got.Source != "" || !got.Raised.Equal(a.Raised) {
		t.Errorf("source/raised = %q / %v", got.Source, got.Raised)
	}
}

// Minutes with no action items section parse to none. Not every bullet puts the owner first either: the minutes prompt asks for "**Owner** — what they agreed to do", but a model writing an unassigned item sometimes puts the note at the end instead, "Decide the X approach — owner unclear", and splitting on the dash there would file a sentence as a person.
func TestParseMinutesActions_EdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		minutes string
		check   func(t *testing.T, got []ActionItem)
	}{
		{
			name:    "no action items section yields none",
			minutes: "# Minutes\n\n## Key points\n- talked about things.\n",
			check: func(t *testing.T, got []ActionItem) {
				if len(got) != 0 {
					t.Fatalf("want none, got %+v", got)
				}
			},
		},
		{
			name:    "an owner written at the end of the bullet is not mistaken for a name",
			minutes: "## Action items\n- Decide the save-state / DB table approach — owner unclear (delegated to the call side).\n",
			check: func(t *testing.T, got []ActionItem) {
				if len(got) != 1 {
					t.Fatalf("want 1 item, got %d", len(got))
				}
				if got[0].Owner != UnknownOwner {
					t.Errorf("owner = %q, want %q", got[0].Owner, UnknownOwner)
				}
				if !strings.HasPrefix(got[0].Text, "Decide the save-state") {
					t.Errorf("the work was lost: text = %q", got[0].Text)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, ParseMinutesActions(c.minutes, "s", time.Time{}))
		})
	}
}

// Half the action items in a meeting belong to somebody else. Asking the user "any progress?" about a task another person owes is the wrong question, so an item has to know whose it is. The identity entry names the user and every way they are written down, and names nobody else, which is what makes it the test. A substring that is not a whole word must not match ("Brax" inside another word is not the user), and with no identity on file nothing can be attributed at all.
func TestOwnedByUser(t *testing.T) {
	const fullIdentity = "The user is Zemna Braxen — goes by Zemna; git handle zbraxen. He is the owner of this computer and the [me] speaker in every meeting recording."
	cases := []struct {
		name     string
		owner    string
		identity string
		want     bool
	}{
		{name: "his full name", owner: "Zemna Braxen", identity: fullIdentity, want: true},
		{name: "the name he goes by", owner: "Zemna", identity: fullIdentity, want: true},
		{name: "whatever case the model chose", owner: "zemna braxen", identity: fullIdentity, want: true},
		{name: "another person", owner: "Ravix", identity: fullIdentity, want: false},
		{name: "another person, again", owner: "Melvorn", identity: fullIdentity, want: false},
		{name: "another person by full name", owner: "Vexil Quorin", identity: fullIdentity, want: false},
		{name: "nobody took it, not the user's by default", owner: UnknownOwner, identity: fullIdentity, want: false},
		{name: "no owner at all", owner: "", identity: fullIdentity, want: false},
		{name: "a fragment of the name is not the whole word", owner: "Brax", identity: "The user is Zemna Braxen.", want: false},
		{name: "no identity on file attributes nothing", owner: "Zemna Braxen", identity: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OwnedByUser(tc.owner, tc.identity); got != tc.want {
				t.Errorf("OwnedByUser(%q, %q) = %v, want %v", tc.owner, tc.identity, got, tc.want)
			}
		})
	}
}

// identityEntry is the personal-context line that says who the user is, copied from what the store actually holds.
const identityEntry = "The user is Zemna Braxen — goes by Zemna; git handle zbraxen. He is the owner of this computer and the [me] speaker in every meeting recording."

// OwnerClass reads an item's owner text against the personal-context identity entry and, when set, an explicit override (the minutes named him, but hearing about a thing does not make it his — the user can say by hand that a "me" reading is wrong, and that call must win over whatever the owner text would otherwise read as). With no identity on file at all his own name is left unclassified as somebody else's, since guessing here would put another person's work on his list.
func TestOwnerClass(t *testing.T) {
	cases := []struct {
		name       string
		owner      string
		text       string
		noIdentity bool // use "" instead of identityEntry
		override   string
		want       string
	}{
		{name: "his full name", owner: "Zemna Braxen", text: "Rework the hardcoded location-finder logic.", want: OwnerMe},
		{name: "the name he goes by", owner: "Zemna", text: "Report status on the TDL task.", want: OwnerMe},
		{name: "the label the prompt asks for", owner: "Me", text: "send the deck by Friday", want: OwnerMe},
		{name: "written as you", owner: "You", text: "send the deck by Friday", want: OwnerMe},
		{name: "written as I", owner: "I", text: "send the deck by Friday", want: OwnerMe},
		{name: "another person", owner: "Vexil Quorin", text: "Create the Northwind Freight test account.", want: OwnerThem},
		{name: "a role rather than a name", owner: "The project lead", text: "Give campaign managers access to his ElevenLabs account.", want: OwnerThem},
		{name: "no subject at all", owner: UnknownOwner, text: "clean up the mixed lockfile situation in the frontend; raised but not assigned.", want: OwnerUnclear},
		{name: "unclear but qualified", owner: "Owner unclear (workstream lead)", text: "Set up the X API dashboard via Proton email.", want: OwnerThem},
		{name: "a bolded person prefix left in the text", owner: UnknownOwner, text: "**Every campaign manager (including Rensil)** — Choose a market and post it in the group chat.", want: OwnerThem},
		{name: "a name before will", owner: UnknownOwner, text: "Melvorn will take the technical interviews for the developer hire.", want: OwnerThem},
		{name: "a name before to", owner: UnknownOwner, text: "Handed to Emzor to finish the load testing task.", want: OwnerThem},
		{name: "a name before should", owner: UnknownOwner, text: "Voskel should add the Verity-workflow task to the sprint board.", want: OwnerThem},
		{name: "a capitalised first word is not a name", owner: UnknownOwner, text: "Define how the knowledge pool concept should work in practice.", want: OwnerUnclear},
		{name: "no identity on file leaves his own name unclassified", owner: "Zemna Braxen", text: "Continue route-planning work.", noIdentity: true, want: OwnerThem},
		{name: "an override wins over the parsed owner", owner: "Zemna Braxen", text: "Continue route-planning work.", override: OwnerThem, want: OwnerThem},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			identity := identityEntry
			if c.noIdentity {
				identity = ""
			}
			a := ActionItem{Owner: c.owner, Text: c.text, OwnerOverride: c.override}
			if got := a.OwnerClass(identity); got != c.want {
				t.Errorf("OwnerClass(%q, %q) = %q, want %q", c.owner, c.text, got, c.want)
			}
		})
	}
}

// EvidenceCloses reads a piece of evidence against an item's own words: naming the work as merged or finished closes it, naming it but not finishing it does not, finishing a different piece of work does not, a reference number is matched whole ("#12" is a different piece of work from "#123"), and a negated sentence ("not done yet") closes nothing even though it carries a completion verb and the item's own words.
func TestEvidenceCloses(t *testing.T) {
	deploy := ActionItem{Owner: "Me", Text: "deploy the Route Planning & risk-statements PR (#5632)."}
	cases := []struct {
		name     string
		item     ActionItem
		evidence string
		want     bool
	}{
		{"the pull request it names was merged", deploy, "Merged the Route Planning risk-statements PR #5632 to main today.", true},
		{"the work is named as finished", deploy, "Finished the Route Planning risk-statements deploy this morning.", true},
		{"named but not finished", deploy, "Discussed the Route Planning risk-statements PR with Vexil and agreed a review order.", false},
		{"finished, but a different piece of work", deploy, "Merged the shipping-factor PR #5601; the Route Planning risk-statements deploy is still pending.", false},
		{"a reference is matched whole, not as a prefix", ActionItem{Owner: "Me", Text: "review #12."}, "Merged #123 this morning.", false},
		{"a reference matched exactly does close", ActionItem{Owner: "Me", Text: "review #12."}, "Merged #12 this morning.", true},
		{"a negated reference closes nothing", deploy, "PR #5632 is not done yet.", false},
		{"the work named and said to be unfinished closes nothing", deploy, "The Route Planning risk-statements deploy is still not finished.", false},
		{"a negated contraction closes nothing", deploy, "The Route Planning risk-statements deploy isn't finished.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EvidenceCloses(c.item, c.evidence); got != c.want {
				t.Errorf("EvidenceCloses(%q) = %v, want %v", c.evidence, got, c.want)
			}
		})
	}
}

func TestActionItem_DoneSourceRoundTrip(t *testing.T) {
	a := ActionItem{Owner: "Me", Text: "deploy the PR", Status: StatusDone, Priority: PriorityNormal, DoneSource: "Daily Platform Sprint Standup 2026-09-04", Source: "standup", Raised: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	back, ok := ParseAction(a.Note())
	if !ok {
		t.Fatalf("ParseAction could not read back %q", a.Note())
	}
	if back.DoneSource != a.DoneSource {
		t.Errorf("done source = %q, want %q (line was %q)", back.DoneSource, a.DoneSource, a.Note())
	}
	if back.Priority != PriorityNormal || back.Status != StatusDone {
		t.Errorf("status/priority = %q/%q", back.Status, back.Priority)
	}
}

// The minutes prompt asks for an em dash, but a model writes whichever dash it feels like. Every separator the window's own renderer accepts (app/src/next/format.ts's minutesLines) has to parse here too, or the user reads a bullet saying they owe something that never becomes a task.
func TestParseMinutesActions_AcceptsEveryDashTheModelWrites(t *testing.T) {
	for _, sep := range []string{"—", "–", "-", ":"} {
		minutes := "## Action items\n- **Me** " + sep + " send the deck by Friday.\n"
		items := ParseMinutesActions(minutes, "Lodestone sync", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
		if len(items) != 1 {
			t.Errorf("separator %q: got %d items, want 1", sep, len(items))
			continue
		}
		if items[0].Owner != MeOwner || items[0].Text != "send the deck by Friday." {
			t.Errorf("separator %q: owner/text = %q / %q", sep, items[0].Owner, items[0].Text)
		}
	}
}

// A hyphen inside a name is not a separator: only a spaced hyphen separates the owner from the work, so "Jean-Luc" keeps his name and his task.
func TestParseMinutesActions_AHyphenatedNameIsNotSplit(t *testing.T) {
	items := ParseMinutesActions("## Action items\n- **Jean-Luc** — book the room.\n", "Lodestone sync", time.Time{})
	if len(items) != 1 || items[0].Owner != "Jean-Luc" {
		t.Fatalf("items = %+v, want one owned by Jean-Luc", items)
	}
}
