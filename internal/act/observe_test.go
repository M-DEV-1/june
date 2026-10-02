package act

import (
	"strings"
	"testing"
)

// On this desktop a raw window tree ran to about 19,000 tokens while the clickable nodes alone were about 1,300 (measured 2026-09-04 with probe_tree.py), so the filter is what makes looking at the screen affordable. It keeps a node only when it is showing, has a size, plays a role a person can act on or read, and either carries a label or is a place to type.
// A password box is kept, because an empty box is still a target, but never with its text: the walk falls back to a node's own text when it has no name, so a revealed password arrives as the label, and from the list it would reach the model and the notes written off its results.
func TestFilterKeepsOnlyShowingActionableNodes(t *testing.T) {
	const secret = "correct horse battery staple"
	nodes := []Node{
		{Role: "push button", Label: "Send", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: ":1.5|/org/a11y/atspi/accessible/42"},
		{Role: "push button", Label: "Hidden", X: 10, Y: 20, W: 80, H: 30, Showing: false},
		{Role: "link", Label: "No size", X: 10, Y: 20, W: 0, H: 30, Showing: true},
		{Role: "section", Label: "Not actionable", X: 10, Y: 20, W: 80, H: 30, Showing: true},
		{Role: "link", Label: "", X: 10, Y: 20, W: 80, H: 30, Showing: true},
		{Role: "entry", Label: "", X: 100, Y: 200, W: 300, H: 30, Showing: true},
		{Role: "table cell", Label: "42", X: 5, Y: 5, W: 40, H: 20, Showing: true},
		{Role: "password text", Label: secret, X: 100, Y: 250, W: 300, H: 30, Showing: true, Ref: "pw"},
	}
	items := Filter(nodes)
	if len(items) != 4 {
		t.Fatalf("Filter kept %d items, want 4: %+v", len(items), items)
	}
	if items[3].Role != "password text" || items[3].Label != "" {
		t.Errorf("password item = %+v, want it kept with no label", items[3])
	}
	if got := Format(items); strings.Contains(got, secret) {
		t.Errorf("Format wrote the password onto a line the model reads:\n%s", got)
	}
	if items[0].N != 1 || items[0].Label != "Send" || items[0].Ref != ":1.5|/org/a11y/atspi/accessible/42" {
		t.Errorf("first item = %+v, want [1] Send carrying its ref", items[0])
	}
	if items[1].N != 2 || items[1].Role != "entry" {
		t.Errorf("second item = %+v, want the unlabeled entry as [2]", items[1])
	}
	if items[2].N != 3 || items[2].Label != "42" {
		t.Errorf("third item = %+v, want the table cell as [3]", items[2])
	}
}

// A chat window lists its transcript before its compose box and its Send button, and a busy one runs past the cap long before the walk reaches them. Stopping at the cap in tree order therefore hands the model a page of read-only messages with nothing on it to press: the button is not merely far down the list, it is absent, and the model cannot press what it cannot see. The read-only nodes are what the cap gives up.
func TestFilterCapGivesUpReadingBeforeControls(t *testing.T) {
	var nodes []Node
	for i := 0; i < MaxItems+50; i++ {
		nodes = append(nodes, Node{Role: "text", Label: "a message from someone", X: 300, Y: 100 + i, W: 600, H: 20, Showing: true})
	}
	nodes = append(nodes,
		Node{Role: "text", Label: "", X: 300, Y: 900, W: 600, H: 40, Showing: true, Ref: "compose"},
		Node{Role: "push button", Label: "Send", X: 920, Y: 900, W: 60, H: 40, Showing: true, Ref: "send"},
	)
	items := Filter(nodes)
	if len(items) != MaxItems {
		t.Fatalf("Filter kept %d items, want the cap %d", len(items), MaxItems)
	}
	var refs []string
	for _, it := range items {
		if it.Ref != "" {
			refs = append(refs, it.Ref)
		}
	}
	if len(refs) != 2 || refs[0] != "compose" || refs[1] != "send" {
		t.Errorf("the compose box and the Send button reached the list as %v, want both of them, in tree order", refs)
	}
	// point_at, click and scroll_to all resolve a number as items[n-1], so the numbering has to stay dense and in list order however the cap cut.
	for i, it := range items {
		if it.N != i+1 {
			t.Fatalf("item %d is numbered %d, want %d", i, it.N, i+1)
		}
	}
}
