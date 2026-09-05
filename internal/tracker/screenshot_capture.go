package tracker

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
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

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if nw == w && nh == h {
		draw.Draw(dst, dst.Bounds(), img, region.Min, draw.Src)
	} else {
		// ponytail: nearest-neighbour, the same as the vision tier's own downscale in jpeg.go. It aliases small text; a look is for pictures, video and games, where it does not show. Upgrade path: golang.org/x/image/draw's CatmullRom if text in a look ever has to be read.
		for y := 0; y < nh; y++ {
			sy := region.Min.Y + y*h/nh
			for x := 0; x < nw; x++ {
				dst.Set(x, y, img.At(region.Min.X+x*w/nw, sy))
			}
		}
	}

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
