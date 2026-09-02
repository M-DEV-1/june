package brain

import "testing"

func TestStripFence(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain json", `{"a":1}`, `{"a":1}`},
		{"fenced", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced with language", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"surrounding whitespace", "  \n```json\n[1,2]\n```  ", `[1,2]`},
		{"no fence, prose", "here you go", "here you go"},
	} {
		if got := StripFence(tc.in); got != tc.want {
			t.Errorf("%s: StripFence(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The outermost value is whichever bracket opens first, so a reply is read the same way whatever shape its caller was expecting — the two hand-written copies this replaced disagreed on exactly that, and a reply holding both an array and an object parsed differently depending on which package read it.
func TestOutermostJSON(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"object after prose", `Sure! {"a":1}`, `{"a":1}`},
		{"array after prose", `Here: [1,2] hope that helps`, `[1,2]`},
		{"array of objects", `[{"a":1},{"b":2}]`, `[{"a":1},{"b":2}]`},
		{"object containing an array", `{"a":[1,2]}`, `{"a":[1,2]}`},
		{"no json at all", "I could not answer that", ""},
		{"opener with no closer", `{"a":1`, ""},
	} {
		if got := OutermostJSON(tc.in); got != tc.want {
			t.Errorf("%s: OutermostJSON(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A model that mentions a bracket before it answers used to defeat the recovery: taking the first bracket and the last matching closer returned a slice that spanned the prose and ended inside the real answer.
func TestOutermostJSON_IgnoresABracketInTheProse(t *testing.T) {
	got := OutermostJSON(`Based on the notes [see above], here is the result: {"items": ["a"]}`)
	if got != `{"items": ["a"]}` {
		t.Errorf("got %q, want the object", got)
	}
}
