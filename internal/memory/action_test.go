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
- **Alex Rivera** — wire up Value Chain Activity Generation on Essentials and implement the small PRDO generation module.
- **Alex Rivera** — finish the local ` + "`acme-essentials`" + ` setup: restore the DB dump, resolve the asdf/` + "`.tool-versions`" + ` Node mismatch.
- **Owner unclear** — clean up the mixed lockfile situation in the frontend; raised but not assigned.

## Open questions
- Nothing further.
`

func TestParseMinutesActions(t *testing.T) {
	raised := time.Date(2026, 8, 28, 14, 17, 0, 0, time.UTC)
	got := ParseMinutesActions(realMinutes, "acme essentials setup", raised)

	if len(got) != 3 {
		t.Fatalf("want 3 action items, got %d: %+v", len(got), got)
	}
	if got[0].Owner != "Alex Rivera" {
		t.Errorf("owner = %q, want %q", got[0].Owner, "Alex Rivera")
	}
	if got[0].Text != "wire up Value Chain Activity Generation on Essentials and implement the small PRDO generation module." {
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
		if a.Source != "acme essentials setup" || !a.Raised.Equal(raised) {
			t.Errorf("item %d provenance = %q/%v", i, a.Source, a.Raised)
		}
	}
}

func TestParseMinutesActions_StopsAtNextSection(t *testing.T) {
	for _, a := range ParseMinutesActions(realMinutes, "s", time.Time{}) {
		if a.Text == "Nothing further." {
			t.Fatal("parser ran past the Action items section into Open questions")
		}
	}
}

func TestParseMinutesActions_NoSection(t *testing.T) {
	if got := ParseMinutesActions("# Minutes\n\n## Key points\n- talked about things.\n", "s", time.Time{}); len(got) != 0 {
		t.Fatalf("want none, got %+v", got)
	}
}

func TestActionItem_NoteRoundTrip(t *testing.T) {
	want := ActionItem{
		Owner:    "Vikram",
		Text:     "carry PR #13 through CI and merge (keeping commits under the MF pipeline account).",
		Status:   StatusDone,
		Priority: PriorityHigh,
		Source:   "md x mf tool",
		Raised:   time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	}
	got, ok := ParseAction(want.Note())
	if !ok {
		t.Fatalf("ParseAction refused its own output: %q", want.Note())
	}
	if got.Owner != want.Owner || got.Text != want.Text {
		t.Errorf("owner/text = %q / %q", got.Owner, got.Text)
	}
	if got.Status != want.Status || got.Priority != want.Priority {
		t.Errorf("status/priority = %q / %q", got.Status, got.Priority)
	}
	if got.Source != want.Source || !got.Raised.Equal(want.Raised) {
		t.Errorf("source/raised = %q / %v", got.Source, got.Raised)
	}
}

func TestActionItem_NoteIsOneReadableLine(t *testing.T) {
	a := ActionItem{Owner: "Krish", Text: "reply on WhatsApp during his leave.", Status: StatusOpen, Priority: PriorityLow, Source: "md x mf tool", Raised: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)}
	note := a.Note()
	if strings.Contains(note, "\n") {
		t.Errorf("note spans lines, which the FTS mirror stores as one blob: %q", note)
	}
	for _, want := range []string{"[open/low]", "Krish", "reply on WhatsApp", "md x mf tool", "2026-08-28"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q is missing %q", note, want)
		}
	}
}

func TestParseAction_HandEditedStatus(t *testing.T) {
	got, ok := ParseAction("[done/high] Alex Rivera — finish the acme-essentials setup. (essentials setup, 2026-08-28)")
	if !ok {
		t.Fatal("refused a hand-edited action note")
	}
	if got.Status != StatusDone || got.Priority != PriorityHigh {
		t.Errorf("status/priority = %q / %q", got.Status, got.Priority)
	}
}

func TestParseAction_NotAnActionNote(t *testing.T) {
	if _, ok := ParseAction("the user prefers dark mode"); ok {
		t.Fatal("parsed an ordinary fact note as an action item")
	}
}

// An action note carries the day its meeting happened, not the minute: "how long has this been sitting" is a question about days, and the note is meant to read like a sentence a person wrote.
func TestParseAction_RaisedIsTruncatedToItsDay(t *testing.T) {
	a := ActionItem{Owner: "Vikram", Text: "settle payment.", Status: StatusOpen, Priority: PriorityNormal, Source: "md x mf tool", Raised: time.Date(2026, 8, 28, 19, 19, 0, 0, time.UTC)}
	got, ok := ParseAction(a.Note())
	if !ok {
		t.Fatal("refused its own output")
	}
	if want := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC); !got.Raised.Equal(want) {
		t.Errorf("raised = %v, want %v", got.Raised, want)
	}
}

// MinutesLabel names the meeting for an action item's provenance line, taken from the title line the minutes open with. The three shapes below are real: two carry a bold title, one has no title line at all.
func TestMinutesLabel(t *testing.T) {
	for _, tc := range []struct{ name, minutes, want string }{
		{"bold title", "# Meeting minutes\n\n**md x mf tool — Google Meet, Fri 28 Aug 2026, 21:36–23:08 IST**\n\n## Attendees\n", "md x mf tool"},
		{"bold title with comma", "# Meeting minutes\n\n**climate risk sync — Fri 28 Aug 2026, 14:03–14:41 (38m)**\n\n## Attendees\n", "climate risk sync"},
		{"no title line", "# Meeting minutes\n\n## Attendees\n- Alex Rivera\n", ""},
		{"empty", "", ""},
	} {
		if got := MinutesLabel(tc.minutes); got != tc.want {
			t.Errorf("%s: MinutesLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Minutes with no title line leave an item with no meeting name. The date still has to survive, and the line must not read as if the name were blank.
func TestActionItem_NoteWithoutSource(t *testing.T) {
	a := ActionItem{Owner: "Alex Rivera", Text: "finish the setup.", Status: StatusOpen, Priority: PriorityNormal, Raised: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)}
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

// Not every bullet puts the owner first. The minutes prompt asks for "**Owner** — what they agreed to do", but a model writing an unassigned item sometimes puts the note at the end instead: "Decide the X approach — owner unclear". Splitting on the dash there would file a sentence as a person, and the brief would read it out as one.
func TestParseMinutesActions_OwnerAtTheEndIsNotAName(t *testing.T) {
	got := ParseMinutesActions("## Action items\n- Decide the save-state / DB table approach — owner unclear (delegated to the call side).\n", "climate risk sync", time.Time{})
	if len(got) != 1 {
		t.Fatalf("want 1 item, got %d", len(got))
	}
	if got[0].Owner != UnknownOwner {
		t.Errorf("owner = %q, want %q", got[0].Owner, UnknownOwner)
	}
	if !strings.HasPrefix(got[0].Text, "Decide the save-state") {
		t.Errorf("the work was lost: text = %q", got[0].Text)
	}
}

// A real owner still splits normally, including the two-word names and the "Owner unclear" the prompt asks for.
func TestParseMinutesActions_KeepsRealOwners(t *testing.T) {
	got := ParseMinutesActions("## Action items\n- **Priya Shah** — continue analysing the dataset.\n- **Owner unclear** — clean up the lockfiles.\n", "s", time.Time{})
	if len(got) != 2 || got[0].Owner != "Priya Shah" || got[1].Owner != "Owner unclear" {
		t.Fatalf("owners = %+v", got)
	}
}

// Half the action items in a meeting belong to somebody else. Asking the user "any progress?" about a task another person owes is the wrong question, so an item has to know whose it is. The identity entry names the user and every way they are written down, and names nobody else, which is what makes it the test.
func TestOwnedByUser(t *testing.T) {
	identity := "The user is Alex Rivera — goes by Alex; git handle M-DEV-1. He is the owner of this computer and the [me] speaker in every meeting recording."
	for _, tc := range []struct {
		owner string
		want  bool
	}{
		{"Alex Rivera", true},
		{"Alex", true},    // the same person, written shorter
		{"alex ks", true}, // and in whatever case the model chose
		{"Vikram", false},
		{"Krish", false},
		{"Priya Shah", false},
		{UnknownOwner, false}, // nobody took it, so it is not the user's by default
		{"", false},
	} {
		if got := OwnedByUser(tc.owner, identity); got != tc.want {
			t.Errorf("OwnedByUser(%q) = %v, want %v", tc.owner, got, tc.want)
		}
	}
}

// A substring that is not a whole word must not match: "KS" appearing inside another word is not the user.
func TestOwnedByUser_WholeWordsOnly(t *testing.T) {
	if OwnedByUser("Mahad", "The user is Alex Rivera.") {
		t.Error("a fragment of the user's name matched as the user")
	}
}

// With no identity on file nothing can be attributed, so nothing claims to be the user's.
func TestOwnedByUser_NoIdentity(t *testing.T) {
	if OwnedByUser("Alex Rivera", "") {
		t.Error("attributed an item to the user with no identity to check against")
	}
}

// The fly-on-the-wall case: a meeting the user sat in on, owing nothing, has no items worth tracking — every one belongs to somebody else and none of it is the user's to answer for.
func TestUserMeetingActions_DropsAMeetingTheUserOwesNothingIn(t *testing.T) {
	identity := "The user is Alex Rivera — goes by Alex."
	observed := []ActionItem{
		{Owner: "Sandeep", Text: "add battery optimisation to the app."},
		{Owner: UnknownOwner, Text: "trial attaching walkthrough videos to PRs."},
	}
	if got := UserMeetingActions(observed, identity); len(got) != 0 {
		t.Errorf("kept %d items from a meeting the user only sat in on: %+v", len(got), got)
	}
}

// A meeting the user owes something in keeps every item, including other people's: work the user is waiting on is work they care about, which is what separates being in a meeting from sitting in on one.
func TestUserMeetingActions_KeepsOthersWorkWhenTheUserIsInvolved(t *testing.T) {
	identity := "The user is Alex Rivera — goes by Alex."
	mixed := []ActionItem{
		{Owner: "Vikram", Text: "carry PR #13 through CI and merge."},
		{Owner: "Alex", Text: "compare the minutes against his own agent's output."},
		{Owner: "Krish", Text: "reply on WhatsApp during his leave."},
	}
	if got := UserMeetingActions(mixed, identity); len(got) != 3 {
		t.Errorf("kept %d of 3 items from a meeting the user was actually in", len(got))
	}
}

// Mine is true only for the user's own items ("Me", however the model cased or spaced it) and for items nobody was named for, since an unassigned item might still be the user's. Anybody else's name is false.
func TestActionItem_Mine(t *testing.T) {
	for _, tc := range []struct {
		owner string
		want  bool
	}{
		{MeOwner, true},
		{"me", true},
		{"  Me  ", true},
		{UnknownOwner, true},
		{"Vikram", false},
		{"Priya Shah", false},
		{"", false},
	} {
		if got := (ActionItem{Owner: tc.owner}).Mine(); got != tc.want {
			t.Errorf("Mine(%q) = %v, want %v", tc.owner, got, tc.want)
		}
	}
}

// identityEntry is the personal-context line that says who the user is, copied from what the store actually holds.
const identityEntry = "The user is Alex Rivera — goes by Alex; git handle M-DEV-1. He is the owner of this computer and the [me] speaker in every meeting recording."

func TestOwnerClass(t *testing.T) {
	cases := []struct {
		name  string
		owner string
		text  string
		want  string
	}{
		{"his full name", "Alex Rivera", "Rework the hardcoded location-finder logic.", OwnerMe},
		{"the name he goes by", "Alex", "Report status on the PFP task.", OwnerMe},
		{"the label the prompt asks for", "Me", "send the deck by Friday", OwnerMe},
		{"written as you", "You", "send the deck by Friday", OwnerMe},
		{"written as I", "I", "send the deck by Friday", OwnerMe},
		{"another person", "Priya Shah", "Create the Northwind Freight test account.", OwnerThem},
		{"a role rather than a name", "The project lead", "Give campaign managers access to his ElevenLabs account.", OwnerThem},
		{"no subject at all", UnknownOwner, "clean up the mixed lockfile situation in the frontend; raised but not assigned.", OwnerUnclear},
		{"unclear but qualified", "Owner unclear (workstream lead)", "Set up the X API dashboard via Proton email.", OwnerThem},
		{"a bolded person prefix left in the text", UnknownOwner, "**Every campaign manager (including Imanshu)** — Choose a market and post it in the group chat.", OwnerThem},
		{"a name before will", UnknownOwner, "Krish will take the technical interviews for the developer hire.", OwnerThem},
		{"a name before to", UnknownOwner, "Handed to Sam to finish the load testing task.", OwnerThem},
		{"a name before should", UnknownOwner, "Nikunj should add the Verity-workflow task to the sprint board.", OwnerThem},
		{"a capitalised first word is not a name", UnknownOwner, "Define how the knowledge pool concept should work in practice.", OwnerUnclear},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := ActionItem{Owner: c.owner, Text: c.text}
			if got := a.OwnerClass(identityEntry); got != c.want {
				t.Errorf("OwnerClass(%q, %q) = %q, want %q", c.owner, c.text, got, c.want)
			}
		})
	}
}

func TestOwnerClass_NoIdentityLeavesHisOwnNameUnclassified(t *testing.T) {
	a := ActionItem{Owner: "Alex Rivera", Text: "Continue transition-risk work."}
	if got := a.OwnerClass(""); got != OwnerThem {
		t.Errorf("with no identity on file OwnerClass = %q, want %q — guessing here would put another person's work on his list", got, OwnerThem)
	}
}

// The minutes named him, but hearing about a thing does not make it his — the user can say by hand that a "me" reading is wrong, and that call must win over whatever the owner text would otherwise read as.
func TestOwnerClass_OverrideWinsOverTheParsedOwner(t *testing.T) {
	a := ActionItem{Owner: "Alex Rivera", Text: "Continue transition-risk work.", OwnerOverride: OwnerThem}
	if got := a.OwnerClass(identityEntry); got != OwnerThem {
		t.Errorf("OwnerClass with an override set = %q, want the override %q", got, OwnerThem)
	}
}

func TestValidOwnerClass(t *testing.T) {
	for _, c := range []struct {
		class string
		want  bool
	}{{OwnerMe, true}, {OwnerThem, true}, {OwnerUnclear, true}, {"", false}, {"sideways", false}} {
		if got := ValidOwnerClass(c.class); got != c.want {
			t.Errorf("ValidOwnerClass(%q) = %v, want %v", c.class, got, c.want)
		}
	}
}

func TestEvidenceCloses(t *testing.T) {
	item := ActionItem{Owner: "Me", Text: "deploy the Value Chain & risk-statements PR (#5632)."}
	cases := []struct {
		name     string
		evidence string
		want     bool
	}{
		{"the pull request it names was merged", "Merged the Value Chain risk-statements PR #5632 to main today.", true},
		{"the work is named as finished", "Finished the Value Chain risk-statements deploy this morning.", true},
		{"named but not finished", "Discussed the Value Chain risk-statements PR with Priya and agreed a review order.", false},
		{"finished, but a different piece of work", "Merged the emissions-factor PR #5601; the Value Chain risk-statements deploy is still pending.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EvidenceCloses(item, c.evidence); got != c.want {
				t.Errorf("EvidenceCloses(%q) = %v, want %v", c.evidence, got, c.want)
			}
		})
	}
}

func TestActionItem_DoneSourceRoundTrip(t *testing.T) {
	a := ActionItem{Owner: "Me", Text: "deploy the PR", Status: StatusDone, Priority: PriorityNormal, DoneSource: "Daily AI Sprint Standup 2026-09-04", Source: "standup", Raised: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
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
