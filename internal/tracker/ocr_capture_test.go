package tracker

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"
)

// tinyPNG is a real, decodable picture, because a vision capture splits the screenshot into frames and a stand-in byte string would give none.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A window that publishes no text is read off the screenshot by the OCR program, but only then and only as often as vision would be: the picture is not taken for a window that has text, for the bare desktop, or when the machine has no OCR program, and a second capture inside the vision interval does not run it again. Pictures are kept only when a model saw them.
func TestTieredCapture_OCRFallback(t *testing.T) {
	shot := tinyPNG(t)
	longText := strings.Repeat("a line of text from the page ", 20)
	seesNothing := func(context.Context, []byte) Sight { return Sight{} }
	seesSomething := func(context.Context, []byte) Sight { return Sight{UserActivity: "reading a manual"} }

	cases := []struct {
		name       string
		app        string
		a11y       string
		vision     func(context.Context, []byte) Sight
		ocrText    string
		noOCR      bool
		wantText   string
		wantShots  int
		wantOCR    int
		wantFrames bool
	}{
		{name: "blind window, no model", app: "Brave", ocrText: "text from pixels", wantText: "text from pixels", wantShots: 1, wantOCR: 1},
		{name: "window with text", app: "Brave", a11y: longText, ocrText: "text from pixels", wantText: longText},
		{name: "window with a little text", app: "Brave", a11y: "Save", ocrText: "text from pixels", wantText: "Save"},
		{name: "model reads the screen", app: "Brave", vision: seesSomething, ocrText: "text from pixels", wantText: "reading a manual", wantShots: 1, wantFrames: true},
		{name: "model reads nothing", app: "Brave", vision: seesNothing, ocrText: "text from pixels", wantText: "text from pixels", wantShots: 1, wantOCR: 1, wantFrames: true},
		{name: "bare desktop", app: "gnome-shell", ocrText: "wallpaper caption"},
		{name: "no OCR program", app: "Brave", noOCR: true},
		{name: "OCR finds nothing", app: "Brave", wantText: "a page title", wantShots: 1, wantOCR: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewDaemon(nil, time.Second, time.Second, nil, nil)
			var shots, reads int
			d.text = func() (screenRead, error) { return screenRead{text: c.a11y}, nil }
			d.media = func(context.Context) bool { return false }
			d.screenshot = func(context.Context) ([]byte, error) { shots++; return shot, nil }
			d.visionFn = c.vision
			d.ocr = func(context.Context, []byte) string { reads++; return c.ocrText }
			if c.noOCR {
				d.ocr = nil
			}

			var lastA11y, lastVision string
			var lastVisionTime time.Time
			act := Activity{App: c.app, Title: "a page title"}
			out := d.tieredCapture(context.Background(), act, &lastA11y, &lastVision, &lastVisionTime)

			if out.text != c.wantText {
				t.Errorf("text = %q, want %q", out.text, c.wantText)
			}
			if shots != c.wantShots || reads != c.wantOCR {
				t.Errorf("screenshots = %d, OCR runs = %d, want %d and %d", shots, reads, c.wantShots, c.wantOCR)
			}
			if got := len(out.frames) > 0; got != c.wantFrames {
				t.Errorf("stored a picture = %v, want %v", got, c.wantFrames)
			}

			// A second capture straight after is inside the vision interval, so the screen is not read again.
			d.tieredCapture(context.Background(), act, &lastA11y, &lastVision, &lastVisionTime)
			if shots != c.wantShots || reads != c.wantOCR {
				t.Errorf("after a second capture: screenshots = %d, OCR runs = %d, want %d and %d", shots, reads, c.wantShots, c.wantOCR)
			}
		})
	}
}

// An OCR run that never returns costs the capture its bound and no more, like every other call out of the capture path.
func TestTieredCapture_ReturnsWhenOCRNeverAnswers(t *testing.T) {
	forever := make(chan struct{})
	defer close(forever)

	d := NewDaemon(nil, time.Second, time.Second, nil, nil)
	d.bounds.ocr = 20 * time.Millisecond
	d.text = func() (screenRead, error) { return screenRead{}, nil }
	d.media = func(context.Context) bool { return false }
	d.screenshot = func(context.Context) ([]byte, error) { return []byte("png"), nil }
	d.visionFn = nil
	d.ocr = func(context.Context, []byte) string { <-forever; return "" }

	var lastA11y, lastVision string
	var lastVisionTime time.Time
	done := make(chan captureOut, 1)
	go func() {
		done <- d.tieredCapture(context.Background(), Activity{App: "Brave", Title: "a page title"}, &lastA11y, &lastVision, &lastVisionTime)
	}()
	select {
	case out := <-done:
		if out.text != "a page title" {
			t.Fatalf("text = %q, want the window title", out.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the capture did not return after its OCR bound passed")
	}
}
