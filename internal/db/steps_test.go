package db

import (
	"strings"
	"testing"
)

// One table covers every rule RenderActStep applies: the fixed words for the screen-reading tools, an item number and its label for the numbered tools, and the two privacy rules — what was typed is never quoted, and a content node's own text (a password, a half-written message) is named by its role instead of being printed.
func TestRenderActStep(t *testing.T) {
	const secret = "correct horse battery staple"
	for _, tc := range []struct {
		name string
		step ActStep
		want string
	}{
		{"observe_screen", ActStep{Name: "observe_screen"}, "looked at the screen"},
		{"show_marks", ActStep{Name: "show_marks"}, "numbered what was on the screen"},
		{"a tool with no plain wording", ActStep{Name: "search_memory"}, ""},
		{"a numbered tool with no readable item number", ActStep{Name: "click", Args: map[string]any{}}, ""},
		{"a button keeps the name a person gave it", ActStep{Name: "click", Args: map[string]any{"n": 3.0}, Result: `clicked [3] push button "Settings" via press; call observe_screen to see the result`}, "clicked item 3 (Settings)"},
		{"point_at with no label in the result falls back to the model's own label argument", ActStep{Name: "point_at", Args: map[string]any{"n": 2.0, "label": "Search"}, Result: `ringed [2] entry "Search"`}, "pointed at item 2 (entry)"},
		{"scroll_to with an item number as a string", ActStep{Name: "scroll_to", Args: map[string]any{"n": "7"}, Result: `scrolled to [7] link "Docs"`}, "scrolled to item 7 (Docs)"},

		{"typed text with the old text argument still present", ActStep{Name: "type_text", Args: map[string]any{"text": secret, "enter": true}, Result: "typed 28 characters"}, "typed into the box in front and pressed Enter"},
		{"typed text with the text already dropped", ActStep{Name: "type_text", Args: map[string]any{"enter": false}, Result: "typed 11 characters"}, "typed into the box in front"},
		{"typed text with no arguments at all", ActStep{Name: "type_text", Result: "typed 3 characters"}, "typed into the box in front"},

		{"a password box is named by its role, not its contents", ActStep{Name: "click", Args: map[string]any{"n": 4.0}, Result: `clicked [4] password text "` + secret + `" via press; call observe_screen to see the result`}, "clicked item 4 (password text)"},
		{"a box the user typed into is named by its role", ActStep{Name: "click", Args: map[string]any{"n": 2.0}, Result: `clicked [2] entry "` + secret + `"; call observe_screen to see the result`}, "clicked item 2 (entry)"},
		{"a run of page text is named by its role", ActStep{Name: "scroll_to", Args: map[string]any{"n": 7.0}, Result: `scrolled to [7] text "` + secret + `"; call observe_screen to see the page now`}, "scrolled to item 7 (text)"},
		{"a content role wins even over the model's own label argument", ActStep{Name: "point_at", Args: map[string]any{"n": 2.0, "label": secret}, Result: `ringed [2] entry "Search"`}, "pointed at item 2 (entry)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderActStep(tc.step)
			if got != tc.want {
				t.Errorf("RenderActStep(%+v) = %q, want %q", tc.step, got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Errorf("%s: rendered the node's own contents into the line: %q", tc.name, got)
			}
		})
	}
}
