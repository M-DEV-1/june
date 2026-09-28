package agent

// End-to-end tests for the screen loop: a question goes to a scripted model, the model's tool calls run through the real dispatch, and those tools read and move a stand-in desktop that behaves the way the real ones did on the days these failures were logged.
//
// What makes these different from the tests beside them: nothing here reaches into one function. Each case names a dated incident from june.log, replays the tool calls that session actually made, and asserts the task got done. Every failure in the 2026-09 audit lived at a seam — the accessibility read against the numbered list, the pointer against the compositor, the look budget against the step budget — and a test that drives one side of a seam cannot see any of them. The fakes stop at the process boundary: the accessibility bus, the screenshot camera, the portal, and the model's HTTP endpoint. Everything between them is the real code.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"june/internal/act"
	"june/internal/input"
	"june/internal/tracker"
)

// flatPNG is a plain grey picture, the shape the camera hands back. The screen tests here are about the accessibility seam, not the pixels, so every capture is the same picture and the press check has nothing to judge either way — which is the "cannot say" answer it passes through.
func flatPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// shadePNG is a small picture of one flat shade, so a test can hand the screen a different picture each time it needs one to have changed.
func shadePNG(shade byte) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	for i := range img.Pix {
		img.Pix[i] = shade
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// fakeDesktop stands in for the machine's screen: which application is in front, what it is called, and the elements it publishes. A test moves it between tool calls the way a real application moves under a model that is working on it — a page scrolls, a toggle renames itself, a click starts audio and the window title grows a suffix.
type fakeDesktop struct {
	app, title string
	items      []act.Node
	// clicks and keys record what actually reached the pointer and the keyboard, so a test can tell a refusal apart from a press that went out and was reported as a miss.
	clicks []string
	keys   []string
	// onClick runs when an element is pressed, standing in for whatever the application does about it.
	onClick func(d *fakeDesktop, ref string)
	// frame is the picture the camera returns, and pixels is what the press check compares; changing it is how a test says the screen did or did not react.
	pixels []byte
}

// node finds an element by the reference observe_screen listed it under, or nil once the application has destroyed it.
func (d *fakeDesktop) node(ref string) *act.Node {
	for i := range d.items {
		if d.items[i].Ref == ref {
			return &d.items[i]
		}
	}
	return nil
}

// desktopAgent wires an Agent to a fakeDesktop through every seam the screen loop crosses: the accessibility walk, the per-element verify, the extents read, the action fire, the pointer, the keyboard and the camera. Input: the desktop to drive. Output: the agent.
// The verify goes through tracker.VerifyAgainst, the same decision the bus-backed read makes, so a test here cannot pass against a rule this file invented.
func desktopAgent(t *testing.T, d *fakeDesktop) *Agent {
	t.Helper()
	a := NewAgent(nil, nil, &toolTestBrain{}, nil, "FAKE_API_KEY")
	a.observe = func(ctx context.Context) (string, string, []act.Node, error) {
		return d.app, d.title, append([]act.Node(nil), d.items...), nil
	}
	a.verify = func(ctx context.Context, ref, role, label string, x, y, w, h int) error {
		n := d.node(ref)
		if n == nil {
			return tracker.VerifyAgainst("", "", role, label)
		}
		return tracker.VerifyAgainst(n.Role, n.Label, role, label)
	}
	a.extents = func(ctx context.Context, ref string) (int, int, int, int, error) {
		n := d.node(ref)
		if n == nil {
			return 0, 0, 0, 0, fmt.Errorf("no such element on screen")
		}
		return n.X, n.Y, n.W, n.H, nil
	}
	a.doAction = func(ctx context.Context, ref string) (string, error) {
		d.clicks = append(d.clicks, ref)
		if d.onClick != nil {
			d.onClick(d, ref)
		}
		return "press", nil
	}
	a.capture = func(ctx context.Context) (tracker.Capture, error) {
		return tracker.Capture{Data: d.pixels, Mime: "image/png", X: 0, Y: 32, W: 1280, H: 704, Scale: 2}, nil
	}
	a.input = onceInput(func(ctx context.Context) (InputDevice, error) { return &desktopInput{d: d}, nil })
	a.Tap = func(x, y int, label string) error { return nil }
	return a
}

// desktopInput stands in for the portal's keyboard and pointer. Unlike a recording fake it refuses a key name the real session would refuse, through the same parser, so a spelling the model reaches for that this keyboard does not know is visible from here rather than only from inside internal/input.
type desktopInput struct{ d *fakeDesktop }

func (i *desktopInput) PressKey(name string) error {
	if err := input.ValidChord(name); err != nil {
		return err
	}
	i.d.keys = append(i.d.keys, name)
	return nil
}

func (i *desktopInput) TypeText(text string) error {
	i.d.keys = append(i.d.keys, "type "+text)
	return nil
}

func (i *desktopInput) ClickAt(x, y float64) error {
	i.d.clicks = append(i.d.clicks, fmt.Sprintf("point %.0f,%.0f", x, y))
	return nil
}

func (i *desktopInput) RightClickAt(x, y float64) error {
	i.d.clicks = append(i.d.clicks, fmt.Sprintf("right-point %.0f,%.0f", x, y))
	return nil
}

func (i *desktopInput) ScrollAt(x, y float64, dy int32) error {
	i.d.clicks = append(i.d.clicks, fmt.Sprintf("scroll %.0f,%.0f %d", x, y, dy))
	return nil
}

// observed runs one observe_screen so the numbered list the following tools resolve against exists, the way every real screen task begins. Output: the ask's context carrying that list and the look allowance, and the listing itself.
func observed(t *testing.T, a *Agent) (context.Context, string) {
	t.Helper()
	ctx := withAskLookState(context.Background())
	listing := a.executeTool(ctx, "observe_screen", map[string]any{})
	if strings.HasPrefix(listing, "error") {
		t.Fatalf("observe_screen: %s", listing)
	}
	return ctx, listing
}

// TestE2E_ScreenTasksThatUsedToStall replays the screen tasks that ran into the seams, one dated incident per case. Each drives the real tool dispatch; the only fakes are the bus, the camera and the pointer.
func TestE2E_ScreenTasksThatUsedToStall(t *testing.T) {
	cases := []struct {
		name    string
		desktop func() *fakeDesktop
		// run issues the tool calls the logged session made, and returns what the model was told each time.
		run func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string
		// check is handed what the first observe_screen listed, then what each of run's calls answered.
		check func(t *testing.T, d *fakeDesktop, listing string, said []string)
	}{
		{
			// 2026-09-11T13:05, Spotify. click n=37 muted the track, which renamed the button, and the next two clicks on the same number were refused for the rename. The model then spent a full observe_screen re-listing to press a button whose reference had never changed.
			name: "a toggle pressed twice",
			desktop: func() *fakeDesktop {
				d := &fakeDesktop{app: "Chromium", title: "Spotify Premium", pixels: flatPNG()}
				d.items = []act.Node{{Role: "push button", Label: "Mute", X: 100, Y: 200, W: 60, H: 30, Ref: "mute", Showing: true}}
				d.onClick = func(d *fakeDesktop, ref string) {
					if n := d.node(ref); n != nil {
						if n.Label == "Mute" {
							n.Label = "Unmute"
						} else {
							n.Label = "Mute"
						}
					}
				}
				return d
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				return []string{
					a.executeTool(ctx, "click", map[string]any{"n": 1.0}),
					a.executeTool(ctx, "click", map[string]any{"n": 1.0}),
				}
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				if len(d.clicks) != 2 {
					t.Fatalf("the button was pressed %d times, want 2: muting and unmuting", len(d.clicks))
				}
				if strings.HasPrefix(said[1], "error") {
					t.Errorf("the second press was refused: %q", said[1])
				}
			},
		},
		{
			// 2026-09-10T12:35-12:41, Teams calendar. The listing and the verify read were shifted by different amounts, so every element looked 32 pixels out — the height of the GNOME top bar — and fourteen clicks in a row were refused with "the page has moved under the list". The rectangle is no longer part of the comparison, so an element that has genuinely moved is still the element.
			name: "the page moved under the list",
			desktop: func() *fakeDesktop {
				d := &fakeDesktop{app: "Google Chrome", title: "Calendar | Microsoft Teams", pixels: flatPNG()}
				d.items = []act.Node{{Role: "entry", Label: "Add title", X: 501, Y: 223, W: 649, H: 40, Ref: "title", Showing: true}}
				return d
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				// The page scrolls between the listing and the click, exactly the 32 pixels the shift was out by.
				d.items[0].Y = 191
				return []string{a.executeTool(ctx, "click", map[string]any{"n": 1.0})}
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				if len(d.clicks) != 1 {
					t.Errorf("the field was pressed %d times, want 1; said %q", len(d.clicks), said[0])
				}
			},
		},
		{
			// 2026-09-09T03:14-03:19, Brave. A click started a video, so the window title grew "- Audio playing -", and the next click was refused because the front window was "not the one observe_screen listed this element in". The agent was refusing its own successful action. The application half is what identifies the window now; the title is left to the element check.
			name: "the click changed the window title",
			desktop: func() *fakeDesktop {
				d := &fakeDesktop{app: "Brave Browser", title: "piano music - YouTube - Brave", pixels: flatPNG()}
				d.items = []act.Node{
					{Role: "push button", Label: "Play", X: 400, Y: 300, W: 40, H: 40, Ref: "play", Showing: true},
					{Role: "push button", Label: "Full screen", X: 800, Y: 300, W: 40, H: 40, Ref: "full", Showing: true},
				}
				d.onClick = func(d *fakeDesktop, ref string) {
					if ref == "play" {
						d.title = "piano music - YouTube - Audio playing - Brave"
					}
				}
				return d
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				return []string{
					a.executeTool(ctx, "click", map[string]any{"n": 1.0}),
					a.executeTool(ctx, "click", map[string]any{"n": 2.0}),
				}
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				if len(d.clicks) != 2 {
					t.Fatalf("clicks = %v, want the play button and then full screen", d.clicks)
				}
				if strings.HasPrefix(said[1], "error") {
					t.Errorf("the second click was refused after the first changed the title: %q", said[1])
				}
			},
		},
		{
			// The other half of the same rule, from 2026-09-12T21:31: the user alt-tabbed and gnome-shell came to the front mid-task. A different application really is a different window, and clicking into it would press something nobody listed.
			name: "another application came to the front",
			desktop: func() *fakeDesktop {
				d := &fakeDesktop{app: "Brave Browser", title: "YouTube - Brave", pixels: flatPNG()}
				d.items = []act.Node{{Role: "push button", Label: "Play", X: 400, Y: 300, W: 40, H: 40, Ref: "play", Showing: true}}
				return d
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				d.app, d.title = "gnome-shell", ""
				return []string{a.executeTool(ctx, "click", map[string]any{"n": 1.0})}
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				if len(d.clicks) != 0 {
					t.Errorf("a click went out into another application: %v", d.clicks)
				}
				if !strings.HasPrefix(said[0], "error") {
					t.Errorf("result = %q, want the click refused", said[0])
				}
			},
		},
		{
			// 2026-09-12T21:34. The model asked for Page_Down, the X11 keysym spelling, and was refused; it then spent the rest of its fifty steps pressing Down instead and ran out without answering.
			name: "the key written the way a model writes it",
			desktop: func() *fakeDesktop {
				return &fakeDesktop{app: "Brave Browser", title: "YouTube - Brave", pixels: flatPNG()}
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				return []string{
					a.executeTool(ctx, "press_key", map[string]any{"keys": "Page_Down"}),
					a.executeTool(ctx, "press_key", map[string]any{"keys": "ctrl+Page_Up"}),
				}
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				for i, got := range said {
					if strings.HasPrefix(got, "error") {
						t.Errorf("press %d refused: %q", i, got)
					}
				}
			},
		},
		{
			// 2026-09-10T11:24 and 17:20. The numbered listing printed each element's centre in desktop pixels beside its number, and the model spent those as if they were picture points: click_at at 3232,821 is one monitor's width plus a second-monitor x, read straight off the list and refused for being outside a 1280-wide picture. The list publishes the number and nothing else now.
			name: "the listing offers no coordinate to misuse",
			desktop: func() *fakeDesktop {
				d := &fakeDesktop{app: "gnome-control-center", title: "Settings", pixels: flatPNG()}
				d.items = []act.Node{{Role: "list item", Label: "JBL Wave Beam 2", X: 2722, Y: 454, W: 308, H: 25, Ref: "jbl", Showing: true}}
				return d
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string { return nil },
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				// Any parenthesised pair at all: the centre this element used to be published under was (2876,466), which is neither number in its rectangle, so checking for the rectangle's own numbers would have missed it.
				if pair := regexp.MustCompile(`\(\d+,\d+\)`).FindString(listing); pair != "" {
					t.Errorf("the listing still hands the model a coordinate %s: %q", pair, listing)
				}
				if !strings.Contains(listing, `[1] list item "JBL Wave Beam 2"`) {
					t.Errorf("listing = %q, want the number, the role and the label", listing)
				}
			},
		},
		{
			// 2026-09-20T20:19-20:21, GNOME Clocks. The window publishes its four tabs and nothing else: the timer's digits are drawn by the app, so the accessibility tree is identical before and after every click on them. Two looks in, the third was refused with "I have already looked at the screen 2 times this turn" and the remaining 24 calls of the run were blind. The allowance is given back by the ask loop at a model round, and the three CLI brains (claude, codex, agy) have no round boundary this side of the tool server, so on those it was never given back at all.
			// The same run was told "(unchanged since the last look" six times, which is true of the tree and says nothing about the screen, and was refused twice by type_text for a list item holding the keyboard without being told that press_key reaches a window like this one.
			name: "a window that draws its own fields",
			desktop: func() *fakeDesktop {
				d := &fakeDesktop{app: "org.gnome.clocks", title: "", pixels: flatPNG()}
				d.items = []act.Node{
					{Role: "tab", Label: "World", X: 500, Y: 40, W: 60, H: 30, Ref: "world", Showing: true},
					{Role: "tab", Label: "Alarms", X: 570, Y: 40, W: 60, H: 30, Ref: "alarms", Showing: true},
					{Role: "tab", Label: "Stopwatch", X: 640, Y: 40, W: 80, H: 30, Ref: "stopwatch", Showing: true},
					{Role: "tab", Label: "Timer", X: 730, Y: 40, W: 60, H: 30, Ref: "timer", Showing: true},
				}
				return d
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				// An unlabelled list item is what really held the keyboard once the Timer tab was open.
				holdsKeyboard(t, act.Node{Role: "list item", Label: "", Ref: "digits"}, true)
				return []string{
					a.executeTool(ctx, "look", map[string]any{}),
					a.executeTool(ctx, "click", map[string]any{"n": 4.0}),
					a.executeTool(ctx, "look", map[string]any{}),
					a.executeTool(ctx, "observe_screen", map[string]any{}),
					a.executeTool(ctx, "press_key", map[string]any{"keys": "Down"}),
					a.executeTool(ctx, "look", map[string]any{}),
					a.executeTool(ctx, "type_text", map[string]any{"text": "15"}),
				}
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				for i, got := range []string{said[0], said[2], said[5]} {
					if strings.Contains(got, "already looked") {
						t.Errorf("look %d was refused: %q; a run that acts between looks must get a picture each time", i+1, got)
					}
				}
				if !strings.Contains(said[3], "accessibility tree") {
					t.Errorf("the repeat listing = %q, want it to say the tree is all it read, so an unchanged tree is not read as an action that did nothing", said[3])
				}
				if !strings.Contains(said[6], "press_key") {
					t.Errorf("the typing refusal = %q, want it to name press_key, the way into a window that publishes no field", said[6])
				}
			},
		},
		{
			// 49 of the 91 looks filed between 2026-09-16 and 09-23 were refused with "I have already looked at the screen 2 times this turn", and the runs that were refused looked again and again. A count of looks says nothing about whether there is anything new to see: a video, a page still loading or a window that redraws itself changes with no action in between, and a screen nobody touched does not. The only look worth refusing is one at the very picture the model already has.
			name: "a look is refused only when the screen is the picture already seen",
			desktop: func() *fakeDesktop {
				return &fakeDesktop{app: "Brave Browser", title: "Video", pixels: shadePNG(10)}
			},
			run: func(t *testing.T, a *Agent, ctx context.Context, d *fakeDesktop) []string {
				said := []string{a.executeTool(ctx, "look", map[string]any{}), a.executeTool(ctx, "look", map[string]any{})}
				for shade := byte(20); shade <= 50; shade += 10 {
					d.pixels = shadePNG(shade)
					said = append(said, a.executeTool(ctx, "look", map[string]any{}))
				}
				return said
			},
			check: func(t *testing.T, d *fakeDesktop, listing string, said []string) {
				if !strings.Contains(said[1], "has not changed") {
					t.Errorf("a second look at the same screen = %q, want it told the screen has not changed since the picture it has", said[1])
				}
				for i, got := range append([]string{said[0]}, said[2:]...) {
					if !strings.Contains(got, "here is the picture") {
						t.Errorf("look %d at a changed screen = %q, want a picture", i+1, got)
					}
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pressSettle, tapLead = 0, 0
			d := c.desktop()
			a := desktopAgent(t, d)
			ctx, listing := observed(t, a)
			c.check(t, d, listing, c.run(t, a, ctx, d))
		})
	}
}

// TestE2E_AScreenTaskGetsToLookAfterEachAction drives a whole ask — question in, answer out — against a scripted model, so the look allowance is spent the way a real run spends it: look, act, look, act, look.
// The allowance used to be attached once per ask and never given back, so a task with a fifty-step budget had two pictures for the whole of it and was refused on the third with "I have already looked at the screen 2 times this turn" — 57 times in the eight days to 2026-09-12, the most frequent refusal in the log, and the message named a turn boundary that nothing reset. Nothing below reaches into the look state; it is spent and given back by the loop itself.
func TestE2E_AScreenTaskGetsToLookAfterEachAction(t *testing.T) {
	pressSettle, tapLead = 0, 0
	d := &fakeDesktop{app: "Brave Browser", title: "a long article - Brave", pixels: flatPNG()}
	d.items = []act.Node{{Role: "push button", Label: "Next page", X: 400, Y: 600, W: 90, H: 30, Ref: "next", Showing: true}}
	a := desktopAgent(t, d)

	// The model looks, acts, looks, acts, looks, then answers — three pictures across five rounds, which is the shape of "scroll through this and tell me what it says".
	rounds := []string{
		toolCallResponse("observe_screen", nil),
		toolCallResponse("look", nil),
		toolCallResponse("click", map[string]any{"n": 1}),
		toolCallResponse("look", nil),
		toolCallResponse("click", map[string]any{"n": 1}),
		toolCallResponse("look", nil),
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Read it, it is about laptop GPUs."}]}}]}`,
	}
	sent := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if sent >= len(rounds) {
			t.Errorf("the model was called %d times, more than the script has", sent+1)
			io.WriteString(w, rounds[len(rounds)-1])
			return
		}
		io.WriteString(w, rounds[sent])
		sent++
	}))
	defer backend.Close()
	geminiBaseURL = backend.URL
	t.Cleanup(func() { geminiBaseURL = "" })

	tr, err := a.AskText(context.Background(), "scroll through this page and tell me what it says")
	if err != nil {
		t.Fatalf("AskText: %v", err)
	}

	looks, refusals := 0, 0
	for _, hop := range tr.ToolHops {
		if hop.Name != "look" {
			continue
		}
		looks++
		if strings.Contains(hop.Result, "already looked") {
			refusals++
		}
	}
	if refusals != 0 {
		t.Errorf("%d of %d looks were refused; a task that acts between looks must get a picture each round", refusals, looks)
	}
	if looks != 3 {
		t.Errorf("the run took %d looks, want the 3 the model asked for", looks)
	}
	if len(d.clicks) != 2 {
		t.Errorf("the run clicked %d times, want 2: it must get through the whole task, not stall on a refusal", len(d.clicks))
	}
	if !strings.Contains(tr.Answer, "laptop GPUs") {
		t.Errorf("answer = %q, want the model's own final text", tr.Answer)
	}
	for i, hop := range tr.ToolHops {
		t.Logf("hop %d: %s %v -> %.120s", i, hop.Name, hop.Args, hop.Result)
	}
}

// toolCallResponse builds one scripted model round that calls a tool, in the shape the Gemini HTTP API returns it. Input: the tool name and its arguments. Output: the JSON body the fake backend writes.
func toolCallResponse(name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	call, err := json.Marshal(map[string]any{"name": name, "args": args})
	if err != nil {
		panic(err)
	}
	return `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":` + string(call) + `}]}}]}`
}

// 2026-09-19: a Spotify job and a WhatsApp ask drove the screen at the same time, each switching windows under the other, and the ask ended on the Spotify window it had never meant to touch. One task drives the screen at a time: a second one is told who has it, and gets it once the first has ended.
func TestE2E_TwoTasksDoNotFightOverTheScreen(t *testing.T) {
	pressSettle, tapLead = 0, 0
	d := &fakeDesktop{app: "Brave Browser", title: "WhatsApp", pixels: flatPNG()}
	d.items = []act.Node{{Role: "push button", Label: "Play", X: 10, Y: 10, W: 20, H: 20, Ref: "play", Showing: true}}
	a := desktopAgent(t, d)

	jobCtx, endJob := context.WithCancel(WithQuestion(withAskLookState(context.Background()), "play a song on Spotify"))
	a.executeTool(jobCtx, "observe_screen", map[string]any{})
	if got := a.executeTool(jobCtx, "click", map[string]any{"n": 1.0}); strings.HasPrefix(got, "error") {
		t.Fatalf("the first task's click = %q", got)
	}

	askCtx := WithQuestion(withAskLookState(context.Background()), "open the WhatsApp group")
	a.executeTool(askCtx, "observe_screen", map[string]any{})
	got := a.executeTool(askCtx, "click", map[string]any{"n": 1.0})
	if !strings.Contains(got, "play a song on Spotify") {
		t.Errorf("a second task's click while the first drives the screen = %q, want a refusal naming the task that has it", got)
	}

	endJob()
	if got := a.executeTool(askCtx, "click", map[string]any{"n": 1.0}); strings.HasPrefix(got, "error") {
		t.Errorf("the click once the first task ended = %q, want it to go ahead", got)
	}
}
