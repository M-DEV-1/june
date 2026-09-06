package act

import (
	"fmt"
	"strings"
	"testing"
)

// On this desktop a raw window tree ran to about 19,000 tokens while the clickable nodes alone were about 1,300 (measured 2026-09-04 with probe_tree.py), so the filter is what makes looking at the screen affordable. It keeps a node only when it is showing, has a size, plays a role a person can act on or read, and either carries a label or is a place to type.
func TestFilterKeepsOnlyShowingActionableNodes(t *testing.T) {
	nodes := []Node{
		{Role: "push button", Label: "Send", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: ":1.5|/org/a11y/atspi/accessible/42"},
		{Role: "push button", Label: "Hidden", X: 10, Y: 20, W: 80, H: 30, Showing: false},
		{Role: "link", Label: "No size", X: 10, Y: 20, W: 0, H: 30, Showing: true},
		{Role: "section", Label: "Not actionable", X: 10, Y: 20, W: 80, H: 30, Showing: true},
		{Role: "link", Label: "", X: 10, Y: 20, W: 80, H: 30, Showing: true},
		{Role: "entry", Label: "", X: 100, Y: 200, W: 300, H: 30, Showing: true},
		{Role: "table cell", Label: "42", X: 5, Y: 5, W: 40, H: 20, Showing: true},
	}
	items := Filter(nodes)
	if len(items) != 3 {
		t.Fatalf("Filter kept %d items, want 3: %+v", len(items), items)
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

// A page can carry thousands of links; past the cap the list stops, because a model reading two hundred numbered lines is no better at choosing than one reading one hundred and fifty, and it costs more.
func TestFilterCapsTheList(t *testing.T) {
	nodes := make([]Node, MaxItems+40)
	for i := range nodes {
		nodes[i] = Node{Role: "link", Label: "x", X: 1, Y: 1, W: 10, H: 10, Showing: true}
	}
	if got := len(Filter(nodes)); got != MaxItems {
		t.Errorf("Filter kept %d items, want the cap %d", got, MaxItems)
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

// The cut takes the read-only nodes from the end of the tree, so what the model reads is the top of the window rather than an arbitrary slice of it, and the list stays in the order the window lists things.
func TestFilterCapCutsTheTailAndKeepsTreeOrder(t *testing.T) {
	nodes := []Node{{Role: "push button", Label: "Reply", X: 0, Y: 0, W: 40, H: 20, Showing: true, Ref: "reply"}}
	for i := 0; i < MaxItems+10; i++ {
		nodes = append(nodes, Node{Role: "table cell", Label: "row", X: 0, Y: 30 + i*20, W: 200, H: 20, Showing: true, Ref: "cell" + string(rune('a'+i%26))})
	}
	items := Filter(nodes)
	if len(items) != MaxItems || items[0].Ref != "reply" {
		t.Fatalf("Filter kept %d items starting at %q, want %d starting at the button", len(items), items[0].Ref, MaxItems)
	}
	for i, it := range items[1:] {
		if it.Role != "table cell" || it.Y != 30+i*20 {
			t.Fatalf("item %d = %+v, want the table cells in tree order from the top of the window", i+1, it)
		}
	}
}

// Every line the model sees is number, role, label and the centre of the node, since the centre is what a click or a ring is aimed at; labels are cut so one long link cannot eat the budget.
func TestFormatLines(t *testing.T) {
	items := []Item{
		{N: 1, Role: "push button", Label: "Send", X: 10, Y: 20, W: 80, H: 30},
		{N: 2, Role: "link", Label: strings.Repeat("long ", 30), X: 0, Y: 0, W: 10, H: 10},
	}
	got := Format(items)
	lines := strings.Split(got, "\n")
	if lines[0] != `[1] push button "Send" (50,35)` {
		t.Errorf("line 1 = %q", lines[0])
	}
	if len(lines[1]) > 90 {
		t.Errorf("line 2 is %d chars, want the label cut: %q", len(lines[1]), lines[1])
	}
	if Format(nil) != "" {
		t.Errorf("Format(nil) = %q, want empty", Format(nil))
	}
}

// A password box is kept in the list, because an empty box to type into is still a target, but it must never carry what is in it. The walk falls back to a node's own text when it has no name, so a password the user has revealed, or one a toolkit publishes anyway, arrives here as the node's label; from the list it reaches the model, the click and point_at results, and the notes written off those results.
func TestFilterNeverCarriesAPasswordFieldsText(t *testing.T) {
	const secret = "correct horse battery staple"
	items := Filter([]Node{
		{Role: "password text", Label: secret, X: 100, Y: 200, W: 300, H: 30, Showing: true, Ref: "pw"},
		{Role: "push button", Label: "Sign in", X: 100, Y: 250, W: 80, H: 30, Showing: true, Ref: "in"},
	})
	if len(items) != 2 {
		t.Fatalf("Filter kept %d items, want the password box and the button: %+v", len(items), items)
	}
	if items[0].Role != "password text" || items[0].Label != "" {
		t.Errorf("password item = %+v, want it kept with no label", items[0])
	}
	if items[1].Label != "Sign in" {
		t.Errorf("button item = %+v, want its label untouched", items[1])
	}
	if got := Format(items); strings.Contains(got, secret) {
		t.Errorf("Format wrote the password onto a line the model reads:\n%s", got)
	}
}

// highestEverActedOn is the largest item number the model has ever clicked, ringed or scrolled, counted on 2026-09-05 across the 49 such actions in every run then in the user's store; the 95th of them was item 56. It is a reading taken once, not one this test can retake — the store it came from is not available here — so it is written down as a number and the test below holds the list to it.
const highestEverActedOn = 84

// maxAffordableItems is the longest list worth sending: each line is roughly thirteen tokens, so 120 of them is about 1,560 tokens on every look, and a screen task takes many looks.
const maxAffordableItems = 120

// A screen with far more actionable nodes than the cap must still produce a list that reaches item 84, because a cap that loses the item the model needed costs more than the tokens it saved, and must still stop short of maxAffordableItems. This fails in both directions, and it fails through Filter rather than by reading MaxItems, so a change to how the cap is applied is caught as well as a change to the number.
func TestFilter_ListReachesTheHighestNumberEverActedOn(t *testing.T) {
	nodes := make([]Node, 500)
	for i := range nodes {
		nodes[i] = Node{Role: "push button", Label: fmt.Sprintf("Button %d", i+1), X: 0, Y: i * 20, W: 100, H: 18, Showing: true}
	}
	items := Filter(nodes)

	if len(items) < highestEverActedOn {
		t.Errorf("Filter gave %d items from a 500-node screen, so item %d — the highest number a recorded run has ever acted on — cannot be reached", len(items), highestEverActedOn)
	}
	if len(items) > maxAffordableItems {
		t.Errorf("Filter gave %d items, about %d tokens a look, and nothing in the record has ever needed a list that long", len(items), len(items)*13)
	}
	if n := len(items); n > 0 && items[n-1].N != n {
		t.Errorf("last item is numbered %d in a list of %d; the numbers must run 1..n so a number the model holds resolves", items[n-1].N, n)
	}
}
