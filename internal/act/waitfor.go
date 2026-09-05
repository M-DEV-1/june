// This file is the verification half of a computer-use step: the expected change a step writes down before it acts, and the pure string match that decides whether the screen now shows it. It costs no model call, which is the point — a job checks every one of its own steps, and a model asked "did that work?" once per step would double what a job costs.
package act

import (
	"fmt"
	"strings"
)

// The four kinds of expected change a step may write down. They are the four things a screen can be asked about without a model reading it: what the window is called, whether a labelled thing is there, whether it has gone, and what is in the box being typed into.
const (
	// TitleContains passes when the front window's title contains the value.
	TitleContains = "title_contains"
	// ItemPresent passes when some item in the observe_screen list has the value in its label.
	ItemPresent = "item_present"
	// ItemAbsent passes when no item in the observe_screen list has the value in its label.
	ItemAbsent = "item_absent"
	// FieldHolds passes when the text of the field with keyboard focus contains the value.
	FieldHolds = "field_holds"
)

// How the wait_for tool opens its answer, either way. These are matched, not just printed: a job's step loop reads the prefix to record its step as a pass or a fail (see internal/actjob), so the two live here, in the pure package both sides already import, rather than as a string one side spells out and the other guesses at.
const (
	// WaitPassPrefix opens the answer when the expected change came, followed by what was found.
	WaitPassPrefix = "the change came: "
	// WaitFailPrefix opens the answer when it did not, followed by how long it waited and what was found.
	WaitFailPrefix = "the change did not come after "
)

// Check is the change a step expects its one action to produce, written down before the action runs. Kind is one of the four constants above; Value is the text looked for, matched case-insensitively as a substring.
type Check struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Describe renders a check as the plain sentence a step record and the next round's prompt both carry. Input: the check. Output: the sentence, or "nothing in particular" when the check names no kind or no text.
func (c Check) Describe() string {
	if c.Value == "" {
		return "nothing in particular"
	}
	switch c.Kind {
	case TitleContains:
		return fmt.Sprintf("the title contains %q", c.Value)
	case ItemPresent:
		return fmt.Sprintf("an item labelled %q is showing", c.Value)
	case ItemAbsent:
		return fmt.Sprintf("no item labelled %q is showing", c.Value)
	case FieldHolds:
		return fmt.Sprintf("the focused field holds %q", c.Value)
	}
	return "nothing in particular"
}

// has reports whether any item's label contains value, case-insensitively.
func has(items []Item, value string) bool {
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Label), value) {
			return true
		}
	}
	return false
}

// Match reports whether the screen as it stands satisfies a step's expected change. Input: the check, the front window's title, the items observe_screen listed, and the text of the field with keyboard focus (empty when nothing is focused or its contents cannot be read). Output: whether it holds, and a plain sentence saying what was found, which goes into the step record either way.
func Match(c Check, title string, items []Item, focused string) (bool, string) {
	value := strings.ToLower(strings.TrimSpace(c.Value))
	if value == "" {
		return false, "the check said what to look at but not what to look for"
	}
	switch c.Kind {
	case TitleContains:
		if strings.Contains(strings.ToLower(title), value) {
			return true, fmt.Sprintf("the title is %q", title)
		}
		return false, fmt.Sprintf("the title is %q", title)
	case ItemPresent:
		if has(items, value) {
			return true, fmt.Sprintf("%q is in the list of %d items", c.Value, len(items))
		}
		return false, fmt.Sprintf("%q is not in the list of %d items", c.Value, len(items))
	case ItemAbsent:
		if has(items, value) {
			return false, fmt.Sprintf("%q is still in the list of %d items", c.Value, len(items))
		}
		return true, fmt.Sprintf("%q has gone from the list of %d items", c.Value, len(items))
	case FieldHolds:
		if strings.Contains(strings.ToLower(focused), value) {
			return true, fmt.Sprintf("the focused field holds %q", focused)
		}
		return false, fmt.Sprintf("the focused field holds %q", focused)
	}
	return false, fmt.Sprintf("there is no check called %q", c.Kind)
}
