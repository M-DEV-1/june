package tracker

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
)

// maxLookSide is the longest side, in pixels, of an image handed to a model. Above this the picture costs more tokens without telling the model anything more: 1280 across is enough to read a face, a video frame or a game, which is what a look is for.
const maxLookSide = 1280

// maxLookBytes is the most one capture may weigh. A data URL of it goes in the request body, so this is a real ceiling on what a look costs to send, and the encoder drops its quality until the image fits.
const maxLookBytes = 400 * 1024

// lookQualities are the JPEG qualities tried in turn until the image fits under maxLookBytes. A screen full of flat colour encodes far below the cap at 80; a photograph or a video frame at full width may not, and losing a little quality is better than sending nothing.
var lookQualities = []int{80, 60, 40}

// Capture is one screenshot handed to a model: the encoded image, its media type, where the region's top-left corner sits in screen coordinates, the image's own size in pixels, and how many screen pixels one captured pixel is worth.
// The last three are what make a point the model reads off the picture usable: without them a coordinate in the image is a coordinate in nothing.
type Capture struct {
	Data  []byte
	Mime  string
	X, Y  int
	W, H  int
	Scale float64
}

// ToScreen turns a point in the captured image into the point on the screen it names. Input: the point in image pixels, with 0,0 at the image's top-left. Output: the screen coordinates.
func (c Capture) ToScreen(x, y int) (int, int) {
	return c.X + int(float64(x)*c.Scale), c.Y + int(float64(y)*c.Scale)
}

// Holds reports whether a point in image pixels is inside this capture, so a caller can refuse a coordinate that was guessed rather than read off the picture.
func (c Capture) Holds(x, y int) bool {
	return x >= 0 && y >= 0 && x < c.W && y < c.H
}

// encodeCapture crops img to region, scales it so its longer side is at most maxLookSide, and encodes it as a JPEG under maxLookBytes. Input: a decoded screenshot and the region of it to send, in the screenshot's own coordinates, which are screen coordinates. Output: the capture, or an error for an empty region or an image that will not encode.
func encodeCapture(img image.Image, region image.Rectangle) (Capture, error) {
	region = region.Intersect(img.Bounds())
	if region.Empty() {
		return Capture{}, errors.New("there is nothing in that part of the screen to capture")
	}
	w, h := region.Dx(), region.Dy()
	longer := w
	if h > longer {
		longer = h
	}
	scale := 1.0
	nw, nh := w, h
	if longer > maxLookSide {
		scale = float64(longer) / float64(maxLookSide)
		nw, nh = int(float64(w)/scale), int(float64(h)/scale)
	}

	dst := shrink(img, region, nw, nh)

	var buf bytes.Buffer
	for _, quality := range lookQualities {
		buf.Reset()
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
			return Capture{}, err
		}
		if buf.Len() <= maxLookBytes {
			break
		}
	}
	return Capture{Data: bytes.Clone(buf.Bytes()), Mime: "image/jpeg", X: region.Min.X, Y: region.Min.Y, W: nw, H: nh, Scale: scale}, nil
}

// shrink copies region of img into a new image of nw by nh pixels, at the region's own size when that is what is asked for. Input: the image, the region of it (already inside its bounds and not empty) and the size wanted, no larger than the region. Output: the RGBA image.
// A smaller picture is an area average: each pixel it holds is the mean of the part of the region it covers, every source pixel weighted by how much of it falls inside. The nearest-neighbour sampling this replaces kept one source pixel in every 1.5 and dropped the rest, so at the non-whole scales a 125% or 150% desktop is sent at, a one-pixel stroke of small text survived in some letters and vanished in others (an end-to-end look at Notepad read "third" as "lhird"), and look is how the model reads text that observe_screen cannot list.
func shrink(img image.Image, region image.Rectangle, nw, nh int) *image.RGBA {
	w, h := region.Dx(), region.Dy()
	if nw == w && nh == h {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), img, region.Min, draw.Src)
		return dst
	}
	src, ok := img.(*image.RGBA)
	if ok {
		src = src.SubImage(region).(*image.RGBA)
	} else {
		// Any other image is copied into RGBA once, so the loop below reads bytes rather than paying an interface call and a colour conversion for every source pixel.
		src = image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(src, src.Bounds(), img, region.Min, draw.Src)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	cols, rows := boxSpans(w, nw), boxSpans(h, nh)
	// acc is one output row's worth of source rows, added up with their weights and not yet narrowed; working a row at a time keeps the extra memory to one row of the region, not a copy of all of it.
	acc := make([]float32, 4*w)
	b := src.Bounds()
	for y, rs := range rows {
		clear(acc)
		for k, wy := range rs.weights {
			off := src.PixOffset(b.Min.X, b.Min.Y+rs.first+k)
			row := src.Pix[off : off+len(acc)]
			for i, v := range row {
				acc[i] += wy * float32(v)
			}
		}
		out := dst.Pix[y*dst.Stride:]
		for x, cs := range cols {
			var px [4]float32
			for k, wx := range cs.weights {
				p := acc[4*(cs.first+k):]
				px[0] += wx * p[0]
				px[1] += wx * p[1]
				px[2] += wx * p[2]
				px[3] += wx * p[3]
			}
			for c, v := range px {
				out[4*x+c] = uint8(min(v+0.5, 255))
			}
		}
	}
	return dst
}

// boxSpan is the run of source pixels one output pixel covers: the first of them, and how much each from there on counts towards it.
type boxSpan struct {
	first   int
	weights []float32
}

// boxSpans divides n source pixels among m output pixels, m no more than n. Output: one boxSpan per output pixel, its weights the share of each source pixel that falls inside it divided by the output pixel's width, so they add up to 1.
func boxSpans(n, m int) []boxSpan {
	spans := make([]boxSpan, m)
	scale := float64(n) / float64(m)
	for i := range spans {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		first, last := int(lo), min(int(math.Ceil(hi)), n)
		ws := make([]float32, 0, last-first)
		for s := first; s < last; s++ {
			ws = append(ws, float32((min(hi, float64(s+1))-max(lo, float64(s)))/scale))
		}
		spans[i] = boxSpan{first: first, weights: ws}
	}
	return spans
}

// screenGuard is what takes June's own hover window off the screen for the moment a picture of it is taken. The daemon wires it to a call that tells the window to conceal itself and hands back the call that puts it where it was.
// It exists because the compositor has only one screen to photograph: the hover is drawn over whatever the user was looking at, so a question asked from the hover about the window behind it came back with a picture of June's own card sitting in the middle of the answer.
var screenGuard func() func()

// SetScreenGuard wires screenGuard; the daemon calls it once at startup. Passing nil takes the guard away, which leaves every capture taking the screen exactly as it is.
func SetScreenGuard(f func() func()) { screenGuard = f }

// standAside runs the guard and returns the call that undoes it, so a capture can write `defer standAside()()`. Input: none. Output: a func to run once the picture has been taken, which does nothing when no guard is wired.
func standAside() func() {
	if screenGuard == nil {
		return func() {}
	}
	if back := screenGuard(); back != nil {
		return back
	}
	return func() {}
}
