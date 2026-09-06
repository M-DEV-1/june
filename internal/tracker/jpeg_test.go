package tracker

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// canvas paints a w x h test image with per-pixel noise so JPEG has something real to compress.
func canvas(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y % 253), B: uint8((x + y) % 255), A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func frameSize(t *testing.T, jpg []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestEncodeFrames_KeepsMonitorsAtNativeResolution is the bug: two 1920x1080 monitors used to come out of one 3840x1080 canvas as a single 960x270 image, where each monitor was a 480x270 postage stamp with no readable text. Each monitor must now be its own frame at its own resolution. It also covers the stacked layout where a small laptop panel sits under a larger external: each frame keeps its own size instead of being letterboxed into a shared one.
func TestEncodeFrames_KeepsMonitorsAtNativeResolution(t *testing.T) {
	cases := []struct {
		name  string
		img   image.Image
		rects []image.Rectangle
		want  [][2]int
	}{
		{
			name: "side by side, equal size",
			img:  canvas(3840, 1080),
			rects: []image.Rectangle{
				image.Rect(0, 0, 1920, 1080),
				image.Rect(1920, 0, 3840, 1080),
			},
			want: [][2]int{{1920, 1080}, {1920, 1080}},
		},
		{
			name: "stacked, mixed sizes",
			img:  canvas(1920, 2160),
			rects: []image.Rectangle{
				image.Rect(0, 0, 1920, 1080),
				image.Rect(0, 1080, 1366, 1848),
			},
			want: [][2]int{{1920, 1080}, {1366, 768}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			frames := encodeFrames(c.img, c.rects)
			if len(frames) != len(c.want) {
				t.Fatalf("got %d frames, want %d", len(frames), len(c.want))
			}
			for i, f := range frames {
				w, h := frameSize(t, f)
				if w != c.want[i][0] || h != c.want[i][1] {
					t.Errorf("frame %d is %dx%d, want %dx%d", i, w, h, c.want[i][0], c.want[i][1])
				}
			}
		})
	}
}

// TestEncodeFrames_CapsVeryWideMonitor checks the one case that still scales: a single monitor wider than the cap.
func TestEncodeFrames_CapsVeryWideMonitor(t *testing.T) {
	img := canvas(3840, 1080)
	frames := encodeFrames(img, []image.Rectangle{img.Bounds()})
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	w, h := frameSize(t, frames[0])
	if w != jpegMaxWidth {
		t.Errorf("width=%d, want the cap %d", w, jpegMaxWidth)
	}
	if h != 1080*jpegMaxWidth/3840 {
		t.Errorf("height=%d, want the aspect-preserving %d", h, 1080*jpegMaxWidth/3840)
	}
}

func TestScreenFrames_Empty(t *testing.T) {
	if screenFrames(nil) != nil {
		t.Error("no bytes in, no frames out")
	}
	if screenFrames([]byte("not an image")) != nil {
		t.Error("undecodable bytes should yield no frames")
	}
}

// TestScreenFrames_FallsBackToWholeCanvas: when the monitor layout is unknown (headless, no RandR, HiDPI mismatch), one frame of the whole screenshot is still better than none.
func TestScreenFrames_FallsBackToWholeCanvas(t *testing.T) {
	src := encodePNG(t, canvas(800, 600))
	frames := screenFrames(src)
	if len(frames) == 0 {
		t.Fatal("want at least one frame")
	}
	if w, h := frameSize(t, frames[0]); w == 0 || h == 0 {
		t.Fatalf("empty frame %dx%d", w, h)
	}
}

func TestOrderMonitors(t *testing.T) {
	wide := image.Rect(0, 0, 3840, 1080)
	left := image.Rect(0, 0, 1920, 1080)
	right := image.Rect(1920, 0, 3840, 1080)

	tall := image.Rect(0, 0, 1920, 2160)
	top := image.Rect(0, 0, 1920, 1080)
	bottom := image.Rect(0, 1080, 1920, 2160)

	cases := []struct {
		name    string
		canvas  image.Rectangle
		mons    []image.Rectangle
		pointer image.Point
		want    []image.Rectangle
	}{
		{"pointer on the right monitor puts it first", wide, []image.Rectangle{left, right}, image.Pt(2500, 400), []image.Rectangle{right, left}},
		{"pointer on the left monitor keeps reading order", wide, []image.Rectangle{left, right}, image.Pt(10, 10), []image.Rectangle{left, right}},
		{"unknown pointer falls back to reading order", wide, []image.Rectangle{right, left}, image.Pt(-1, -1), []image.Rectangle{left, right}},
		{"stacked layout reads top to bottom", tall, []image.Rectangle{bottom, top}, image.Pt(-1, -1), []image.Rectangle{top, bottom}},
		{"pointer on the bottom monitor puts it first", tall, []image.Rectangle{top, bottom}, image.Pt(100, 1500), []image.Rectangle{bottom, top}},
		{"mirrored monitors collapse to one frame", left, []image.Rectangle{left, left}, image.Pt(5, 5), []image.Rectangle{left}},
		{"no monitors means whole canvas", wide, nil, image.Pt(0, 0), nil},
		{"layout that does not cover the canvas means whole canvas", wide, []image.Rectangle{left}, image.Pt(0, 0), nil},
		{"monitor outside the canvas means whole canvas", left, []image.Rectangle{left, right}, image.Pt(0, 0), nil},
		{"empty monitors are ignored", wide, []image.Rectangle{left, right, {}}, image.Pt(-1, -1), []image.Rectangle{left, right}},
	}

	for _, c := range cases {
		got := orderMonitors(c.canvas, c.mons, c.pointer)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: rect %d is %v, want %v", c.name, i, got[i], c.want[i])
			}
		}
	}
}
