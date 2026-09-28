package cmd

import (
	"testing"

	"june/internal/window"
)

// pickPlacedWindow is what closes the Teams bug: the accessibility bus's window placer used to match by exact title alone, and Teams' title carries a live memory count ("High memory usage - 985 MB") that had changed to "1,005 MB" by the time the click that followed the listing asked for it, so the lookup missed and every rectangle in the listing was shifted by the fallback's guess instead of the real one — refusing the click every time. Matching by pid first, since the shell extension and the accessibility bus agree on it even when the title has moved on, is the fix.
func TestPickPlacedWindow(t *testing.T) {
	teams := window.Window{Pid: 4242, Title: "Teams - High memory usage - 985 MB", Focused: true, X: 501, Y: 191, W: 649, H: 823}
	teamsStale := window.Window{Pid: 4242, Title: "Teams - High memory usage - 1,005 MB", Focused: true, X: 501, Y: 191, W: 649, H: 823}
	other := window.Window{Pid: 1, Title: "Brave", X: 0, Y: 0, W: 1920, H: 1080}

	cases := []struct {
		name    string
		windows []window.Window
		pid     uint32
		title   string
		want    window.Window
		wantOK  bool
	}{
		{name: "exact title among the pid's windows wins first", windows: []window.Window{other, teams}, pid: 4242, title: teams.Title, want: teams, wantOK: true},
		{name: "the title changed since the listing, so the pid's focused window is used instead", windows: []window.Window{other, teamsStale}, pid: 4242, title: teams.Title, want: teamsStale, wantOK: true},
		{name: "a pid with exactly one window is used even unfocused and untitled", windows: []window.Window{other, {Pid: 4242, Title: "something else", X: 9, Y: 9, W: 9, H: 9}}, pid: 4242, title: "not this either", want: window.Window{Pid: 4242, Title: "something else", X: 9, Y: 9, W: 9, H: 9}, wantOK: true},
		{name: "pid 0 falls back to the exact title across every window", windows: []window.Window{other, teams}, pid: 0, title: teams.Title, want: teams, wantOK: true},
		{name: "an unknown pid falls back to the exact title across every window", windows: []window.Window{other, teams}, pid: 9999, title: teams.Title, want: teams, wantOK: true},
		{name: "nothing matches at all", windows: []window.Window{other}, pid: 9999, title: "gone", wantOK: false},
	}
	for _, c := range cases {
		got, ok := pickPlacedWindow(c.windows, c.pid, c.title)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: pickPlacedWindow(...) = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
