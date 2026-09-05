// Package act holds the pure half of computer use: turning a window's accessibility nodes into the short numbered list a model can act on. Nothing here touches the bus or the screen.
package act

import (
	"fmt"
	"strings"
)

// Node is one accessibility node as the tracker read it: its role, the label a person would use for it (its name, or its text when it has no name), its rectangle in screen pixels, and whether it is showing on screen right now.
type Node struct {
	Role, Label string
	X, Y, W, H  int
	Showing     bool
	// Ref is the tracker's handle on the node (a bus name and object path) so an action can be fired on it later.
	Ref string
}

// Item is one line of the list the model sees: a number to refer to it by, and the node's role, label and rectangle.
type Item struct {
	N           int
	Role, Label string
	X, Y, W, H  int
	Ref         string
}

// MaxItems caps the list; a longer one costs more tokens without making the choice any easier. Each line is about thirteen tokens, so the cap is also what one look at the screen costs the model: a hundred items is roughly 1,300 tokens, and a screen task takes many looks.
// A hundred, measured on 2026-09-05 from the element numbers the model actually acted on across every run in the user's store: 49 clicks, rings and scrolls, the highest of them item 84 and the 95th percentile item 56. That leaves the cap about a fifth above the highest number ever needed, and takes roughly 650 tokens off every look at the pages long enough to hit it. What a lower cap gives up first is read-only text, not controls — see the drop order below — so the items being lost are the ones a model could only read, never press.
const MaxItems = 100

// labelRunes is the longest label a line carries.
const labelRunes = 60

// actionable is the set of roles a person can click, type into, or read a value from. Measured on this desktop on 2026-09-04: keeping only these took a Teams page from 19,000 tokens to about 1,300.
var actionable = map[string]bool{
	"push button": true, "toggle button": true, "check box": true, "radio button": true,
	"menu item": true, "check menu item": true, "radio menu item": true, "menu": true,
	"link": true, "text": true, "entry": true, "password text": true, "combo box": true,
	"list item": true, "tab": true, "page tab": true, "tree item": true, "slider": true,
	"spin button": true, "table cell": true,
}

// Actionable reports whether a node with this role belongs in the list. Input: an AT-SPI role name such as "push button". Output: true for roles a person can act on or read a value from.
func Actionable(role string) bool { return actionable[role] }

// typable is the roles kept even without a label, because an empty box to type in is still a target.
var typable = map[string]bool{"entry": true, "text": true, "password text": true}

// secretRoles are the roles whose label is the user's secret rather than a name for the thing: a password box. The walk that reads a window falls back to a node's own text when it has no name, so a password the user has revealed, or one a toolkit publishes regardless, arrives here as that node's label. Filter drops it, because from the list it reaches the model, the click and point_at results, and the notes written off those results.
var secretRoles = map[string]bool{"password text": true}

// contentRoles are the roles whose label may be the node's own contents rather than a name a person chose: a password box, a run of page text, and a box the user types into. Callers that write a label down somewhere lasting use this to refuse it and say the role instead.
var contentRoles = map[string]bool{"password text": true, "text": true, "entry": true}

// ContentRole reports whether a node of this role may carry its own contents as its label. Input: an AT-SPI role name such as "entry". Output: true for a password box, a run of text or an entry, whose label can be what is on the screen — a password, a half-written message — and so must not be stored or written into a note.
func ContentRole(role string) bool { return contentRoles[role] }

// toRead is the roles that carry a window's content rather than its controls: a run of page text, a cell of a table, a row of a list. A page holds far more of these than it holds buttons, and they are what a list at the cap gives up first, because a button the model cannot see is a button it cannot press, while a paragraph it cannot see costs it a paragraph.
var toRead = map[string]bool{"text": true, "table cell": true, "list item": true}

// readOnly reports whether a node is there to be read rather than acted on. Input: a node that has already passed the keep rules. Output: true for a labelled run of text, table cell or list row; false for every control, and false for an unlabelled node of a typable role, since an empty box to type in is a target and not a piece of content.
func readOnly(n Node) bool { return toRead[n.Role] && n.Label != "" }

// Filter keeps the nodes worth showing the model and numbers them in tree order. Input: the raw nodes of one window. Output: at most MaxItems items, each showing, actionable, of positive size, and labelled unless it is a place to type; a password box is kept, since an empty box to type into is still a target, but never with a label (see secretRoles).
// When more nodes pass the keep rules than the cap allows, the ones given up are the read-only ones (see readOnly), taken from the end of the tree; the controls all survive, still in tree order. Cutting the tail of the tree instead, which is what stopping at the cap does, drops whatever the window lists last, and a web app lists its transcript before its compose box and its Send button.
func Filter(nodes []Node) []Item {
	kept := make([]Node, 0, 64)
	for _, n := range nodes {
		if !n.Showing || !Actionable(n.Role) || n.W <= 0 || n.H <= 0 {
			continue
		}
		if secretRoles[n.Role] {
			n.Label = ""
		}
		if n.Label == "" && !typable[n.Role] {
			continue
		}
		kept = append(kept, n)
	}
	// How much room is left for read-only nodes once every control has its place. Negative means the controls alone overrun the cap, in which case the list is the first MaxItems of them.
	room := len(kept)
	if len(kept) > MaxItems {
		controls := 0
		for _, n := range kept {
			if !readOnly(n) {
				controls++
			}
		}
		room = MaxItems - controls
	}
	items := make([]Item, 0, min(len(kept), MaxItems))
	for _, n := range kept {
		if len(items) == MaxItems {
			break
		}
		if readOnly(n) {
			if room <= 0 {
				continue
			}
			room--
		}
		items = append(items, Item{N: len(items) + 1, Role: n.Role, Label: n.Label, X: n.X, Y: n.Y, W: n.W, H: n.H, Ref: n.Ref})
	}
	return items
}

// Format renders the list one item per line as `[n] role "label" (cx,cy)`, the centre being the point a ring or a click is aimed at. Input: the filtered items. Output: the lines joined with newlines, or "" for none.
func Format(items []Item) string {
	lines := make([]string, len(items))
	for i, it := range items {
		label := []rune(it.Label)
		if len(label) > labelRunes {
			label = append(label[:labelRunes-1], '…')
		}
		lines[i] = fmt.Sprintf("[%d] %s %q (%d,%d)", it.N, it.Role, string(label), it.X+it.W/2, it.Y+it.H/2)
	}
	return strings.Join(lines, "\n")
}
