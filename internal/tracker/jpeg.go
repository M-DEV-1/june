package tracker

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"sort"
)

// Stored frames keep each monitor at its own resolution, because screen text is 12-16px and stops being readable — to the vision model and to anyone reading the memory later — below roughly 0.75x scale. Only a single monitor wider than this cap gets scaled down.
const jpegMaxWidth = 2560
const jpegQuality = 75

// screenFrames splits one screenshot of the whole desktop canvas into a JPEG per monitor, the monitor under the pointer first.
// Input: the raw PNG (or JPEG) bytes of the whole canvas. Output: one frame per monitor, or a single whole-canvas frame when the monitor layout is unavailable; nil when the bytes cannot be decoded, so a capture still proceeds without an image.
func screenFrames(src []byte) [][]byte {
	if len(src) == 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil
	}
	mons, pointer := screenLayout()
	rects := orderMonitors(img.Bounds(), mons, pointer)
	if len(rects) == 0 {
		rects = []image.Rectangle{img.Bounds()}
	}
	return encodeFrames(img, rects)
}

// orderMonitors turns the display server's monitor rectangles into the crop rectangles for one whole-canvas screenshot, the monitor holding the pointer first and the rest in reading order (top to bottom, then left to right).
// Input: the screenshot's own bounds, the monitor rectangles in canvas coordinates, and the pointer position — any point outside every monitor, such as (-1,-1), means "pointer unknown".
// Output: nil when the monitors do not add up to exactly the canvas, which is the case for rotated or HiDPI-scaled layouts where the rectangles and the screenshot pixels disagree; the caller then keeps the whole canvas as one frame.
func orderMonitors(canvas image.Rectangle, mons []image.Rectangle, pointer image.Point) []image.Rectangle {
	var rects []image.Rectangle
	seen := map[image.Rectangle]bool{}
	union := image.Rectangle{}
	for _, m := range mons {
		// Mirrored monitors report the same rectangle twice and would otherwise store the same pixels twice.
		if m.Empty() || seen[m] {
			continue
		}
		seen[m] = true
		rects = append(rects, m)
		union = union.Union(m)
	}
	if len(rects) == 0 || union != canvas {
		return nil
	}

	sort.Slice(rects, func(i, j int) bool {
		if rects[i].Min.Y != rects[j].Min.Y {
			return rects[i].Min.Y < rects[j].Min.Y
		}
		return rects[i].Min.X < rects[j].Min.X
	})

	for i, r := range rects {
		if pointer.In(r) {
			copy(rects[1:i+1], rects[:i])
			rects[0] = r
			break
		}
	}
	return rects
}

// encodeFrames crops img to each rectangle and encodes every crop as its own JPEG at jpegQuality, scaling down only a crop wider than jpegMaxWidth. Crops that fall outside the image or fail to encode are dropped.
func encodeFrames(img image.Image, rects []image.Rectangle) [][]byte {
	frames := make([][]byte, 0, len(rects))
	for _, r := range rects {
		if f := encodeFrame(img, r.Intersect(img.Bounds())); f != nil {
			frames = append(frames, f)
		}
	}
	if len(frames) == 0 {
		return nil
	}
	return frames
}

// encodeFrame returns the region r of img as JPEG bytes, at native resolution unless r is wider than jpegMaxWidth. Returns nil for an empty region or an encode failure.
func encodeFrame(img image.Image, r image.Rectangle) []byte {
	if r.Empty() {
		return nil
	}
	w, h := r.Dx(), r.Dy()
	nw, nh := w, h
	if w > jpegMaxWidth {
		nw, nh = jpegMaxWidth, h*jpegMaxWidth/w
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if nw == w && nh == h {
		draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
	} else {
		// Nearest-neighbor: no extra dependency, and this path only runs for a monitor wider than the cap.
		for y := 0; y < nh; y++ {
			sy := r.Min.Y + y*h/nh
			for x := 0; x < nw; x++ {
				dst.Set(x, y, img.At(r.Min.X+x*w/nw, sy))
			}
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil
	}
	return buf.Bytes()
}
