package act

import (
	"strings"
	"testing"
)

// TestMatch is the whole matcher table: every check kind against a screen that satisfies it and one that does not, plus the two malformed checks a model can write.
func TestMatch(t *testing.T) {
	items := []Item{
		{N: 1, Role: "push button", Label: "Reload"},
		{N: 2, Role: "entry", Label: "example.com"},
		{N: 3, Role: "list item", Label: "S16 E8 The Last Ride"},
	}
	cases := []struct {
		name    string
		check   Check
		title   string
		focused string
		want    bool
	}{
		{"title contains, hit", Check{Kind: TitleContains, Value: "S16 E8"}, "Watching S16 E8 — Netflix", "", true},
		{"title contains, case folded", Check{Kind: TitleContains, Value: "s16 e8"}, "Watching S16 E8 — Netflix", "", true},
		{"title contains, miss", Check{Kind: TitleContains, Value: "S16 E9"}, "Watching S16 E8 — Netflix", "", false},
		{"item present, hit", Check{Kind: ItemPresent, Value: "Reload"}, "Netflix", "", true},
		{"item present, partial label", Check{Kind: ItemPresent, Value: "Last Ride"}, "Netflix", "", true},
		{"item present, miss", Check{Kind: ItemPresent, Value: "Stop"}, "Netflix", "", false},
		{"item absent, hit", Check{Kind: ItemAbsent, Value: "Stop"}, "Netflix", "", true},
		{"item absent, miss", Check{Kind: ItemAbsent, Value: "Reload"}, "Netflix", "", false},
		{"field holds, hit", Check{Kind: FieldHolds, Value: "example.com"}, "Netflix", "example.com", true},
		{"field holds, miss", Check{Kind: FieldHolds, Value: "example.com"}, "Netflix", "", false},
		{"unknown kind", Check{Kind: "vibes", Value: "good"}, "Netflix", "", false},
		{"no text to look for", Check{Kind: TitleContains}, "Netflix", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := Match(tc.check, tc.title, items, tc.focused)
			if got != tc.want {
				t.Errorf("Match(%+v) = %v (%s), want %v", tc.check, got, why, tc.want)
			}
			if why == "" {
				t.Error("Match returned no sentence saying what it found")
			}
		})
	}
}

// A field_holds verdict never repeats what the field holds, in either direction, because the field was just typed into and that text is what every other guard keeps off disk and off the stream.
func TestMatch_FieldHoldsVerdictNeverEchoesTheField(t *testing.T) {
	c := Check{Kind: FieldHolds, Value: "hunter2"}
	ok, why := Match(c, "", nil, "hunter2correcthorse")
	if !ok || strings.Contains(why, "hunter2") || strings.Contains(why, "correcthorse") {
		t.Errorf("pass verdict = %v %q, want a pass that does not echo the field", ok, why)
	}
	ok, why = Match(c, "", nil, "secret sauce")
	if ok || strings.Contains(why, "secret") {
		t.Errorf("fail verdict = %v %q, want a fail that does not echo the field", ok, why)
	}
}
