package pii

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"ora/internal/db"
	"ora/internal/db/dbtest"
)

// people is the gazetteer every test below tears against: two known people whose personal_context subjects overlap on the first name "Vexil", so the longest-match rule has something to decide.
func people() []db.PersonalEntry {
	return []db.PersonalEntry{
		{Subject: "identity", Content: "the user, works on ORA"},
		{Subject: "preferences-food", Content: "no mushrooms"},
		{Subject: "vexil-quorin", Content: "his partner, lives in Bangalore"},
		{Subject: "vexil-quorin-mother", Content: "her mother, calls on Sundays"},
		{Subject: "ravix-tavrek", Content: "colleague at work"},
	}
}

// TestTear_NoPII_ReturnsByteIdentical is the do-no-harm case: a sentence with nobody and nothing structured in it must come back exactly as it went in, and must produce no entities.
func TestTear_NoPII_ReturnsByteIdentical(t *testing.T) {
	in := "The build finished at 10:30 on 12 September 2026 and cost $1500 for 4096 tokens."
	torn := New(people()).Tear(in)
	if torn.Text != in {
		t.Errorf("text changed\n got: %q\nwant: %q", torn.Text, in)
	}
	if len(torn.Entities) != 0 {
		t.Errorf("found entities in clean text: %+v", torn.Entities)
	}
}

// TestTear_StrangerNameLeftAlone pins the empty half of the package: a name the gazetteer has never seen passes through untouched, because NoUnknownNames finds nothing.
func TestTear_StrangerNameLeftAlone(t *testing.T) {
	in := "Bralthine Quendral sent the contract."
	torn := New(people()).Tear(in)
	if torn.Text != in {
		t.Errorf("stranger's name was torn: %q", torn.Text)
	}
}

// TestTear_KnownPersonIDIsStable checks the one property the whole design hangs on: the placeholder for a known person is derived from their personal_context subject, so it is the same token in every call, in every process, forever. The literal is pinned here on purpose -- if this test has to be edited, every memory already written with the old token has lost its association.
func TestTear_KnownPersonIDIsStable(t *testing.T) {
	const want = "[[PERSON_c2538a6c]]"

	first := New(people()).Tear("Vexil said the venue is booked")
	second := New(people()).Tear("ask Vexil Quorin about the venue")

	if len(first.Entities) != 1 || first.Entities[0].Placeholder != want {
		t.Fatalf("first tear: %+v, want one entity with %s", first.Entities, want)
	}
	if len(second.Entities) != 1 || second.Entities[0].Placeholder != want {
		t.Fatalf("second tear: %+v, want one entity with %s", second.Entities, want)
	}
	if first.Text != "[[PERSON_c2538a6c]] said the venue is booked" {
		t.Errorf("first text: %q", first.Text)
	}
	if strings.Contains(first.Text, "Vexil") {
		t.Errorf("the name survived in the torn text: %q", first.Text)
	}
}

// TestTear_MatchesFirstNameAndIsCaseInsensitive covers how a person is actually written in a sentence: first name alone, full name, and any casing.
func TestTear_MatchesFirstNameAndIsCaseInsensitive(t *testing.T) {
	tr := New(people())
	for _, in := range []string{"vexil called", "VEXIL called", "Vexil Quorin called", "quorin called"} {
		torn := tr.Tear(in)
		if len(torn.Entities) != 1 {
			t.Errorf("%q: got %d entities, want 1: %+v", in, len(torn.Entities), torn.Entities)
			continue
		}
		if torn.Entities[0].Subject != "vexil-quorin" {
			t.Errorf("%q: subject %q, want vexil-quorin", in, torn.Entities[0].Subject)
		}
	}
}

// TestTear_LongestMatchWins resolves two subjects that overlap: "Vexil Quorin" and "Vexil Quorin Mother" both start at the same word, and the sentence is about the mother.
func TestTear_LongestMatchWins(t *testing.T) {
	torn := New(people()).Tear("Vexil Quorin Mother calls on Sundays")
	if len(torn.Entities) != 1 {
		t.Fatalf("got %d entities, want 1: %+v", len(torn.Entities), torn.Entities)
	}
	if got := torn.Entities[0].Subject; got != "vexil-quorin-mother" {
		t.Errorf("subject %q, want vexil-quorin-mother", got)
	}
	if strings.Contains(torn.Text, "Vexil") || strings.Contains(torn.Text, "Mother") {
		t.Errorf("part of the longer name was left behind: %q", torn.Text)
	}
}

// TestTear_TwoPeopleInOneSentence checks each person gets their own token and the record lists both.
func TestTear_TwoPeopleInOneSentence(t *testing.T) {
	torn := New(people()).Tear("Vexil and Ravix are both coming")
	if got := torn.Subjects(); !reflect.DeepEqual(got, []string{"vexil-quorin", "ravix-tavrek"}) {
		t.Errorf("subjects %v, want [vexil-quorin ravix-tavrek]", got)
	}
	if torn.Text != "[[PERSON_c2538a6c]] and [[PERSON_23518061]] are both coming" {
		t.Errorf("text: %q", torn.Text)
	}
}

// TestTear_NameInsideAnotherWordIsLeftAlone is the false-positive guard on the gazetteer: a name is only a name on word boundaries.
func TestTear_NameInsideAnotherWordIsLeftAlone(t *testing.T) {
	in := "the vexilfoo branch and Ravixanda street"
	if torn := New(people()).Tear(in); torn.Text != in {
		t.Errorf("matched inside a word: %q", torn.Text)
	}
}

// TestTear_StructuredPII covers the rule pass: email, +91 and bare Indian mobile, and a card-length digit run, each replaced by a token derived from the value itself.
func TestTear_StructuredPII(t *testing.T) {
	cases := []struct {
		in   string
		kind string
	}{
		{"mail me at vexil@example.com please", "email"},
		{"call +91 98765 43210 tonight", "phone"},
		{"call +919876543210 tonight", "phone"},
		{"her number is 9876543210", "phone"},
		{"the account is 4111111111111111", "account"},
		{"card 4111 1111 1111 1111 on file", "account"},
	}
	tr := New(nil)
	for _, c := range cases {
		torn := tr.Tear(c.in)
		if len(torn.Entities) != 1 {
			t.Errorf("%q: got %d entities, want 1: %+v", c.in, len(torn.Entities), torn.Entities)
			continue
		}
		if torn.Entities[0].Kind != c.kind {
			t.Errorf("%q: kind %q, want %q", c.in, torn.Entities[0].Kind, c.kind)
		}
		if torn.Stitch(torn.Text) != c.in {
			t.Errorf("%q: did not round-trip, got %q", c.in, torn.Stitch(torn.Text))
		}
	}
}

// TestTear_OrdinaryNumbersSurvive is the other half of the rule pass: years, times, prices, ports and version numbers are not PII and must not be eaten.
func TestTear_OrdinaryNumbersSurvive(t *testing.T) {
	in := "in 2026 at 09:45 the 8080 port served 150000 requests for 1234 users, v1.50.1"
	if torn := New(nil).Tear(in); torn.Text != in {
		t.Errorf("an ordinary number was torn: %q\nentities: %+v", torn.Text, torn.Entities)
	}
}

// TestTear_SameValueTwiceGetsOneEntity checks a value repeated in one text is one entity and one token, not two.
func TestTear_SameValueTwiceGetsOneEntity(t *testing.T) {
	torn := New(people()).Tear("Vexil booked it, then Vexil paid")
	if len(torn.Entities) != 1 {
		t.Fatalf("got %d entities, want 1: %+v", len(torn.Entities), torn.Entities)
	}
	if strings.Count(torn.Text, "[[PERSON_c2538a6c]]") != 2 {
		t.Errorf("token not used for both mentions: %q", torn.Text)
	}
}

// TestPlaceholderSurvivesProseAndMarkup is the test behind the choice of form. The token has to come back from a model verbatim, so it is plain ASCII in a bracket pair that does not occur in ordinary English, and it has to stay one token through the things a model does to text around it: quoting, a possessive 's, a full stop, a markdown list or bold. None of those touch the inside of the brackets, so Stitch still finds it.
func TestPlaceholderSurvivesProseAndMarkup(t *testing.T) {
	torn := New(people()).Tear("Vexil said the venue is booked")
	ph := torn.Entities[0].Placeholder

	for _, reply := range []string{
		ph + " confirmed it.",
		"- **" + ph + "** confirmed it",
		"According to " + ph + "'s message, it is booked.",
		"\"" + ph + "\" is the one who booked it (" + ph + ").",
	} {
		got := torn.Stitch(reply)
		if strings.Contains(got, "[[") || strings.Contains(got, "PERSON_") {
			t.Errorf("placeholder not stitched back in %q: got %q", reply, got)
		}
		if !strings.Contains(got, "Vexil") {
			t.Errorf("name did not come back in %q: got %q", reply, got)
		}
	}
}

// TestStitch_UnknownPlaceholderIsLeftAlone checks a token the model invented, that the tear never issued, is passed through untouched rather than crashing or being blanked.
func TestStitch_UnknownPlaceholderIsLeftAlone(t *testing.T) {
	torn := New(people()).Tear("Vexil said the venue is booked")
	got := torn.Stitch("[[PERSON_deadbeef]] and [[PERSON_c2538a6c]] agree")
	if want := "[[PERSON_deadbeef]] and Vexil agree"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAssociationSurvivesWhenModelReturnsAPronoun is the most important property in the package. The model usually does not hand the token back: it answers with "she", "your colleague", or a paraphrase with no token in it at all. Nothing can recover the name from that prose, and the package does not try. What it guarantees instead is that the tear itself hands back the list of entities that were in the exchange, so the caller still knows this turn was about Vexil Quorin and can file the memory under her -- which is the association the user is paying for. Lose this and the memory is worthless even though the text reads fine.
func TestAssociationSurvivesWhenModelReturnsAPronoun(t *testing.T) {
	torn := New(people()).Tear("Vexil said the venue is booked")

	reply := torn.Stitch("She has already confirmed it with your colleague.")
	if reply != "She has already confirmed it with your colleague." {
		t.Errorf("Stitch rewrote prose it should not have touched: %q", reply)
	}

	if got := torn.Subjects(); !reflect.DeepEqual(got, []string{"vexil-quorin"}) {
		t.Fatalf("the association was lost: subjects %v, want [vexil-quorin]", got)
	}
}

// TestNewFromStore tears against the real personal_context table, so the gazetteer is proven to be keyed on what the personal_context tool actually wrote rather than on a hand-built slice.
func TestNewFromStore(t *testing.T) {
	store := dbtest.Open(t)
	ctx := context.Background()
	if err := store.SetPersonalContext(ctx, "vexil-quorin", "his partner"); err != nil {
		t.Fatalf("SetPersonalContext: %v", err)
	}
	entries, err := store.PersonalContext(ctx)
	if err != nil {
		t.Fatalf("PersonalContext: %v", err)
	}

	torn := New(entries).Tear("Vexil said the venue is booked")
	if got := torn.Subjects(); !reflect.DeepEqual(got, []string{"vexil-quorin"}) {
		t.Fatalf("subjects %v, want [vexil-quorin]", got)
	}
	if torn.Stitch(torn.Text) != "Vexil said the venue is booked" {
		t.Errorf("round trip: %q", torn.Stitch(torn.Text))
	}
}
