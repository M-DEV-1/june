package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"june/internal/act"
)

func observingAgent(t *testing.T) (*Agent, *[]string) {
	t.Helper()
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "brave", "PR #13 · GitHub", []act.Node{
			{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"},
			{Role: "link", Label: "Checks", X: 100, Y: 200, W: 60, H: 20, Showing: true, Ref: "r-checks"},
		}, nil
	}
	var rings []string
	a.Point = func(x, y, w, h int, label string) error {
		rings = append(rings, label+" "+strconv.Itoa(x)+" "+strconv.Itoa(y)+" "+strconv.Itoa(w)+" "+strconv.Itoa(h))
		return nil
	}
	// NewAgent defaults verify and extents to the tracker's, which talk to a real accessibility bus. point_at checks the element is still the one the list described and then reads back where it is before ringing it, so both are stood in for here against the fixture window.
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error { return nil }
	a.focused = func(context.Context, string) (bool, error) { return true, nil }
	a.extents = rectFromObserve(a)
	return a, &rings
}

// rectFromObserve stands in for tracker.Extents in these tests: it re-reads the fixture window and hands back the rectangle of the node carrying this ref, which is what the accessibility bus answers for an element that has not moved since the list was made. Input: the agent whose observe fake holds the fixture. Output: an extents function to assign to a.extents.
func rectFromObserve(a *Agent) func(ctx context.Context, ref string) (int, int, int, int, error) {
	return func(ctx context.Context, ref string) (int, int, int, int, error) {
		_, _, nodes, err := a.observe(ctx)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		for _, n := range nodes {
			if n.Ref == ref {
				return n.X, n.Y, n.W, n.H, nil
			}
		}
		return 0, 0, 0, 0, errors.New("no such element on screen")
	}
}

// observe_screen is the model's eyes: it names the window in front and lists its actionable nodes numbered, so the next tool call can refer to one by number instead of by guessing at pixels.
func TestExecuteTool_ObserveScreen_ListsNumberedNodes(t *testing.T) {
	a, _ := observingAgent(t)
	got := a.executeTool(context.Background(), "observe_screen", map[string]any{})
	if !strings.HasPrefix(got, "brave · PR #13 · GitHub\n") {
		t.Errorf("result does not start with the window line: %q", got)
	}
	if !strings.Contains(got, `[1] push button "Merge"`) || !strings.Contains(got, `[2] link "Checks"`) {
		t.Errorf("result lacks the numbered lines: %q", got)
	}
	// The number is the whole handle. A centre printed beside it is a coordinate in a third space no tool on this list takes, and the model spends it as if it were one (see act.TestFormatLines).
	if strings.Contains(got, "(50,35)") || strings.Contains(got, "(130,210)") {
		t.Errorf("the listing still publishes screen coordinates: %q", got)
	}
}

// point_at draws the ring around the node the model means, by the number observe_screen gave it, and only ever around something the model has actually seen in this session.
func TestExecuteTool_PointAt_RingsTheObservedNode(t *testing.T) {
	a, rings := observingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(2), "label": "here"})
	if len(*rings) != 1 || (*rings)[0] != "here 100 200 60 20" {
		t.Errorf("rings = %v, want one ring on node 2 labelled here", *rings)
	}
	if !strings.Contains(got, "Checks") {
		t.Errorf("result = %q, want it to name the node ringed", got)
	}
}

func TestExecuteTool_PointAt_RefusesWhatItHasNotSeen(t *testing.T) {
	a, rings := observingAgent(t)
	got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(1)})
	if len(*rings) != 0 || !strings.Contains(got, "observe_screen") {
		t.Errorf("before any observation point_at must refuse and say to observe first; rings=%v result=%q", *rings, got)
	}
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got = a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(9)})
	if len(*rings) != 0 || !strings.Contains(got, "9") {
		t.Errorf("an unknown number must be refused by number; rings=%v result=%q", *rings, got)
	}
}

type fakeActions struct {
	clicked      []string
	rightClicked []string
	scrolled     []string
	typed        []string
}

// fakeActions doubles as the keyboard and pointer, recording what type_text sent and taking every key or click without complaint.
func (f *fakeActions) TypeText(text string) error     { f.typed = append(f.typed, text); return nil }
func (f *fakeActions) PressKey(string) error          { return nil }
func (f *fakeActions) ClickAt(float64, float64) error { return nil }
func (f *fakeActions) RightClickAt(x, y float64) error {
	f.rightClicked = append(f.rightClicked, fmt.Sprintf("%.0f,%.0f", x, y))
	return nil
}
func (f *fakeActions) ScrollAt(float64, float64, int32) error { return nil }

func actingAgent(t *testing.T) (*Agent, *fakeActions) {
	t.Helper()
	a, _ := observingAgent(t)
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "brave", "PR #13 · GitHub", []act.Node{
			{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"},
			{Role: "entry", Label: "Address and search bar", X: 100, Y: 200, W: 600, H: 20, Showing: true, Ref: "r-address"},
		}, nil
	}
	f := &fakeActions{}
	a.doAction = func(ctx context.Context, ref string) (string, error) {
		f.clicked = append(f.clicked, ref)
		return "press", nil
	}
	a.scrollTo = func(ctx context.Context, ref string) error { f.scrolled = append(f.scrolled, ref); return nil }
	a.input = onceInput(func(context.Context) (InputDevice, error) { return f, nil })
	// NewAgent defaults verify to tracker.Verify, which talks to a real accessibility bus; a fake node's ref would fail that unconditionally, so tests stand in with one that always confirms the node still matches.
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error { return nil }
	return a, f
}

// click presses the node the model names by number, through the node's own accessibility action, so no pointer has to move and no pixel has to be guessed.
func TestExecuteTool_Click_PressesTheObservedNode(t *testing.T) {
	a, f := actingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 1 || f.clicked[0] != "r-merge" {
		t.Errorf("clicked = %v, want the Merge button's ref", f.clicked)
	}
	if !strings.Contains(got, "Merge") || !strings.Contains(got, "press") {
		t.Errorf("result = %q, want the node named and the action used", got)
	}
}

// Nothing in the accessibility tree opens a context menu: a node's showContextMenu is the one action pickAction never fires, and most toolkits publish no such action at all. A right-click therefore has to drive the real pointer's right button at the element, whatever actions that element offers.
func TestExecuteTool_Click_RightButtonOpensTheContextMenuWithThePointer(t *testing.T) {
	a, f := actingAgent(t)
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(2), "button": "right"})
	if len(f.rightClicked) != 1 || f.rightClicked[0] != "400,210" {
		t.Errorf("right clicks = %v, want one at the centre of the address bar", f.rightClicked)
	}
	if len(f.clicked) != 0 {
		t.Errorf("accessibility actions fired = %v, want none: none of them opens a context menu", f.clicked)
	}
	if !strings.Contains(got, "right-clicked") {
		t.Errorf("result = %q, want it to say it right-clicked", got)
	}
}

// A click must never land in whatever grabbed focus since the list was built — a desktop overview, a popup, another application — unless the request that started the turn actually named it. This covers the 2026-09-05 scroll run that ended in five clicks walking out of the browser and into the GNOME overview.
func TestExecuteTool_Click_RefusesWhenTheFrontWindowHasChanged(t *testing.T) {
	a, f := actingAgent(t)
	calls := 0
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		calls++
		if calls == 1 {
			return "brave", "PR #13 · GitHub", []act.Node{
				{Role: "push button", Label: "Merge", X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"},
			}, nil
		}
		return "gnome-shell", "Activities", nil, nil
	}
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "click", map[string]any{"n": float64(1)})
	if len(f.clicked) != 0 {
		t.Errorf("clicked = %v, want no click once the front window changed out from under the list", f.clicked)
	}
	if !strings.Contains(got, "gnome-shell") || !strings.Contains(got, "PR #13") {
		t.Errorf("result = %q, want it to name both the window now in front and the one the list came from", got)
	}
}

// type_text sends keystrokes to whatever has keyboard focus, which is why the description tells the model to click the field first; "enter" appends a newline so a search can be submitted in the same call.
func TestExecuteTool_TypeText_TypesAndOptionallySubmits(t *testing.T) {
	a, f := actingAgent(t)
	got := a.executeTool(context.Background(), "type_text", map[string]any{"text": "hello"})
	if len(f.typed) != 1 || f.typed[0] != "hello" || !strings.Contains(got, "typed") {
		t.Errorf("typed = %v result = %q", f.typed, got)
	}
	a.executeTool(context.Background(), "type_text", map[string]any{"text": "june", "enter": true})
	if len(f.typed) != 2 || f.typed[1] != "june\n" {
		t.Errorf("typed = %v, want june with a newline when enter is set", f.typed)
	}
	if got := a.executeTool(context.Background(), "type_text", map[string]any{}); !strings.Contains(got, "text") {
		t.Errorf("type_text without text must say so, got %q", got)
	}
}

// show_marks puts a numbered mark over every element observe_screen listed, so the user can see on their own screen which number the model means without asking it to ring them one at a time.
func TestExecuteTool_ShowMarks_MarksEveryObservedItem(t *testing.T) {
	a, _ := observingAgent(t)
	var marked []act.Item
	a.Marks = func(items []act.Item) error { marked = items; return nil }
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	got := a.executeTool(context.Background(), "show_marks", map[string]any{})
	if len(marked) != 2 || marked[0].N != 1 || marked[1].N != 2 {
		t.Errorf("marked = %+v, want the two observed items numbered 1 and 2", marked)
	}
	if !strings.Contains(got, "2") {
		t.Errorf("result = %q, want it to say how many were marked", got)
	}
}

// A look at a screen that has not moved since the last look costs a full listing and tells the model nothing it does not already have. The daemon keeps the last listing, so a repeat look says so in one line, and the numbers from the earlier list stay the ones that count.
func TestExecuteTool_ObserveScreen_SaysUnchangedWhenNothingMoved(t *testing.T) {
	a, _ := observingAgent(t)
	first := a.executeTool(context.Background(), "observe_screen", map[string]any{})
	if !strings.Contains(first, `[1] push button "Merge"`) {
		t.Fatalf("the first look must carry the whole list: %q", first)
	}
	second := a.executeTool(context.Background(), "observe_screen", map[string]any{})
	if strings.Contains(second, `[1] push button "Merge"`) {
		t.Errorf("a second identical look sent the list again: %q", second)
	}
	if !strings.HasPrefix(second, "brave · PR #13 · GitHub\n") {
		t.Errorf("a repeat look must still name the window: %q", second)
	}
	if !strings.Contains(second, "unchanged") || !strings.Contains(second, "2 items") {
		t.Errorf("a repeat look must say it is unchanged and how many items stand: %q", second)
	}
	// The numbers have to keep working: a repeat look is a shorter answer, not a forgotten list.
	if got := a.executeTool(context.Background(), "point_at", map[string]any{"n": float64(1)}); !strings.Contains(got, "ringed [1]") {
		t.Errorf("point_at after a repeat look = %q, want the number still to resolve", got)
	}
}

// When the window is the same and only a line or two moved — a count, a status, a label — the changed lines are what the model needs, not the hundred that did not move. The list is only sent whole when the change is too big to state as a few lines.
func TestExecuteTool_ObserveScreen_SendsOnlyTheChangedLines(t *testing.T) {
	a, _ := observingAgent(t)
	label := "Merge"
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return "brave", "PR #13 · GitHub", []act.Node{
			{Role: "push button", Label: label, X: 10, Y: 20, W: 80, H: 30, Showing: true, Ref: "r-merge"},
			{Role: "link", Label: "Checks", X: 100, Y: 200, W: 60, H: 20, Showing: true, Ref: "r-checks"},
		}, nil
	}
	a.executeTool(context.Background(), "observe_screen", map[string]any{})
	label = "Merged"
	got := a.executeTool(context.Background(), "observe_screen", map[string]any{})
	if !strings.Contains(got, `[1] push button "Merged"`) {
		t.Errorf("the changed line must be sent: %q", got)
	}
	if strings.Contains(got, `[2] link "Checks"`) {
		t.Errorf("an unchanged line was sent again: %q", got)
	}
	if !strings.Contains(got, "changed") {
		t.Errorf("the result must say these are the changed lines: %q", got)
	}
}
