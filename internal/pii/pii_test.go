package pii

import (
	"reflect"
	"strings"
	"testing"

	"june/internal/db"
)

// people is the gazetteer every test below tears against: two known people whose personal_context subjects overlap on the first name "Vexil", so the longest-match rule has something to decide.
func people() []db.PersonalEntry {
	return []db.PersonalEntry{
		{Subject: "identity", Content: "the user, works on June"},
		{Subject: "preferences-food", Content: "no mushrooms"},
		{Subject: "vexil-quorin", Content: "his partner, lives in Bangalore"},
		{Subject: "vexil-quorin-mother", Content: "her mother, calls on Sundays"},
		{Subject: "ravix-tavrek", Content: "colleague at work"},
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

// A person is matched however a sentence writes them: any casing, the surname alone, and, when two subjects overlap ("Vexil Quorin" and "Vexil Quorin Mother"), the longest one, with no part of the name left behind in the text.
func TestTear_MatchesHowANameIsWritten(t *testing.T) {
	tr := New(people())
	for _, c := range []struct{ in, subject string }{
		{"VEXIL called", "vexil-quorin"},
		{"quorin called", "vexil-quorin"},
		{"Vexil Quorin Mother called", "vexil-quorin-mother"},
	} {
		torn := tr.Tear(c.in)
		if len(torn.Entities) != 1 || torn.Entities[0].Subject != c.subject {
			t.Errorf("%q: entities %+v, want one for %s", c.in, torn.Entities, c.subject)
			continue
		}
		if want := torn.Entities[0].Placeholder + " called"; torn.Text != want {
			t.Errorf("%q: text %q, want %q", c.in, torn.Text, want)
		}
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
		{"her number is 9876543210", "phone"},
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

// TestPlaceholderSurvivesProseAndMarkup is the test behind the choice of form. The token has to come back from a model verbatim, so it is plain ASCII in a bracket pair that does not occur in ordinary English, and it has to stay one token through the things a model does to text around it: quoting, a possessive 's, a full stop, a markdown list or bold. None of those touch the inside of the brackets, so Stitch still finds it.
func TestPlaceholderSurvivesProseAndMarkup(t *testing.T) {
	torn := New(people()).Tear("Vexil said the venue is booked")
	ph := torn.Entities[0].Placeholder

	for _, reply := range []string{
		ph + " confirmed it.",
		"According to " + ph + "'s message, it is booked.",
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
