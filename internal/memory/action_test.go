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
- **Mahadevan KS** — wire up Value Chain Activity Generation on Essentials and implement the small CRRO generation module.
- **Mahadevan KS** — finish the local ` + "`credibl-essentials`" + ` setup: restore the DB dump, resolve the asdf/` + "`.tool-versions`" + ` Node mismatch.
- **Owner unclear** — clean up the mixed lockfile situation in the frontend; raised but not assigned.

## Open questions
- Nothing further.
`

func TestParseMinutesActions(t *testing.T) {
	raised := time.Date(2026, 8, 28, 14, 17, 0, 0, time.UTC)
	got := ParseMinutesActions(realMinutes, "credibl essentials setup", raised)

	if len(got) != 3 {
		t.Fatalf("want 3 action items, got %d: %+v", len(got), got)
	}
	if got[0].Owner != "Mahadevan KS" {
		t.Errorf("owner = %q, want %q", got[0].Owner, "Mahadevan KS")
	}
	if got[0].Text != "wire up Value Chain Activity Generation on Essentials and implement the small CRRO generation module." {
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
		if a.Source != "credibl essentials setup" || !a.Raised.Equal(raised) {
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
		Owner:    "Arjun",
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
	got, ok := ParseAction("[done/high] Mahadevan KS — finish the credibl-essentials setup. (essentials setup, 2026-08-28)")
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
	a := ActionItem{Owner: "Arjun", Text: "settle payment.", Status: StatusOpen, Priority: PriorityNormal, Source: "md x mf tool", Raised: time.Date(2026, 8, 28, 19, 19, 0, 0, time.UTC)}
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
		{"no title line", "# Meeting minutes\n\n## Attendees\n- Mahadevan KS\n", ""},
		{"empty", "", ""},
	} {
		if got := MinutesLabel(tc.minutes); got != tc.want {
			t.Errorf("%s: MinutesLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Minutes with no title line leave an item with no meeting name. The date still has to survive, and the line must not read as if the name were blank.
func TestActionItem_NoteWithoutSource(t *testing.T) {
	a := ActionItem{Owner: "Mahadevan KS", Text: "finish the setup.", Status: StatusOpen, Priority: PriorityNormal, Raised: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)}
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
	got := ParseMinutesActions("## Action items\n- **Trupti Hosmani** — continue analysing the dataset.\n- **Owner unclear** — clean up the lockfiles.\n", "s", time.Time{})
	if len(got) != 2 || got[0].Owner != "Trupti Hosmani" || got[1].Owner != "Owner unclear" {
		t.Fatalf("owners = %+v", got)
	}
}

// Half the action items in a meeting belong to somebody else. Asking the user "any progress?" about a task another person owes is the wrong question, so an item has to know whose it is. The identity entry names the user and every way they are written down, and names nobody else, which is what makes it the test.
func TestOwnedByUser(t *testing.T) {
	identity := "The user is Mahadevan KS — goes by Mahadevan; git handle M-DEV-1. He is the owner of this computer and the [me] speaker in every meeting recording."
	for _, tc := range []struct {
		owner string
		want  bool
	}{
		{"Mahadevan KS", true},
		{"Mahadevan", true},    // the same person, written shorter
		{"mahadevan ks", true}, // and in whatever case the model chose
		{"Arjun", false},
		{"Krish", false},
		{"Trupti Hosmani", false},
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
	if OwnedByUser("Mahad", "The user is Mahadevan KS.") {
		t.Error("a fragment of the user's name matched as the user")
	}
}

// With no identity on file nothing can be attributed, so nothing claims to be the user's.
func TestOwnedByUser_NoIdentity(t *testing.T) {
	if OwnedByUser("Mahadevan KS", "") {
		t.Error("attributed an item to the user with no identity to check against")
	}
}

// The fly-on-the-wall case: a meeting the user sat in on, owing nothing, has no items worth tracking — every one belongs to somebody else and none of it is the user's to answer for.
func TestUserMeetingActions_DropsAMeetingTheUserOwesNothingIn(t *testing.T) {
	identity := "The user is Mahadevan KS — goes by Mahadevan."
	observed := []ActionItem{
		{Owner: "Tushar", Text: "add battery optimisation to the app."},
		{Owner: UnknownOwner, Text: "trial attaching walkthrough videos to PRs."},
	}
	if got := UserMeetingActions(observed, identity); len(got) != 0 {
		t.Errorf("kept %d items from a meeting the user only sat in on: %+v", len(got), got)
	}
}

// A meeting the user owes something in keeps every item, including other people's: work the user is waiting on is work they care about, which is what separates being in a meeting from sitting in on one.
func TestUserMeetingActions_KeepsOthersWorkWhenTheUserIsInvolved(t *testing.T) {
	identity := "The user is Mahadevan KS — goes by Mahadevan."
	mixed := []ActionItem{
		{Owner: "Arjun", Text: "carry PR #13 through CI and merge."},
		{Owner: "Mahadevan", Text: "compare the minutes against his own agent's output."},
		{Owner: "Krish", Text: "reply on WhatsApp during his leave."},
	}
	if got := UserMeetingActions(mixed, identity); len(got) != 3 {
		t.Errorf("kept %d of 3 items from a meeting the user was actually in", len(got))
	}
}
