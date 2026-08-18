package tracker

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png"
)

// Memory thumbnail: small enough to keep a year of lecture captures on disk, large enough to re-read a slide.
const jpegMaxWidth = 960
const jpegQuality = 55

// encodeJPEG decodes png/jpeg bytes, downsamples if wider than jpegMaxWidth, and re-encodes as JPEG. Returns nil on failure so the capture still proceeds without an image.
func encodeJPEG(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > jpegMaxWidth && w > 0 {
		nh := h * jpegMaxWidth / w
		dst := image.NewRGBA(image.Rect(0, 0, jpegMaxWidth, nh))
		// Nearest-neighbor: no extra dependency, good enough for a memory thumbnail.
		for y := 0; y < nh; y++ {
			sy := b.Min.Y + y*h/nh
			for x := 0; x < jpegMaxWidth; x++ {
				sx := b.Min.X + x*w/jpegMaxWidth
				dst.Set(x, y, img.At(sx, sy))
			}
		}
		img = dst
	} else {
		// Ensure a concrete type jpeg.Encode accepts without extra alpha surprises.
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
		img = rgba
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil
	}
	return buf.Bytes()
}
