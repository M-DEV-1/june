//go:build linux

package tracker

import (
	"context"
	"testing"
	"time"

	"ora/internal/act"
)

// observeDesktop is the fallback for a daemon that has not yet seen a window take focus; on this desktop it must find the browser or editor rather than the shell or Ora's own windows.
func TestObserveDesktop_FindsARealWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := dialA11y(ctx)
	if err != nil {
		t.Skip("no accessibility bus:", err)
	}
	defer conn.Close()
	app, title, nodes, err := observeDesktop(ctx, conn)
	if err != nil {
		t.Skip("nothing publishes on this desktop:", err)
	}
	if app == "gnome-shell" || IsOraWindow(app, title) {
		t.Errorf("observeDesktop picked %q · %q, want a real application window", app, title)
	}
	t.Logf("%s · %s: %d nodes, %d kept", app, title, len(nodes), len(act.Filter(nodes)))
}

// Ora's window reaches the accessibility bus under two application names: "ora", and "mutter-x11-frames" when the compositor owns the frame, which is the shape the stored episodes show ({App: "mutter-x11-frames", Title: "Ora"}). Only the title tells the second one apart from any other framed window, so observeDesktop has to read a candidate window's title before it can leave Ora's own out. Checking the application name against an empty title, as it did, lets Ora's own hover into the running and, on a desktop whose real window publishes little, win it — Ora would then describe its own buttons back to the user and click them.
func TestSkipWindow_CatchesOraUnderTheCompositorsFrame(t *testing.T) {
	if skipWindow("mutter-x11-frames", "") {
		t.Fatal("the premise no longer holds: the application name alone now identifies Ora's framed window")
	}
	cases := []struct {
		app, title string
		want       bool
	}{
		{"mutter-x11-frames", "Ora", true},
		{"ora", "Ora", true},
		{"gnome-shell", "", true},
		{"mutter-x11-frames", "Slack", false},
		{"brave", "PR #13 · GitHub", false},
	}
	for _, c := range cases {
		if got := skipWindow(c.app, c.title); got != c.want {
			t.Errorf("skipWindow(%q, %q) = %v, want %v", c.app, c.title, got, c.want)
		}
	}
}

// keptCount scores a window the way act.Filter lists one, so the two rules have to agree on every reason a node is dropped. This holds them together: change act.Filter and this fails.
func TestKeptCountAgreesWithFilter(t *testing.T) {
	nodes := []act.Node{
		{Role: "push button", Label: "Send", W: 80, H: 30, Showing: true},
		{Role: "push button", Label: "Hidden", W: 80, H: 30, Showing: false},
		{Role: "link", Label: "No size", W: 0, H: 30, Showing: true},
		{Role: "section", Label: "Not actionable", W: 80, H: 30, Showing: true},
		{Role: "link", Label: "", W: 80, H: 30, Showing: true},
		{Role: "entry", Label: "", W: 300, H: 30, Showing: true},
		{Role: "text", Label: "", W: 300, H: 30, Showing: true},
		{Role: "password text", Label: "", W: 300, H: 30, Showing: true},
		{Role: "table cell", Label: "42", W: 40, H: 20, Showing: true},
	}
	if got, want := keptCount(nodes), len(act.Filter(nodes)); got != want {
		t.Errorf("keptCount = %d, act.Filter kept %d: the two rules have drifted apart", got, want)
	}
}

// observeDesktop picks the window with the most actionable nodes. It used to score each one with len(act.Filter), which stops at act.MaxItems, so every window past that many scored exactly the same: a browser showing nine hundred elements tied with a side panel showing a hundred and fifty, and the tie went to whichever application the bus listed first. Scoring has to keep counting past the cap.
func TestKeptCountDoesNotSaturateAtTheListCap(t *testing.T) {
	window := func(n int) []act.Node {
		nodes := make([]act.Node, n)
		for i := range nodes {
			nodes[i] = act.Node{Role: "link", Label: "x", W: 10, H: 10, Showing: true}
		}
		return nodes
	}
	small, big := window(act.MaxItems), window(act.MaxItems*6)
	if len(act.Filter(small)) != len(act.Filter(big)) {
		t.Fatal("the premise no longer holds: act.Filter no longer caps the list")
	}
	if keptCount(small) >= keptCount(big) {
		t.Errorf("keptCount scored %d and %d: the bigger window has to win", keptCount(small), keptCount(big))
	}
}

// A listing is what the model reads coordinates off, and the drawing tool passes coordinates the model supplies straight through, so the numbers in a listing have to be real screen pixels. Brave is a native Wayland client: it cannot know where its own window sits on the desktop, so it answers a request for screen coordinates with window coordinates, and on 2026-09-05 its maximized frame reported 0,0 1920x1048 while _NET_WORKAREA said the work area starts at y=32. Every row of the list read out of that window was then 32 pixels high, the height of the top bar. One shift is worked out for the window and every node in the pass is moved by it.
func TestToScreen(t *testing.T) {
	screen := rect{X: 0, Y: 0, W: 1920, H: 1080}
	work := rect{X: 0, Y: 32, W: 1920, H: 1048}
	read := []act.Node{
		{Role: "entry", Label: "Address bar", X: 200, Y: 45, W: 1500, H: 30, Showing: true},
		{Role: "push button", Label: "Reload", X: 160, Y: 45, W: 30, H: 30, Showing: true},
		{Role: "link", Label: "Off screen", X: 0, Y: 0, W: 0, H: 0, Showing: false},
	}
	cases := []struct {
		name           string
		frame          rect
		work, screen   rect
		wantDX, wantDY int
	}{
		{name: "a maximized window reports window coordinates and the listing must be moved", frame: rect{X: 0, Y: 0, W: 1920, H: 1048}, work: work, screen: screen, wantDY: 32},
		{name: "a full screen window covers the top bar and its listing is already right", frame: rect{X: 0, Y: 0, W: 1920, H: 1080}, work: work, screen: screen},
		{name: "a floating window cannot be placed from its size and its listing is left alone", frame: rect{X: 300, Y: 200, W: 800, H: 600}, work: work, screen: screen},
		{name: "a maximized window that already answers correctly is left alone", frame: rect{X: 0, Y: 32, W: 1920, H: 1048}, work: work, screen: screen},
		{name: "no window rectangle could be read, so nothing is moved", frame: rect{}, work: work, screen: screen},
	}
	for _, c := range cases {
		nodes := append([]act.Node(nil), read...)
		toScreen(nodes, c.frame, c.work, c.screen)
		for i, got := range nodes {
			want := read[i]
			if want.W > 0 && want.H > 0 {
				want.X, want.Y = want.X+c.wantDX, want.Y+c.wantDY
			}
			if got != want {
				t.Errorf("%s: node %d came out %+v, want %+v", c.name, i, got, want)
			}
		}
	}
}

// observeDesktop used to walk every window with one visited-node budget shared for the whole desktop, so a first window big enough to spend the whole cap left kept == 0 for every window walked after it, and it could never be chosen no matter how little the first window actually had worth keeping. walkWithOwnBudget is what gives each window its own budget: this proves a huge first window does not poison the next window's count.
func TestWalkWithOwnBudget_GivesEachWindowItsOwnBudget(t *testing.T) {
	huge := func(visited *int) []act.Node {
		// Simulates a window whose subtree alone would spend the whole desktop-wide cap, like gnome-shell's own tree.
		for *visited < maxNodes+500 {
			*visited++
		}
		return nil
	}
	normal := func(visited *int) []act.Node {
		*visited += 5
		return []act.Node{{Role: "push button", Label: "Send", W: 10, H: 10, Showing: true}}
	}

	walkWithOwnBudget(context.Background(), huge)
	got := walkWithOwnBudget(context.Background(), normal)

	if kept := keptCount(got); kept != 1 {
		t.Fatalf("the window walked after a huge one kept %d nodes, want 1: its budget must start at zero, not wherever the previous window left off", kept)
	}
}
